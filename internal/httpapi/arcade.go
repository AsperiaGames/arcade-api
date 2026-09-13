package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/HoseaCodes/arcade-api/internal/arcade"
)

// Session routes are additive. The 13 existing game builds post a bare
// {amount, gameId, gameName} to /points/earn with no session, and none of them
// can be redeployed on this service's schedule — so sessions must never be
// required for earning to work. They are the stronger path for games updated to
// use them, not a replacement for the weaker one.

type startSessionRequest struct {
	GameID string `json:"gameId"`
}

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}
	// Sessions award through the ledger, which needs a real account. Guests keep
	// the legacy earn path, which routes to the claimable bucket instead.
	if p.IsGuest {
		writeError(w, http.StatusForbidden, "Sign in to track scored sessions")
		return
	}

	var req startSessionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.GameID == "" {
		writeError(w, http.StatusBadRequest, "gameId required")
		return
	}

	session, err := s.arcade.StartSession(r.Context(), p.UserID, req.GameID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "success",
		"sessionId": session.ID,
		"gameId":    session.GameID,
		"startedAt": session.StartedAt,
	})
}

type settleRequest struct {
	Score json.Number `json:"score"`
}

// handleSettleSession reports a final score.
//
// A rejection is a 422 rather than a 400: the request was well-formed, the
// *claim* was not believable. The distinction matters when reading logs — a 400
// means a broken client, a 422 means either a broken game or someone probing.
func (s *Server) handleSettleSession(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}

	sessionID := chi.URLParam(r, "sessionID")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "sessionId required")
		return
	}

	var req settleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	score, err := req.Score.Int64()
	if err != nil {
		writeError(w, http.StatusBadRequest, "score must be an integer")
		return
	}

	settlement, err := s.arcade.SettleSession(r.Context(), p.UserID, sessionID, score)
	switch {
	case errors.Is(err, arcade.ErrNotFound):
		writeError(w, http.StatusNotFound, "Session not found")
		return
	case errors.Is(err, arcade.ErrRejected):
		writeErrorWith(w, http.StatusUnprocessableEntity, "Score rejected",
			map[string]any{"reason": settlement.Reason, "accepted": false})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	body := map[string]any{
		"status":   "success",
		"accepted": settlement.Accepted,
		"credited": settlement.Credited,
		"balance":  settlement.Balance,
		"score":    settlement.Score,
	}
	// Present when the run counted but paid nothing — the daily budget is spent.
	// The score is still on the leaderboard.
	if settlement.Reason != "" {
		body["reason"] = settlement.Reason
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleGameLeaderboard(w http.ResponseWriter, r *http.Request) {
	gameID := chi.URLParam(r, "gameID")
	if gameID == "" {
		writeError(w, http.StatusBadRequest, "gameId required")
		return
	}

	window := arcade.ParseWindow(r.URL.Query().Get("window"))
	entries, err := s.arcade.Leaderboard(r.Context(), gameID, window, leaderboardLimit(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"gameId":  gameID,
		"window":  string(window),
		"entries": entries,
		"count":   len(entries),
	})
}

func (s *Server) handleGlobalLeaderboard(w http.ResponseWriter, r *http.Request) {
	window := arcade.ParseWindow(r.URL.Query().Get("window"))
	entries, err := s.arcade.GlobalLeaderboard(r.Context(), window, leaderboardLimit(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"window":  string(window),
		"entries": entries,
		"count":   len(entries),
	})
}

func leaderboardLimit(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 20
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 20
	}
	return n
}
