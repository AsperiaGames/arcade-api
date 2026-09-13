// Package ledger owns the points balance and its audit log.
//
// It is the only package permitted to write `accounts` and `receipts`. Before
// this service existed the same logic lived in three Node controllers, each with
// its own copy of the atomic debit and its own compensating refund; the rule
// that a balance may never go negative was a query predicate repeated in three
// places. Here it is one function and one database constraint.
//
// Two invariants hold throughout:
//
//  1. Every balance movement is a single atomic UPDATE with a precondition,
//     never a read-then-write. A losing racer gets no rows back instead of
//     overdrawing.
//
//  2. A balance movement and its receipt commit in the same transaction. In the
//     Mongo original these were separate writes, which forced an awkward choice
//     between losing receipts and reversing completed charges. Postgres removes
//     the dilemma entirely.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors. Callers map these to HTTP status codes; the ledger itself has
// no opinion about transport.
var (
	// ErrInsufficient means the balance could not cover the debit — including
	// the case where a concurrent debit won the race.
	ErrInsufficient = errors.New("insufficient points")

	// ErrDailyCap means today's earn budget is exhausted.
	ErrDailyCap = errors.New("daily earn limit reached")

	// ErrAlreadyClaimed means a one-shot claim has already been consumed.
	ErrAlreadyClaimed = errors.New("already claimed")

	// ErrNotFound means no such account, hold, or receipt.
	ErrNotFound = errors.New("not found")
)

// ReceiptType mirrors the receipt_type enum. The direction of a movement is
// implied by the type; amounts are always positive.
type ReceiptType string

const (
	TypeEarn     ReceiptType = "earn"
	TypeSpend    ReceiptType = "spend"
	TypePurchase ReceiptType = "purchase"
	TypeSync     ReceiptType = "sync"
	TypeAdjust   ReceiptType = "adjust"
)

// Meta is the free-form context attached to a receipt: game ids, product ids,
// PayPal order ids, and so on.
type Meta map[string]any

type Account struct {
	UserID            string    `json:"userId"`
	Balance           int64     `json:"balance"`
	LifetimeEarned    int64     `json:"lifetimeEarned"`
	LifetimeSpent     int64     `json:"lifetimeSpent"`
	LifetimePurchased int64     `json:"lifetimePurchased"`
	ClaimedOffline    bool      `json:"claimedOffline"`
	CreatedAt         time.Time `json:"createdAt"`
}

type Receipt struct {
	ID           int64       `json:"_id"`
	UserID       string      `json:"userId"`
	Type         ReceiptType `json:"type"`
	Amount       int64       `json:"amount"`
	BalanceAfter int64       `json:"balanceAfter"`
	Meta         Meta        `json:"meta"`
	CreatedAt    time.Time   `json:"createdAt"`
}

// Ledger is safe for concurrent use; pgxpool handles connection sharing.
type Ledger struct {
	pool         *pgxpool.Pool
	maxDailyEarn int64
	holdTTL      time.Duration
	now          func() time.Time // injectable so tests can cross a day boundary
}

func New(pool *pgxpool.Pool, maxDailyEarn int64, holdTTL time.Duration) *Ledger {
	return &Ledger{
		pool:         pool,
		maxDailyEarn: maxDailyEarn,
		holdTTL:      holdTTL,
		now:          time.Now,
	}
}

// SetClock replaces the clock. Test-only.
func (l *Ledger) SetClock(now func() time.Time) { l.now = now }

// ---------------------------------------------------------------- reads

// EnsureAccount creates the account row if absent and returns it. Safe under
// concurrency: the ON CONFLICT turns a lost insert race into a plain read.
func (l *Ledger) EnsureAccount(ctx context.Context, userID string) (Account, error) {
	const q = `
		INSERT INTO accounts (user_id) VALUES ($1)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING user_id, balance, lifetime_earned, lifetime_spent,
		          lifetime_purchased, claimed_offline, created_at`
	var a Account
	err := l.pool.QueryRow(ctx, q, userID).Scan(
		&a.UserID, &a.Balance, &a.LifetimeEarned, &a.LifetimeSpent,
		&a.LifetimePurchased, &a.ClaimedOffline, &a.CreatedAt)
	if err != nil {
		return Account{}, fmt.Errorf("ensure account: %w", err)
	}
	return a, nil
}

// Transactions returns the most recent receipts, newest first.
func (l *Ledger) Transactions(ctx context.Context, userID string, limit int) ([]Receipt, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	const q = `
		SELECT id, user_id, type, amount, balance_after, meta, created_at
		  FROM receipts
		 WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2`
	rows, err := l.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list transactions: %w", err)
	}
	defer rows.Close()

	// Non-nil so an empty history marshals as [] rather than null.
	out := make([]Receipt, 0, limit)
	for rows.Next() {
		var r Receipt
		var raw []byte
		if err := rows.Scan(&r.ID, &r.UserID, &r.Type, &r.Amount, &r.BalanceAfter, &raw, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan transaction: %w", err)
		}
		if err := json.Unmarshal(raw, &r.Meta); err != nil {
			return nil, fmt.Errorf("decode receipt meta: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- writes

// Spend debits the balance and writes the receipt in one transaction.
//
// Returns ErrInsufficient when the balance cannot cover the amount. The
// precondition lives in the UPDATE's WHERE clause, so two concurrent spends
// against a balance that covers only one cannot both succeed.
func (l *Ledger) Spend(ctx context.Context, userID string, amount int64, meta Meta) (Account, error) {
	if amount <= 0 {
		return Account{}, fmt.Errorf("spend amount must be positive, got %d", amount)
	}
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Account{}, err
	}

	return l.inTx(ctx, func(tx pgx.Tx) (Account, error) {
		balance, err := debit(ctx, tx, userID, amount)
		if err != nil {
			return Account{}, err
		}
		if err := insertReceipt(ctx, tx, userID, TypeSpend, amount, balance, meta, nil); err != nil {
			return Account{}, err
		}
		return l.load(ctx, tx, userID)
	})
}

// Earn credits play winnings, bounded by the daily budget.
//
// Budget and balance move in the same transaction, so a failure after the budget
// is consumed cannot silently eat the player's allowance — the whole thing rolls
// back together. The Node implementation needed an explicit compensating write
// for exactly this case.
func (l *Ledger) Earn(ctx context.Context, userID string, amount int64, meta Meta) (Account, error) {
	if amount <= 0 {
		return Account{}, fmt.Errorf("earn amount must be positive, got %d", amount)
	}
	if amount > l.maxDailyEarn {
		return Account{}, ErrDailyCap
	}
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Account{}, err
	}

	window := l.now().UTC().Format(time.DateOnly)

	return l.inTx(ctx, func(tx pgx.Tx) (Account, error) {
		// Consume the budget first: it is the cheaper rollback if the credit
		// then fails, and it is the check most likely to reject.
		const budget = `
			INSERT INTO earn_windows (user_id, window_start, earned)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id, window_start) DO UPDATE
			   SET earned = earn_windows.earned + EXCLUDED.earned
			 WHERE earn_windows.earned + EXCLUDED.earned <= $4
			RETURNING earned`
		var earnedToday int64
		err := tx.QueryRow(ctx, budget, userID, window, amount, l.maxDailyEarn).Scan(&earnedToday)
		if errors.Is(err, pgx.ErrNoRows) {
			// The WHERE on DO UPDATE suppressed the write: today is full.
			return Account{}, ErrDailyCap
		}
		if err != nil {
			return Account{}, fmt.Errorf("consume earn budget: %w", err)
		}

		balance, err := credit(ctx, tx, userID, amount, "lifetime_earned")
		if err != nil {
			return Account{}, err
		}
		if err := insertReceipt(ctx, tx, userID, TypeEarn, amount, balance, meta, nil); err != nil {
			return Account{}, err
		}
		return l.load(ctx, tx, userID)
	})
}

// ---------------------------------------------------------------- internals

// debit applies the atomic decrement. The `balance >= $2` precondition is the
// whole safety property; without it this would be a read-then-write and two
// racing spends could both pass.
func debit(ctx context.Context, tx pgx.Tx, userID string, amount int64) (int64, error) {
	const q = `
		UPDATE accounts
		   SET balance        = balance - $2,
		       lifetime_spent = lifetime_spent + $2,
		       updated_at     = now()
		 WHERE user_id = $1 AND balance >= $2
		RETURNING balance`
	var balance int64
	err := tx.QueryRow(ctx, q, userID, amount).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInsufficient
	}
	if err != nil {
		return 0, fmt.Errorf("debit: %w", err)
	}
	return balance, nil
}

// credit applies an increment, also bumping the named lifetime counter.
// counterColumn is never caller-supplied — it comes from a fixed set here — so
// the interpolation cannot carry untrusted input.
func credit(ctx context.Context, tx pgx.Tx, userID string, amount int64, counterColumn string) (int64, error) {
	switch counterColumn {
	case "lifetime_earned", "lifetime_purchased", "":
	default:
		return 0, fmt.Errorf("unsupported lifetime counter %q", counterColumn)
	}

	q := `UPDATE accounts SET balance = balance + $2, updated_at = now()`
	if counterColumn != "" {
		q += fmt.Sprintf(", %s = %s + $2", counterColumn, counterColumn)
	}
	q += ` WHERE user_id = $1 RETURNING balance`

	var balance int64
	if err := tx.QueryRow(ctx, q, userID, amount).Scan(&balance); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("credit: %w", err)
	}
	return balance, nil
}

func insertReceipt(
	ctx context.Context, tx pgx.Tx, userID string, t ReceiptType,
	amount, balanceAfter int64, meta Meta, idempotencyKey *string,
) error {
	if meta == nil {
		meta = Meta{}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode receipt meta: %w", err)
	}

	const q = `
		INSERT INTO receipts (user_id, type, amount, balance_after, meta, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, q, userID, string(t), amount, balanceAfter, raw, idempotencyKey); err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}
	return nil
}

func (l *Ledger) load(ctx context.Context, tx pgx.Tx, userID string) (Account, error) {
	const q = `
		SELECT user_id, balance, lifetime_earned, lifetime_spent,
		       lifetime_purchased, claimed_offline, created_at
		  FROM accounts WHERE user_id = $1`
	var a Account
	err := tx.QueryRow(ctx, q, userID).Scan(
		&a.UserID, &a.Balance, &a.LifetimeEarned, &a.LifetimeSpent,
		&a.LifetimePurchased, &a.ClaimedOffline, &a.CreatedAt)
	if err != nil {
		return Account{}, fmt.Errorf("load account: %w", err)
	}
	return a, nil
}

// inTx runs fn inside a transaction, rolling back on any error. The deferred
// rollback is a no-op once Commit has succeeded, so the happy path is unaffected.
func (l *Ledger) inTx(ctx context.Context, fn func(pgx.Tx) (Account, error)) (Account, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out, err := fn(tx)
	if err != nil {
		return Account{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}
