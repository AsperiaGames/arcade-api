// Package auth verifies callers.
//
// Two completely separate paths, deliberately never interchangeable:
//
//   - Players present a Storm-Gate RS256 JWT, verified locally against the
//     published JWKS. No shared signing secret exists here, which is the whole
//     reason this service could be written in another language at all.
//
//   - Trusted backends present a service token on /internal routes. A player
//     JWT is never accepted there, and a service token is never accepted
//     anywhere else.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Principal is the authenticated caller. It reaches handlers via the request
// context and is only ever set by this package's middleware.
type Principal struct {
	// UserID is Storm-Gate's `id` claim. Note it is NOT `sub`: Storm-Gate puts
	// the account identifier in `id`, and every consumer reads that.
	UserID string

	// IsGuest marks an anonymous session. Storm-Gate hands these out
	// unauthenticated and mints a fresh identity each time, so guests may earn
	// into a claimable bucket but must never spend, purchase, or reach a real
	// account. Handlers enforce that; this flag is how they know.
	IsGuest bool
}

type contextKey struct{}

// FromContext returns the caller, if the request passed through RequireUser.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}

// Verifier validates player tokens.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewVerifier builds a token verifier for the given issuer.
//
// It does NOT perform OIDC discovery at construction: the remote key set fetches
// lazily and caches, so a cold Storm-Gate cannot stop this service from
// starting. On a platform that scales to zero, coupling our boot to another
// service's boot is a needless way to fail.
//
// jwksURL may be empty, in which case the conventional path under the issuer is
// used — which is what Storm-Gate serves.
func NewVerifier(ctx context.Context, issuer, jwksURL string) *Verifier {
	issuer = strings.TrimRight(issuer, "/")
	if jwksURL == "" {
		jwksURL = issuer + "/.well-known/jwks.json"
	}

	keySet := oidc.NewRemoteKeySet(ctx, jwksURL)
	return &Verifier{
		verifier: oidc.NewVerifier(issuer, keySet, &oidc.Config{
			// Storm-Gate access tokens carry no `aud`, so there is no client ID
			// to match. Issuer and signature are the checks that matter.
			SkipClientIDCheck:    true,
			SupportedSigningAlgs: []string{oidc.RS256},
		}),
	}
}

var errNoUserClaim = errors.New("token has no id claim")

// Verify checks the signature, issuer and expiry, then extracts the caller.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, fmt.Errorf("verify token: %w", err)
	}

	var claims struct {
		ID      string `json:"id"`
		IsGuest bool   `json:"isGuest"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return Principal{}, fmt.Errorf("decode claims: %w", err)
	}
	if claims.ID == "" {
		return Principal{}, errNoUserClaim
	}
	return Principal{UserID: claims.ID, IsGuest: claims.IsGuest}, nil
}

// BearerToken pulls the credential out of an Authorization header.
//
// Both "Bearer <jwt>" and a bare "<jwt>" are accepted: Storm-Gate's own
// middleware accepts both, and the browser SDK sends the bare form. Rejecting it
// here would break existing callers for no security gain.
func BearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	if len(header) >= 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	// A bare "Bearer" with nothing after it is not a token.
	if strings.EqualFold(header, "bearer") {
		return ""
	}
	return header
}

// ErrorWriter renders auth failures. The HTTP layer supplies it so this package
// stays free of response-shape decisions.
type ErrorWriter func(w http.ResponseWriter, status int, msg string)

// RequireUser authenticates a player and attaches the Principal to the context.
func RequireUser(v *Verifier, writeErr ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := BearerToken(r.Header.Get("Authorization"))
			if token == "" {
				writeErr(w, http.StatusUnauthorized, "Missing authentication token")
				return
			}

			principal, err := v.Verify(r.Context(), token)
			if err != nil {
				// A key-set fetch failure is a dependency outage, not a bad
				// credential. Answering 401 would make clients discard a
				// perfectly good session; 503 tells them to retry.
				if isKeySetUnavailable(err) {
					writeErr(w, http.StatusServiceUnavailable, "Unable to verify token: key set unavailable")
					return
				}
				writeErr(w, http.StatusUnauthorized, "Invalid authentication token")
				return
			}

			ctx := context.WithValue(r.Context(), contextKey{}, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireService gates /internal routes on the shared service token.
//
// Compared in constant time: a naive == leaks the token a byte at a time under
// timing analysis, and this credential authorizes balance mutations.
func RequireService(token string, writeErr ErrorWriter) func(http.Handler) http.Handler {
	expected := []byte(token)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := []byte(BearerToken(r.Header.Get("Authorization")))
			if len(expected) == 0 || subtle.ConstantTimeCompare(presented, expected) != 1 {
				writeErr(w, http.StatusUnauthorized, "Invalid service credentials")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isKeySetUnavailable distinguishes "we could not reach the key set" from "this
// token is bad". go-oidc wraps transport errors rather than typing them, so this
// matches on the message — narrow, but the alternative is telling every client
// their session expired during a Storm-Gate outage.
func isKeySetUnavailable(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"fetching keys",
		"connection refused",
		"context deadline exceeded",
		"no such host",
		"EOF",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
