// Package arcade owns game sessions, score validation and leaderboards.
//
// It sits on top of the ledger rather than beside it: a settled session awards
// points by calling Earn, so the daily budget and every ledger invariant still
// apply. Nothing here writes a balance directly.
//
// The problem this solves: /points/earn takes a client-supplied amount from a
// browser, capped only by a per-call and per-day ceiling. Within those bounds a
// forged request is indistinguishable from real play. A session gives the server
// something to check the claim against — when the run started, which game it
// was, and whether the score is achievable in the elapsed time.
package arcade

import (
	"math"
	"time"
)

// Rule bounds what a single run of a game may claim.
//
// These are deliberately loose. The goal is to make bulk fabrication expensive,
// not to police legitimate play — a false rejection costs a real player their
// run, which is worse than letting one inflated score through.
type Rule struct {
	// MaxScore is the highest believable score for one run. Zero means no
	// ceiling beyond the per-call earn cap.
	MaxScore int64

	// MinDuration is the shortest a genuine run can take. A score reported
	// milliseconds after the session opened did not come from playing.
	MinDuration time.Duration

	// MaxDuration bounds how long a session may stay open before settling. A
	// session settled hours later is more likely a replayed request than a very
	// patient player.
	MaxDuration time.Duration

	// PointsPerScore converts raw score to points. Games score on wildly
	// different scales — a Pac-Man board is thousands of points, a Tic-Tac-Toe
	// win is one — so a shared conversion would be meaningless.
	PointsPerScore float64
}

// defaultRule applies to any game without an entry below. Conservative on
// duration, permissive on score.
var defaultRule = Rule{
	MaxScore:       100000,
	MinDuration:    3 * time.Second,
	MaxDuration:    2 * time.Hour,
	PointsPerScore: 0.1,
}

// rules holds per-game overrides, keyed by the slug the arcade sends as gameId.
//
// These live in code for now because there are thirteen of them and they change
// when a game changes, which is a deploy either way. If that stops being true —
// or if tuning becomes frequent — they belong in the database.
var rules = map[string]Rule{
	// Board and puzzle games: low scores, longer play.
	"tic-tac-toe":  {MaxScore: 100, MinDuration: 5 * time.Second, MaxDuration: time.Hour, PointsPerScore: 5},
	"connect-four": {MaxScore: 100, MinDuration: 10 * time.Second, MaxDuration: time.Hour, PointsPerScore: 5},
	"sudoku":       {MaxScore: 1000, MinDuration: 30 * time.Second, MaxDuration: 3 * time.Hour, PointsPerScore: 1},

	// Reaction games: short runs are legitimate here.
	"whac-a-mole": {MaxScore: 500, MinDuration: 3 * time.Second, MaxDuration: 30 * time.Minute, PointsPerScore: 1},
	"speed-test":  {MaxScore: 500, MinDuration: 2 * time.Second, MaxDuration: 30 * time.Minute, PointsPerScore: 1},

	// Arcade action: high scores, moderate runs.
	"pac-man":        {MaxScore: 100000, MinDuration: 10 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"space-invaders": {MaxScore: 100000, MinDuration: 10 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"frogger":        {MaxScore: 50000, MinDuration: 5 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"bird-shooter":   {MaxScore: 50000, MinDuration: 5 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"food-fall":      {MaxScore: 50000, MinDuration: 5 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"sweet-crush":    {MaxScore: 100000, MinDuration: 10 * time.Second, MaxDuration: 2 * time.Hour, PointsPerScore: 0.05},
	"race":           {MaxScore: 50000, MinDuration: 10 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
	"scroll":         {MaxScore: 50000, MinDuration: 5 * time.Second, MaxDuration: time.Hour, PointsPerScore: 0.05},
}

// RuleFor returns the rule for a game, falling back to the default.
//
// An unknown gameId is not an error: new games should not need a deploy of this
// service before they can award points.
func RuleFor(gameID string) Rule {
	if r, ok := rules[gameID]; ok {
		return r
	}
	return defaultRule
}

// KnownGames lists the games with explicit rules. Used by the leaderboard to
// validate a requested game without hitting the database.
func KnownGames() []string {
	out := make([]string, 0, len(rules))
	for g := range rules {
		out = append(out, g)
	}
	return out
}

// RejectReason explains why a settle was refused. Returned to the caller so a
// player sees something better than a bare failure.
type RejectReason string

const (
	RejectTooFast  RejectReason = "implausibly-fast"
	RejectTooSlow  RejectReason = "session-too-old"
	RejectTooHigh  RejectReason = "score-out-of-range"
	RejectNegative RejectReason = "score-negative"
	RejectNotYours RejectReason = "not-your-session"
	RejectSettled  RejectReason = "already-settled"
)

// Validate checks a reported score against the rule and the elapsed time.
//
// Returns the points to award, or a reason. Points are floored: a run worth 4.9
// points awards 4, never 5, so rounding can never manufacture value.
func (r Rule) Validate(score int64, elapsed time.Duration) (int64, RejectReason, bool) {
	if score < 0 {
		return 0, RejectNegative, false
	}
	if elapsed < r.MinDuration {
		return 0, RejectTooFast, false
	}
	if r.MaxDuration > 0 && elapsed > r.MaxDuration {
		return 0, RejectTooSlow, false
	}
	if r.MaxScore > 0 && score > r.MaxScore {
		return 0, RejectTooHigh, false
	}

	points := int64(math.Floor(float64(score) * r.PointsPerScore))
	if points < 0 {
		points = 0
	}
	return points, "", true
}
