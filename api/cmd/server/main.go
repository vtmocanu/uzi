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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/auth"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/handler"
	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/notifysvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/poller"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/runlifecycle"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/seed"
	"gitlab.example.com/vtmocanu/uzi/api/internal/selfimprove"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/slacksvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/sweeper"
	"gitlab.example.com/vtmocanu/uzi/api/internal/vault"
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
	// Same for the builtin agent skills (PRD #16): missing builtin skills are
	// inserted, admin edits survive restarts.
	if err := store.ReconcileBuiltinSkills(ctx, q); err != nil {
		return err
	}

	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}

	// Instance settings (PRD #19): a per-process read-through cache over
	// app_settings, shared by the HTTP handlers (read + invalidate on write) and
	// the forge sync/poller (read the configured PRD label every cycle). One
	// process, so one cache. Built before the forge service, which reads it.
	settingsCache := settings.New(q, cfg.SettingsCacheTTL)
	// Wire the secret cipher + ENV-source overlay (PRD #25): the Slack tokens and
	// public base URL an operator set via environment win over their DB rows, and
	// the box seals/opens the secret keys. Only keys actually set appear in the
	// overlay, so an unconfigured Slack instance stays a strict no-op.
	slackEnv := map[string]string{}
	if cfg.SlackBotToken != "" {
		slackEnv[settings.KeySlackBotToken] = cfg.SlackBotToken
	}
	if cfg.SlackAppToken != "" {
		slackEnv[settings.KeySlackAppToken] = cfg.SlackAppToken
	}
	if cfg.PublicBaseURL != "" {
		slackEnv[settings.KeyPublicBaseURL] = cfg.PublicBaseURL
	}
	settingsCache.ConfigureSecrets(box, slackEnv)

	// Shared bot-token Slack surface (PRD #25 M3): the run notifier, the account
	// linker, and the /me/slack test-DM endpoint all post through it. It reads the
	// CURRENT bot token per call (hot-rotation safe), bounded by the Slack HTTP
	// timeout.
	slackPoster := slacksvc.NewPoster(settingsCache.SlackBotToken, &http.Client{Timeout: cfg.SlackHTTPTimeout})

	// Account linker (PRD #25 M3): the email auto-match pass (fired on every socket
	// connect) plus the link-confirmation DM round-trip (Confirm / Not-me). It is
	// the Manager's inbound Block Kit handler and its on-connected hook, and it
	// backs the user-settings override / test-DM endpoints. Best-effort throughout —
	// it never affects a run or the socket.
	slackLinker := slacksvc.NewLinker(q, slackPoster, slog.Default())

	// Agent-runtime service (PRD #4): the run queue, worker protocol, and sweeper
	// DB work. Shares the same secret cipher (sole key holder) as the forge svc.
	// Constructed here (ahead of the seeds it used to follow) because the Slack
	// approval-gate handler needs its SubmitInput and the socket manager captures
	// its inbound handler at construction.
	wsvc := workersvc.New(q, box, workersvc.Params{
		RunTimeout:                  cfg.RunTimeout,
		RunIdleTimeout:              cfg.RunIdleTimeout,
		RunMaxIterations:            cfg.RunMaxIterations,
		RunMaxRequeues:              cfg.RunMaxRequeues,
		WorkerHeartbeatStale:        cfg.WorkerHeartbeatStale,
		WorkerAffinityGrace:         cfg.WorkerAffinityGrace,
		SkillMaxBytes:               cfg.SkillMaxBytes,
		SkillsMaxPerRun:             cfg.SkillsMaxPerRun,
		ChatIdleTimeout:             cfg.ChatIdleTimeout,
		ChatMaxTurns:                cfg.ChatMaxTurns,
		WorkerChatIdleTimeout:       cfg.WorkerChatIdleTimeout,
		WorkerChatTurnTimeout:       cfg.WorkerChatTurnTimeout,
		ProposalConfirmStuckTimeout: cfg.ProposalConfirmStuckTimeout,
	})

	// Plan-approval gatekeeper (PRD #25 M4): handles the Slack Approve / Reject /
	// Reject-without-reason buttons. It rides workersvc's ownership-checked
	// SubmitInput (via the gateSubmitter adapter, which keeps slacksvc free of a
	// workersvc import) and reads run status itself for stale-click handling.
	slackGate := slacksvc.NewGatekeeper(q, gateSubmitter{wsvc}, slackPoster, slog.Default())

	// Reply-from-Slack handler (PRD #25 M5): inbound message.im thread replies →
	// reasoned reject during a reject-pending gate, follow_up on a live run, a nudge
	// during an open gate, or a coalesced ephemeral otherwise. Same ownership-checked
	// submitter as the gatekeeper.
	slackReplier := slacksvc.NewReplier(q, gateSubmitter{wsvc}, slackPoster, slog.Default())

	// Slack Socket Mode manager (PRD #25 M2). Supervises the single outbound
	// connection: it polls the settings cache and, while Slack is enabled with both
	// tokens present, keeps a socket up (backoff reconnect, hot-restart on a
	// token/enable change); otherwise it idles as a strict no-op. It never touches
	// the run lifecycle — Slack is best-effort. Run in the background WaitGroup below.
	// Inbound Block Kit actions fan out through an InboundMux to the linker (Confirm
	// / Not-me) and the gatekeeper (gate buttons); message.im thread replies go to
	// the replier.
	slackManager := slacksvc.NewManager(settingsCache, slacksvc.Config{
		HTTPTimeout: cfg.SlackHTTPTimeout,
		Inbound:     slacksvc.InboundMux{slackLinker, slackGate},
		Messages:    slackReplier,
		OnConnected: slackLinker.AutoMatch,
	})

	svc := forgesvc.New(q, box, cfg.ForgeHTTPTimeout, settingsCache)

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
	// Per-user vault (PRD #32): password-wrapped DEKs sealing each user's personal
	// secrets. One instance, shared by the HTTP handlers (unlock at login, secret
	// save) and — from M3 — the worker service (claim-time open), so a login on the
	// API and a claim by the worker see one DEK cache. Built on the master box
	// (which now seals only connection PATs + legacy master-sealed rows) and the store.
	vlt := vault.New(box, q)
	// Boot-unlock the seed admin so a headless deployment runs overnight autopilot
	// from the first boot: the DEK cache is empty after every restart, and without
	// this the seed admin would sit locked until an interactive login (defeating the
	// bootstrap case). Fatal when seeding is configured — a seed admin whose vault
	// cannot be unlocked at boot is a broken bootstrap, matching the other seed
	// steps' loud-on-misconfig stance. First boot creates the vault here; a later
	// boot re-derives it and lazily rewraps any legacy master-sealed secret to the DEK.
	if cfg.SeedEmail != "" {
		seedUser, err := q.GetUserByEmail(ctx, cfg.SeedEmail)
		if err != nil {
			return fmt.Errorf("boot-unlock seed admin: look up %q: %w", cfg.SeedEmail, err)
		}
		if err := vlt.Unlock(ctx, seedUser.ID, cfg.SeedPassword); err != nil {
			return fmt.Errorf("boot-unlock seed admin vault: %w", err)
		}
		slog.Info("boot-unlocked seed admin vault", "email", cfg.SeedEmail)
	}
	// Optional startup Anthropic-token seed for the seed admin (dev convenience:
	// survives `docker compose down -v` so the token need not be re-pasted). Runs
	// after the admin seed (whose user it belongs to) and the boot-unlock (so it can
	// DEK-seal). Create-only and format-checked only — it seeds the operator's
	// EXISTING token, never mints a credential, never does a live/network check, and
	// never logs the value. A DB error or a malformed configured token aborts boot;
	// an already-present token is left untouched.
	if err := seed.AnthropicToken(ctx, q, vlt, cfg); err != nil {
		return err
	}
	// Optional startup Slack-settings seed (UZI_SEED_SLACK_*): create-only
	// app_settings rows — tokens sealed at rest, slack_enabled flipped on, and
	// optionally public_base_url — so a fresh `down -v` stack comes up
	// Slack-configured from .env while the admin UI stays the editable source of
	// truth afterwards (unlike the SLACK_* overlay, which pins and greys the
	// fields). No network validation at boot: a bad token surfaces as the socket
	// manager's failed connect. DB or seal errors abort boot; existing rows are
	// left untouched. Runs before the manager goroutine starts, and the cache is
	// invalidated so its first read sees the seeded rows.
	if err := seed.SlackSettings(ctx, q, box, cfg); err != nil {
		return err
	}
	settingsCache.Invalidate()

	// Claim gating + claim-time token open share the same vault instance the HTTP
	// handlers hold (PRD #32 M3): a locked owner's runs stay queued instead of
	// claiming, and a 'dek'-sealed Anthropic token opens only while unlocked. wsvc
	// is built above (ahead of the Slack setup that needs it); vlt just above.
	wsvc.SetVault(vlt)

	// Instance settings (PRD #46): gate the judge terminal-funnel enqueue on the
	// global judge_enabled kill-switch and ride the judge model into the claim.
	wsvc.SetSettings(settingsCache)

	// Browser live-event hub (M5): workersvc broadcasts persisted run events to
	// it, and the WS handler fans them out to subscribed browsers. In-process and
	// stateless — every event is already durable in the DB.
	liveHub := hub.New()

	// Slack run-notifier (PRD #25 M3): a Broadcaster that turns run state
	// transitions into per-owner DMs. It shares the workersvc broadcast seam with
	// the WS hub via MultiBroadcaster, so a persisted transition fans out to both
	// the browser and Slack. PublishState never blocks (it enqueues); a Slack
	// failure is logged redacted and never affects the run. Unconfigured users are
	// dropped silently, so this is a strict no-op until linking is set up.
	slackNotifier := slacksvc.NewNotifier(
		q,
		slackPoster,
		settingsCache.PublicBaseURL,
		slog.Default(),
	)
	wsvc.SetBroadcaster(workersvc.MultiBroadcaster{liveHub, slackNotifier})

	// Notifications write seam (PRD #46 M2): the one place that creates inbox rows.
	// It persists the row first, then delivers best-effort through slackNotifier
	// (reusing its per-user opt-in gating + drain goroutine via a separate queue).
	// M3+ tenants (the judge) call notifier.Notify; the M2 REST read endpoints go
	// straight to the store, so the handler only needs the seam wired for future
	// producers.
	notifier := notifysvc.New(q, slackNotifier, notifysvc.DefaultUserCap, slog.Default())

	// Board column automation (PRD #12): reacts to run status changes with
	// forge-first label moves, plus a reconcile loop that retries moves a down
	// forge dropped. Wired into workersvc as the status-change hook and run as its
	// own goroutine, isolated from the liveness sweep so a stalled forge never
	// delays worker-loss recovery.
	lifecycle := runlifecycle.New(q, svc, cfg.FrontendOrigin)
	wsvc.SetLifecycle(lifecycle)

	// Background sync engine: pulls forge changes into the issue cache for every
	// enabled repo. Its lifetime is tracked so shutdown waits for it before the
	// pool is closed (a mid-tick query must not race pool.Close).
	engine := poller.New(svc, q, cfg.ForgePollInterval, cfg.ForgeReconcileEvery)
	// Autopilot (PRD #19 M4): the poller's post-sync detector turns an autopilot-label
	// application on a PRD issue into an auto_approve run for the mapped consenting
	// user (or one explanatory issue comment). Wired post-construction like the other
	// optional poller collaborators; run creation reuses workersvc's manual-start path
	// (same state machine and gates) and the label comes from the settings cache.
	engine.SetAutopilot(poller.NewAutopilot(q, wsvc, settingsCache))
	// CI status sync (PRD #6): the poller tick also refreshes the pipeline-status
	// cache (default branch + watched run branches). CI_WATCH_MAX_REFS=0 disables it
	// (and the badges + Fix CI), preserving today's behaviour.
	engine.SetPipelineWatch(cfg.CIWatchRunWindow, cfg.CIWatchMaxRefs)

	// Run-liveness sweeper (sibling of the poller). Boot runs one orphan sweep
	// immediately, then the goroutine sweeps on its own interval. Both lifetimes
	// are awaited on shutdown before the pool closes.
	sweep := sweeper.New(wsvc, 0)
	sweep.Boot(ctx)

	// PAT least-privilege service + sweep (PRD #5). The service is shared by the
	// on-demand handler and the sweep; it reuses forgesvc to build a driver from a
	// stored connection. The sweep is the poller's/worker-sweeper's sibling: a Boot
	// pass back-fills reports for grandfathered (never-checked) connections right
	// after deploy, then it ticks on UZI_PRIVILEGE_CHECK_INTERVAL. 0 disables it
	// entirely (no boot pass, no loop).
	pcheck := privcheck.NewService(q, svc)

	var bgWG sync.WaitGroup
	bgWG.Add(5)
	go func() {
		defer bgWG.Done()
		engine.Run(ctx)
	}()
	go func() {
		defer bgWG.Done()
		sweep.Run(ctx)
	}()
	go func() {
		defer bgWG.Done()
		lifecycle.RunReconciler(ctx)
	}()
	go func() {
		defer bgWG.Done()
		slackManager.Run(ctx)
	}()
	go func() {
		defer bgWG.Done()
		slackNotifier.Run(ctx)
	}()
	if cfg.PrivilegeCheckInterval > 0 {
		privSweep := privcheck.NewEngine(pcheck, cfg.PrivilegeCheckInterval)
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			// Boot pass runs INSIDE the goroutine (poller precedent), not on the
			// boot path: it makes forge network calls, so a slow/down forge at
			// deploy must not delay ListenAndServe or the /api/health endpoint (and
			// trip the compose healthcheck chain). Async still back-fills
			// grandfathered reports within seconds of a healthy forge.
			privSweep.Boot(ctx)
			privSweep.Run(ctx)
		}()
	} else {
		slog.Info("privilege sweeper disabled (UZI_PRIVILEGE_CHECK_INTERVAL=0)")
	}

	// Self-improvement engine (PRD #46 M5): a privcheck-shaped scheduler that, on a
	// due cycle, files/reuses a tracking issue on the connected uzi repo and creates
	// an auto-approved self_improve run folding the accumulated improve_uzi backlog.
	// Shares the same collaborators the rest of the run machinery uses: workersvc
	// creates the run, forgesvc builds the driver from the stored connection, the
	// vault gates on the enabling admin being unlocked, and notifysvc delivers the
	// tick-skip / run-started notifications. 0 disables it entirely; the Boot pass
	// runs inside the goroutine (poller precedent) so a slow forge can't delay serve.
	if cfg.SelfimproveCheckInterval > 0 {
		siEngine := selfimprove.New(q, settingsCache, wsvc, svc, vlt, notifier, cfg.SelfimproveCheckInterval, slog.Default())
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			siEngine.Boot(ctx)
			siEngine.Run(ctx)
		}()
	} else {
		slog.Info("self-improvement engine disabled (UZI_SELFIMPROVE_CHECK_INTERVAL=0)")
	}

	authLimiter := mw.NewLimiter(cfg.RateLimitMax, cfg.RateLimitWindow, cfg.TrustedProxies)
	forgeLimiter := mw.NewLimiter(cfg.ForgeRateLimitMax, cfg.ForgeRateLimitWindow, cfg.TrustedProxies)
	// Dedicated tighter budget for the two Slack-DM-triggering /me/slack endpoints
	// (PRD #25 M3 fast-follow) — see the wiring in handler.Routes.
	slackDMLimiter := mw.NewLimiter(cfg.SlackDMRateLimitMax, cfg.SlackDMRateLimitWindow, cfg.TrustedProxies)
	// Per-user chat budget (PRD #39): a spend guard on chat create + message posts.
	chatLimiter := mw.NewLimiter(cfg.ChatRateLimitMax, cfg.ChatRateLimitWindow, cfg.TrustedProxies)
	// Per-user re-run-judge budget (PRD #46 Decision 8): a dedicated spend guard on the
	// re-run-judge action, separate from chat so neither consumes the other's allowance.
	judgeLimiter := mw.NewLimiter(cfg.JudgeRateLimitMax, cfg.JudgeRateLimitWindow, cfg.TrustedProxies)
	// Per-worker budget on the propose_issue endpoint (PRD #39 M3): a proposal-spam
	// guard complementing the per-run pending cap.
	proposalLimiter := mw.NewLimiter(cfg.ProposalRateLimitMax, cfg.ProposalRateLimitWindow, cfg.TrustedProxies)
	h := handler.New(pool, q, cfg, box, svc, wsvc, pcheck, liveHub, settingsCache)
	// The settings PUT handler asks the poller to full-sync every repo when a label
	// changes (PRD #19 M2). Wired post-construction: the poller is built above but
	// the signal target is the handler.
	h.SetReconciler(engine)
	// Share the vault with the HTTP handlers: unlock at login, DEK-seal on secret
	// save, the /api/vault endpoints, and vault status on /api/me (PRD #32). M3 adds
	// the same instance to workersvc for claim-time gating + open.
	h.SetVault(vlt)
	// Surface the live Slack connection state on the admin settings DTO (PRD #25 M2).
	h.SetSlackStatus(slackManager.State)
	// Wire the account linker so the /me/slack override + test-DM endpoints can send
	// their Slack DMs (PRD #25 M3). Best-effort: a nil linker (never in production)
	// would make those endpoints report Slack as unavailable.
	h.SetSlackLinker(slackLinker)
	// Wire the notifications write seam (PRD #46 M2) for future producers (the judge,
	// M4). The M2 read endpoints don't need it; this makes the seam available.
	h.SetNotifier(notifier)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h.Routes(authLimiter, forgeLimiter, slackDMLimiter, chatLimiter, proposalLimiter, judgeLimiter),
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
	stop() // cancel ctx so the poller + sweeper + reconciler Run return (covers the server-error path too)
	bgWG.Wait()
	// The HTTP server has drained, so no new inline Notify can be fired; wait for
	// any in-flight ones to finish before the deferred pool.Close so they don't
	// race a closing pool.
	lifecycle.Wait()
	return runErr
}

// gateSubmitter adapts *workersvc.Service to slacksvc.PlanGateSubmitter — the
// slice the Slack approval gatekeeper needs (read a run, submit an approve/reject
// input). It drops SubmitInput's result (the gatekeeper only cares whether it
// succeeded) and keeps slacksvc free of a workersvc import.
type gateSubmitter struct{ svc *workersvc.Service }

func (g gateSubmitter) GetRun(ctx context.Context, userID, runID uuid.UUID) (store.Run, error) {
	return g.svc.GetRun(ctx, userID, runID)
}

// SubmitInput adapts the Slack gate's reject path to the run service. Approve goes
// through SubmitApproval (which carries the agent source); this carries no
// selection (reject_plan / — never approve_plan from the gate).
func (g gateSubmitter) SubmitInput(ctx context.Context, userID, runID uuid.UUID, kind, body string) error {
	_, err := g.svc.SubmitInput(ctx, userID, runID, kind, body, nil)
	return err
}

// SubmitApproval adapts the Slack agent-picker approve (PRD #37 M7): the gatekeeper
// passes a source string from a closed action-id set; here we build the
// workersvc.AgentSelection (Slack never sets exclusions — those live in the web UI).
// The server re-reads the run's roster and validates; ErrInvalidSelection (the
// source no longer holds) is translated to the slacksvc sentinel so the gatekeeper
// leaves the gate open. Keeping this translation in main keeps slacksvc free of a
// workersvc import.
func (g gateSubmitter) SubmitApproval(ctx context.Context, userID, runID uuid.UUID, source string) error {
	_, err := g.svc.SubmitInput(ctx, userID, runID, "approve_plan", "",
		&workersvc.AgentSelection{Source: source, Exclusions: []string{}})
	if errors.Is(err, workersvc.ErrInvalidSelection) {
		return slacksvc.ErrSelectionRejected
	}
	return err
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
