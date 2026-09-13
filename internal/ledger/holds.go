package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hold is a debit awaiting confirmation.
//
// blog-portfolio still owns products and purchases in MongoDB, so a debit here
// and a purchase row there cannot share a transaction. The caller takes a hold,
// writes its own row, then commits or releases.
//
// Holds expire. That is the part worth caring about: spend-then-refund leaves
// the player short whenever the *refund* is the call that fails, with no way to
// recover from the caller's side. An expiring hold heals with nobody online.
type Hold struct {
	ID           string    `json:"holdId"`
	UserID       string    `json:"userId"`
	Amount       int64     `json:"amount"`
	Status       string    `json:"status"`
	BalanceAfter int64     `json:"balance"`
	ExpiresAt    time.Time `json:"expiresAt"`
}

const (
	HoldHeld      = "held"
	HoldCommitted = "committed"
	HoldReleased  = "released"
	HoldExpired   = "expired"
)

// ErrHoldSettled means the hold was already committed, released or expired.
var ErrHoldSettled = errors.New("hold already settled")

// Hold debits immediately and records the claim. The points leave the balance
// now; the receipt is only written on commit, because until then we do not know
// what the spend was actually for.
func (l *Ledger) Hold(ctx context.Context, userID string, amount int64, meta Meta) (Hold, error) {
	if amount <= 0 {
		return Hold{}, fmt.Errorf("hold amount must be positive, got %d", amount)
	}
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Hold{}, err
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Hold{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	balance, err := debit(ctx, tx, userID, amount)
	if err != nil {
		return Hold{}, err
	}

	raw, err := marshalMeta(meta)
	if err != nil {
		return Hold{}, err
	}

	const q = `
		INSERT INTO holds (user_id, amount, meta, expires_at)
		VALUES ($1, $2, $3, now() + $4::interval)
		RETURNING id, status, expires_at`
	h := Hold{UserID: userID, Amount: amount, BalanceAfter: balance}
	if err := tx.QueryRow(ctx, q, userID, amount, raw, l.holdTTL.String()).
		Scan(&h.ID, &h.Status, &h.ExpiresAt); err != nil {
		return Hold{}, fmt.Errorf("insert hold: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Hold{}, fmt.Errorf("commit: %w", err)
	}
	return h, nil
}

// CommitHold finalises a held debit and writes its receipt.
//
// Idempotent by hold id: committing an already-committed hold succeeds without
// doing anything twice, so a caller that retries after a timeout is safe.
func (l *Ledger) CommitHold(ctx context.Context, holdID string, meta Meta) (Account, error) {
	return l.settle(ctx, holdID, HoldCommitted, meta)
}

// ReleaseHold returns held points to the balance.
//
// Idempotent by hold id, for the same reason as CommitHold.
func (l *Ledger) ReleaseHold(ctx context.Context, holdID string) (Account, error) {
	return l.settle(ctx, holdID, HoldReleased, nil)
}

func (l *Ledger) settle(ctx context.Context, holdID, target string, extraMeta Meta) (Account, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row so two concurrent settles cannot both act on it.
	const lock = `
		SELECT user_id, amount, status, meta, expires_at
		  FROM holds WHERE id = $1 FOR UPDATE`
	var (
		userID    string
		amount    int64
		status    string
		rawMeta   []byte
		expiresAt time.Time
	)
	err = tx.QueryRow(ctx, lock, holdID).Scan(&userID, &amount, &status, &rawMeta, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("load hold: %w", err)
	}

	// Already settled: report the current account rather than acting again. A
	// retried commit must not debit twice, and a retried release must not
	// credit twice.
	if status != HoldHeld {
		if status == target {
			acct, err := l.load(ctx, tx, userID)
			if err != nil {
				return Account{}, err
			}
			return acct, tx.Commit(ctx)
		}
		return Account{}, ErrHoldSettled
	}

	// An expired hold has had its points returned by the sweeper, so committing
	// it would charge the player for something already refunded.
	if target == HoldCommitted && !expiresAt.After(time.Now()) {
		return Account{}, ErrHoldSettled
	}

	switch target {
	case HoldCommitted:
		meta, err := mergeMeta(rawMeta, extraMeta)
		if err != nil {
			return Account{}, err
		}
		// The balance already moved when the hold was taken; the receipt
		// records the balance as it stands now.
		acct, err := l.load(ctx, tx, userID)
		if err != nil {
			return Account{}, err
		}
		var receiptID int64
		if receiptID, err = insertReceiptReturningID(ctx, tx, userID, TypeSpend, amount, acct.Balance, meta); err != nil {
			return Account{}, err
		}
		const mark = `UPDATE holds SET status = $2, settled_at = now(), receipt_id = $3 WHERE id = $1`
		if _, err := tx.Exec(ctx, mark, holdID, HoldCommitted, receiptID); err != nil {
			return Account{}, fmt.Errorf("mark hold committed: %w", err)
		}
		return acct, tx.Commit(ctx)

	case HoldReleased:
		if _, err := credit(ctx, tx, userID, amount, ""); err != nil {
			return Account{}, err
		}
		// Undo the lifetime_spent bump the debit applied — the spend never
		// happened, so it should not appear in lifetime totals.
		const unwind = `UPDATE accounts SET lifetime_spent = lifetime_spent - $2 WHERE user_id = $1`
		if _, err := tx.Exec(ctx, unwind, userID, amount); err != nil {
			return Account{}, fmt.Errorf("unwind lifetime spent: %w", err)
		}
		const mark = `UPDATE holds SET status = $2, settled_at = now() WHERE id = $1`
		if _, err := tx.Exec(ctx, mark, holdID, HoldReleased); err != nil {
			return Account{}, fmt.Errorf("mark hold released: %w", err)
		}
		acct, err := l.load(ctx, tx, userID)
		if err != nil {
			return Account{}, err
		}
		return acct, tx.Commit(ctx)

	default:
		return Account{}, fmt.Errorf("unsupported hold target %q", target)
	}
}

// SweepExpiredHolds returns points from holds nobody settled.
//
// This is what makes the two-phase spend safe: a caller that dies between
// taking a hold and settling it does not strand the player's points. Run it on
// a ticker. Safe to run concurrently from several instances — each row is
// claimed by the UPDATE's own predicate.
func (l *Ledger) SweepExpiredHolds(ctx context.Context) (int, error) {
	const q = `
		WITH expired AS (
			UPDATE holds
			   SET status = $1, settled_at = now()
			 WHERE status = $2 AND expires_at <= now()
			RETURNING user_id, amount
		), restored AS (
			UPDATE accounts a
			   SET balance        = a.balance + e.amount,
			       lifetime_spent = a.lifetime_spent - e.amount,
			       updated_at     = now()
			  FROM expired e
			 WHERE a.user_id = e.user_id
			RETURNING 1
		)
		SELECT count(*) FROM expired`
	var n int
	if err := l.pool.QueryRow(ctx, q, HoldExpired, HoldHeld).Scan(&n); err != nil {
		return 0, fmt.Errorf("sweep expired holds: %w", err)
	}
	if n > 0 {
		// Worth noticing: a steady stream means a caller is failing to settle.
		logSweep(n)
	}
	return n, nil
}

// logSweep is a seam so the sweeper can be observed without the ledger taking a
// dependency on a logger.
var logSweep = func(int) {}

// SetSweepObserver installs a callback invoked with the number of holds
// reclaimed by each sweep.
func SetSweepObserver(fn func(int)) {
	if fn != nil {
		logSweep = fn
	}
}
