package arcade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/HoseaCodes/arcade-api/internal/ledger"
)

var (
	// ErrNotFound means no such session.
	ErrNotFound = errors.New("session not found")

	// ErrRejected means the session exists but the claim was refused. The
	// accompanying RejectReason says why.
	ErrRejected = errors.New("score rejected")
)

const (
	StatusOpen     = "open"
	StatusSettled  = "settled"
	StatusRejected = "rejected"
)

type Session struct {
	ID        string    `json:"sessionId"`
	GameID    string    `json:"gameId"`
	StartedAt time.Time `json:"startedAt"`
}

// Settlement is the outcome of reporting a score.
type Settlement struct {
	Accepted bool         `json:"accepted"`
	Credited int64        `json:"credited"`
	Balance  int64        `json:"balance"`
	Score    int64        `json:"score"`
	Reason   RejectReason `json:"reason,omitempty"`
}

type Arcade struct {
	pool   *pgxpool.Pool
	ledger *ledger.Ledger
}

func New(pool *pgxpool.Pool, l *ledger.Ledger) *Arcade {
	return &Arcade{pool: pool, ledger: l}
}

// StartSession opens a run.
//
// The returned id is what makes a later score checkable: without it, a reported
// score is an unverifiable assertion about something that happened in a browser.
func (a *Arcade) StartSession(ctx context.Context, userID, gameID string) (Session, error) {
	if gameID == "" {
		return Session{}, fmt.Errorf("gameId required")
	}

	const q = `
		INSERT INTO game_sessions (user_id, game_id)
		VALUES ($1, $2)
		RETURNING id, game_id, started_at`
	var s Session
	if err := a.pool.QueryRow(ctx, q, userID, gameID).Scan(&s.ID, &s.GameID, &s.StartedAt); err != nil {
		return Session{}, fmt.Errorf("start session: %w", err)
	}
	return s, nil
}

// SettleSession reports a final score and awards points.
//
// The session is claimed atomically before anything else happens, so a replayed
// request cannot be paid twice — that guard is the reason this is worth having
// at all.
//
// Points are awarded through the ledger's Earn, so the daily budget still
// applies. Hitting the cap is a successful settle that credits zero: the run
// happened and belongs on the leaderboard, there is simply no budget left.
func (a *Arcade) SettleSession(ctx context.Context, userID, sessionID string, score int64) (Settlement, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return Settlement{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const lock = `
		SELECT user_id, game_id, started_at, status
		  FROM game_sessions WHERE id = $1 FOR UPDATE`
	var (
		owner     string
		gameID    string
		startedAt time.Time
		status    string
	)
	err = tx.QueryRow(ctx, lock, sessionID).Scan(&owner, &gameID, &startedAt, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settlement{}, ErrNotFound
	}
	if err != nil {
		return Settlement{}, fmt.Errorf("load session: %w", err)
	}

	// Ownership before anything else. Without this check any authenticated
	// player could settle a stranger's session and bank the points onto their
	// own account.
	if owner != userID {
		return Settlement{Reason: RejectNotYours}, ErrRejected
	}
	if status != StatusOpen {
		return Settlement{Reason: RejectSettled}, ErrRejected
	}

	rule := RuleFor(gameID)
	points, reason, ok := rule.Validate(score, time.Since(startedAt))

	if !ok {
		// Record the rejection rather than discarding it: a run of these from
		// one player is the signal worth acting on.
		const mark = `
			UPDATE game_sessions
			   SET status = $2, settled_at = now(), reported_score = $3, accepted_amount = 0
			 WHERE id = $1`
		if _, err := tx.Exec(ctx, mark, sessionID, StatusRejected, score); err != nil {
			return Settlement{}, fmt.Errorf("mark session rejected: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Settlement{}, fmt.Errorf("commit: %w", err)
		}
		return Settlement{Accepted: false, Score: score, Reason: reason}, ErrRejected
	}

	// Claim the session and record the score for the leaderboard in one
	// transaction. Committing here — before awarding points — is deliberate: it
	// closes the replay window even if the award below fails.
	const settle = `
		UPDATE game_sessions
		   SET status = $2, settled_at = now(), reported_score = $3, accepted_amount = $4
		 WHERE id = $1`
	if _, err := tx.Exec(ctx, settle, sessionID, StatusSettled, score, points); err != nil {
		return Settlement{}, fmt.Errorf("settle session: %w", err)
	}

	const recordScore = `
		INSERT INTO scores (user_id, game_id, score, session_id)
		VALUES ($1, $2, $3, $4)`
	if _, err := tx.Exec(ctx, recordScore, userID, gameID, score, sessionID); err != nil {
		return Settlement{}, fmt.Errorf("record score: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Settlement{}, fmt.Errorf("commit: %w", err)
	}

	out := Settlement{Accepted: true, Score: score}
	if points == 0 {
		// A legitimate run worth less than one point. Nothing to credit.
		acct, err := a.ledger.EnsureAccount(ctx, userID)
		if err != nil {
			return Settlement{}, err
		}
		out.Balance = acct.Balance
		return out, nil
	}

	meta := ledger.Meta{"gameId": gameID, "sessionId": sessionID, "score": score}
	acct, _, err := a.ledger.Earn(ctx, userID, points, meta)
	if errors.Is(err, ledger.ErrDailyCap) {
		// Not a failure: the run counted and is on the leaderboard, there is
		// just no budget left today.
		current, loadErr := a.ledger.EnsureAccount(ctx, userID)
		if loadErr != nil {
			return Settlement{}, loadErr
		}
		out.Credited = 0
		out.Balance = current.Balance
		out.Reason = "daily-cap"
		return out, nil
	}
	if err != nil {
		return Settlement{}, fmt.Errorf("award points: %w", err)
	}

	out.Credited = points
	out.Balance = acct.Balance
	return out, nil
}

// LeaderboardEntry is one row of a board.
type LeaderboardEntry struct {
	Rank       int       `json:"rank"`
	UserID     string    `json:"userId"`
	GameID     string    `json:"gameId"`
	Score      int64     `json:"score"`
	AchievedAt time.Time `json:"achievedAt"`
}

// Window bounds a leaderboard in time.
type Window string

const (
	WindowAll  Window = "all"
	WindowWeek Window = "week"
	WindowDay  Window = "day"
)

// ParseWindow maps a query parameter to a window, defaulting to all-time.
func ParseWindow(raw string) Window {
	switch Window(raw) {
	case WindowDay:
		return WindowDay
	case WindowWeek:
		return WindowWeek
	default:
		return WindowAll
	}
}

func (w Window) since() (time.Time, bool) {
	switch w {
	case WindowDay:
		return time.Now().Add(-24 * time.Hour), true
	case WindowWeek:
		return time.Now().Add(-7 * 24 * time.Hour), true
	default:
		return time.Time{}, false
	}
}

// Leaderboard returns the top scores for one game.
//
// One row per player — their personal best — rather than one row per run.
// Otherwise a single strong player fills the whole board with their own
// attempts, which is a worse board and a worse incentive.
func (a *Arcade) Leaderboard(ctx context.Context, gameID string, w Window, limit int) ([]LeaderboardEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	// Two explicit forms rather than one assembled string. The windowed variant
	// needs an extra placeholder, and juggling argument positions to share a
	// single query is how a leaderboard quietly starts filtering on the wrong
	// value.
	const unbounded = `
		SELECT user_id, game_id, score, created_at FROM (
			SELECT DISTINCT ON (user_id) user_id, game_id, score, created_at
			  FROM scores
			 WHERE game_id = $1
			 ORDER BY user_id, score DESC, created_at ASC
		) best
		 ORDER BY score DESC, created_at ASC
		 LIMIT $2`

	const windowed = `
		SELECT user_id, game_id, score, created_at FROM (
			SELECT DISTINCT ON (user_id) user_id, game_id, score, created_at
			  FROM scores
			 WHERE game_id = $1 AND created_at >= $3
			 ORDER BY user_id, score DESC, created_at ASC
		) best
		 ORDER BY score DESC, created_at ASC
		 LIMIT $2`

	q, args := unbounded, []any{gameID, limit}
	if since, bounded := w.since(); bounded {
		q, args = windowed, []any{gameID, limit, since}
	}

	rows, err := a.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("leaderboard: %w", err)
	}
	defer rows.Close()

	out := make([]LeaderboardEntry, 0, limit)
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.GameID, &e.Score, &e.AchievedAt); err != nil {
			return nil, fmt.Errorf("scan leaderboard row: %w", err)
		}
		e.Rank = len(out) + 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// GlobalLeaderboard ranks players by their total best score across all games.
func (a *Arcade) GlobalLeaderboard(ctx context.Context, w Window, limit int) ([]LeaderboardEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	const unbounded = `
		SELECT user_id, sum(best) AS total, max(achieved) AS achieved FROM (
			SELECT user_id, game_id, max(score) AS best, max(created_at) AS achieved
			  FROM scores
			 GROUP BY user_id, game_id
		) per_game
		 GROUP BY user_id
		 ORDER BY total DESC, achieved ASC
		 LIMIT $1`

	const windowed = `
		SELECT user_id, sum(best) AS total, max(achieved) AS achieved FROM (
			SELECT user_id, game_id, max(score) AS best, max(created_at) AS achieved
			  FROM scores
			 WHERE created_at >= $2
			 GROUP BY user_id, game_id
		) per_game
		 GROUP BY user_id
		 ORDER BY total DESC, achieved ASC
		 LIMIT $1`

	q, args := unbounded, []any{limit}
	if since, bounded := w.since(); bounded {
		q, args = windowed, []any{limit, since}
	}

	rows, err := a.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("global leaderboard: %w", err)
	}
	defer rows.Close()

	out := make([]LeaderboardEntry, 0, limit)
	for rows.Next() {
		var e LeaderboardEntry
		if err := rows.Scan(&e.UserID, &e.Score, &e.AchievedAt); err != nil {
			return nil, fmt.Errorf("scan global row: %w", err)
		}
		e.Rank = len(out) + 1
		out = append(out, e)
	}
	return out, rows.Err()
}
