package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/HoseaCodes/arcade-api/internal/ledger"
)

// guestEarningTTL is how long an anonymous balance waits to be claimed. Long
// enough to sign up after a session; short enough that abandoned guest rows
// clear themselves out.
const guestEarningTTL = 30 * 24 * time.Hour

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}

	// A guest has no account, and creating one on a balance read is exactly the
	// orphan-row problem guest earnings exist to avoid. Report the claimable
	// bucket instead.
	if p.IsGuest {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":            "success",
			"balance":           0,
			"lifetimeEarned":    0,
			"lifetimeSpent":     0,
			"lifetimePurchased": 0,
			"claimedOffline":    false,
			"guest":             true,
		})
		return
	}

	acct, err := s.ledger.EnsureAccount(r.Context(), p.UserID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "success",
		"balance":           acct.Balance,
		"lifetimeEarned":    acct.LifetimeEarned,
		"lifetimeSpent":     acct.LifetimeSpent,
		"lifetimePurchased": acct.LifetimePurchased,
		"claimedOffline":    acct.ClaimedOffline,
	})
}

func (s *Server) handleTransactions(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}

	items, err := s.ledger.Transactions(r.Context(), p.UserID, limitParam(r))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"items":  items,
		"count":  len(items),
	})
}

type earnRequest struct {
	Amount   json.Number `json:"amount"`
	GameID   string      `json:"gameId"`
	GameName string      `json:"gameName"`
}

func (s *Server) handleEarn(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}

	var req earnRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}
	// Per-call ceiling, separate from the daily budget. The arcade client caps
	// itself at 500; this is the backstop against a forged request.
	if amount > s.cfg.MaxSingleEarn {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("Single-game earn exceeds limit of %d", s.cfg.MaxSingleEarn))
		return
	}

	// Guests earn into a claimable bucket, never onto an account. Storm-Gate
	// mints a fresh guest identity per anonymous login, so crediting `accounts`
	// here would leave one unreachable balance per session — from an
	// unauthenticated endpoint.
	if p.IsGuest {
		balance, err := s.ledger.RecordGuestEarn(r.Context(), p.UserID, amount, guestEarningTTL)
		if errors.Is(err, ledger.ErrDailyCap) {
			s.writeDailyCap(w, r, p.UserID)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "success",
			"credited": amount,
			"balance":  balance,
			"guest":    true,
		})
		return
	}

	meta := ledger.Meta{}
	if req.GameID != "" {
		meta["gameId"] = req.GameID
	}
	if req.GameName != "" {
		meta["gameName"] = req.GameName
	}

	acct, earnedToday, err := s.ledger.Earn(r.Context(), p.UserID, amount, meta)
	if errors.Is(err, ledger.ErrDailyCap) {
		s.writeDailyCap(w, r, p.UserID)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "success",
		"credited":  amount,
		"balance":   acct.Balance,
		"remaining": s.cfg.MaxDailyEarn - earnedToday,
	})
}

// writeDailyCap emits 429 rather than 400 — the arcade client already maps that
// status to `daily-cap` and stops retrying, where a 400 reads as a malformed
// request and is retried. It carries `earnedToday` so a client can show how much
// of the day's budget is spent; a read failure there is not worth failing the
// response over, so it degrades to zero.
func (s *Server) writeDailyCap(w http.ResponseWriter, r *http.Request, userID string) {
	earned, _ := s.ledger.EarnedToday(r.Context(), userID)
	writeErrorWith(w, http.StatusTooManyRequests,
		fmt.Sprintf("Daily earn limit of %d reached", s.cfg.MaxDailyEarn),
		map[string]any{"cap": s.cfg.MaxDailyEarn, "earnedToday": earned})
}

type spendRequest struct {
	Amount json.Number    `json:"amount"`
	Reason string         `json:"reason"`
	Meta   map[string]any `json:"meta"`
}

func (s *Server) handleSpend(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}
	if p.IsGuest {
		writeError(w, http.StatusForbidden, "Guests cannot spend points")
		return
	}

	var req spendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason required")
		return
	}

	meta := ledger.Meta{"reason": req.Reason}
	for k, v := range req.Meta {
		meta[k] = v
	}

	acct, err := s.ledger.Spend(r.Context(), p.UserID, amount, meta)
	if errors.Is(err, ledger.ErrInsufficient) {
		current, loadErr := s.ledger.EnsureAccount(r.Context(), p.UserID)
		balance := int64(0)
		if loadErr == nil {
			balance = current.Balance
		}
		writeErrorWith(w, http.StatusPaymentRequired, "Insufficient points",
			map[string]any{"balance": balance, "required": amount})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"spent":   amount,
		"balance": acct.Balance,
	})
}

type syncRequest struct {
	Amount json.Number `json:"amount"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}
	if p.IsGuest {
		writeError(w, http.StatusForbidden, "Guests cannot claim offline points")
		return
	}

	var req syncRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}
	if amount > s.cfg.MaxOfflineClaim {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("Offline claim exceeds limit of %d", s.cfg.MaxOfflineClaim))
		return
	}

	acct, err := s.ledger.ClaimOffline(r.Context(), p.UserID, amount, ledger.Meta{"source": "localStorage"})
	if errors.Is(err, ledger.ErrAlreadyClaimed) {
		current, loadErr := s.ledger.EnsureAccount(r.Context(), p.UserID)
		balance := int64(0)
		if loadErr == nil {
			balance = current.Balance
		}
		writeErrorWith(w, http.StatusConflict, "Offline points already claimed",
			map[string]any{"balance": balance})
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "success",
		"credited": amount,
		"balance":  acct.Balance,
	})
}

type claimGuestRequest struct {
	GuestID string `json:"guestId"`
}

// handleClaimGuest moves an anonymous balance onto the caller's real account.
//
// Authenticated as the *real* user, carrying the guest id they used to hold.
// That ordering matters: the guest token alone must never be able to move points
// onto an account, or anyone could claim into someone else's wallet.
func (s *Server) handleClaimGuest(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Missing authentication token")
		return
	}
	if p.IsGuest {
		writeError(w, http.StatusForbidden, "Sign in to claim guest points")
		return
	}

	var req claimGuestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.GuestID == "" {
		writeError(w, http.StatusBadRequest, "guestId required")
		return
	}

	acct, moved, err := s.ledger.ClaimGuestEarnings(r.Context(), req.GuestID, p.UserID)
	if errors.Is(err, ledger.ErrAlreadyClaimed) {
		writeError(w, http.StatusConflict, "Those guest points were already claimed or have expired")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "success",
		"credited": moved,
		"balance":  acct.Balance,
	})
}
