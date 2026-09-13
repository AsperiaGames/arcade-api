package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation is Postgres SQLSTATE 23505.
const uniqueViolation = "23505"

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// Credit adds purchased points, keyed by an idempotency token.
//
// This replaces a read-then-write check in the Node original, which asked "have
// we already recorded this PayPal order?" and then inserted — so two concurrent
// captures could both look, both miss, and both credit. Here the UNIQUE index on
// receipts.idempotency_key decides, and a duplicate is reported as success
// because the caller's intent ("this order is paid") is already satisfied.
//
// Returns the account and whether this call was the one that applied the credit.
func (l *Ledger) Credit(
	ctx context.Context, userID string, amount int64, idempotencyKey string, meta Meta,
) (Account, bool, error) {
	if amount <= 0 {
		return Account{}, false, fmt.Errorf("credit amount must be positive, got %d", amount)
	}
	if idempotencyKey == "" {
		return Account{}, false, errors.New("credit requires an idempotency key")
	}
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Account{}, false, err
	}

	applied := true
	acct, err := l.inTx(ctx, func(tx pgx.Tx) (Account, error) {
		balance, err := credit(ctx, tx, userID, amount, "lifetime_purchased")
		if err != nil {
			return Account{}, err
		}
		// Bump lifetime_earned too: purchased points are also earned points, as
		// in the original.
		const bump = `UPDATE accounts SET lifetime_earned = lifetime_earned + $2 WHERE user_id = $1`
		if _, err := tx.Exec(ctx, bump, userID, amount); err != nil {
			return Account{}, fmt.Errorf("bump lifetime earned: %w", err)
		}

		if err := insertReceipt(ctx, tx, userID, TypePurchase, amount, balance, meta, &idempotencyKey); err != nil {
			if isUniqueViolation(err) {
				// Someone already credited this order. Abandon the transaction
				// so our increment is discarded, and report the existing state.
				applied = false
				return Account{}, errDuplicateCredit
			}
			return Account{}, err
		}
		return l.load(ctx, tx, userID)
	})

	if errors.Is(err, errDuplicateCredit) {
		current, loadErr := l.EnsureAccount(ctx, userID)
		if loadErr != nil {
			return Account{}, false, loadErr
		}
		return current, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return acct, applied, nil
}

// errDuplicateCredit unwinds the transaction on an idempotency collision. It is
// never returned to callers — Credit converts it into a non-applied success.
var errDuplicateCredit = errors.New("duplicate credit")

// ClaimOffline performs the one-shot claim of points banked in localStorage
// while signed out.
//
// The `claimed_offline = false` precondition makes this unrepeatable: a second
// call, concurrent or later, matches no rows and returns ErrAlreadyClaimed.
func (l *Ledger) ClaimOffline(ctx context.Context, userID string, amount int64, meta Meta) (Account, error) {
	if amount <= 0 {
		return Account{}, fmt.Errorf("claim amount must be positive, got %d", amount)
	}
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Account{}, err
	}

	return l.inTx(ctx, func(tx pgx.Tx) (Account, error) {
		const q = `
			UPDATE accounts
			   SET balance                = balance + $2,
			       lifetime_earned        = lifetime_earned + $2,
			       claimed_offline        = true,
			       claimed_offline_amount = $2,
			       updated_at             = now()
			 WHERE user_id = $1 AND claimed_offline = false
			RETURNING balance`
		var balance int64
		err := tx.QueryRow(ctx, q, userID, amount).Scan(&balance)
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrAlreadyClaimed
		}
		if err != nil {
			return Account{}, fmt.Errorf("claim offline: %w", err)
		}

		if err := insertReceipt(ctx, tx, userID, TypeSync, amount, balance, meta, nil); err != nil {
			return Account{}, err
		}
		return l.load(ctx, tx, userID)
	})
}

// RecordGuestEarn accumulates play winnings for an anonymous session.
//
// Storm-Gate mints a fresh guest identity per anonymous login, so these must not
// touch `accounts` — that would leave one unreachable balance per session,
// created from an unauthenticated endpoint. They live apart until claimed.
func (l *Ledger) RecordGuestEarn(ctx context.Context, guestID string, amount int64, ttl time.Duration) (int64, error) {
	if amount <= 0 {
		return 0, fmt.Errorf("guest earn amount must be positive, got %d", amount)
	}
	if amount > l.maxDailyEarn {
		return 0, ErrDailyCap
	}

	const q = `
		INSERT INTO guest_earnings (guest_id, balance, expires_at)
		VALUES ($1, $2, now() + $3::interval)
		ON CONFLICT (guest_id) DO UPDATE
		   SET balance = guest_earnings.balance + EXCLUDED.balance
		 WHERE guest_earnings.claimed_at IS NULL
		   AND guest_earnings.balance + EXCLUDED.balance <= $4
		RETURNING balance`
	var balance int64
	err := l.pool.QueryRow(ctx, q, guestID, amount, ttl.String(), l.maxDailyEarn).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either already claimed or over the guest ceiling; both mean "no more".
		return 0, ErrDailyCap
	}
	if err != nil {
		return 0, fmt.Errorf("record guest earn: %w", err)
	}
	return balance, nil
}

// ClaimGuestEarnings moves an anonymous balance onto a real account, once.
func (l *Ledger) ClaimGuestEarnings(ctx context.Context, guestID, userID string) (Account, int64, error) {
	if _, err := l.EnsureAccount(ctx, userID); err != nil {
		return Account{}, 0, err
	}

	var moved int64
	acct, err := l.inTx(ctx, func(tx pgx.Tx) (Account, error) {
		const claim = `
			UPDATE guest_earnings
			   SET claimed_by = $2, claimed_at = now()
			 WHERE guest_id = $1 AND claimed_at IS NULL AND expires_at > now()
			RETURNING balance`
		err := tx.QueryRow(ctx, claim, guestID, userID).Scan(&moved)
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrAlreadyClaimed
		}
		if err != nil {
			return Account{}, fmt.Errorf("claim guest earnings: %w", err)
		}

		if moved > 0 {
			balance, err := credit(ctx, tx, userID, moved, "lifetime_earned")
			if err != nil {
				return Account{}, err
			}
			meta := Meta{"reason": "guest-claim", "guestId": guestID}
			if err := insertReceipt(ctx, tx, userID, TypeSync, moved, balance, meta, nil); err != nil {
				return Account{}, err
			}
		}
		return l.load(ctx, tx, userID)
	})
	if err != nil {
		return Account{}, 0, err
	}
	return acct, moved, nil
}
