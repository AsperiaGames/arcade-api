package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/HoseaCodes/arcade-api/internal/auth"
	"github.com/HoseaCodes/arcade-api/internal/config"
	"github.com/HoseaCodes/arcade-api/internal/httpapi"
	"github.com/HoseaCodes/arcade-api/internal/ledger"
	"github.com/HoseaCodes/arcade-api/internal/store"
)

// End-to-end through the real router, against a real Postgres and a real signing
// key. These exist to pin the wire contract: blog-portfolio's React app and 13
// arcade game builds parse these exact bodies, and none of them can be redeployed
// on our schedule.

const (
	serviceToken    = "test-service-token-long-enough-to-be-accepted"
	maxSingleEarn   = 500
	maxDailyEarn    = 1000
	maxOfflineClaim = 5000
	allowedOrigin   = "https://asperiagames.com"
)

var (
	testPool *pgxpool.Pool
	issuer   *testIssuer
)

type testIssuer struct {
	url string
	key *rsa.PrivateKey
	srv *httptest.Server
}

func newTestIssuer() (*testIssuer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	ti := &testIssuer{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
		}}}
		_ = json.NewEncoder(w).Encode(set)
	})
	ti.srv = httptest.NewServer(mux)
	ti.url = ti.srv.URL
	return ti, nil
}

func (ti *testIssuer) token(t *testing.T, id string, guest bool) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: ti.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"id": id, "isGuest": guest, "iss": ti.url,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("arcade"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n(is Docker running?)\n", err)
		os.Exit(1)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dsn: %v\n", err)
		os.Exit(1)
	}
	if err := store.Migrate(dsn); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	if testPool, err = store.Connect(ctx, dsn); err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}
	if issuer, err = newTestIssuer(); err != nil {
		fmt.Fprintf(os.Stderr, "issuer: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	issuer.srv.Close()
	testPool.Close()
	_ = testcontainers.TerminateContainer(ctr)
	os.Exit(code)
}

// newServer returns a handler over a freshly emptied schema.
func newServer(t *testing.T) http.Handler {
	t.Helper()
	const truncate = `TRUNCATE receipts, holds, earn_windows, guest_earnings, accounts RESTART IDENTITY CASCADE`
	if _, err := testPool.Exec(context.Background(), truncate); err != nil {
		t.Fatalf("reset: %v", err)
	}

	cfg := config.Config{
		Port:            8080,
		StormGateURL:    issuer.url,
		ServiceToken:    serviceToken,
		AllowedOrigins:  []string{allowedOrigin},
		MaxSingleEarn:   maxSingleEarn,
		MaxDailyEarn:    maxDailyEarn,
		MaxOfflineClaim: maxOfflineClaim,
		HoldTTL:         time.Minute,
	}
	l := ledger.New(testPool, cfg.MaxDailyEarn, cfg.HoldTTL)
	v := auth.NewVerifier(context.Background(), issuer.url, "")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.New(cfg, l, v, log).Routes()
}

type response struct {
	code int
	body map[string]any
}

func do(t *testing.T, h http.Handler, method, path, token string, body any) response {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = 1 // non-zero so optional-body handlers decode
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	out := response{code: rec.Code}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out.body); err != nil {
			t.Fatalf("decode response (%d): %v\nbody: %s", rec.Code, err, rec.Body.String())
		}
	}
	return out
}

func num(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	v, ok := body[key]
	if !ok {
		t.Fatalf("response has no %q field: %v", key, body)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%q = %v, not a number", key, v)
	}
	return f
}

func TestHealthReportsVersionAndCommit(t *testing.T) {
	h := newServer(t)
	res := do(t, h, http.MethodGet, "/health", "", nil)

	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.code)
	}
	// Both existing services can report neither, which cost real time working
	// out what was deployed. This one answers from day one.
	for _, key := range []string{"status", "version", "commit"} {
		if _, ok := res.body[key]; !ok {
			t.Errorf("/health has no %q field: %v", key, res.body)
		}
	}
}

func TestPointsRoutesRequireAuth(t *testing.T) {
	h := newServer(t)
	for _, path := range []string{"/api/points/balance", "/api/points/transactions"} {
		res := do(t, h, http.MethodGet, path, "", nil)
		if res.code != http.StatusUnauthorized {
			t.Errorf("%s unauthenticated = %d, want 401", path, res.code)
		}
		if res.body["msg"] == nil {
			t.Errorf("%s: error body has no msg field: %v", path, res.body)
		}
	}
}

func TestBalanceShape(t *testing.T) {
	h := newServer(t)
	res := do(t, h, http.MethodGet, "/api/points/balance", issuer.token(t, "u1", false), nil)

	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.code)
	}
	if res.body["status"] != "success" {
		t.Errorf("status = %v, want success", res.body["status"])
	}
	// The React app destructures every one of these.
	for _, key := range []string{
		"balance", "lifetimeEarned", "lifetimeSpent", "lifetimePurchased", "claimedOffline",
	} {
		if _, ok := res.body[key]; !ok {
			t.Errorf("balance response missing %q: %v", key, res.body)
		}
	}
}

func TestEarnCreditsAndReportsBalance(t *testing.T) {
	h := newServer(t)
	tok := issuer.token(t, "u1", false)

	res := do(t, h, http.MethodPost, "/api/points/earn", tok,
		map[string]any{"amount": 120, "gameId": "pac-man", "gameName": "Pac-Man"})

	if res.code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %v", res.code, res.body)
	}
	if got := num(t, res.body, "credited"); got != 120 {
		t.Errorf("credited = %v, want 120", got)
	}
	if got := num(t, res.body, "balance"); got != 120 {
		t.Errorf("balance = %v, want 120", got)
	}
}

func TestEarnRejectsAboveTheSingleCallCap(t *testing.T) {
	h := newServer(t)
	res := do(t, h, http.MethodPost, "/api/points/earn", issuer.token(t, "u1", false),
		map[string]any{"amount": maxSingleEarn + 1})

	if res.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.code)
	}
}

// 429, specifically. The arcade client maps that status to `daily-cap` and stops
// retrying; a 400 would read as a malformed request and be retried.
func TestEarnReturns429WhenTheDailyBudgetIsGone(t *testing.T) {
	h := newServer(t)
	tok := issuer.token(t, "u1", false)

	for range maxDailyEarn / maxSingleEarn {
		res := do(t, h, http.MethodPost, "/api/points/earn", tok, map[string]any{"amount": maxSingleEarn})
		if res.code != http.StatusOK {
			t.Fatalf("setup earn failed: %d %v", res.code, res.body)
		}
	}

	res := do(t, h, http.MethodPost, "/api/points/earn", tok, map[string]any{"amount": 1})
	if res.code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — the arcade client depends on this exact status", res.code)
	}
	if res.body["msg"] == nil {
		t.Error("429 body has no msg field")
	}
}

func TestSpendInsufficientReturns402WithContext(t *testing.T) {
	h := newServer(t)
	tok := issuer.token(t, "u1", false)

	if res := do(t, h, http.MethodPost, "/api/points/earn", tok, map[string]any{"amount": 50}); res.code != http.StatusOK {
		t.Fatalf("setup earn: %d", res.code)
	}

	res := do(t, h, http.MethodPost, "/api/points/spend", tok,
		map[string]any{"amount": 500, "reason": "too-much"})

	if res.code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", res.code)
	}
	if got := num(t, res.body, "balance"); got != 50 {
		t.Errorf("balance = %v, want 50", got)
	}
	if got := num(t, res.body, "required"); got != 500 {
		t.Errorf("required = %v, want 500", got)
	}
}

func TestSpendRequiresAReason(t *testing.T) {
	h := newServer(t)
	res := do(t, h, http.MethodPost, "/api/points/spend", issuer.token(t, "u1", false),
		map[string]any{"amount": 10})
	if res.code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.code)
	}
}

func TestSyncIsOneShotAndReturns409(t *testing.T) {
	h := newServer(t)
	tok := issuer.token(t, "u1", false)

	if res := do(t, h, http.MethodPost, "/api/points/sync", tok, map[string]any{"amount": 300}); res.code != http.StatusOK {
		t.Fatalf("first sync = %d: %v", res.code, res.body)
	}
	res := do(t, h, http.MethodPost, "/api/points/sync", tok, map[string]any{"amount": 300})

	if res.code != http.StatusConflict {
		t.Fatalf("second sync = %d, want 409", res.code)
	}
	if got := num(t, res.body, "balance"); got != 300 {
		t.Errorf("balance = %v, want 300 (unchanged)", got)
	}
}

// Guests may earn, but the points go to a claimable bucket — never to an
// account. Storm-Gate mints a fresh guest id per anonymous login, so crediting
// accounts here would leave one unreachable balance per session.
func TestGuestEarnDoesNotCreateAnAccount(t *testing.T) {
	h := newServer(t)

	res := do(t, h, http.MethodPost, "/api/points/earn", issuer.token(t, "guest-1", true),
		map[string]any{"amount": 40, "gameId": "frogger"})
	if res.code != http.StatusOK {
		t.Fatalf("guest earn = %d: %v", res.code, res.body)
	}
	if res.body["guest"] != true {
		t.Errorf("response should mark the balance as a guest bucket: %v", res.body)
	}

	var accounts int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 0 {
		t.Fatalf("guest earning created %d account rows, want 0", accounts)
	}
}

func TestGuestsCannotSpendOrSync(t *testing.T) {
	h := newServer(t)
	tok := issuer.token(t, "guest-1", true)

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/api/points/spend", map[string]any{"amount": 10, "reason": "x"}},
		{"/api/points/sync", map[string]any{"amount": 10}},
	} {
		res := do(t, h, http.MethodPost, tc.path, tok, tc.body)
		if res.code != http.StatusForbidden {
			t.Errorf("%s as guest = %d, want 403", tc.path, res.code)
		}
	}
}

func TestGuestEarningsCanBeClaimedOnce(t *testing.T) {
	h := newServer(t)

	if res := do(t, h, http.MethodPost, "/api/points/earn", issuer.token(t, "guest-7", true),
		map[string]any{"amount": 75}); res.code != http.StatusOK {
		t.Fatalf("guest earn: %d", res.code)
	}

	realTok := issuer.token(t, "real-user", false)
	res := do(t, h, http.MethodPost, "/api/points/claim-guest", realTok,
		map[string]any{"guestId": "guest-7"})
	if res.code != http.StatusOK {
		t.Fatalf("claim = %d: %v", res.code, res.body)
	}
	if got := num(t, res.body, "balance"); got != 75 {
		t.Errorf("balance = %v, want 75", got)
	}

	again := do(t, h, http.MethodPost, "/api/points/claim-guest", realTok,
		map[string]any{"guestId": "guest-7"})
	if again.code != http.StatusConflict {
		t.Errorf("second claim = %d, want 409", again.code)
	}
}

// A player token must never reach /internal. That boundary is the only thing
// standing between a browser and arbitrary balance mutation.
func TestInternalRoutesRejectPlayerTokens(t *testing.T) {
	h := newServer(t)
	playerTok := issuer.token(t, "u1", false)

	res := do(t, h, http.MethodPost, "/internal/credit", playerTok,
		map[string]any{"userId": "u1", "amount": 1000000, "idempotencyKey": "forged"})

	if res.code != http.StatusUnauthorized {
		t.Fatalf("player token reached /internal: status %d", res.code)
	}

	var balance int64
	_ = testPool.QueryRow(context.Background(),
		`SELECT coalesce(sum(balance), 0) FROM accounts`).Scan(&balance)
	if balance != 0 {
		t.Fatalf("balance moved via a forged internal call: %d", balance)
	}
}

func TestInternalCreditIsIdempotent(t *testing.T) {
	h := newServer(t)
	body := map[string]any{
		"userId": "u1", "amount": 500, "idempotencyKey": "paypal:ORDER-42",
		"meta": map[string]any{"packId": "pack-500"},
	}

	first := do(t, h, http.MethodPost, "/internal/credit", serviceToken, body)
	if first.code != http.StatusOK {
		t.Fatalf("first credit = %d: %v", first.code, first.body)
	}
	if first.body["applied"] != true {
		t.Errorf("applied = %v, want true", first.body["applied"])
	}

	second := do(t, h, http.MethodPost, "/internal/credit", serviceToken, body)
	if second.code != http.StatusOK {
		t.Fatalf("replay = %d, want 200 — a paid order is not an error", second.code)
	}
	if second.body["applied"] != false {
		t.Errorf("applied = %v on replay, want false", second.body["applied"])
	}
	if got := num(t, second.body, "balance"); got != 500 {
		t.Errorf("balance = %v, want 500 — the order was credited twice", got)
	}
}

func TestHoldCommitFlow(t *testing.T) {
	h := newServer(t)
	do(t, h, http.MethodPost, "/internal/credit", serviceToken,
		map[string]any{"userId": "u1", "amount": 500, "idempotencyKey": "seed-1"})

	hold := do(t, h, http.MethodPost, "/internal/holds", serviceToken,
		map[string]any{"userId": "u1", "amount": 120, "meta": map[string]any{"reason": "store-redeem"}})
	if hold.code != http.StatusOK {
		t.Fatalf("hold = %d: %v", hold.code, hold.body)
	}
	if got := num(t, hold.body, "balance"); got != 380 {
		t.Errorf("balance after hold = %v, want 380 (debited up front)", got)
	}
	holdID, _ := hold.body["holdId"].(string)
	if holdID == "" {
		t.Fatal("no holdId in response")
	}

	commit := do(t, h, http.MethodPost, "/internal/holds/"+holdID+"/commit", serviceToken,
		map[string]any{"meta": map[string]any{"purchaseId": "p-1"}})
	if commit.code != http.StatusOK {
		t.Fatalf("commit = %d: %v", commit.code, commit.body)
	}
	if got := num(t, commit.body, "balance"); got != 380 {
		t.Errorf("balance after commit = %v, want 380 (commit must not debit again)", got)
	}
}

func TestHoldReleaseRestoresTheBalance(t *testing.T) {
	h := newServer(t)
	do(t, h, http.MethodPost, "/internal/credit", serviceToken,
		map[string]any{"userId": "u1", "amount": 500, "idempotencyKey": "seed-2"})

	hold := do(t, h, http.MethodPost, "/internal/holds", serviceToken,
		map[string]any{"userId": "u1", "amount": 120})
	holdID := hold.body["holdId"].(string)

	release := do(t, h, http.MethodPost, "/internal/holds/"+holdID+"/release", serviceToken, nil)
	if release.code != http.StatusOK {
		t.Fatalf("release = %d: %v", release.code, release.body)
	}
	if got := num(t, release.body, "balance"); got != 500 {
		t.Errorf("balance after release = %v, want 500", got)
	}

	// Committing a released hold must fail rather than charge again.
	commit := do(t, h, http.MethodPost, "/internal/holds/"+holdID+"/commit", serviceToken, nil)
	if commit.code != http.StatusConflict {
		t.Errorf("commit after release = %d, want 409", commit.code)
	}
}

func TestCORSAllowsTheArcadeOrigin(t *testing.T) {
	h := newServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/api/points/earn", nil)
	req.Header.Set("Origin", allowedOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Errorf("Allow-Origin = %q, want %q", got, allowedOrigin)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin — caches must not cross origins", got)
	}

	// An unlisted origin gets no CORS headers, so the browser blocks it.
	req2 := httptest.NewRequest(http.MethodOptions, "/api/points/earn", nil)
	req2.Header.Set("Origin", "https://evil.example")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin was allowed: %q", got)
	}
}

func TestUnknownRouteReturnsTheStandardErrorShape(t *testing.T) {
	h := newServer(t)
	res := do(t, h, http.MethodGet, "/api/points/nope", issuer.token(t, "u1", false), nil)
	if res.code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.code)
	}
	if res.body["msg"] == nil {
		t.Errorf("404 body has no msg field: %v", res.body)
	}
}
