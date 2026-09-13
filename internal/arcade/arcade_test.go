package arcade_test

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

	"github.com/HoseaCodes/arcade-api/internal/arcade"
	"github.com/HoseaCodes/arcade-api/internal/ledger"
	"github.com/HoseaCodes/arcade-api/internal/store"
)

const maxDailyEarn = 1000

var testPool *pgxpool.Pool

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

	code := m.Run()

	testPool.Close()
	_ = testcontainers.TerminateContainer(ctr)
	os.Exit(code)
}

func newArcade(t *testing.T) (*arcade.Arcade, *ledger.Ledger) {
	t.Helper()
	const truncate = `TRUNCATE scores, game_sessions, receipts, holds, earn_windows, guest_earnings, accounts RESTART IDENTITY CASCADE`
	if _, err := testPool.Exec(context.Background(), truncate); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	l := ledger.New(testPool, maxDailyEarn, time.Minute)
	return arcade.New(testPool, l), l
}

// backdate moves a session's start time into the past so duration rules can be
// exercised without the test actually waiting.
func backdate(t *testing.T, sessionID string, d time.Duration) {
	t.Helper()
	const q = `UPDATE game_sessions SET started_at = now() - $2::interval WHERE id = $1`
	if _, err := testPool.Exec(context.Background(), q, sessionID, d.String()); err != nil {
		t.Fatalf("backdate session: %v", err)
	}
}

func TestStartSessionOpensARun(t *testing.T) {
	a, _ := newArcade(t)

	s, err := a.StartSession(context.Background(), "u1", "pac-man")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.ID == "" {
		t.Error("no session id")
	}
	if s.GameID != "pac-man" {
		t.Errorf("gameId = %q, want pac-man", s.GameID)
	}
	if s.StartedAt.IsZero() {
		t.Error("startedAt not set")
	}
}

func TestSettleAwardsPointsAndRecordsTheScore(t *testing.T) {
	a, l := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "u1", "pac-man")
	backdate(t, s.ID, time.Minute) // a plausible run length

	// pac-man converts at 0.05 points per score: 2000 -> 100 points.
	settlement, err := a.SettleSession(ctx, "u1", s.ID, 2000)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !settlement.Accepted {
		t.Fatalf("settlement rejected: %+v", settlement)
	}
	if settlement.Credited != 100 {
		t.Errorf("credited = %d, want 100", settlement.Credited)
	}
	if settlement.Balance != 100 {
		t.Errorf("balance = %d, want 100", settlement.Balance)
	}

	// The award went through the ledger, so there is a receipt carrying the
	// session that produced it.
	receipts, _ := l.Transactions(ctx, "u1", 10)
	if len(receipts) != 1 {
		t.Fatalf("got %d receipts, want 1", len(receipts))
	}
	if receipts[0].Meta["sessionId"] != s.ID {
		t.Errorf("receipt meta.sessionId = %v, want %s", receipts[0].Meta["sessionId"], s.ID)
	}

	entries, err := a.Leaderboard(ctx, "pac-man", arcade.WindowAll, 10)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if len(entries) != 1 || entries[0].Score != 2000 {
		t.Errorf("leaderboard = %+v, want one entry scoring 2000", entries)
	}
}

// The check that actually matters. Without it any authenticated player could
// settle a stranger's session and bank the points onto their own account.
func TestSettleRejectsSomeoneElsesSession(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "victim", "pac-man")
	backdate(t, s.ID, time.Minute)

	settlement, err := a.SettleSession(ctx, "attacker", s.ID, 100000)
	if !errors.Is(err, arcade.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if settlement.Reason != arcade.RejectNotYours {
		t.Errorf("reason = %q, want %q", settlement.Reason, arcade.RejectNotYours)
	}

	// Neither account gained anything.
	var total int64
	_ = testPool.QueryRow(ctx, `SELECT coalesce(sum(balance),0) FROM accounts`).Scan(&total)
	if total != 0 {
		t.Fatalf("balance moved on a stolen session: %d", total)
	}
	// And the victim's session is still open for them to settle themselves.
	var status string
	_ = testPool.QueryRow(ctx, `SELECT status FROM game_sessions WHERE id = $1`, s.ID).Scan(&status)
	if status != arcade.StatusOpen {
		t.Errorf("victim's session status = %q, want open", status)
	}
}

func TestSettleRejectsAnImplausiblyFastRun(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	// Settled immediately: pac-man requires at least 10 seconds of play.
	s, _ := a.StartSession(ctx, "u1", "pac-man")

	settlement, err := a.SettleSession(ctx, "u1", s.ID, 50000)
	if !errors.Is(err, arcade.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if settlement.Reason != arcade.RejectTooFast {
		t.Errorf("reason = %q, want %q", settlement.Reason, arcade.RejectTooFast)
	}

	// The rejection is recorded rather than discarded — a run of these from one
	// player is the signal worth acting on.
	var status string
	var reported int64
	_ = testPool.QueryRow(ctx,
		`SELECT status, reported_score FROM game_sessions WHERE id = $1`, s.ID).Scan(&status, &reported)
	if status != arcade.StatusRejected {
		t.Errorf("status = %q, want rejected", status)
	}
	if reported != 50000 {
		t.Errorf("reported_score = %d, want the claimed 50000 kept for review", reported)
	}
}

func TestSettleRejectsAnImpossibleScore(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	// tic-tac-toe tops out at 100.
	s, _ := a.StartSession(ctx, "u1", "tic-tac-toe")
	backdate(t, s.ID, time.Minute)

	settlement, err := a.SettleSession(ctx, "u1", s.ID, 999999)
	if !errors.Is(err, arcade.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if settlement.Reason != arcade.RejectTooHigh {
		t.Errorf("reason = %q, want %q", settlement.Reason, arcade.RejectTooHigh)
	}
}

func TestSettleRejectsAStaleSession(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "u1", "pac-man") // max duration one hour
	backdate(t, s.ID, 3*time.Hour)

	settlement, err := a.SettleSession(ctx, "u1", s.ID, 1000)
	if !errors.Is(err, arcade.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if settlement.Reason != arcade.RejectTooSlow {
		t.Errorf("reason = %q, want %q", settlement.Reason, arcade.RejectTooSlow)
	}
}

// Replay protection. Without it a captured settle request is free points on
// repeat.
func TestSettleCannotBeReplayed(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "u1", "pac-man")
	backdate(t, s.ID, time.Minute)

	if _, err := a.SettleSession(ctx, "u1", s.ID, 2000); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	settlement, err := a.SettleSession(ctx, "u1", s.ID, 2000)
	if !errors.Is(err, arcade.ErrRejected) {
		t.Fatalf("replay err = %v, want ErrRejected", err)
	}
	if settlement.Reason != arcade.RejectSettled {
		t.Errorf("reason = %q, want %q", settlement.Reason, arcade.RejectSettled)
	}

	var balance int64
	_ = testPool.QueryRow(ctx, `SELECT balance FROM accounts WHERE user_id = 'u1'`).Scan(&balance)
	if balance != 100 {
		t.Errorf("balance = %d, want 100 — the replay paid out again", balance)
	}
}

func TestConcurrentSettlesPayOnce(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "u1", "pac-man")
	backdate(t, s.ID, time.Minute)

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		paid  int
		start = make(chan struct{})
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, err := a.SettleSession(ctx, "u1", s.ID, 2000)
			if err == nil && st.Credited > 0 {
				mu.Lock()
				paid++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if paid != 1 {
		t.Errorf("paid %d times, want exactly 1", paid)
	}
	var balance int64
	_ = testPool.QueryRow(ctx, `SELECT balance FROM accounts WHERE user_id = 'u1'`).Scan(&balance)
	if balance != 100 {
		t.Errorf("balance = %d, want 100", balance)
	}
	var scoreRows int
	_ = testPool.QueryRow(ctx, `SELECT count(*) FROM scores`).Scan(&scoreRows)
	if scoreRows != 1 {
		t.Errorf("recorded %d scores, want 1", scoreRows)
	}
}

// Hitting the daily cap is a successful settle that credits nothing. The run
// happened and belongs on the leaderboard; there is simply no budget left.
func TestSettleAtTheDailyCapStillCountsTheScore(t *testing.T) {
	a, l := newArcade(t)
	ctx := context.Background()

	if _, err := l.Earn(ctx, "u1", maxDailyEarn, nil); err != nil {
		t.Fatalf("exhaust budget: %v", err)
	}

	s, _ := a.StartSession(ctx, "u1", "pac-man")
	backdate(t, s.ID, time.Minute)

	settlement, err := a.SettleSession(ctx, "u1", s.ID, 2000)
	if err != nil {
		t.Fatalf("settle should succeed at the cap, got %v", err)
	}
	if !settlement.Accepted {
		t.Error("settlement should be accepted even with no budget left")
	}
	if settlement.Credited != 0 {
		t.Errorf("credited = %d, want 0", settlement.Credited)
	}
	if settlement.Reason != "daily-cap" {
		t.Errorf("reason = %q, want daily-cap", settlement.Reason)
	}

	entries, _ := a.Leaderboard(ctx, "pac-man", arcade.WindowAll, 10)
	if len(entries) != 1 {
		t.Errorf("score did not reach the leaderboard: %+v", entries)
	}
}

func TestSettleUnknownSession(t *testing.T) {
	a, _ := newArcade(t)
	_, err := a.SettleSession(context.Background(), "u1",
		"00000000-0000-0000-0000-000000000000", 100)
	if !errors.Is(err, arcade.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// One row per player, not one per run — otherwise a single strong player fills
// the board with their own attempts.
func TestLeaderboardShowsPersonalBests(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	play := func(user string, score int64) {
		t.Helper()
		s, _ := a.StartSession(ctx, user, "pac-man")
		backdate(t, s.ID, time.Minute)
		if _, err := a.SettleSession(ctx, user, s.ID, score); err != nil {
			t.Fatalf("settle %s/%d: %v", user, score, err)
		}
	}

	play("alice", 1000)
	play("alice", 3000) // alice's best
	play("alice", 2000)
	play("bob", 2500)

	entries, err := a.Leaderboard(ctx, "pac-man", arcade.WindowAll, 10)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (one per player): %+v", len(entries), entries)
	}
	if entries[0].UserID != "alice" || entries[0].Score != 3000 {
		t.Errorf("first = %+v, want alice with 3000", entries[0])
	}
	if entries[1].UserID != "bob" || entries[1].Score != 2500 {
		t.Errorf("second = %+v, want bob with 2500", entries[1])
	}
	if entries[0].Rank != 1 || entries[1].Rank != 2 {
		t.Errorf("ranks = %d,%d, want 1,2", entries[0].Rank, entries[1].Rank)
	}
}

func TestLeaderboardWindowExcludesOldScores(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	s, _ := a.StartSession(ctx, "u1", "pac-man")
	backdate(t, s.ID, time.Minute)
	if _, err := a.SettleSession(ctx, "u1", s.ID, 2000); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Age the score past the daily window.
	if _, err := testPool.Exec(ctx,
		`UPDATE scores SET created_at = now() - interval '3 days'`); err != nil {
		t.Fatalf("age score: %v", err)
	}

	all, _ := a.Leaderboard(ctx, "pac-man", arcade.WindowAll, 10)
	if len(all) != 1 {
		t.Errorf("all-time board lost the score: %+v", all)
	}
	day, _ := a.Leaderboard(ctx, "pac-man", arcade.WindowDay, 10)
	if len(day) != 0 {
		t.Errorf("daily board included a 3-day-old score: %+v", day)
	}
	week, _ := a.Leaderboard(ctx, "pac-man", arcade.WindowWeek, 10)
	if len(week) != 1 {
		t.Errorf("weekly board excluded a 3-day-old score: %+v", week)
	}
}

func TestGlobalLeaderboardSumsPerGameBests(t *testing.T) {
	a, _ := newArcade(t)
	ctx := context.Background()

	play := func(user, game string, score int64) {
		t.Helper()
		s, _ := a.StartSession(ctx, user, game)
		backdate(t, s.ID, 2*time.Minute)
		if _, err := a.SettleSession(ctx, user, s.ID, score); err != nil {
			t.Fatalf("settle %s/%s: %v", user, game, err)
		}
	}

	// alice: 3000 best at pac-man + 400 at whac-a-mole = 3400
	play("alice", "pac-man", 1000)
	play("alice", "pac-man", 3000)
	play("alice", "whac-a-mole", 400)
	// bob: 5000 at space-invaders
	play("bob", "space-invaders", 5000)

	entries, err := a.GlobalLeaderboard(ctx, arcade.WindowAll, 10)
	if err != nil {
		t.Fatalf("global: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].UserID != "bob" || entries[0].Score != 5000 {
		t.Errorf("first = %+v, want bob with 5000", entries[0])
	}
	if entries[1].UserID != "alice" || entries[1].Score != 3400 {
		t.Errorf("second = %+v, want alice with 3400 (3000 + 400, not every run)", entries[1])
	}
}

func TestLeaderboardIsEmptyNotNull(t *testing.T) {
	a, _ := newArcade(t)
	entries, err := a.Leaderboard(context.Background(), "pac-man", arcade.WindowAll, 10)
	if err != nil {
		t.Fatalf("leaderboard: %v", err)
	}
	if entries == nil {
		t.Error("empty leaderboard returned nil, want an empty slice so it marshals as []")
	}
}
