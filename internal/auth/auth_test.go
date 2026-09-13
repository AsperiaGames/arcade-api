package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/HoseaCodes/arcade-api/internal/auth"
)

// A local issuer: a real RSA key, a real JWKS endpoint, real signed tokens.
// Nothing is stubbed except the network hop, so these exercise the same
// verification path that runs against Storm-Gate in production.
type issuer struct {
	url    string
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	iss := &issuer{key: key, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &key.PublicKey,
			KeyID:     iss.kid,
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})

	iss.server = httptest.NewServer(mux)
	iss.url = iss.server.URL
	t.Cleanup(iss.server.Close)
	return iss
}

type claims struct {
	ID      string `json:"id,omitempty"`
	IsGuest bool   `json:"isGuest,omitempty"`
	Iss     string `json:"iss"`
	Exp     int64  `json:"exp"`
	Iat     int64  `json:"iat"`
}

func (i *issuer) sign(t *testing.T, c claims, alg jose.SignatureAlgorithm, key any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.kid),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
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

// token mints a valid, current token for the given subject.
func (i *issuer) token(t *testing.T, id string, guest bool) string {
	t.Helper()
	now := time.Now()
	return i.sign(t, claims{
		ID:      id,
		IsGuest: guest,
		Iss:     i.url,
		Iat:     now.Unix(),
		Exp:     now.Add(time.Hour).Unix(),
	}, jose.RS256, i.key)
}

func TestVerifyAcceptsAValidToken(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	principal, err := v.Verify(context.Background(), iss.token(t, "user-123", false))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if principal.UserID != "user-123" {
		t.Errorf("UserID = %q, want user-123", principal.UserID)
	}
	if principal.IsGuest {
		t.Error("IsGuest = true, want false")
	}
}

func TestVerifyCarriesTheGuestFlag(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	principal, err := v.Verify(context.Background(), iss.token(t, "guest-9", true))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !principal.IsGuest {
		t.Error("IsGuest = false — guests would be treated as real accounts")
	}
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	past := time.Now().Add(-2 * time.Hour)
	tok := iss.sign(t, claims{
		ID: "u1", Iss: iss.url, Iat: past.Unix(), Exp: past.Add(time.Hour).Unix(),
	}, jose.RS256, iss.key)

	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("expired token was accepted")
	}
}

func TestVerifyRejectsAForeignIssuer(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	now := time.Now()
	tok := iss.sign(t, claims{
		ID: "u1", Iss: "https://evil.example", Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(),
	}, jose.RS256, iss.key)

	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("token from another issuer was accepted")
	}
}

func TestVerifyRejectsAnotherKey(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	now := time.Now()
	tok := iss.sign(t, claims{
		ID: "attacker", Iss: iss.url, Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(),
	}, jose.RS256, attacker)

	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("token signed by an unpublished key was accepted")
	}
}

// Publishing an RSA public key makes this reachable: HMAC-sign a token using
// that public key as the shared secret. A verifier that lets the token's own
// `alg` choose the key material accepts it. Restricting to RS256 is what stops
// it — the same bug class found in Storm-Gate's own middleware.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	pubDER, err := json.Marshal(iss.key.PublicKey.N)
	if err != nil {
		t.Fatalf("marshal modulus: %v", err)
	}
	now := time.Now()
	forged := iss.sign(t, claims{
		ID: "attacker", Iss: iss.url, Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(),
	}, jose.HS256, pubDER)

	if _, err := v.Verify(context.Background(), forged); err == nil {
		t.Fatal("HS256 token signed with the public key was accepted")
	}
}

func TestVerifyRejectsATokenWithNoIDClaim(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	now := time.Now()
	tok := iss.sign(t, claims{
		Iss: iss.url, Iat: now.Unix(), Exp: now.Add(time.Hour).Unix(),
	}, jose.RS256, iss.key)

	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("token without an id claim was accepted")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name, header, want string
	}{
		{"bearer prefix", "Bearer abc.def.ghi", "abc.def.ghi"},
		{"lowercase prefix", "bearer abc.def.ghi", "abc.def.ghi"},
		{"bare token", "abc.def.ghi", "abc.def.ghi"},
		{"surrounding space", "  Bearer   abc.def.ghi  ", "abc.def.ghi"},
		{"empty", "", ""},
		{"bearer with nothing after", "Bearer", ""},
		{"bearer and only space", "Bearer   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.BearerToken(tc.header); got != tc.want {
				t.Errorf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestRequireServiceRejectsAnythingButTheToken(t *testing.T) {
	const secret = "a-service-token-long-enough-to-be-real-0123456789"

	var writeErr auth.ErrorWriter = func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
	}
	reached := false
	handler := auth.RequireService(secret, writeErr)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }),
	)

	cases := []struct {
		name, header string
		wantStatus   int
		wantReached  bool
	}{
		{"correct token", "Bearer " + secret, http.StatusOK, true},
		{"bare correct token", secret, http.StatusOK, true},
		{"wrong token", "Bearer nope", http.StatusUnauthorized, false},
		{"empty", "", http.StatusUnauthorized, false},
		{"prefix of the real token", "Bearer " + secret[:10], http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/credit", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			handler.ServeHTTP(rec, req)

			if reached != tc.wantReached {
				t.Errorf("handler reached = %v, want %v", reached, tc.wantReached)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// An empty configured token must never turn into "accept everything".
func TestRequireServiceWithNoTokenConfiguredRejectsAll(t *testing.T) {
	var writeErr auth.ErrorWriter = func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
	}
	handler := auth.RequireService("", writeErr)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("handler reached with no service token configured")
		}),
	)

	for _, header := range []string{"", "Bearer ", "Bearer anything"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/internal/credit", nil)
		req.Header.Set("Authorization", header)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: status = %d, want 401", header, rec.Code)
		}
	}
}

func TestRequireUserRejectsAMissingHeader(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	var gotStatus int
	var writeErr auth.ErrorWriter = func(w http.ResponseWriter, status int, msg string) {
		gotStatus = status
		w.WriteHeader(status)
	}
	handler := auth.RequireUser(v, writeErr)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("handler reached without a token")
		}),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/points/balance", nil))
	if gotStatus != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", gotStatus)
	}
}

func TestRequireUserAttachesThePrincipal(t *testing.T) {
	iss := newIssuer(t)
	v := auth.NewVerifier(context.Background(), iss.url, "")

	var writeErr auth.ErrorWriter = func(w http.ResponseWriter, status int, msg string) {
		t.Errorf("unexpected auth failure: %d %s", status, msg)
	}

	var seen auth.Principal
	handler := auth.RequireUser(v, writeErr)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := auth.FromContext(r.Context())
			if !ok {
				t.Fatal("no principal on the request context")
			}
			seen = p
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/points/balance", nil)
	req.Header.Set("Authorization", "Bearer "+iss.token(t, "user-77", false))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen.UserID != "user-77" {
		t.Errorf("principal.UserID = %q, want user-77", seen.UserID)
	}
}

// A key set we cannot reach is an outage, not a bad credential. Answering 401
// would make every client discard a valid session during a Storm-Gate blip.
func TestRequireUserReportsAnUnreachableKeySetAs503(t *testing.T) {
	iss := newIssuer(t)
	tok := iss.token(t, "user-1", false)
	url := iss.url
	iss.server.Close() // issuer goes away after the token was minted

	v := auth.NewVerifier(context.Background(), url, "")

	var gotStatus int
	var writeErr auth.ErrorWriter = func(w http.ResponseWriter, status int, msg string) {
		gotStatus = status
		w.WriteHeader(status)
	}
	handler := auth.RequireUser(v, writeErr)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("handler reached despite an unreachable key set")
		}),
	)

	req := httptest.NewRequest(http.MethodGet, "/points/balance", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotStatus != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", gotStatus)
	}
}
