// Package httpapi is the transport layer: routing, middleware, and the JSON
// shapes callers already depend on.
//
// The response bodies here are not a fresh design. blog-portfolio's React app
// and the 13 arcade game builds both parse the existing Express responses, so
// the contract is fixed: success bodies carry `status: "success"`, and every
// error is `{"msg": "..."}`. Changing that would mean redeploying a static site
// on someone else's cadence, which is exactly the kind of coupling this service
// should avoid introducing on day one.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/HoseaCodes/arcade-api/internal/auth"
	"github.com/HoseaCodes/arcade-api/internal/config"
	"github.com/HoseaCodes/arcade-api/internal/ledger"
)

type Server struct {
	cfg      config.Config
	ledger   *ledger.Ledger
	verifier *auth.Verifier
	log      *slog.Logger
}

func New(cfg config.Config, l *ledger.Ledger, v *auth.Verifier, log *slog.Logger) *Server {
	return &Server{cfg: cfg, ledger: l, verifier: v, log: log}
}

// Routes builds the handler tree.
//
// Three tiers, and the separation is deliberate: /health is open, /api is
// player-authenticated, and /internal is service-authenticated. A player token
// can never reach /internal and a service token can never reach /api.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(s.recoverPanics)
	r.Use(s.logRequests)
	r.Use(s.cors)

	r.Get("/health", s.handleHealth)

	r.Route("/api/points", func(r chi.Router) {
		r.Use(auth.RequireUser(s.verifier, writeError))

		r.Get("/balance", s.handleBalance)
		r.Get("/transactions", s.handleTransactions)
		r.Post("/earn", s.handleEarn)
		r.Post("/spend", s.handleSpend)
		r.Post("/sync", s.handleSync)
		r.Post("/claim-guest", s.handleClaimGuest)
	})

	r.Route("/internal", func(r chi.Router) {
		r.Use(auth.RequireService(s.cfg.ServiceToken, writeError))

		r.Post("/credit", s.handleCredit)
		r.Post("/holds", s.handleCreateHold)
		r.Post("/holds/{holdID}/commit", s.handleCommitHold)
		r.Post("/holds/{holdID}/release", s.handleReleaseHold)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
	})

	return r
}

// ---------------------------------------------------------------- responses

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already sent, so there is nothing useful left to
		// do but avoid pretending it succeeded.
		slog.Error("encode response", "err", err)
	}
}

// writeError emits the `{"msg": ...}` shape every existing client parses.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"msg": msg})
}

// writeErrorWith adds fields alongside `msg` — an insufficient-funds response
// carries the current balance and the amount required, for example.
func writeErrorWith(w http.ResponseWriter, status int, msg string, extra map[string]any) {
	body := map[string]any{"msg": msg}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// decodeJSON reads a request body, rejecting unknown fields.
//
// Strict decoding is a deliberate choice for a service that moves balances: a
// typo'd field name should fail loudly rather than silently default to zero.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---------------------------------------------------------------- middleware

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request",
					"err", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "Internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// No request bodies and no tokens in logs: this service sees balances
		// and credentials, and neither belongs in CloudWatch.
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// cors allows the configured browser origins.
//
// The arcade is a separate origin calling this API directly, so the allowlist is
// load-bearing. An unlisted origin simply gets no CORS headers — the browser
// then blocks it, which is the correct failure.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.cfg.AllowedOrigins, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
			// Responses vary by origin, so shared caches must not serve one
			// origin's response to another.
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- health

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": config.Version,
		"commit":  config.Commit,
	})
}

// ---------------------------------------------------------------- helpers

// positiveAmount validates and floors a client-supplied amount.
func positiveAmount(raw json.Number) (int64, bool) {
	f, err := raw.Float64()
	if err != nil || f <= 0 {
		return 0, false
	}
	n := int64(f)
	if n <= 0 {
		return 0, false
	}
	return n, true
}

func limitParam(r *http.Request) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 50
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 50
	}
	return n
}

// principal pulls the authenticated caller off the context. RequireUser has
// already run, so absence is a programming error rather than a request problem.
func principal(r *http.Request) (auth.Principal, bool) {
	return auth.FromContext(r.Context())
}

// serverError logs the cause and returns a generic message. Internal error text
// can name tables and constraints, which is not something to hand to a browser.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed",
		"method", r.Method,
		"path", r.URL.Path,
		"err", err)
	writeError(w, http.StatusInternalServerError, "Internal server error")
}
