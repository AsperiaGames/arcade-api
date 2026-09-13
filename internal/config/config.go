// Package config turns the process environment into a validated, typed struct.
//
// Everything is read once at startup and passed down explicitly; nothing else in
// the service touches os.Getenv. That keeps configuration errors to a single
// loud failure at boot rather than a surprise on some request path hours later.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Build metadata, injected at link time:
//
//	go build -ldflags "-X .../internal/config.Version=$(git describe --tags --always)"
//
// Both existing services in this organization can report neither their version
// nor their commit, which has cost real time working out what was deployed.
// This service answers that from /health on day one.
var (
	Version = "dev"
	Commit  = "unknown"
)

type Config struct {
	Port int

	// DatabaseURL is a standard Postgres URL. On Neon, prefer the DIRECT
	// endpoint over the pooled one: the pooler runs PgBouncer in transaction
	// mode, which breaks pgx's default extended protocol, and this service runs
	// on long-lived machines where pgxpool already pools connections.
	DatabaseURL string

	// StormGateURL is the OIDC issuer. Its discovery document supplies the JWKS
	// used to verify RS256 access tokens locally, so no shared signing secret is
	// ever needed here.
	StormGateURL string

	// ServiceToken authenticates trusted server-to-server callers on /internal.
	// A user JWT is never accepted there.
	ServiceToken string

	// AllowedOrigins is the browser CORS allowlist. The arcade is a separate
	// origin, so this is load-bearing rather than decorative.
	AllowedOrigins []string

	MaxSingleEarn   int64
	MaxDailyEarn    int64
	MaxOfflineClaim int64

	// HoldTTL bounds how long a debit may sit uncommitted before the sweeper
	// returns it. Long enough for a caller's own write, short enough that a
	// crashed caller does not strand points for hours.
	HoldTTL time.Duration
}

// Load reads and validates the environment. The error names every problem found
// rather than only the first, so a misconfigured deploy takes one round trip to
// fix instead of several.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		Port:            envInt("PORT", 8080, &problems),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		StormGateURL:    strings.TrimRight(os.Getenv("STORM_GATE_URL"), "/"),
		ServiceToken:    os.Getenv("SERVICE_TOKEN"),
		AllowedOrigins:  envList("CORS_ORIGINS"),
		MaxSingleEarn:   int64(envInt("POINTS_MAX_SINGLE_EARN", 10000, &problems)),
		MaxDailyEarn:    int64(envInt("POINTS_MAX_DAILY_EARN", 10000, &problems)),
		MaxOfflineClaim: int64(envInt("POINTS_MAX_OFFLINE_CLAIM", 50000, &problems)),
		HoldTTL:         time.Duration(envInt("HOLD_TTL_SECONDS", 120, &problems)) * time.Second,
	}

	if cfg.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if cfg.StormGateURL == "" {
		problems = append(problems, "STORM_GATE_URL is required (the OIDC issuer)")
	}
	// An empty service token would silently let any caller reach /internal, so
	// treat it as a hard failure rather than defaulting to something guessable.
	if len(cfg.ServiceToken) < 32 {
		problems = append(problems, "SERVICE_TOKEN is required and must be at least 32 characters")
	}
	if cfg.MaxSingleEarn > cfg.MaxDailyEarn {
		problems = append(problems, fmt.Sprintf(
			"POINTS_MAX_SINGLE_EARN (%d) exceeds POINTS_MAX_DAILY_EARN (%d), so the daily cap could never be reached in one call",
			cfg.MaxSingleEarn, cfg.MaxDailyEarn))
	}

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func envInt(key string, def int, problems *[]string) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s must be an integer, got %q", key, raw))
		return def
	}
	if n < 0 {
		*problems = append(*problems, fmt.Sprintf("%s must not be negative, got %d", key, n))
		return def
	}
	return n
}

func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
