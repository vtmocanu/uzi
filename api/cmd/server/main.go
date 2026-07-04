// Command server is the uzi API: it runs DB migrations, then serves the auth
// and admin HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/handler"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/poller"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/seed"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/sweeper"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

func main() {
	// -health is a shell-free container healthcheck: the distroless runtime
	// image has no wget/curl, so the binary probes itself.
	health := flag.Bool("health", false, "probe the local /api/health endpoint and exit")
	flag.Parse()
	if *health {
		os.Exit(healthcheck())
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// healthcheck hits the local health endpoint and returns a process exit code.
func healthcheck() int {
	addr := os.Getenv("API_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/api/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("running migrations")
	if err := store.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}

	pool, err := store.OpenPool(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	q := store.New(pool)

	// Seed/refresh the builtin agent templates. Idempotent and edit-preserving:
	// missing builtins are inserted, existing rows are left untouched.
	if err := store.ReconcileBuiltinTemplates(ctx, q); err != nil {
		return err
	}

	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	svc := forgesvc.New(q, box, cfg.ForgeHTTPTimeout)

	// Optional startup admin seed. Runs after migrations, before serving. A
	// failure here (e.g. DB error) aborts boot; an already-present seed user is
	// left untouched.
	if err := seedAdmin(ctx, q, cfg); err != nil {
		return err
	}
	// Optional startup forge-connection seed for the seed admin. Runs after the
	// admin seed (whose user it belongs to) and before the poller starts (so a
	// seeded repo is picked up on the first tick). A forge outage here is
	// non-fatal — it logs and skips, retrying on the next boot — but a DB error
	// aborts boot, same as the admin seed.
	if err := seed.ForgeConnection(ctx, q, svc, cfg); err != nil {
		return err
	}
	// Optional startup Anthropic-token seed for the seed admin (dev convenience:
	// survives `docker compose down -v` so the token need not be re-pasted). Runs
	// after the admin seed (whose user it belongs to). Create-only and
	// format-checked only — it seeds the operator's EXISTING token, never mints a
	// credential, never does a live/network check, and never logs the value. A DB
	// error or a malformed configured token aborts boot; an already-present token
	// is left untouched.
	if err := seed.AnthropicToken(ctx, q, box, cfg); err != nil {
		return err
	}

	// Agent-runtime service (PRD #4): the run queue, worker protocol, and sweeper
	// DB work. Shares the same secret cipher (sole key holder) as the forge svc.
	wsvc := workersvc.New(q, box, workersvc.Params{
		RunTimeout:           cfg.RunTimeout,
		RunIdleTimeout:       cfg.RunIdleTimeout,
		RunMaxIterations:     cfg.RunMaxIterations,
		RunMaxRequeues:       cfg.RunMaxRequeues,
		WorkerHeartbeatStale: cfg.WorkerHeartbeatStale,
		WorkerAffinityGrace:  cfg.WorkerAffinityGrace,
	})

	// Browser live-event hub (M5): workersvc broadcasts persisted run events to
	// it, and the WS handler fans them out to subscribed browsers. In-process and
	// stateless — every event is already durable in the DB.
	liveHub := hub.New()
	wsvc.SetBroadcaster(liveHub)

	// Background sync engine: pulls forge changes into the issue cache for every
	// enabled repo. Its lifetime is tracked so shutdown waits for it before the
	// pool is closed (a mid-tick query must not race pool.Close).
	engine := poller.New(svc, q, cfg.ForgePollInterval, cfg.ForgeReconcileEvery)

	// Run-liveness sweeper (sibling of the poller). Boot runs one orphan sweep
	// immediately, then the goroutine sweeps on its own interval. Both lifetimes
	// are awaited on shutdown before the pool closes.
	sweep := sweeper.New(wsvc, 0)
	sweep.Boot(ctx)

	var bgWG sync.WaitGroup
	bgWG.Add(2)
	go func() {
		defer bgWG.Done()
		engine.Run(ctx)
	}()
	go func() {
		defer bgWG.Done()
		sweep.Run(ctx)
	}()

	authLimiter := mw.NewLimiter(cfg.RateLimitMax, cfg.RateLimitWindow, cfg.TrustedProxies)
	forgeLimiter := mw.NewLimiter(cfg.ForgeRateLimitMax, cfg.ForgeRateLimitWindow, cfg.TrustedProxies)
	h := handler.New(pool, q, cfg, box, svc, wsvc, liveHub)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h.Routes(authLimiter, forgeLimiter),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// Drain the HTTP server, then stop and await the background goroutines before
	// returning so the deferred pool.Close() cannot race an in-flight query.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && runErr == nil {
		runErr = err
	}
	stop() // cancel ctx so the poller + sweeper Run return (covers the server-error path too)
	bgWG.Wait()
	return runErr
}

// seedAdmin provisions the configured admin user if seeding is enabled and no
// user with that email exists yet. It uses the exact same argon2id hashing as
// registration and never touches an existing user's password. A concurrent
// create (unique violation) is treated as "already exists".
func seedAdmin(ctx context.Context, q *store.Queries, cfg config.Config) error {
	if cfg.SeedEmail == "" {
		return nil
	}
	if _, err := q.GetUserByEmail(ctx, cfg.SeedEmail); err == nil {
		slog.Info("seed admin already present, leaving untouched", "email", cfg.SeedEmail)
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("seed admin lookup: %w", err)
	}

	hash, err := auth.HashPassword(cfg.SeedPassword)
	if err != nil {
		return fmt.Errorf("seed admin hash: %w", err)
	}
	name := pgtype.Text{}
	if cfg.SeedName != "" {
		name = pgtype.Text{String: cfg.SeedName, Valid: true}
	}
	if _, err := q.CreateUser(ctx, store.CreateUserParams{
		Email:        cfg.SeedEmail,
		PasswordHash: hash,
		DisplayName:  name,
		IsAdmin:      true,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil // raced with another creator; the user now exists
		}
		return fmt.Errorf("seed admin create: %w", err)
	}
	slog.Info("seeded admin user", "email", cfg.SeedEmail)
	return nil
}
