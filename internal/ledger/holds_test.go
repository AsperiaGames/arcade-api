package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/HoseaCodes/arcade-api/internal/ledger"
)

// newLedgerTTL is newLedger with a custom hold lifetime, for expiry tests.
func newLedgerTTL(t *testing.T, ttl time.Duration) *ledger.Ledger {
	t.Helper()
	const truncate = `TRUNCATE receipts, holds, earn_windows, guest_earnings, accounts RESTART IDENTITY CASCADE`
	if _, err := testPool.Exec(context.Background(), truncate); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return ledger.New(testPool, maxDailyEarn, ttl)
}

func TestHoldDebitsImmediately(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, err := l.Hold(ctx, "u1", 30, ledger.Meta{"reason": "store-redeem"})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if h.Amount != 30 || h.Status != ledger.HoldHeld || h.BalanceAfter != 70 {
		t.Errorf("hold = %+v, want amount 30, status held, balance 70", h)
	}

	// The points are gone from the balance right away — that is what stops the
	// player spending them twice while the caller does its own write.
	if acct := mustAccount(t, l, "u1"); acct.Balance != 70 {
		t.Errorf("balance = %d, want 70", acct.Balance)
	}
	// But no receipt yet: until commit, we do not know what the spend was for.
	if r, _ := l.Transactions(ctx, "u1", 10); len(r) != 0 {
		t.Errorf("got %d receipts before commit, want 0", len(r))
	}
}

func TestHoldRejectsAnUnaffordableAmount(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 10)

	if _, err := l.Hold(ctx, "u1", 50, nil); !errors.Is(err, ledger.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 10 {
		t.Errorf("balance = %d, want 10", acct.Balance)
	}
}

func TestCommitHoldWritesTheReceipt(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, err := l.Hold(ctx, "u1", 30, ledger.Meta{"reason": "store-redeem"})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}

	acct, err := l.CommitHold(ctx, h.ID, ledger.Meta{"purchaseId": "p-99"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if acct.Balance != 70 {
		t.Errorf("balance = %d, want 70 (commit must not debit again)", acct.Balance)
	}

	receipts, _ := l.Transactions(ctx, "u1", 10)
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(receipts))
	}
	r := receipts[0]
	if r.Type != ledger.TypeSpend || r.Amount != 30 || r.BalanceAfter != 70 {
		t.Errorf("receipt = %+v, want spend/30/70", r)
	}
	// Meta captured at hold time is merged with meta known only at commit time.
	if r.Meta["reason"] != "store-redeem" {
		t.Errorf("meta.reason = %v, want store-redeem", r.Meta["reason"])
	}
	if r.Meta["purchaseId"] != "p-99" {
		t.Errorf("meta.purchaseId = %v, want p-99", r.Meta["purchaseId"])
	}
}

func TestReleaseHoldRestoresTheBalance(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)
	acct, err := l.ReleaseHold(ctx, h.ID)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	if acct.Balance != 100 {
		t.Errorf("balance = %d, want 100", acct.Balance)
	}
	// The spend never happened, so it must not show in lifetime totals.
	if acct.LifetimeSpent != 0 {
		t.Errorf("lifetimeSpent = %d, want 0 — a released hold is not a spend", acct.LifetimeSpent)
	}
	if r, _ := l.Transactions(ctx, "u1", 10); len(r) != 0 {
		t.Errorf("got %d receipts, want 0", len(r))
	}
}

// A caller that times out and retries must not be charged twice.
func TestCommitHoldIsIdempotent(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)
	if _, err := l.CommitHold(ctx, h.ID, nil); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	acct, err := l.CommitHold(ctx, h.ID, nil)
	if err != nil {
		t.Fatalf("second commit should be a no-op success, got %v", err)
	}

	if acct.Balance != 70 {
		t.Errorf("balance = %d, want 70 — the retry charged again", acct.Balance)
	}
	if r, _ := l.Transactions(ctx, "u1", 10); len(r) != 1 {
		t.Errorf("got %d receipts, want 1 — the retry wrote a second receipt", len(r))
	}
}

func TestReleaseHoldIsIdempotent(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)
	if _, err := l.ReleaseHold(ctx, h.ID); err != nil {
		t.Fatalf("first release: %v", err)
	}
	acct, err := l.ReleaseHold(ctx, h.ID)
	if err != nil {
		t.Fatalf("second release should be a no-op success, got %v", err)
	}
	if acct.Balance != 100 {
		t.Errorf("balance = %d, want 100 — the retry credited again", acct.Balance)
	}
}

func TestCommitAfterReleaseIsRefused(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)
	if _, err := l.ReleaseHold(ctx, h.ID); err != nil {
		t.Fatalf("release: %v", err)
	}

	if _, err := l.CommitHold(ctx, h.ID, nil); !errors.Is(err, ledger.ErrHoldSettled) {
		t.Fatalf("err = %v, want ErrHoldSettled", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 100 {
		t.Errorf("balance = %d, want 100", acct.Balance)
	}
}

func TestCommitUnknownHold(t *testing.T) {
	l := newLedger(t)
	_, err := l.CommitHold(context.Background(), "00000000-0000-0000-0000-000000000000", nil)
	if !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The property that makes two-phase spend safe: a caller that dies between
// taking a hold and settling it does not strand the player's points.
func TestSweepReturnsExpiredHolds(t *testing.T) {
	l := newLedgerTTL(t, 1*time.Millisecond)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, err := l.Hold(ctx, "u1", 30, nil)
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 70 {
		t.Fatalf("balance = %d, want 70 before the sweep", acct.Balance)
	}

	time.Sleep(20 * time.Millisecond)

	n, err := l.SweepExpiredHolds(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d holds, want 1", n)
	}

	acct := mustAccount(t, l, "u1")
	if acct.Balance != 100 {
		t.Errorf("balance = %d, want 100 — the sweep did not return the points", acct.Balance)
	}
	if acct.LifetimeSpent != 0 {
		t.Errorf("lifetimeSpent = %d, want 0", acct.LifetimeSpent)
	}

	// And the expired hold can no longer be committed — the points came back,
	// so charging for it now would take them twice.
	if _, err := l.CommitHold(ctx, h.ID, nil); !errors.Is(err, ledger.ErrHoldSettled) {
		t.Errorf("commit after sweep err = %v, want ErrHoldSettled", err)
	}
}

func TestSweepLeavesLiveHoldsAlone(t *testing.T) {
	l := newLedger(t) // one-minute TTL
	ctx := context.Background()
	seed(t, l, "u1", 100)

	if _, err := l.Hold(ctx, "u1", 30, nil); err != nil {
		t.Fatalf("hold: %v", err)
	}

	n, err := l.SweepExpiredHolds(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("swept %d holds, want 0 — a live hold was reclaimed", n)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 70 {
		t.Errorf("balance = %d, want 70", acct.Balance)
	}
}

// An expired hold must not be committable even before the sweeper runs: the
// points are owed back, and a late commit would take them anyway.
func TestExpiredHoldCannotBeCommittedBeforeTheSweep(t *testing.T) {
	l := newLedgerTTL(t, 1*time.Millisecond)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)
	time.Sleep(20 * time.Millisecond)

	if _, err := l.CommitHold(ctx, h.ID, nil); !errors.Is(err, ledger.ErrHoldSettled) {
		t.Fatalf("err = %v, want ErrHoldSettled", err)
	}
}

// Concurrent settles race for the same row; FOR UPDATE serialises them so only
// one takes effect.
func TestConcurrentSettlesActOnce(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	h, _ := l.Hold(ctx, "u1", 30, nil)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		okN   int
		start = make(chan struct{})
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := l.CommitHold(ctx, h.ID, nil); err == nil {
				mu.Lock()
				okN++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	// Every caller may report success — commit is idempotent — but the ledger
	// must only have moved once.
	if acct := mustAccount(t, l, "u1"); acct.Balance != 70 {
		t.Errorf("balance = %d, want 70 after %d concurrent commits", acct.Balance, okN)
	}
	if r, _ := l.Transactions(ctx, "u1", 50); len(r) != 1 {
		t.Errorf("got %d receipts, want exactly 1", len(r))
	}
}

// The whole reason holds exist rather than spend-then-refund: when the caller's
// own write fails, the points come back without needing a second successful
// call from a process that may already be gone.
func TestHoldSurvivesACallerThatNeverSettles(t *testing.T) {
	l := newLedgerTTL(t, 1*time.Millisecond)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	// Caller takes a hold, writes its own row, and crashes. No commit, no
	// release, no refund call — nothing.
	if _, err := l.Hold(ctx, "u1", 30, nil); err != nil {
		t.Fatalf("hold: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	if _, err := l.SweepExpiredHolds(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if acct := mustAccount(t, l, "u1"); acct.Balance != 100 {
		t.Fatalf("player lost %d points to a caller that never came back", 100-acct.Balance)
	}
}
