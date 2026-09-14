package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/HoseaCodes/arcade-api/internal/ledger"
)

// Routes under /internal are for trusted backends, authenticated by the service
// token. They take an explicit userId rather than reading one from a JWT,
// because the caller is acting on a player's behalf rather than as that player.
//
// blog-portfolio uses these because it still owns products, purchases and the
// PayPal flow: it needs to move points it does not store.

type creditRequest struct {
	UserID         string         `json:"userId"`
	Amount         json.Number    `json:"amount"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Meta           map[string]any `json:"meta"`
}

// handleCredit adds purchased points after a confirmed payment capture.
//
// The idempotency key is required, not optional. The Node original checked "have
// we recorded this order?" and then inserted, so two concurrent captures could
// both look, both miss, and both credit. Here a UNIQUE index decides, and a
// repeat is reported as success with `applied: false` — the caller's intent
// ("this order is paid") is already satisfied, so an error would be misleading.
func (s *Server) handleCredit(w http.ResponseWriter, r *http.Request) {
	var req creditRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "idempotencyKey required")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}

	acct, applied, err := s.ledger.Credit(r.Context(), req.UserID, amount, req.IdempotencyKey, ledger.Meta(req.Meta))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"balance": acct.Balance,
		// False means this exact credit had already been applied. Useful for the
		// caller's own logging; not an error.
		"applied": applied,
	})
}

type holdRequest struct {
	UserID string         `json:"userId"`
	Amount json.Number    `json:"amount"`
	Meta   map[string]any `json:"meta"`
}

// handleCreateHold debits now, pending confirmation.
//
// The caller then writes its own record and calls commit or release. If it never
// does either, the hold expires and the sweeper returns the points — which is
// the entire reason this is a hold rather than a spend followed by a refund. A
// refund only helps if the caller is still alive to issue it.
func (s *Server) handleCreateHold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}

	hold, err := s.ledger.Hold(r.Context(), req.UserID, amount, ledger.Meta(req.Meta))
	if errors.Is(err, ledger.ErrInsufficient) {
		current, loadErr := s.ledger.EnsureAccount(r.Context(), req.UserID)
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
		"status":    "success",
		"holdId":    hold.ID,
		"balance":   hold.BalanceAfter,
		"expiresAt": hold.ExpiresAt,
	})
}

type commitRequest struct {
	Meta map[string]any `json:"meta"`
}

// handleCommitHold finalises a held debit and writes its receipt.
//
// Meta supplied here is merged over what the hold captured, which is how a
// purchase id — only known after the caller's own write — ends up on the
// receipt.
func (s *Server) handleCommitHold(w http.ResponseWriter, r *http.Request) {
	holdID := chi.URLParam(r, "holdID")
	if holdID == "" {
		writeError(w, http.StatusBadRequest, "holdId required")
		return
	}

	var req commitRequest
	// An empty body is fine here: not every caller has extra context to add.
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
	}

	acct, err := s.ledger.CommitHold(r.Context(), holdID, ledger.Meta(req.Meta))
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "Hold not found")
		return
	case errors.Is(err, ledger.ErrHoldSettled):
		// Typically an expired hold whose points were already returned.
		// Committing now would charge for something already refunded.
		writeError(w, http.StatusConflict, "Hold already settled or expired")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"balance": acct.Balance,
	})
}

// handleReleaseHold returns held points when the caller's own work failed.
func (s *Server) handleReleaseHold(w http.ResponseWriter, r *http.Request) {
	holdID := chi.URLParam(r, "holdID")
	if holdID == "" {
		writeError(w, http.StatusBadRequest, "holdId required")
		return
	}

	acct, err := s.ledger.ReleaseHold(r.Context(), holdID)
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "Hold not found")
		return
	case errors.Is(err, ledger.ErrHoldSettled):
		writeError(w, http.StatusConflict, "Hold already settled")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "success",
		"balance": acct.Balance,
	})
}

// The handlers below mirror the /api/points reads and credits, but keyed by an
// explicit userId instead of a JWT subject. They exist so blog-portfolio can act
// as a trusted backend on a player's behalf: its own controllers (and their
// characterization tests) call a ledger module by userId, and that module now
// speaks to this service rather than to Mongo. A player JWT is deliberately not
// accepted here — /internal is service-authenticated — and there is no guest
// path, because the blog only ever acts for a real, signed-in user.
//
// The response shapes are identical to the /api equivalents on purpose: the blog
// facade returns them onward to a React app that already parses that contract.

type userScopedRequest struct {
	UserID string `json:"userId"`
}

type userAmountRequest struct {
	UserID string         `json:"userId"`
	Amount json.Number    `json:"amount"`
	Meta   map[string]any `json:"meta"`
}

// handleInternalBalance returns an account, creating it if absent — the same
// getOrCreate the Node facade relied on.
func (s *Server) handleInternalBalance(w http.ResponseWriter, r *http.Request) {
	var req userScopedRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}

	acct, err := s.ledger.EnsureAccount(r.Context(), req.UserID)
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

type internalTransactionsRequest struct {
	UserID string `json:"userId"`
	Limit  int    `json:"limit"`
}

func (s *Server) handleInternalTransactions(w http.ResponseWriter, r *http.Request) {
	var req internalTransactionsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}

	items, err := s.ledger.Transactions(r.Context(), req.UserID, req.Limit)
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

// handleInternalEarn credits play winnings for a named user, subject to the same
// per-call and daily ceilings as the player-facing route, and returning the same
// 429 the arcade client understands as `daily-cap`.
func (s *Server) handleInternalEarn(w http.ResponseWriter, r *http.Request) {
	var req userAmountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}
	if amount > s.cfg.MaxSingleEarn {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("Single-game earn exceeds limit of %d", s.cfg.MaxSingleEarn))
		return
	}

	acct, earnedToday, err := s.ledger.Earn(r.Context(), req.UserID, amount, ledger.Meta(req.Meta))
	if errors.Is(err, ledger.ErrDailyCap) {
		s.writeDailyCap(w, r, req.UserID)
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

type internalSpendRequest struct {
	UserID string         `json:"userId"`
	Amount json.Number    `json:"amount"`
	Reason string         `json:"reason"`
	Meta   map[string]any `json:"meta"`
}

// handleInternalSpend is the direct debit path — a spend with no dependent write
// to bridge. Purchases that must also touch Mongo use the hold endpoints instead,
// so a failure on the Mongo side releases the points rather than stranding them.
func (s *Server) handleInternalSpend(w http.ResponseWriter, r *http.Request) {
	var req internalSpendRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
		return
	}
	amount, ok := positiveAmount(req.Amount)
	if !ok {
		writeError(w, http.StatusBadRequest, "amount must be a positive integer")
		return
	}

	meta := ledger.Meta{}
	for k, v := range req.Meta {
		meta[k] = v
	}
	if req.Reason != "" {
		meta["reason"] = req.Reason
	}

	acct, err := s.ledger.Spend(r.Context(), req.UserID, amount, meta)
	if errors.Is(err, ledger.ErrInsufficient) {
		current, loadErr := s.ledger.EnsureAccount(r.Context(), req.UserID)
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

// handleInternalSync is the one-shot claim of a user's offline (localStorage)
// earnings. Idempotent by the account's claimed_offline flag, so a retry after a
// timeout reports the conflict rather than crediting twice.
func (s *Server) handleInternalSync(w http.ResponseWriter, r *http.Request) {
	var req userAmountRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "userId required")
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

	meta := ledger.Meta{"source": "localStorage"}
	for k, v := range req.Meta {
		meta[k] = v
	}

	acct, err := s.ledger.ClaimOffline(r.Context(), req.UserID, amount, meta)
	if errors.Is(err, ledger.ErrAlreadyClaimed) {
		current, loadErr := s.ledger.EnsureAccount(r.Context(), req.UserID)
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
