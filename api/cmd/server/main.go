// Command server is the uzi API: it runs DB migrations, then serves the auth
// and admin HTTP API.
package main

import (
	"context"
	"encoding/json"
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

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/handler"
	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/oidc"
	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/runlifecycle"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/seed"
	"github.com/vtmocanu/uzi/api/internal/selfimprove"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/sweeper"
	"github.com/vtmocanu/uzi/api/internal/tlsx"
	"github.com/vtmocanu/uzi/api/internal/toolseed"
	"github.com/vtmocanu/uzi/api/internal/usagepoller"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// version is stamped at build time via -ldflags "-X main.version=X.Y.Z" — BARE, with
// no leading v. api/Dockerfile stamps ${UZI_VERSION#v} from the release git tag, and
// that strip is the point of Model B rather than a detail: the served value equals the
// image tag equals the chart appVersion, all bare, and the SPA re-adds a "v" only for
// display. Unset on a plain local `go build`, so it defaults to "dev" and is served
// that way at GET /api/version — alongside the coordinates below.
//
// api/cmd/uzi/root.go carries the IDENTICAL sentence and says vX.Y.Z, and it is
// correct there: Formula/uzi-cli.rb stamps -X main.version=v#{version}, so the CLI
// binary really does carry the v. The two binaries genuinely differ here. Do not
// "fix" one to match the other — root CLAUDE.md's semver.Compare warning is entirely
// about which side of this line a string sits on, and it cites this Dockerfile strip
// as the reason every version this project ships is bare.
var version = "dev"

// commit, builtAt, commits, prdsDone and prdsOpen are the rest of GET /api/version's
// build info (PRD #175, #245), stamped from the same Dockerfile ldflags line as
// version:
//
//	-X main.commit=<full 40-char source SHA>
//	-X main.builtAt=<RFC3339 UTC>
//	-X main.commits=<decimal commit count>
//	-X main.prdsDone=<decimal count of prds/done/*.md>
//	-X main.prdsOpen=<decimal count of prds/*.md>
//
// commits — and, for the same reason, prdsDone/prdsOpen — need what the image build
// does not have: commits needs git history, and the PRD counts need the repo root the
// api/ build context lacks (prds/ is never copied into the api/ image). The publish
// context is api/ with no .git and no prds/, and the kaniko image carries neither. CI
// computes all three in publish:assert-changelog and delivers them as a dotenv report
// (PRD #175 M3, #245), which is why their CI path looks nothing like the other two.
//
// They live in THIS package because that is the only place the linker can reach: the
// values are served from internal/handler, but -X names a package-level string var by
// its own package path, so -X main.commit targets cmd/server and not handler.
//
// Only the tag (publish) build passes them, so all five are empty on a plain
// `go build`, on `docker compose build`, and on the MR/main validation image. That is
// deliberate rather than a gap — a `dev` build's commit is not a release coordinate —
// and an empty value is OMITTED from the response, never served as "" or as a zero
// timestamp.
var (
	commit   = ""
	builtAt  = ""
	commits  = ""
	prdsDone = ""
	prdsOpen = ""
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
	defer func() { _ = resp.Body.Close() }()
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
	//
	// This MUST precede the skills reconciler: a builtin skill's default
	// allocation resolves its agent template BY NAME and is seeded only on the
	// boot that first inserts the skill, so running skills first would seed
	// nothing and never retry (PRD #72 M2, Decision 9). `templatesDone` is what
	// makes that ordering a compile-time dependency rather than a convention —
	// swapping these two blocks does not compile.
	templatesDone, err := store.ReconcileBuiltinTemplates(ctx, q)
	if err != nil {
		return err
	}
	// Same for the builtin agent skills (PRD #16): missing builtin skills are
	// inserted, admin edits survive restarts. Newly-inserted builtins also get
	// their default shared allocations (PRD #72 M2).
	if err := store.ReconcileBuiltinSkills(ctx, store.PoolTxer{Pool: pool, Q: q}, templatesDone); err != nil {
		return err
	}

	// Boot-time offender report (PRD #123 M3): name any tool_allowlist rows that
	// are NOT in the baked worker toolchain, so an admin can fix them deliberately.
	// A run requesting such a package would hang then fail behind the worker egress
	// block; the write-time gates (CreateToolAllowlistEntry / SetRepoToolProfile)
	// block new ones, and this surfaces rows that predate the gate. Non-fatal: a
	// query error is logged and boot continues. This replaces the PRD's "reporting
	// migration" — toolseed is the single source of truth, so this cannot drift.
	if rows, err := q.ListToolAllowlist(ctx); err != nil {
		slog.Warn("tool_allowlist coverage report skipped (list failed)", "error", err)
	} else {
		var unbaked []string
		for _, row := range rows {
			if !toolseed.Covered(row.Name) {
				unbaked = append(unbaked, row.Name)
			}
		}
		if len(unbaked) > 0 {
			slog.Warn("tool_allowlist entries are not in the baked worker toolchain; runs requesting them will fail until the image is rolled or the rows removed", "packages", unbaked)
		}
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
		PlanMaxRevisions:            cfg.PlanMaxRevisions,
		QuestionMax:                 cfg.QuestionMax,
		QuestionTimeoutSeconds:      cfg.QuestionTimeoutSeconds,
		RunMaxRequeues:              cfg.RunMaxRequeues,
		WorkerHeartbeatStale:        cfg.WorkerHeartbeatStale,
		WorkerAffinityGrace:         cfg.WorkerAffinityGrace,
		WorkerSpreadGrace:           cfg.WorkerSpreadGrace,
		WorkerBackgroundGrace:       cfg.WorkerBackgroundGrace,
		SkillMaxBytes:               cfg.SkillMaxBytes,
		SkillsMaxPerRun:             cfg.SkillsMaxPerRun,
		ChatIdleTimeout:             cfg.ChatIdleTimeout,
		ChatMaxTurns:                cfg.ChatMaxTurns,
		WorkerChatIdleTimeout:       cfg.WorkerChatIdleTimeout,
		WorkerChatTurnTimeout:       cfg.WorkerChatTurnTimeout,
		ProposalConfirmStuckTimeout: cfg.ProposalConfirmStuckTimeout,
		AutoStopEnabled:             cfg.AutoStopEnabled,
		// The SAME constructor the settings page reads (PRD #111 D21) — not a second
		// literal mapping the four UZI_AUTOSELECT_* knobs. Two mappings would each be
		// internally consistent while the meter and the selector disagreed about the
		// same token, with nothing going red.
		Autoselect: cfg.AutoselectPolicy(),
		// PRD #35 usage-limit park. Both are server-side bounds on a WORKER-REPORTED
		// event: the worker asks to park, the server decides for how long and how often.
		RunLimitMaxWaits: cfg.RunLimitMaxWaits,
		RunLimitMaxPark:  cfg.RunLimitMaxPark,
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

	// Per-user chat budget (PRD #39): a spend guard on chat create + message posts.
	// Created here (ahead of the other route limiters) because the Slack chat opener
	// captures it below — the socket manager starts before the route wiring — and
	// because web and Slack MUST share ONE bucket per user (PRD #191 Decision 9). The
	// opener keys it identically to the web POST /chats mount (RoutePattern|userID) so a
	// heavy Slack day rate-limits the web Chat page and vice versa.
	chatLimiter := mw.NewLimiter(cfg.ChatRateLimitMax, cfg.ChatRateLimitWindow, cfg.TrustedProxies)
	slackReplier.SetChatSpendGuard(func(userID uuid.UUID) bool {
		return chatLimiter.Allow(handler.ChatCreateRoutePattern + "|" + userID.String())
	})

	// Chat-card handler (PRD #191 M4): the slack_chat_* Block Kit buttons — Create /
	// Dismiss on an issue-proposal card — routed as a THIRD InboundMux member beside the
	// linker and the gatekeeper. The forge write rides the same claim-first
	// ConfirmProposalForUser the web confirm uses (lifted in M1).
	slackChatActions := slacksvc.NewChatActions(q, gateSubmitter{wsvc}, slackPoster, settingsCache.PublicBaseURL, slog.Default())
	// The Continue button mints a run and spends the owner's token, so it draws from the
	// SAME shared per-user chat budget the opener uses (PRD #191 M6) — same key, one pool.
	slackChatActions.SetChatSpendGuard(func(userID uuid.UUID) bool {
		return chatLimiter.Allow(handler.ChatCreateRoutePattern + "|" + userID.String())
	})

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
		Inbound:     slacksvc.InboundMux{slackLinker, slackGate, slackChatActions},
		Messages:    slackReplier,
		OnConnected: slackLinker.AutoMatch,
	})

	svc := forgesvc.New(q, box, cfg.ForgeHTTPTimeout, settingsCache)

	// Wire the forge builder into workersvc so its composite forge-write operations —
	// ConfirmProposalForUser, StartRunForUser (PRD #191 Decision 8) — reach the forge
	// through the same decryption path the handlers use, without a forgesvc↔workersvc
	// import cycle. selfimprove/privcheck already pass this same *forgesvc.Service.
	wsvc.SetForges(svc)

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
	// Run-health detector settings (PRD #47): the sweeper reads the runtime-tunable
	// health thresholds from the same settings cache the HTTP handlers hold, so an
	// admin change takes effect within the cache TTL. Nil would disable detection;
	// wiring it here turns it on with the compiled-in defaults.
	wsvc.SetHealthSettings(settingsCache)
	// Docker-worker repo allowlist (PRD #89 M-allow): the claim gate reads which repos
	// a docker-enabled worker may claim runs for from the same settings cache, so an
	// admin change takes effect within the cache TTL. This is the accepted-risk
	// likelihood control for the non-rootless DinD tier — a docker worker fail-closes
	// (claims no repo-bearing run) for any repo not on the list. Non-docker workers are
	// unaffected.
	wsvc.SetDockerAllowlist(settingsCache)

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
	// Notifications write seam (PRD #46 M2): the one place that creates inbox rows.
	// It persists the row first, then delivers best-effort through slackNotifier
	// (reusing its per-user opt-in gating + drain goroutine via a separate queue).
	// M3+ tenants (the judge) call notifier.Notify; the M2 REST read endpoints go
	// straight to the store, so the handler only needs the seam wired for future
	// producers. Built before SetBroadcaster because failNotifier (below) writes
	// through it and joins the broadcast fan-out.
	notifier := notifysvc.New(q, slackNotifier, notifysvc.DefaultUserCap, slog.Default())

	// Run-failure inbox notifier (PRD #284 M5): a Broadcaster that lands an inbox
	// notification when a run transitions to "failed", so a user WITHOUT Slack gets
	// a badge on failure (the slacksvc ❌ DM only reaches opted-in users). It writes
	// through notifier with Slack == nil (inbox-only — no double-DM), and sits in
	// the MultiBroadcaster so it covers worker-reported AND sweep-driven failures
	// uniformly. PublishState never blocks; a drain goroutine (below) does the work.
	// CI-autofix landed notification (PRD #71 M6): when a ci_fix run's fix pipeline
	// goes green on a ref that had an auto-fix ledger, forgesvc lands an inbox row for
	// the owner. Nil-safe; wired here so the sync's reset-on-green path can reach it.
	svc.SetNotifier(notifier)

	failNotifier := notifysvc.NewRunFailureNotifier(q, notifier, slog.Default())
	wsvc.SetBroadcaster(workersvc.MultiBroadcaster{liveHub, slackNotifier, failNotifier})

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
	// Floor the per-tick deadline at the forge HTTP timeout so a poll interval shorter
	// than a single forge call (the e2e harness pins 2s, under the 15s default) can't
	// cancel an in-flight sync call — decouples the tick DEADLINE from the poll cadence
	// (issue #139).
	engine.SetForgeTimeout(cfg.ForgeHTTPTimeout)
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
	// CI-autofix (PRD #71 M6): the poller's post-pipeline-sync detector turns an
	// eligible failing agent-MR branch into an automatic ci_fix run (or one halt
	// comment), through the same workersvc create path the manual "Fix CI" button uses
	// and the M4 loop-guard ledger. Wired unconditionally — the instance kill-switch is
	// simply NOT wiring it; per-user ci_autofix_enabled (default-OFF) and the
	// pipelineMaxRefs>0 gate control activation. notifier lands the inbox rows.
	engine.SetCIAutoFix(poller.NewCIAutoFix(q, wsvc, notifier, cfg.CIFixMaxJobs, cfg.CIFixLogTailBytes, cfg.CIAutofixMaxAttempts, cfg.CIAutofixConfigPaths))

	// Run-liveness sweeper (sibling of the poller). Boot runs one orphan sweep
	// immediately, then the goroutine sweeps on its own interval. Both lifetimes
	// are awaited on shutdown before the pool closes.
	//
	// It also carries PRD #58's pending-join-token expiry, which bounds how long a
	// sealed, undelivered token sits at rest under UZI_SECRET_KEY. That pass is
	// wired UNCONDITIONALLY — deliberately NOT behind cfg.WorkerHostingEnabled: a
	// stack that provisioned hosted workers and then turned hosting off is exactly
	// the one whose tokens would otherwise never be swept. Riding this existing
	// ticker (rather than a goroutine of its own) also keeps the flag-off footprint
	// to one indexed UPDATE that matches nothing on a stack with no hosted workers.
	sweep := sweeper.New(wsvc, 0,
		sweeper.Pass{
			Name: "hosted_tokens_expired",
			Run: func(ctx context.Context) (int64, error) {
				return hostedsvc.ExpirePendingTokens(ctx, q, cfg.HostedTokenTTL)
			},
		},
		// Browser-brokered CLI login requests (PRD #64 M5) are short-lived (~5 min) but
		// nothing deletes them once expired. The start handler sweeps opportunistically;
		// this rides the same ticker so a stack no one has logged in through recently
		// still gets its expired rows cleared, folding into the existing goroutine rather
		// than spawning its own.
		sweeper.Pass{
			Name: "cli_auth_requests_expired",
			Run: func(ctx context.Context) (int64, error) {
				return q.DeleteExpiredCLIAuthRequests(ctx)
			},
		},
		// Stranded issue-filing claims (PRD #68 M3): a file handler killed after the
		// claim (filing_since set) but before it settled leaves a row that blocks the
		// coordinate forever. This DELETEs claims older than the clamped cutoff (>= 2x
		// ForgeHTTPTimeout, config.go) so a slow-but-alive CreateIssue is never reaped
		// mid-flight. 0 disables the sweep. Rides this ticker rather than its own
		// goroutine, like the passes above.
		sweeper.Pass{
			Name: "issue_filing_claims_stranded",
			Run: func(ctx context.Context) (int64, error) {
				if cfg.IssueFilingStuckTimeout <= 0 {
					return 0, nil
				}
				cutoff := time.Now().Add(-cfg.IssueFilingStuckTimeout)
				return q.SweepStrandedRecommendationClaims(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
			},
		},
		// Stranded incidental-finding filing claims (PRD #333 M5): the finding sibling of
		// the pass above. A FileFinding killed after ClaimFindingForFiling (status='filing')
		// but before it settled/reverted leaves the coordinate `filing` forever — both
		// ClaimFindingForFiling and DismissFinding guard status='open', so nothing else can
		// move it. This resets it to `open` past the SAME clamped cutoff (>= 2x
		// ForgeHTTPTimeout) so a slow-but-alive CreateIssue is never reset mid-flight. Reuses
		// IssueFilingStuckTimeout (identical semantics — no new knob); 0 disables it.
		sweeper.Pass{
			Name: "finding_filing_claims_stranded",
			Run: func(ctx context.Context) (int64, error) {
				if cfg.IssueFilingStuckTimeout <= 0 {
					return 0, nil
				}
				cutoff := time.Now().Add(-cfg.IssueFilingStuckTimeout)
				return q.SweepStrandedFilingFindings(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
			},
		},
	)
	sweep.Boot(ctx)

	// PAT least-privilege service + sweep (PRD #5). The service is shared by the
	// on-demand handler and the sweep; it reuses forgesvc to build a driver from a
	// stored connection. The sweep is the poller's/worker-sweeper's sibling: a Boot
	// pass back-fills reports for grandfathered (never-checked) connections right
	// after deploy, then it ticks on UZI_PRIVILEGE_CHECK_INTERVAL. 0 disables it
	// entirely (no boot pass, no loop).
	pcheck := privcheck.NewService(q, svc)
	// #66 D1 layer 2: wire the default-branch guardrail into the run-create service.
	// Late-injected here because pcheck is built after wsvc; *privcheck.Service
	// satisfies workersvc.RepoGuard via its GuardRepo method (M2).
	wsvc.SetRepoGuard(pcheck)

	var bgWG sync.WaitGroup
	bgWG.Add(6)
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
	go func() {
		defer bgWG.Done()
		failNotifier.Run(ctx)
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

	// Run scheduler (PRD #241): a time-driven run origin alongside autopilot. Each
	// tick it claims due run_schedules and fires each through the SAME shared
	// run-creation seam autopilot uses (workersvc), so every gate — PRDLESS, fresh
	// forge fetch, active-run dedup, usage-limit park — is inherited. It shares the
	// same collaborators: workersvc creates the run, forgesvc builds the driver from
	// the stored connection, settingsCache resolves PRDLESS/PRD labels, notifysvc
	// delivers error notifications. 0 disables it; the Boot pass runs inside the
	// goroutine (poller precedent) so a slow forge can't delay serve, and it makes a
	// schedule that came due while the api was down fire promptly after a restart.
	if cfg.SchedulerCheckInterval > 0 {
		scheduler := schedsvc.New(q, wsvc, svc, settingsCache, notifier, cfg.SchedulerCheckInterval, slog.Default())
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			scheduler.Boot(ctx)
			scheduler.Run(ctx)
		}()
	} else {
		slog.Info("run scheduler disabled (UZI_SCHEDULER_CHECK_INTERVAL=0)")
	}

	// Per-user Claude rate-limit poller (PRD #53): each tick it polls every
	// token-holding user's two Anthropic windows (free usage endpoint first, ~1-token
	// header probe fallback) and upserts one gauge row per user. The token opener is
	// the same vault path the run lane uses (secretopen), so a locked owner is
	// skipped and a master-sealed token still polled. 0 disables it entirely; the
	// Boot pass runs inside the goroutine (poller precedent) so a slow Anthropic can't
	// delay serve. Held in an outer var so the token-save handler can poke it (D3b).
	var usageEngine *usagepoller.Engine
	if cfg.UsagePollInterval > 0 {
		anthropicClient := anthropic.New(&http.Client{Timeout: cfg.AnthropicHTTPTimeout})
		usageEngine = usagepoller.New(q, secretopen.NewOpener(q, vlt, box), anthropicClient, cfg.UsagePollInterval, cfg.UsageProbe, slog.Default())
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			usageEngine.Boot(ctx)
			usageEngine.Run(ctx)
		}()
	} else {
		slog.Info("usage poller disabled (UZI_USAGE_POLL_INTERVAL=0)")
	}

	authLimiter := mw.NewLimiter(cfg.RateLimitMax, cfg.RateLimitWindow, cfg.TrustedProxies)
	forgeLimiter := mw.NewLimiter(cfg.ForgeRateLimitMax, cfg.ForgeRateLimitWindow, cfg.TrustedProxies)
	// Dedicated tighter budget for the two Slack-DM-triggering /me/slack endpoints
	// (PRD #25 M3 fast-follow) — see the wiring in handler.Routes.
	slackDMLimiter := mw.NewLimiter(cfg.SlackDMRateLimitMax, cfg.SlackDMRateLimitWindow, cfg.TrustedProxies)
	// Per-user re-run-judge budget (PRD #46 Decision 8): a dedicated spend guard on the
	// re-run-judge action, separate from chat so neither consumes the other's allowance.
	judgeLimiter := mw.NewLimiter(cfg.JudgeRateLimitMax, cfg.JudgeRateLimitWindow, cfg.TrustedProxies)
	// Per-worker budget on the propose_issue endpoint (PRD #39 M3): a proposal-spam
	// guard complementing the per-run pending cap.
	proposalLimiter := mw.NewLimiter(cfg.ProposalRateLimitMax, cfg.ProposalRateLimitWindow, cfg.TrustedProxies)
	// Per-user budget on the cluster-object-churning endpoints (PRD #58 Decision 8):
	// hosted provision and worker delete. Built unconditionally — it also covers
	// external-worker deletes, which exist whether or not hosting is enabled.
	hostedLimiter := mw.NewLimiter(cfg.HostedRateLimitMax, cfg.HostedRateLimitWindow, cfg.TrustedProxies)
	// Dedicated per-(path,IP) budget for POST /api/auth/cli/poll (PRD #64 M5). Sized to
	// comfortably exceed the poll cadence the server itself returns (12/min at 5s) so
	// uzi login never trips its own limit — which the shared 10/min authLimiter would.
	cliPollLimiter := mw.NewLimiter(cfg.CLIPollRateLimitMax, cfg.CLIPollRateLimitWindow, cfg.TrustedProxies)
	// Dedicated per-user budget for the board reorder (PRD #102 M5). Deliberately NOT
	// the forge budget: a reorder makes zero forge calls, and charging it there would
	// let a burst of dragging starve the user's real forge operations.
	boardOrderLimiter := mw.NewLimiter(cfg.BoardOrderRateLimitMax, cfg.BoardOrderRateLimitWindow, cfg.TrustedProxies)
	h := handler.New(pool, q, cfg, box, svc, wsvc, pcheck, liveHub, settingsCache)
	h.SetVersion(version)
	// The rest of the build coordinates (PRD #175). Passed raw: the handler decides
	// what an absent or unparseable stamp means, so there is one such place.
	h.SetBuildInfo(handler.BuildStamp{Commit: commit, BuiltAt: builtAt, Commits: commits, PrdsDone: prdsDone, PrdsOpen: prdsOpen})
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
	// Hosted k8s workers (PRD #58 Decision 12). Only when the feature is on: off (the
	// compose default) Routes mounts no controller endpoint, so the service would have
	// no caller. Shares the same secret cipher (sole key holder) as the forge/worker
	// services — it seals pending join tokens with it.
	if cfg.WorkerHostingEnabled {
		h.SetHostedSvc(hostedsvc.New(q, box))
		slog.Info("hosted k8s workers enabled; serving the controller protocol on /api/controller")
	}
	// Wire the rate-limit poller so saving/replacing an Anthropic token pokes it for
	// an immediate poll (PRD #53 D3b). Only when the poller is enabled — a nil
	// *usagepoller.Engine must never be handed to the handler.
	if usageEngine != nil {
		h.SetUsagePoker(usageEngine)
	}
	// Wire the OIDC relying party when configured (PRD #45). Discovery is warmed once
	// here so a misconfigured or unreachable IdP is surfaced loudly at boot; a failure
	// leaves the provider configured-but-degraded (login attempts retry discovery)
	// rather than crash-looping the API (Decision 8).
	if cfg.OIDCEnabled() {
		oidcProvider := oidc.New(oidc.Config{
			IssuerURL:    cfg.OIDCIssuerURL,
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			RedirectURL:  cfg.OIDCRedirectURL,
			Scopes:       cfg.OIDCScopes,
			HTTPTimeout:  cfg.OIDCHTTPTimeout,
			GroupsClaim:  cfg.OIDCGroupsClaim,
		})
		warmCtx, cancelWarm := context.WithTimeout(ctx, cfg.OIDCHTTPTimeout)
		if err := oidcProvider.Discover(warmCtx); err != nil {
			slog.Error("oidc discovery failed at boot; SSO is configured but degraded (login attempts will retry discovery)",
				"issuer", cfg.OIDCIssuerURL, "error", err)
		} else {
			slog.Info("oidc provider discovered", "issuer", cfg.OIDCIssuerURL, "provider_name", cfg.OIDCProviderName)
		}
		cancelWarm()
		h.SetOIDC(oidcProvider)
	}

	// One router, shared by both listeners: the TLS listener (when configured) is
	// the SAME api on a second port, not a second surface. Building Routes twice
	// would be two independent middleware chains — and two rate limiters, so a
	// per-IP budget would silently double.
	routes := h.Routes(authLimiter, forgeLimiter, slackDMLimiter, chatLimiter, proposalLimiter, judgeLimiter, hostedLimiter, cliPollLimiter, boardOrderLimiter)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           routes,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Optional TLS listener (PRD #58 Decision 4): the hop the hosted workers and
	// the controller dial across a shared cluster's pod network, carrying claim
	// responses that hold a decrypted forge PAT and Anthropic token. Unconfigured
	// (the compose default) nothing below happens and the api is exactly what it
	// was. The plain listener is NOT replaced when this is on: web's nginx and the
	// kubelet probes speak to it, and the NetworkPolicy is what keeps anything else
	// off it.
	var tlsSrv *http.Server
	if cfg.TLSEnabled() {
		reloader, err := tlsx.NewReloader(cfg.TLSCertFile, cfg.TLSKeyFile, slog.Default())
		if err != nil {
			return err
		}
		tlsSrv = &http.Server{
			Addr: cfg.TLSAddr,
			// NOT `routes`. Two independent layers, and both are wanted — this codebase's
			// guardrails are layered on purpose and no layer may be weakened on the theory
			// another covers it (PRD #58 M3).
			//
			// (a) A SUBSET router: only /api/worker/* and /api/controller/* — the exact set
			//     the agent and the controller dial, derived from their code rather than
			//     assumed (the agent's only base is WORKER_API_PREFIX = "/api/worker"; it
			//     opens no websocket). So /api/auth/* and /api/admin/* are not reachable
			//     from a hosted worker at all, rather than reachable-but-rate-limited.
			// (b) stripXFF: the header cannot be forged because it is gone.
			//
			// It shares the limiter INSTANCES with the plain listener (same pointers), so
			// this is not a second middleware chain and no per-IP budget is doubled.
			Handler:           stripXFF(h.WorkerRoutes(proposalLimiter)),
			TLSConfig:         reloader.ServerConfig(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}

	// Buffered for both listeners: an unbuffered channel would leak whichever
	// goroutine lost the race to report its error.
	errCh := make(chan error, 2)
	go func() {
		slog.Info("api listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if tlsSrv != nil {
		go func() {
			slog.Info("api listening (tls)", "addr", cfg.TLSAddr, "cert_file", cfg.TLSCertFile)
			// The pair is already loaded and served by the reloader's GetCertificate;
			// the empty arguments are how net/http says "use TLSConfig".
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	} else {
		slog.Info("api tls listener disabled (API_TLS_CERT/API_TLS_KEY unset)")
	}

	var runErr error
	select {
	case runErr = <-errCh:
	case <-ctx.Done():
		slog.Info("shutting down")
	}

	// Drain the HTTP server(s), then stop and await the background goroutines before
	// returning so the deferred pool.Close() cannot race an in-flight query. Both
	// listeners share one router and one pool, so both must drain before that.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// CONCURRENTLY, against the shared deadline — not one after the other. Draining
	// them in sequence lets the plain listener consume the whole budget (WriteTimeout
	// is 15s, LONGER than the 10s here), leaving tlsSrv.Shutdown an already-expired
	// context: it would close the listener and return WITHOUT draining in-flight TLS
	// requests, and the deferred pool.Close() would then race an in-flight worker
	// claim. Each server gets the full 10s this way, which is what the budget meant.
	var drainWG sync.WaitGroup
	var drainMu sync.Mutex
	drain := func(s *http.Server) {
		defer drainWG.Done()
		if err := s.Shutdown(shutdownCtx); err != nil {
			drainMu.Lock()
			if runErr == nil {
				runErr = err
			}
			drainMu.Unlock()
		}
	}
	drainWG.Add(1)
	go drain(srv)
	if tlsSrv != nil {
		drainWG.Add(1)
		go drain(tlsSrv)
	}
	drainWG.Wait()
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
	// PRD #41: translate the revision-cap sentinel so the Slack replier can show
	// the "revision limit reached" ephemeral (mirrors ErrSelectionRejected below).
	if errors.Is(err, workersvc.ErrReviseCapReached) {
		return slacksvc.ErrReviseCapReached
	}
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

// SubmitAnswer adapts the Slack clarification reply (PRD #88 M3): the replier passes
// the question id it derived from the thread plus the reply text, and the JSON body is
// built HERE from workersvc.AnswerBody — the same type the web and CLI paths marshal.
//
// That is the point of routing it through main rather than letting slacksvc marshal
// its own struct: the wire shape is declared exactly once, so Slack cannot drift into
// a second contract for the same input kind. It is the same trade SubmitApproval makes
// above, for the same reason — keeping the translation here keeps slacksvc free of a
// workersvc import.
//
// The two sentinels are translated for the same reason ErrInvalidSelection is: the run
// can leave the question between the replier reading its status and this call landing,
// and a silent drop would leave the user with neither a ✅ nor an explanation.
func (g gateSubmitter) SubmitAnswer(ctx context.Context, userID, runID uuid.UUID, questionID, text string) error {
	body, err := answerInputBody(questionID, text)
	if err != nil {
		return err
	}
	_, err = g.svc.SubmitInput(ctx, userID, runID, "answer", string(body), nil)
	switch {
	case errors.Is(err, workersvc.ErrStaleAnswer):
		return slacksvc.ErrAnswerStale
	case errors.Is(err, workersvc.ErrRunNotAwaitingInput):
		return slacksvc.ErrNotAwaitingInput
	}
	return err
}

// answerInputBody encodes a Slack reply as the `answer` steering-input body. Split out
// of SubmitAnswer so the two claims it makes are testable without a run service:
//
//   - the reply lands as EXACTLY ONE answer, at index 0. The worker aligns answers with
//     the question payload's `questions` array, so a card carrying several questions
//     shows the rest as unanswered to the lead. That is deliberate — repeating one
//     message's prose under every question would assert it answers each of them, and a
//     lead that re-asks is a visible failure where a wrong assumption is a silent one.
//   - the question id passes through UNMODIFIED. It is compared for equality against
//     runs.open_question_id and never parsed for meaning, so anything that trimmed,
//     normalised or defaulted it here would break the identity guard silently.
func answerInputBody(questionID, text string) ([]byte, error) {
	return json.Marshal(workersvc.AnswerBody{QuestionID: questionID, Answers: []string{text}})
}

// LiveChatForUser adapts the Slack chat opener's Decision 3 refusal to the run
// service (PRD #191 M2): the newest non-terminal chat run for the user, if any.
func (g gateSubmitter) LiveChatForUser(ctx context.Context, userID uuid.UUID) (store.Run, bool, error) {
	return g.svc.LiveChatForUser(ctx, userID)
}

// CreateChatRun adapts the Slack chat opener to the run service (PRD #191 M2): it
// queues a kind='chat' run seeded with the opening message. The Slack path has
// already drawn from the shared chat spend budget before calling this.
func (g gateSubmitter) CreateChatRun(ctx context.Context, userID uuid.UUID, message string) (store.Run, error) {
	return g.svc.CreateChatRun(ctx, userID, message)
}

// HasOnlineWorker adapts the opener's no-worker check (PRD #191 M6).
func (g gateSubmitter) HasOnlineWorker(ctx context.Context, userID uuid.UUID) (bool, error) {
	return g.svc.HasOnlineWorker(ctx, userID)
}

// EndChat adapts the Slack End-chat button (PRD #191 M6): cancel the live chat, with
// the terminal/not-found sentinels translated for the ChatActions handler.
func (g gateSubmitter) EndChat(ctx context.Context, userID, runID uuid.UUID) error {
	_, err := g.svc.EndChat(ctx, userID, runID)
	switch {
	case errors.Is(err, workersvc.ErrRunTerminal):
		return slacksvc.ErrChatEnded
	case errors.Is(err, workersvc.ErrRunNotFound):
		return slacksvc.ErrChatGone
	}
	return err
}

// ContinueChat adapts the Slack Continue button (PRD #191 M6): mint a fresh chat
// resuming a terminal one, returning the new run's id, with sentinels translated.
func (g gateSubmitter) ContinueChat(ctx context.Context, userID, runID uuid.UUID) (uuid.UUID, error) {
	run, err := g.svc.ContinueChat(ctx, userID, runID)
	switch {
	case errors.Is(err, workersvc.ErrChatNotEnded):
		return uuid.Nil, slacksvc.ErrChatNotEndedYet
	case errors.Is(err, workersvc.ErrRunNotFound):
		return uuid.Nil, slacksvc.ErrChatGone
	case err != nil:
		return uuid.Nil, err
	}
	return run.ID, nil
}

// SubmitChatMessage adapts a Slack thread reply on a chat run to the run service
// (PRD #191 Decision 5): it rides SubmitChatMessage (turn cap + terminal 409 enforced
// at the boundary), drops the result the replier does not use, and translates the two
// user-facing sentinels into the slacksvc ones so the replier can say which happened —
// the same translate-in-main pattern SubmitInput/SubmitApproval/SubmitAnswer use to
// keep slacksvc free of a workersvc import.
func (g gateSubmitter) SubmitChatMessage(ctx context.Context, userID, runID uuid.UUID, message string) error {
	_, err := g.svc.SubmitChatMessage(ctx, userID, runID, message)
	switch {
	case errors.Is(err, workersvc.ErrChatTurnCapReached):
		return slacksvc.ErrChatTurnCapReached
	case errors.Is(err, workersvc.ErrRunTerminal):
		return slacksvc.ErrChatEnded
	}
	return err
}

// ConfirmProposalForUser adapts the Slack proposal card's Create to the run service
// (PRD #191 M4): the lifted claim-first forge write (M1). It converts the workersvc
// CreatedIssue to the slacksvc one and translates the proposal sentinels so ChatActions
// can tell already-handled (edit the card) from not-yours (ephemeral) from a forge
// failure (retry), all without a workersvc import on the slacksvc side.
func (g gateSubmitter) ConfirmProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) (slacksvc.CreatedIssue, error) {
	ci, err := g.svc.ConfirmProposalForUser(ctx, userID, runID, propID)
	switch {
	case errors.Is(err, workersvc.ErrProposalNotPending):
		return slacksvc.CreatedIssue{}, slacksvc.ErrChatProposalHandled
	case errors.Is(err, workersvc.ErrProposalNotFound):
		return slacksvc.CreatedIssue{}, slacksvc.ErrChatProposalGone
	case errors.Is(err, workersvc.ErrProposalRepoGone),
		errors.Is(err, workersvc.ErrForgeBuild),
		errors.Is(err, workersvc.ErrForgeIssueWrite),
		errors.Is(err, workersvc.ErrForgesUnavailable):
		return slacksvc.CreatedIssue{}, slacksvc.ErrChatProposalForge
	case err != nil:
		return slacksvc.CreatedIssue{}, err
	}
	return slacksvc.CreatedIssue{IID: ci.IID, WebURL: ci.WebURL, Title: ci.Title}, nil
}

// DismissProposalForUser adapts the Slack proposal card's Dismiss (PRD #191 M4): the
// ownership-checked, forge-free dismiss, with the two lookup sentinels translated.
func (g gateSubmitter) DismissProposalForUser(ctx context.Context, userID, runID, propID uuid.UUID) error {
	err := g.svc.DismissProposalForUser(ctx, userID, runID, propID)
	switch {
	case errors.Is(err, workersvc.ErrProposalNotPending):
		return slacksvc.ErrChatProposalHandled
	case errors.Is(err, workersvc.ErrProposalNotFound):
		return slacksvc.ErrChatProposalGone
	}
	return err
}

// StartRunFromCard adapts the Slack start-run card's Start (PRD #191 M5): the lifted,
// path-keyed, PRD-gated StartRunForUserByPath. On refusal it returns an error whose
// MESSAGE is user-safe (built from the gate sentinels below, mirroring the web start
// button's intent) and logs the raw cause, so slacksvc surfaces a helpful line without
// importing workersvc.
func (g gateSubmitter) StartRunFromCard(ctx context.Context, userID uuid.UUID, repoPath string, issueIID int64) (uuid.UUID, error) {
	run, err := g.svc.StartRunForUserByPath(ctx, userID, repoPath, issueIID, nil, nil)
	if err == nil {
		return run.ID, nil
	}
	if isInternalStartRunErr(err) {
		slog.Error("slack start-run card", "repo", repoPath, "issue_iid", issueIID, "error", err)
	}
	return uuid.Nil, errors.New(startRunCardMessage(err))
}

// isInternalStartRunErr reports whether a StartRun error is an unexpected/internal
// failure worth logging (vs a user-actionable gate refusal).
func isInternalStartRunErr(err error) bool {
	switch {
	case errors.Is(err, workersvc.ErrRepoNotFound), errors.Is(err, workersvc.ErrIssueNotFound),
		errors.Is(err, workersvc.ErrNotPRDIssue), errors.Is(err, workersvc.ErrNoPRDLink),
		errors.Is(err, workersvc.ErrActiveRunExists), errors.Is(err, workersvc.ErrBranchInUse),
		errors.Is(err, workersvc.ErrDescriptionTooLarge), errors.Is(err, workersvc.ErrForgeIssueRead):
		return false
	default:
		return true
	}
}

// startRunCardMessage maps a StartRun error to a user-safe Slack message, mirroring the
// intent of the web start button's per-sentinel copy (the web chat card gets the
// byte-identical HTTP copy; this is the DM paraphrase).
func startRunCardMessage(err error) string {
	switch {
	case errors.Is(err, workersvc.ErrRepoNotFound):
		return "That repo isn't yours, or it no longer exists."
	case errors.Is(err, workersvc.ErrIssueNotFound):
		return "That issue isn't on this repo's board."
	case errors.Is(err, workersvc.ErrNotPRDIssue):
		return "This issue isn't marked as uzi's work — promote it (add the PRD label) in uzi first."
	case errors.Is(err, workersvc.ErrNoPRDLink):
		return "This issue has no PRD link — add a prds/*.md link (or the PRD-less label) before starting a run."
	case errors.Is(err, workersvc.ErrActiveRunExists):
		return "A run is already in progress for this issue."
	case errors.Is(err, workersvc.ErrBranchInUse):
		return "A CI-fix run is already working this issue's branch — cancel it first."
	case errors.Is(err, workersvc.ErrDescriptionTooLarge):
		return "That issue's description is too large to run."
	case errors.Is(err, workersvc.ErrForgeIssueRead):
		return "Couldn't read that issue from the forge — check the issue number."
	default:
		return "Couldn't start the run right now — try from the Chat page in uzi."
	}
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
		PasswordHash: pgtype.Text{String: hash, Valid: true},
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

// stripXFF removes the forwarding headers from every request on the TLS listener
// (PRD #58 M3). Layer (b) of two — layer (a) is the subset router in
// handler.WorkerRoutes.
//
// WHY THIS EXISTS, so nobody simplifies it away:
//
// mw.ClientIP trusts X-Forwarded-For based on the PEER IP alone — it knows nothing
// about which listener or route the request arrived on. In a cluster deployment
// TRUSTED_PROXIES is set to the whole pod CIDR, because pod IPs are dynamic and no
// narrower value is maintainable. Hosted worker pods get IPs inside it, so they are
// trusted proxies BY CONSTRUCTION. Until PRD #58 M3 nothing could
// exploit that, because the api NetworkPolicy admitted web pods and nothing else —
// and M3's own Decision 5(a) rule, which admits the worker namespace to this port,
// is exactly what removes that mitigation. Without this, a compromised worker (the
// agent runs a model against a user's cloned repo — squarely in the threat model,
// it is why agent/src/guardrails.ts exists) could POST /api/auth/login with a
// rotating XFF and defeat the per-IP auth rate limit outright. Layer (a) already
// takes /api/auth/* off this listener; this makes the property hold for every route
// on it, now and later.
//
// The blast radius is the RATE LIMITER ONLY, and stating it precisely matters more
// than stating it dramatically: ClientIP has exactly three call sites, all in
// ratelimit.go, and no migration defines an IP column — uzi persists no client IP
// anywhere, so there is no audit attribution to forge. A bypassed brute-force
// control on the admin login is the whole of it, and is reason enough.
//
// Narrowing TRUSTED_PROXIES is NOT the fix and was rejected: pod IPs are dynamic,
// which is why the whole-CIDR value exists in the first place. This makes
// docs/configuration.md's claim ("no X-Forwarded-For is trusted from workers") true
// BY CONSTRUCTION rather than by CIDR bookkeeping that a future pod-CIDR change
// would silently invalidate.
//
// THE ONE CONDITION THAT WOULD CHANGE THIS: the TLS listener exists for clients
// that dial the api DIRECTLY — hosted workers and the controller — so they are
// never proxied and an XFF on this hop can only be forgery. If anyone ever fronts
// this port with a real reverse proxy, this is the line they must revisit; deleting
// it silently re-opens the bypass above.
//
// Only X-Forwarded-For is read today (mw.ClientIP); X-Real-IP and Forwarded are
// dropped too so a future middleware that reads either cannot inherit the hole.
func stripXFF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("X-Forwarded-For")
		r.Header.Del("X-Real-IP")
		r.Header.Del("Forwarded")
		next.ServeHTTP(w, r)
	})
}
