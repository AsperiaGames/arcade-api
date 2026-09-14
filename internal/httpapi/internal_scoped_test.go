package httpapi_test

import (
	"net/http"
	"testing"
)

// The user-scoped /internal endpoints back blog-portfolio's ledger facade: the
// blog holds no balances any more, so a call it once served from Mongo now
// travels here as a service-authenticated request carrying an explicit userId.
// These tests pin that contract — the same bodies the /api routes return, minus
// the JWT.

func TestInternalUserScopedRoutesRejectPlayerTokens(t *testing.T) {
	h := newServer(t)
	playerTok := issuer.token(t, "u1", false)

	// Every one of these moves or reads a balance for an arbitrary userId. A
	// player token must never be enough to reach them, or any signed-in user
	// could act on any other user's wallet.
	for _, path := range []string{
		"/internal/balance", "/internal/transactions",
		"/internal/earn", "/internal/spend", "/internal/sync",
	} {
		res := do(t, h, http.MethodPost, path, playerTok,
			map[string]any{"userId": "victim", "amount": 100})
		if res.code != http.StatusUnauthorized {
			t.Errorf("%s admitted a player token: status %d", path, res.code)
		}
	}
}

func TestInternalBalanceCreatesAndReports(t *testing.T) {
	h := newServer(t)

	// A first read of an unknown user creates the account at zero, exactly as
	// getOrCreateAccount did on the Node side.
	res := do(t, h, http.MethodPost, "/internal/balance", serviceToken,
		map[string]any{"userId": "u1"})
	if res.code != http.StatusOK {
		t.Fatalf("balance = %d: %v", res.code, res.body)
	}
	if got := num(t, res.body, "balance"); got != 0 {
		t.Errorf("balance = %v, want 0", got)
	}
	for _, k := range []string{"lifetimeEarned", "lifetimeSpent", "lifetimePurchased"} {
		if _, ok := res.body[k]; !ok {
			t.Errorf("balance response missing %q: %v", k, res.body)
		}
	}
	if res.body["claimedOffline"] != false {
		t.Errorf("claimedOffline = %v, want false", res.body["claimedOffline"])
	}
}

func TestInternalEarnCreditsAndHonoursTheDailyCap(t *testing.T) {
	h := newServer(t)

	res := do(t, h, http.MethodPost, "/internal/earn", serviceToken,
		map[string]any{"userId": "u1", "amount": 300, "meta": map[string]any{"gameId": "pacman"}})
	if res.code != http.StatusOK {
		t.Fatalf("earn = %d: %v", res.code, res.body)
	}
	if got := num(t, res.body, "balance"); got != 300 {
		t.Errorf("balance = %v, want 300", got)
	}
	// The blog facade passes `remaining` straight through to its clients.
	if got := num(t, res.body, "remaining"); got != maxDailyEarn-300 {
		t.Errorf("remaining = %v, want %d", got, maxDailyEarn-300)
	}

	// A single earn over the per-call ceiling is a 400, not a silent clamp.
	over := do(t, h, http.MethodPost, "/internal/earn", serviceToken,
		map[string]any{"userId": "u1", "amount": maxSingleEarn + 1})
	if over.code != http.StatusBadRequest {
		t.Errorf("over-cap single earn = %d, want 400", over.code)
	}

	// Fill the rest of the daily budget in per-call-sized chunks (a single earn
	// cannot exceed maxSingleEarn), then confirm the next earn is a 429 — the
	// status the arcade client maps to `daily-cap` and stops retrying on.
	// Already earned 300; cap is maxDailyEarn.
	for remaining := maxDailyEarn - 300; remaining > 0; {
		chunk := remaining
		if chunk > maxSingleEarn {
			chunk = maxSingleEarn
		}
		fill := do(t, h, http.MethodPost, "/internal/earn", serviceToken,
			map[string]any{"userId": "u1", "amount": chunk})
		if fill.code != http.StatusOK {
			t.Fatalf("budget-fill earn of %d = %d: %v", chunk, fill.code, fill.body)
		}
		remaining -= chunk
	}
	capped := do(t, h, http.MethodPost, "/internal/earn", serviceToken,
		map[string]any{"userId": "u1", "amount": 1})
	if capped.code != http.StatusTooManyRequests {
		t.Errorf("earn past daily cap = %d, want 429", capped.code)
	}
	if got := num(t, capped.body, "earnedToday"); got != maxDailyEarn {
		t.Errorf("earnedToday = %v, want %d", got, maxDailyEarn)
	}
}

func TestInternalSpendDebitsAndRefusesOverdraw(t *testing.T) {
	h := newServer(t)
	do(t, h, http.MethodPost, "/internal/credit", serviceToken,
		map[string]any{"userId": "u1", "amount": 100, "idempotencyKey": "seed-spend"})

	ok := do(t, h, http.MethodPost, "/internal/spend", serviceToken,
		map[string]any{"userId": "u1", "amount": 40, "reason": "store-redeem"})
	if ok.code != http.StatusOK {
		t.Fatalf("spend = %d: %v", ok.code, ok.body)
	}
	if got := num(t, ok.body, "balance"); got != 60 {
		t.Errorf("balance = %v, want 60", got)
	}

	// Overdraw is a 402 carrying the current balance and what was required — the
	// shape the store UI renders as "not enough points".
	broke := do(t, h, http.MethodPost, "/internal/spend", serviceToken,
		map[string]any{"userId": "u1", "amount": 1000, "reason": "too-much"})
	if broke.code != http.StatusPaymentRequired {
		t.Fatalf("overdraw = %d, want 402", broke.code)
	}
	if got := num(t, broke.body, "balance"); got != 60 {
		t.Errorf("balance after refused spend = %v, want 60 (unchanged)", got)
	}
	if got := num(t, broke.body, "required"); got != 1000 {
		t.Errorf("required = %v, want 1000", got)
	}
}

func TestInternalSyncIsOneShot(t *testing.T) {
	h := newServer(t)

	first := do(t, h, http.MethodPost, "/internal/sync", serviceToken,
		map[string]any{"userId": "u1", "amount": 250})
	if first.code != http.StatusOK {
		t.Fatalf("sync = %d: %v", first.code, first.body)
	}
	if got := num(t, first.body, "balance"); got != 250 {
		t.Errorf("balance = %v, want 250", got)
	}

	// The offline claim is idempotent by the account flag: a second attempt is a
	// 409, not a second credit.
	second := do(t, h, http.MethodPost, "/internal/sync", serviceToken,
		map[string]any{"userId": "u1", "amount": 250})
	if second.code != http.StatusConflict {
		t.Fatalf("second sync = %d, want 409", second.code)
	}
	if got := num(t, second.body, "balance"); got != 250 {
		t.Errorf("balance after refused re-claim = %v, want 250", got)
	}
}

func TestInternalTransactionsListsReceipts(t *testing.T) {
	h := newServer(t)
	do(t, h, http.MethodPost, "/internal/credit", serviceToken,
		map[string]any{"userId": "u1", "amount": 100, "idempotencyKey": "seed-tx"})
	do(t, h, http.MethodPost, "/internal/spend", serviceToken,
		map[string]any{"userId": "u1", "amount": 30, "reason": "redeem"})

	res := do(t, h, http.MethodPost, "/internal/transactions", serviceToken,
		map[string]any{"userId": "u1", "limit": 50})
	if res.code != http.StatusOK {
		t.Fatalf("transactions = %d: %v", res.code, res.body)
	}
	if got := num(t, res.body, "count"); got != 2 {
		t.Errorf("count = %v, want 2 (a purchase and a spend)", got)
	}
	if _, ok := res.body["items"].([]any); !ok {
		t.Errorf("items is not a list: %v", res.body["items"])
	}
}

func TestInternalUserScopedRoutesRequireUserID(t *testing.T) {
	h := newServer(t)
	for _, path := range []string{
		"/internal/balance", "/internal/transactions",
		"/internal/earn", "/internal/spend", "/internal/sync",
	} {
		res := do(t, h, http.MethodPost, path, serviceToken,
			map[string]any{"amount": 10})
		if res.code != http.StatusBadRequest {
			t.Errorf("%s with no userId = %d, want 400", path, res.code)
		}
	}
}
