package httpapi

import (
	"encoding/json"
	"errors"
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
