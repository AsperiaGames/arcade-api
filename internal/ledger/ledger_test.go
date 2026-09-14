package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/HoseaCodes/arcade-api/internal/ledger"
	"github.com/HoseaCodes/arcade-api/internal/store"
)

// These run against a real Postgres, never a mock. The properties under test —
// atomic debits, the balance >= 0 constraint, UNIQUE-based idempotency — are
// database behaviours. A fake would assert that the test double works.
//
// The same container and the same embedded migrations back every test, so a
// green run exercises the schema that actually ships.

const maxDailyEarn = 100

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("arcade"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n(is Docker running?)\n", err)
		os.Exit(1)
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
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

	code := m.Run()

	testPool.Close()
	_ = testcontainers.TerminateContainer(ctr)
	os.Exit(code)
}

// newLedger returns a ledger over a freshly emptied schema.
func newLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	const truncate = `TRUNCATE scores, game_sessions, receipts, holds, earn_windows, guest_earnings, accounts RESTART IDENTITY CASCADE`
	if _, err := testPool.Exec(context.Background(), truncate); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	return ledger.New(testPool, maxDailyEarn, time.Minute)
}

func mustAccount(t *testing.T, l *ledger.Ledger, userID string) ledger.Account {
	t.Helper()
	a, err := l.EnsureAccount(context.Background(), userID)
	if err != nil {
		t.Fatalf("load account: %v", err)
	}
	return a
}

// seed puts a starting balance on an account without going through Earn, so
// balance setup is never bounded by the daily cap.
func seed(t *testing.T, l *ledger.Ledger, userID string, balance int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		t.Fatalf("ensure account: %v", err)
	}
	if balance == 0 {
		return
	}
	const q = `UPDATE accounts SET balance = $2 WHERE user_id = $1`
	if _, err := testPool.Exec(ctx, q, userID, balance); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
}

func TestEnsureAccountIsIdempotent(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()

	first, err := l.EnsureAccount(ctx, "u1")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := l.EnsureAccount(ctx, "u1")
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.CreatedAt != second.CreatedAt {
		t.Errorf("account was recreated: %v then %v", first.CreatedAt, second.CreatedAt)
	}
	if second.Balance != 0 {
		t.Errorf("balance = %d, want 0", second.Balance)
	}
}

func TestEarnCreditsAndRecordsReceipt(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	acct, _, err := l.Earn(ctx, "u1", 40, ledger.Meta{"gameId": "pac-man"})
	if err != nil {
		t.Fatalf("earn: %v", err)
	}
	if acct.Balance != 40 || acct.LifetimeEarned != 40 {
		t.Errorf("balance=%d lifetimeEarned=%d, want 40/40", acct.Balance, acct.LifetimeEarned)
	}
	if acct.LifetimeSpent != 0 {
		t.Errorf("lifetimeSpent = %d, want 0", acct.LifetimeSpent)
	}

	receipts, err := l.Transactions(ctx, "u1", 10)
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(receipts))
	}
	r := receipts[0]
	if r.Type != ledger.TypeEarn || r.Amount != 40 || r.BalanceAfter != 40 {
		t.Errorf("receipt = %+v, want earn/40/40", r)
	}
	if r.Meta["gameId"] != "pac-man" {
		t.Errorf("meta.gameId = %v, want pac-man", r.Meta["gameId"])
	}
}

func TestSpendDebitsAndRecordsReceipt(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	acct, err := l.Spend(ctx, "u1", 30, ledger.Meta{"reason": "unit-test"})
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if acct.Balance != 70 || acct.LifetimeSpent != 30 {
		t.Errorf("balance=%d lifetimeSpent=%d, want 70/30", acct.Balance, acct.LifetimeSpent)
	}

	receipts, _ := l.Transactions(ctx, "u1", 10)
	if len(receipts) != 1 || receipts[0].Type != ledger.TypeSpend || receipts[0].BalanceAfter != 70 {
		t.Errorf("receipts = %+v, want one spend with balanceAfter 70", receipts)
	}
}

func TestSpendInsufficientLeavesLedgerUntouched(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 10)

	_, err := l.Spend(ctx, "u1", 50, nil)
	if !errors.Is(err, ledger.ErrInsufficient) {
		t.Fatalf("err = %v, want ErrInsufficient", err)
	}

	acct := mustAccount(t, l, "u1")
	if acct.Balance != 10 || acct.LifetimeSpent != 0 {
		t.Errorf("ledger moved: balance=%d lifetimeSpent=%d", acct.Balance, acct.LifetimeSpent)
	}
	if r, _ := l.Transactions(ctx, "u1", 10); len(r) != 0 {
		t.Errorf("got %d receipts, want none", len(r))
	}
}

// The property the whole design rests on. The `balance >= $2` precondition means
// a losing racer updates no rows; a read-then-write would let all ten observe
// the same balance and collectively overdraw.
func TestSpendConcurrentCannotOverdraw(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 100)

	const attempts, cost = 10, 30
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		ok          int
		insufficent int
		balances    []int64
		start       = make(chan struct{})
	)

	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, to actually contend
			acct, err := l.Spend(ctx, "u1", cost, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
				balances = append(balances, acct.Balance)
			case errors.Is(err, ledger.ErrInsufficient):
				insufficent++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok != 3 || insufficent != 7 {
		t.Errorf("ok=%d insufficient=%d, want 3/7", ok, insufficent)
	}

	acct := mustAccount(t, l, "u1")
	if acct.Balance != 10 {
		t.Errorf("final balance = %d, want 10", acct.Balance)
	}
	if acct.Balance < 0 {
		t.Fatalf("balance went negative: %d", acct.Balance)
	}
	if acct.LifetimeSpent != 90 {
		t.Errorf("lifetimeSpent = %d, want 90", acct.LifetimeSpent)
	}

	// Each winner saw a distinct post-debit balance — proof the updates
	// serialized rather than interleaving.
	seen := map[int64]bool{}
	for _, b := range balances {
		if seen[b] {
			t.Errorf("two spends reported the same balance %d", b)
		}
		seen[b] = true
	}
	for _, want := range []int64{10, 40, 70} {
		if !seen[want] {
			t.Errorf("no spend reported balance %d; got %v", want, balances)
		}
	}

	if r, _ := l.Transactions(ctx, "u1", 50); len(r) != 3 {
		t.Errorf("got %d receipts, want 3", len(r))
	}
}

func TestEarnRejectsAboveDailyCap(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	if _, _, err := l.Earn(ctx, "u1", maxDailyEarn+1, nil); !errors.Is(err, ledger.ErrDailyCap) {
		t.Fatalf("err = %v, want ErrDailyCap", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 0 {
		t.Errorf("balance = %d, want 0", acct.Balance)
	}
}

func TestEarnExhaustsDailyBudget(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	if _, _, err := l.Earn(ctx, "u1", 60, nil); err != nil {
		t.Fatalf("first earn: %v", err)
	}
	if _, _, err := l.Earn(ctx, "u1", 40, nil); err != nil {
		t.Fatalf("second earn (exactly at cap): %v", err)
	}
	if _, _, err := l.Earn(ctx, "u1", 1, nil); !errors.Is(err, ledger.ErrDailyCap) {
		t.Fatalf("third earn err = %v, want ErrDailyCap", err)
	}

	if acct := mustAccount(t, l, "u1"); acct.Balance != maxDailyEarn {
		t.Errorf("balance = %d, want %d", acct.Balance, maxDailyEarn)
	}
}

// A rate limit that can be raced is not a rate limit. The budget lives in the
// same transaction as the credit, so the two can never disagree.
func TestEarnConcurrentCannotExceedDailyCap(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	const attempts, amount = 10, 30
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		ok     int
		capped int
		start  = make(chan struct{})
	)

	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := l.Earn(ctx, "u1", amount, nil)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ledger.ErrDailyCap):
				capped++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok != 3 || capped != 7 {
		t.Errorf("ok=%d capped=%d, want 3/7", ok, capped)
	}
	acct := mustAccount(t, l, "u1")
	if acct.Balance != 90 {
		t.Errorf("balance = %d, want 90", acct.Balance)
	}
	if acct.Balance > maxDailyEarn {
		t.Fatalf("daily cap breached: %d > %d", acct.Balance, maxDailyEarn)
	}
}

func TestEarnBudgetIsPerUser(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "a", 0)
	seed(t, l, "b", 0)

	if _, _, err := l.Earn(ctx, "a", maxDailyEarn, nil); err != nil {
		t.Fatalf("a earn: %v", err)
	}
	if _, _, err := l.Earn(ctx, "a", 1, nil); !errors.Is(err, ledger.ErrDailyCap) {
		t.Fatalf("a should be capped, got %v", err)
	}
	if _, _, err := l.Earn(ctx, "b", 50, nil); err != nil {
		t.Fatalf("b should be unaffected by a's cap: %v", err)
	}
}

func TestEarnBudgetResetsNextDay(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	day := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return day })
	if _, _, err := l.Earn(ctx, "u1", maxDailyEarn, nil); err != nil {
		t.Fatalf("day one: %v", err)
	}
	if _, _, err := l.Earn(ctx, "u1", 1, nil); !errors.Is(err, ledger.ErrDailyCap) {
		t.Fatalf("day one should be exhausted, got %v", err)
	}

	l.SetClock(func() time.Time { return day.Add(24 * time.Hour) })
	if _, _, err := l.Earn(ctx, "u1", 50, nil); err != nil {
		t.Fatalf("day two should have a fresh budget: %v", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != maxDailyEarn+50 {
		t.Errorf("balance = %d, want %d", acct.Balance, maxDailyEarn+50)
	}
}

// The PayPal double-credit race, closed by a database constraint rather than a
// read-then-write check. The Mongo original had no unique index here, so two
// concurrent captures could both look, both miss, and both credit.
func TestCreditIsIdempotentUnderConcurrency(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	const attempts = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		start   = make(chan struct{})
	)

	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, didApply, err := l.Credit(ctx, "u1", 500, "paypal:ORDER-1", ledger.Meta{"packId": "pack-500"})
			if err != nil {
				t.Errorf("credit: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if didApply {
				applied++
			}
		}()
	}
	close(start)
	wg.Wait()

	if applied != 1 {
		t.Errorf("applied %d times, want exactly 1", applied)
	}

	acct := mustAccount(t, l, "u1")
	if acct.Balance != 500 {
		t.Errorf("balance = %d, want 500 — the order was credited more than once", acct.Balance)
	}
	if acct.LifetimePurchased != 500 {
		t.Errorf("lifetimePurchased = %d, want 500", acct.LifetimePurchased)
	}
	if r, _ := l.Transactions(ctx, "u1", 50); len(r) != 1 {
		t.Errorf("got %d receipts, want 1", len(r))
	}
}

// The guarantee Mongo could not give: when the receipt cannot be written, the
// balance does not move either. Here the duplicate idempotency key fails the
// INSERT after the UPDATE has already run inside the same transaction.
func TestFailedReceiptRollsBackTheBalance(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	if _, _, err := l.Credit(ctx, "u1", 500, "paypal:ORDER-1", nil); err != nil {
		t.Fatalf("first credit: %v", err)
	}
	before := mustAccount(t, l, "u1")

	// Same key again: the receipt insert violates UNIQUE mid-transaction.
	_, applied, err := l.Credit(ctx, "u1", 500, "paypal:ORDER-1", nil)
	if err != nil {
		t.Fatalf("second credit returned an error rather than reporting a duplicate: %v", err)
	}
	if applied {
		t.Error("second credit reported as applied")
	}

	after := mustAccount(t, l, "u1")
	if after.Balance != before.Balance {
		t.Errorf("balance moved on the rolled-back credit: %d -> %d", before.Balance, after.Balance)
	}
	if after.LifetimePurchased != before.LifetimePurchased {
		t.Errorf("lifetimePurchased moved: %d -> %d", before.LifetimePurchased, after.LifetimePurchased)
	}
}

func TestClaimOfflineIsOneShot(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 0)

	acct, err := l.ClaimOffline(ctx, "u1", 250, ledger.Meta{"source": "localStorage"})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if acct.Balance != 250 || !acct.ClaimedOffline {
		t.Errorf("balance=%d claimed=%v, want 250/true", acct.Balance, acct.ClaimedOffline)
	}

	if _, err := l.ClaimOffline(ctx, "u1", 250, nil); !errors.Is(err, ledger.ErrAlreadyClaimed) {
		t.Fatalf("second claim err = %v, want ErrAlreadyClaimed", err)
	}
	if acct := mustAccount(t, l, "u1"); acct.Balance != 250 {
		t.Errorf("balance = %d after a refused re-claim, want 250", acct.Balance)
	}
}

func TestGuestEarningsStayOffTheAccountUntilClaimed(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()

	if _, err := l.RecordGuestEarn(ctx, "guest-1", 40, time.Hour); err != nil {
		t.Fatalf("guest earn: %v", err)
	}
	balance, err := l.RecordGuestEarn(ctx, "guest-1", 25, time.Hour)
	if err != nil {
		t.Fatalf("second guest earn: %v", err)
	}
	if balance != 65 {
		t.Errorf("guest balance = %d, want 65", balance)
	}

	// No account exists for the guest — that is the entire point.
	var accounts int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 0 {
		t.Fatalf("guest earning created %d account rows, want 0", accounts)
	}

	acct, moved, err := l.ClaimGuestEarnings(ctx, "guest-1", "real-user")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if moved != 65 || acct.Balance != 65 {
		t.Errorf("moved=%d balance=%d, want 65/65", moved, acct.Balance)
	}

	if _, _, err := l.ClaimGuestEarnings(ctx, "guest-1", "real-user"); !errors.Is(err, ledger.ErrAlreadyClaimed) {
		t.Fatalf("re-claim err = %v, want ErrAlreadyClaimed", err)
	}
}

func TestTransactionsAreNewestFirstAndCapped(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	seed(t, l, "u1", 1000)

	for i := range 5 {
		if _, err := l.Spend(ctx, "u1", 10, ledger.Meta{"n": i}); err != nil {
			t.Fatalf("spend %d: %v", i, err)
		}
	}

	receipts, err := l.Transactions(ctx, "u1", 3)
	if err != nil {
		t.Fatalf("transactions: %v", err)
	}
	if len(receipts) != 3 {
		t.Fatalf("got %d receipts, want 3 (limit honoured)", len(receipts))
	}
	for i := 1; i < len(receipts); i++ {
		if receipts[i-1].ID < receipts[i].ID {
			t.Errorf("receipts not newest-first: %d before %d", receipts[i-1].ID, receipts[i].ID)
		}
	}

	// An empty history marshals as [] rather than null.
	empty, err := l.Transactions(ctx, "nobody", 10)
	if err != nil {
		t.Fatalf("empty transactions: %v", err)
	}
	if empty == nil {
		t.Error("empty history returned nil slice, want empty non-nil")
	}
}
