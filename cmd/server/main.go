// Command server runs the arcade points API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HoseaCodes/arcade-api/internal/auth"
	"github.com/HoseaCodes/arcade-api/internal/config"
	"github.com/HoseaCodes/arcade-api/internal/httpapi"
	"github.com/HoseaCodes/arcade-api/internal/ledger"
	"github.com/HoseaCodes/arcade-api/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log.Info("starting", "version", config.Version, "commit", config.Commit, "port", cfg.Port)

	// Migrate before opening the pool: the service should not accept traffic
	// against a schema it has not brought up to date.
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Signal-aware from the very start, so a shutdown during a slow database
	// connect is still honoured.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	l := ledger.New(pool, cfg.MaxDailyEarn, cfg.HoldTTL)
	ledger.SetSweepObserver(func(n int) {
		// A steady stream here means some caller is taking holds and never
		// settling them, which is worth investigating rather than absorbing.
		log.Warn("reclaimed expired holds", "count", n)
	})

	// Token verification needs no shared secret: Storm-Gate publishes a JWKS and
	// this verifies RS256 signatures locally. The key set is fetched lazily, so
	// a cold Storm-Gate cannot stop this service from starting.
	verifier := auth.NewVerifier(ctx, cfg.StormGateURL, "")

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: httpapi.New(cfg, l, verifier, log).Routes(),

		// Explicit timeouts: the default zero values mean "wait forever", which
		// on a public endpoint is how a handful of slow clients exhaust the
		// server.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	sweepDone := startHoldSweeper(ctx, l, cfg.HoldTTL, log)

	serverErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Give in-flight requests a chance to finish. A request that is mid-ledger
	// transaction should be allowed to commit or roll back cleanly rather than
	// having its connection torn out.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	<-sweepDone

	log.Info("stopped")
	return nil
}

// startHoldSweeper returns expired holds on a ticker.
//
// This is what makes the two-phase spend safe to rely on: a caller that dies
// between taking a hold and settling it does not strand a player's points. It
// runs at half the hold TTL so an expired hold is never outstanding for much
// longer than the TTL itself.
func startHoldSweeper(ctx context.Context, l *ledger.Ledger, ttl time.Duration, log *slog.Logger) <-chan struct{} {
	interval := ttl / 2
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Bounded independently of ctx so a sweep in progress is not
				// cancelled mid-statement by a shutdown.
				sweepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if _, err := l.SweepExpiredHolds(sweepCtx); err != nil {
					// Non-fatal: the next tick tries again, and holds stay
					// expired until one succeeds.
					log.Error("hold sweep failed", "err", err)
				}
				cancel()
			}
		}
	}()
	return done
}
