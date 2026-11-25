package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// usage_reserve_livedb_test.go pins the weekly usage reserve's claim admission (PRD #3522 M3)
// against a real Postgres: ClaimRun's SQL gate against the Go decision core (reserveHeld), then the
// assembled claim end to end (svc.Claim). Skipped unless UZI_TEST_DATABASE_URL is set; run via
// ./e2e/run-store-it.sh.

// reserveFix is one owner with one worker, a repo and a threshold.
type reserveFix struct {
	t        *testing.T
	env      codexTestEnv
	svc      *Service
	userID   uuid.UUID
	repoID   uuid.UUID
	workerID uuid.UUID
	targetID uuid.UUID // a completed issue run: the judge runs' target
	// lastIID numbers the issues this fixture's runs claim: each seeded run gets a distinct
	// issue_iid, so two non-terminal runs never trip uq_runs_one_active_per_issue.
	lastIID int64
	// newRunID mints seeded run ids; uuid.New unless a test pins colliding ids.
	newRunID func() uuid.UUID
	// prov is the owner's personal Claude provider and sealed gateway key, seeded on first use by a
	// provider-bound run (PRD #4322 M1a: such a run spends its own key and is outside the reserve).
	prov *reserveProvider
}

// reserveProvider is the provider binding a provider-bound run freezes.
type reserveProvider struct {
	provider, key uuid.UUID
	keyRev        int64
	plain         string
}

// nextIssueIID returns the next issue number this fixture has not used.
func (fx *reserveFix) nextIssueIID() int64 {
	fx.lastIID++
	return 3000 + fx.lastIID
}

func newReserveFix(t *testing.T, threshold int16, pause bool) *reserveFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	env.sealBotPAT(t, userID)
	t.Cleanup(func() { _, _ = env.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
	p := autoParams()
	p.UsagePollInterval = 5 * time.Minute
	p.CodexUsagePollInterval = 5 * time.Minute
	svc := New(env.q, env.box, p)
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	fx := &reserveFix{t: t, env: env, svc: svc, userID: userID, repoID: repoID, workerID: workerID, targetID: uuid.New(), newRunID: uuid.New}
	env.exec(`UPDATE workers SET last_heartbeat_at = now(), protocol_capabilities = $2 WHERE id = $1`,
		workerID, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3,
			capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1})
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	          VALUES ($1, $2, $3, 'issue', 3522, 't', 'd', 'completed')`, fx.targetID, userID, repoID)
	fx.setReserve(threshold, pause)
	return fx
}

// setReserve writes the owner's settings; threshold 0 clears it (NULL).
func (fx *reserveFix) setReserve(threshold int16, pause bool) {
	fx.t.Helper()
	var pct pgtype.Int2
	if threshold > 0 {
		pct = pgtype.Int2{Int16: threshold, Valid: true}
	}
	if _, err := fx.env.q.SetUserUsageReserve(fx.env.ctx, store.SetUserUsageReserveParams{
		ID: fx.userID, WeeklyReservePct: pct, WeeklyReservePauseRunning: pause,
	}); err != nil {
		fx.t.Fatalf("SetUserUsageReserve: %v", err)
	}
}

// token seeds an Anthropic token. pct < 0 leaves it without a reading; the 5-hour window is 10%.
func (fx *reserveFix) token(label string, isDefault, pooled bool, pct int) uuid.UUID {
	fx.t.Helper()
	id := fx.env.seedAnthropicSecret(fx.t, fx.userID, label, isDefault)
	if pooled {
		fx.env.exec(`UPDATE user_secrets SET auto_eligible = true WHERE id = $1`, id)
	}
	if pct >= 0 {
		fx.reading(id, pct, 10, "1 minute")
	}
	return id
}

// reading (re)writes a token's gauge: 7-day pct, 5-hour pct, synced `ago` ago.
func (fx *reserveFix) reading(id uuid.UUID, weekly, fiveHour int, ago string) {
	fx.t.Helper()
	fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
	                 seven_day_pct, seven_day_resets_at, source, synced_at)
	             VALUES ($1, $2, $4, now() + interval '1 hour', $3, now() + interval '48 hours', 'usage_endpoint',
	                 now() - $5::interval)
	             ON CONFLICT (user_secret_id) DO UPDATE SET five_hour_pct = EXCLUDED.five_hour_pct,
	                 seven_day_pct = EXCLUDED.seven_day_pct, synced_at = EXCLUDED.synced_at,
	                 seven_day_resets_at = EXCLUDED.seven_day_resets_at`,
		id, fx.userID, weekly, fiveHour, ago)
}

func weeklyBuckets(primary, secondary string) string {
	return fmt.Sprintf(`[{"id":"codex","allowed":true,"limit_reached":false,"primary":%s,"secondary":%s}]`, primary, secondary)
}

func codexWindow(secs int64, used float64) string {
	return fmt.Sprintf(`{"used_percent":%v,"limit_window_seconds":%d,"reset_after_seconds":100000,"reset_at":null}`, used, secs)
}

const (
	fiveHourSecs = 18000
	weekSecs     = 604800
)

// subAlias seeds a linked subscription alias; buckets "" leaves the account without a reading.
func (fx *reserveFix) subAlias(buckets string) uuid.UUID {
	fx.t.Helper()
	id := fx.env.seedLinkedSubscription(fx.t, fx.userID, "codex-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
	if buckets != "" {
		fx.codexReading(id, buckets, "1 minute")
	}
	return id
}

func (fx *reserveFix) codexReading(alias uuid.UUID, buckets, ago string) {
	fx.t.Helper()
	st, err := fx.env.q.GetCodexCredentialState(fx.env.ctx, store.GetCodexCredentialStateParams{UserSecretID: alias, UserID: fx.userID})
	if err != nil || !st.ProviderAccountID.Valid {
		fx.t.Fatalf("alias %s has no linked account: %v", alias, err)
	}
	fx.env.exec(`INSERT INTO codex_account_rate_limits (user_id, provider_account_id, buckets, last_success_at, last_attempt_at, attempt_status)
	             VALUES ($1, $2, $3::jsonb, now() - $4::interval, now() - $4::interval, 'ok')
	             ON CONFLICT (user_id, provider_account_id) DO UPDATE SET buckets = EXCLUDED.buckets,
	                 last_success_at = EXCLUDED.last_success_at, last_attempt_at = EXCLUDED.last_attempt_at`,
		fx.userID, uuid.UUID(st.ProviderAccountID.Bytes), buckets, ago)
}

// bindWorker sets the worker's Anthropic lane.
func (fx *reserveFix) bindWorker(mode string, secret *uuid.UUID) {
	fx.t.Helper()
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = $2, anthropic_secret_id = $3 WHERE id = $1`, fx.workerID, mode, secret)
}

func (fx *reserveFix) bindJudge(mode string, secret *uuid.UUID) {
	fx.t.Helper()
	fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = $2, judge_anthropic_secret_id = $3 WHERE id = $1`, fx.userID, mode, secret)
}

// reserveRun is one queued run's shape.
type reserveRun struct {
	kind         string // "" = issue
	codexAlias   *uuid.UUID
	override     string
	overrideTok  *uuid.UUID
	firstStarted bool
	dead         *uuid.UUID
	deadFuture   bool // retry_not_before ahead (the dead credential is still excluded)
	exemptFor    time.Duration
	workerPin    *uuid.UUID
	provider     bool // frozen to the owner's personal provider binding (no vendor credential)
}

// providerBinding seeds the owner's personal provider with a sealed key once (revision 1 at the
// allowlisted gateway) and returns it. The cleanup clears the Claude source before the owner delete
// (users_claude_provider_fkey is RESTRICT); it is registered after newReserveFix's own cleanup, so it
// runs first.
func (fx *reserveFix) providerBinding() *reserveProvider {
	fx.t.Helper()
	if fx.prov != nil {
		return fx.prov
	}
	plain := "gw-key-" + uuid.NewString()
	provider, key := seedPersonalSealed(fx.t, fx.env, fx.userID, plain)
	fx.t.Cleanup(func() {
		if _, err := fx.env.pool.Exec(context.Background(), `UPDATE users SET claude_provider_id = NULL, claude_provider_scope_id = NULL WHERE id = $1`, fx.userID); err != nil {
			fx.t.Errorf("clear the Claude source in cleanup: %v", err)
		}
	})
	fx.svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	var rev int64
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT enablement_rev FROM user_secrets WHERE id = $1`, key).Scan(&rev); err != nil {
		fx.t.Fatalf("read key rev: %v", err)
	}
	fx.prov = &reserveProvider{provider: provider, key: key, keyRev: rev, plain: plain}
	return fx.prov
}

// freezeProvider stamps the five binding columns of a run the fixture seeded without a vendor
// credential, exactly as the create-time freeze does.
func (fx *reserveFix) freezeProvider(id uuid.UUID) {
	fx.t.Helper()
	b := fx.providerBinding()
	fx.env.exec(`UPDATE runs SET harness = 'claude', model_provider_id = $2, model_provider_revision = 1, model_provider_key_mode = $3,
	             model_provider_key_secret_id = $4, model_provider_key_rev = $5 WHERE id = $1`,
		id, b.provider, providerKeyModeUser, b.key, b.keyRev)
}

func (fx *reserveFix) queueRun(o reserveRun) uuid.UUID {
	fx.t.Helper()
	id := fx.newRunID()
	iid := fx.nextIssueIID()
	switch o.kind {
	case runkind.Judge:
		fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, target_run_id)
		             VALUES ($1, $2, 'judge', 't', 'd', 'queued', $3)`, id, fx.userID, fx.targetID)
	case runkind.SelfImprove:
		fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		             VALUES ($1, $2, $3, 'self_improve', $4, 't', 'd', 'queued')`, id, fx.userID, fx.repoID, iid)
	default:
		fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		             VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued')`, id, fx.userID, fx.repoID, iid)
	}
	if o.codexAlias != nil {
		if err := fx.svc.FreezeCodexBinding(fx.env.ctx, fx.userID, id, *o.codexAlias, codexAuthModeSubscription); err != nil {
			fx.t.Fatalf("FreezeCodexBinding: %v", err)
		}
	}
	if o.override != "" {
		fx.env.exec(`UPDATE runs SET credential_override_mode = $2, credential_override_secret_id = $3 WHERE id = $1`, id, o.override, o.overrideTok)
	}
	if o.firstStarted {
		fx.env.exec(`UPDATE runs SET first_started_at = now() - interval '2 hours' WHERE id = $1`, id)
	}
	if o.dead != nil {
		retry := "now() - interval '1 minute'"
		if o.deadFuture {
			retry = "now() + interval '1 hour'"
		}
		fx.env.exec(`UPDATE runs SET limit_dead_secret_id = $2, retry_not_before = `+retry+` WHERE id = $1`, id, o.dead)
	}
	if o.exemptFor != 0 {
		fx.env.exec(`UPDATE runs SET reserve_exempt_until = now() + make_interval(secs => $2) WHERE id = $1`, id, o.exemptFor.Seconds())
	}
	if o.workerPin != nil {
		fx.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, id, o.workerPin)
	}
	if o.provider {
		fx.freezeProvider(id)
	}
	return id
}

func (fx *reserveFix) dropRuns() {
	fx.t.Helper()
	fx.env.exec(`DELETE FROM runs WHERE user_id = $1 AND status = 'queued'`, fx.userID)
}

// claimParams is ClaimRun's parameter set exactly as claimLane derives it for the fixture worker.
func (fx *reserveFix) claimParams(workerID uuid.UUID) store.ClaimRunParams {
	fx.t.Helper()
	w := wkrRow(fx.t, fx.env, workerID)
	now := time.Now()
	snap, err := fx.svc.reserveSnapshot(fx.env.ctx, fx.userID, now)
	if err != nil {
		fx.t.Fatalf("reserveSnapshot: %v", err)
	}
	peers, err := fx.svc.reserveBlockedWorkers(fx.env.ctx, snap)
	if err != nil {
		fx.t.Fatalf("reserveBlockedWorkers: %v", err)
	}
	p := claimRunParamsFor(w)
	reserveClaimParams(&p, snap, w, peers, now)
	return p
}

// admits reports whether ClaimRun, with the reserve parameters claimLane derives, claims a run for
// the fixture worker. The claimed run is deleted so the next probe starts clean.
func (fx *reserveFix) admits(runID uuid.UUID) bool {
	fx.t.Helper()
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		fx.t.Fatalf("ClaimRun: %v", err)
	}
	if run.ID != runID {
		fx.t.Fatalf("ClaimRun claimed %s, want %s", run.ID, runID)
	}
	fx.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = 0 WHERE id = $1`, runID)
	return true
}

// goHeld is the Go decision core's verdict for the run on the fixture worker's lane.
func (fx *reserveFix) goHeld(runID uuid.UUID) reserveDecision {
	fx.t.Helper()
	now := time.Now()
	snap, err := fx.svc.reserveSnapshot(fx.env.ctx, fx.userID, now)
	if err != nil {
		fx.t.Fatalf("reserveSnapshot: %v", err)
	}
	return reserveHeld(mustRun(fx.t, fx.env, runID), snap, laneOfWorker(wkrRow(fx.t, fx.env, fx.workerID)), now)
}

// TestReserveClaimGateParityMatrixLiveDB: for a matrix of run shapes, ClaimRun admits the run iff
// the Go core (reserveHeld) does not hold it, and both match the expected verdict written down here
// independently of either implementation. This is the SQL twin of TestReserveHeldResolution.
//
// MUTATION: dropping the codex arm, the judge arm, the override arms or the exemption arms from
// ClaimRun's reserve clause each reddens at least one row below.
func TestReserveClaimGateParityMatrixLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	defTok := fx.token("default", true, false, 72) // owner default, held
	poolOK := fx.token("pool-ok", false, true, 10) // pooled, below
	poolHi := fx.token("pool-hi", false, true, 90) // pooled, held
	pinOK := fx.token("pin-ok", false, false, 5)   // non-pooled, below
	pinHi := fx.token("pin-hi", false, false, 80)  // non-pooled, held
	noRead := fx.token("no-reading", false, false, -1)
	aliasHi := fx.subAlias(weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 90)))  // weekly = secondary
	aliasLo := fx.subAlias(weeklyBuckets(codexWindow(weekSecs, 10), codexWindow(fiveHourSecs, 99))) // weekly = PRIMARY, 5h hot
	aliasNone := fx.subAlias("")                                                                    // no reading: unknown
	aliasKey := fx.env.seedStaticAPIKey(t, fx.userID, "key", "fixture-"+uuid.NewString())           // structurally unmetered
	aliasStale := fx.subAlias("")
	fx.codexReading(aliasStale, weeklyBuckets(codexWindow(weekSecs, 5), codexWindow(fiveHourSecs, 5)), "2 hours")

	type tc struct {
		name  string
		setup func()
		run   reserveRun
		want  bool // held
	}
	worker := func(mode string, id *uuid.UUID) func() { return func() { fx.bindWorker(mode, id) } }
	judge := func(mode string, id *uuid.UUID) func() {
		return func() { fx.bindWorker(BindModeDefault, nil); fx.bindJudge(mode, id) }
	}
	cases := []tc{
		// worker lane
		{"worker default, blocked default", worker(BindModeDefault, nil), reserveRun{}, true},
		{"worker pinned ok", worker(BindModePinned, &pinOK), reserveRun{}, false},
		{"worker pinned held", worker(BindModePinned, &pinHi), reserveRun{}, true},
		{"worker pinned NULL id resolves to the blocked default", worker(BindModePinned, nil), reserveRun{}, true},
		{"worker pinned to a token with no reading", worker(BindModePinned, &noRead), reserveRun{}, true},
		{"worker auto, a permitted pooled token", worker(BindModeAuto, nil), reserveRun{}, false},
		{"worker auto, permitted token excluded as dead (active)", worker(BindModeAuto, nil), reserveRun{dead: &poolOK, deadFuture: true}, true},
		{"worker auto, dead credential window reopened", worker(BindModeAuto, nil), reserveRun{dead: &poolOK}, false},
		{"worker auto, blocked token excluded as dead", worker(BindModeAuto, nil), reserveRun{dead: &poolHi, deadFuture: true}, false},
		// per-run override
		{"override pinned ok beats a blocked default worker", worker(BindModeDefault, nil), reserveRun{override: BindModePinned, overrideTok: &pinOK}, false},
		{"override pinned held beats an ok worker", worker(BindModePinned, &pinOK), reserveRun{override: BindModePinned, overrideTok: &pinHi}, true},
		{"override pinned with a nulled id falls to the worker (blocked)", worker(BindModeDefault, nil), reserveRun{override: BindModePinned}, true},
		{"override pinned with a nulled id falls to the worker (ok)", worker(BindModePinned, &pinOK), reserveRun{override: BindModePinned}, false},
		{"override default, blocked default", worker(BindModePinned, &pinOK), reserveRun{override: BindModeDefault}, true},
		{"override auto beats a blocked default worker", worker(BindModeDefault, nil), reserveRun{override: BindModeAuto}, false},
		{"override auto with the permitted token excluded", worker(BindModePinned, &pinOK), reserveRun{override: BindModeAuto, dead: &poolOK, deadFuture: true}, true},
		// judge lane, self_improve follows it
		{"judge default blocked", judge(BindModeDefault, nil), reserveRun{kind: runkind.Judge}, true},
		{"judge pinned ok", judge(BindModePinned, &pinOK), reserveRun{kind: runkind.Judge}, false},
		{"judge pinned held", judge(BindModePinned, &pinHi), reserveRun{kind: runkind.Judge}, true},
		{"judge auto, permitted pooled token", judge(BindModeAuto, nil), reserveRun{kind: runkind.Judge}, false},
		{"self_improve follows the judge binding (ok)", judge(BindModePinned, &pinOK), reserveRun{kind: runkind.SelfImprove}, false},
		{"self_improve follows the judge binding (held)", judge(BindModePinned, &pinHi), reserveRun{kind: runkind.SelfImprove}, true},
		{"self_improve ignores the run override", judge(BindModePinned, &pinHi), reserveRun{kind: runkind.SelfImprove, override: BindModePinned, overrideTok: &pinOK}, true},
		// codex
		{"codex alias at 90% weekly (secondary window)", worker(BindModeDefault, nil), reserveRun{codexAlias: &aliasHi}, true},
		{"codex alias weekly in the PRIMARY slot, 5h hot, is ok", worker(BindModeDefault, nil), reserveRun{codexAlias: &aliasLo}, false},
		{"codex alias without a reading is unknown", worker(BindModeDefault, nil), reserveRun{codexAlias: &aliasNone}, true},
		{"codex alias with a stale reading is unknown", worker(BindModeDefault, nil), reserveRun{codexAlias: &aliasStale}, true},
		// exemptions
		{"started run, toggle on", worker(BindModeDefault, nil), reserveRun{firstStarted: true}, true},
		{"active manual-resume override", worker(BindModeDefault, nil), reserveRun{exemptFor: time.Hour}, false},
		{"expired manual-resume override", worker(BindModeDefault, nil), reserveRun{exemptFor: -time.Hour}, true},
		// These rows mutate the shared tokens; they stay last, in order.
		{"worker auto: a probe-rejected pooled token with a fresh ok reading is not permitted, the rest are blocked",
			func() {
				worker(BindModeAuto, nil)()
				fx.env.exec(`UPDATE user_secrets SET anthropic_rejected_at = now() WHERE id = $1`, poolOK)
			}, reserveRun{}, true},
		{"judge auto, every pooled token blocked (default ok): held, no default fallback",
			func() {
				judge(BindModeAuto, nil)()
				fx.env.exec(`UPDATE user_secrets SET anthropic_rejected_at = NULL WHERE id = $1`, poolOK)
				fx.reading(poolOK, 90, 10, "1 minute")
				fx.reading(defTok, 10, 10, "1 minute")
			}, reserveRun{kind: runkind.Judge}, true},
		{"judge auto, empty pool falls back to a blocked owner default: held",
			func() {
				judge(BindModeAuto, nil)()
				fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE user_id = $1`, fx.userID)
				fx.reading(defTok, 72, 10, "1 minute")
			}, reserveRun{kind: runkind.Judge}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer fx.dropRuns()
			fx.bindWorker(BindModeDefault, nil)
			fx.bindJudge(BindModeDefault, nil)
			fx.setReserve(70, true)
			c.setup()
			id := fx.queueRun(c.run)
			if got := fx.goHeld(id).held; got != c.want {
				t.Fatalf("Go core held = %v, want %v", got, c.want)
			}
			if got := !fx.admits(id); got != c.want {
				t.Fatalf("ClaimRun held = %v, want %v (admitted=%v)", got, c.want, !got)
			}
		})
	}

	// The api-key alias is structurally unmetered, and the other exemptions that need a different
	// owner setting, in their own subtests.
	t.Run("codex api-key alias is never gated", func(t *testing.T) {
		defer fx.dropRuns()
		fx.bindWorker(BindModeDefault, nil)
		// FreezeCodexBinding takes the auth mode: a static alias is frozen as api_key.
		id := uuid.New()
		fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
		             VALUES ($1, $2, $3, 'issue', 3599, 't', 'd', 'queued')`, id, fx.userID, fx.repoID)
		if err := fx.svc.FreezeCodexBinding(fx.env.ctx, fx.userID, id, aliasKey, codexAuthModeAPIKey); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		if fx.goHeld(id).held || !fx.admits(id) {
			t.Fatal("an API-key alias must never be held, whatever the Anthropic readings say")
		}
	})
	t.Run("started run with the pause toggle off is not gated", func(t *testing.T) {
		defer fx.dropRuns()
		fx.bindWorker(BindModeDefault, nil)
		fx.setReserve(70, false)
		started := fx.queueRun(reserveRun{firstStarted: true})
		if fx.goHeld(started).held || !fx.admits(started) {
			t.Fatal("a previously started run must continue above the threshold when pausing is off")
		}
		// New work stays held with the toggle off.
		fx.dropRuns()
		fresh := fx.queueRun(reserveRun{})
		if !fx.goHeld(fresh).held || fx.admits(fresh) {
			t.Fatal("a never-started run is gated whatever the toggle says")
		}
	})
	t.Run("threshold cleared releases everything", func(t *testing.T) {
		defer fx.dropRuns()
		fx.bindWorker(BindModeDefault, nil)
		fx.setReserve(0, true)
		id := fx.queueRun(reserveRun{})
		if fx.goHeld(id).held || !fx.admits(id) {
			t.Fatal("no threshold must mean no gate")
		}
	})
	t.Run("empty auto pool stays a pool_wait case, not a reserve hold", func(t *testing.T) {
		defer fx.dropRuns()
		fx.bindWorker(BindModeAuto, nil)
		fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE user_id = $1`, fx.userID)
		id := fx.queueRun(reserveRun{})
		if fx.goHeld(id).held || !fx.admits(id) {
			t.Fatal("with nothing pooled the reserve must not hold the run: it claims and pool_waits as before")
		}
	})
}

// TestReserveBoundaryAndFiveHourLiveDB: a 7-day reading exactly at the threshold holds, one below
// admits, and the 5-hour window is ignored in both directions.
func TestReserveBoundaryAndFiveHourLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 70)
	fx.bindWorker(BindModeDefault, nil)
	probe := func(weekly, fiveHour int) bool {
		t.Helper()
		defer fx.dropRuns()
		fx.reading(tok, weekly, fiveHour, "1 minute")
		id := fx.queueRun(reserveRun{})
		return fx.admits(id)
	}
	if probe(70, 0) {
		t.Fatal("pct == threshold must hold")
	}
	if probe(100, 0) {
		t.Fatal("above the threshold must hold")
	}
	if !probe(69, 0) {
		t.Fatal("one below the threshold must admit")
	}
	if !probe(10, 100) {
		t.Fatal("a 100% 5-hour window must not hold a run whose 7-day window is below the threshold")
	}
	if probe(70, 0) {
		t.Fatal("a 5-hour reading of zero must not release a held 7-day reading")
	}
}

// TestReserveTwoOwnersIndependentLiveDB: two owners with different thresholds over the same
// 80% reading are gated independently.
func TestReserveTwoOwnersIndependentLiveDB(t *testing.T) {
	a := newReserveFix(t, 70, true)
	b := newReserveFix(t, 95, true)
	for _, fx := range []*reserveFix{a, b} {
		fx.token("only", true, false, 80)
		fx.bindWorker(BindModeDefault, nil)
	}
	ida, idb := a.queueRun(reserveRun{}), b.queueRun(reserveRun{})
	if a.admits(ida) {
		t.Fatal("owner A (70%) must be held at 80%")
	}
	if !b.admits(idb) {
		t.Fatal("owner B (95%) must be admitted at 80%")
	}
}

// TestReserveStaleAndMissingReadingsHoldLiveDB: with a threshold set, a missing, stale or
// utilization-less reading holds new work as unknown, and an Anthropic token without any row is
// held rather than assumed unmetered.
func TestReserveStaleAndMissingReadingsHoldLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, -1)
	fx.bindWorker(BindModeDefault, nil)
	check := func(name string, wantAdmit bool) {
		t.Helper()
		defer fx.dropRuns()
		id := fx.queueRun(reserveRun{})
		if got := fx.admits(id); got != wantAdmit {
			t.Fatalf("%s: admitted = %v, want %v", name, got, wantAdmit)
		}
	}
	check("token with no reading at all", false)
	fx.reading(tok, 10, 10, "1 hour")
	check("stale reading (older than 3 poll intervals)", false)
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_pct = NULL, synced_at = now() WHERE user_secret_id = $1`, tok)
	check("successful poll with no weekly utilization", false)
	fx.reading(tok, 10, 10, "1 minute")
	check("fresh reading below the threshold", true)
	// A reset that has already passed with a reading synced before it proves nothing about the new week.
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = now() - interval '30 seconds',
	             synced_at = now() - interval '2 minutes' WHERE user_secret_id = $1`, tok)
	check("reading synced before an observed reset", false)
	// The poller disabled (interval 0) makes every reading stale, whatever it says.
	fx.reading(tok, 10, 10, "1 minute")
	fx.svc.p.UsagePollInterval = 0
	check("poller disabled", false)
}

// ---- assembled claims (svc.Claim) ----------------------------------------------------------------

func (fx *reserveFix) claim() *ClaimPayload {
	fx.t.Helper()
	ctx, cancel := context.WithTimeout(fx.env.ctx, 30*time.Second)
	defer cancel()
	p, err := fx.svc.Claim(ctx, wkrRow(fx.t, fx.env, fx.workerID), nil)
	if err != nil {
		fx.t.Fatalf("Claim: %v", err)
	}
	return p
}

// TestReserveClaimHoldsAndAdmitsLiveDB: the whole claim. A reading at the threshold leaves the run
// queued (claim_generation untouched, no payload); the same run is claimed below it and records the
// credential it spent; a scheduled-style second run behaves the same.
func TestReserveClaimHoldsAndAdmitsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 72)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})

	if p := fx.claim(); p != nil {
		t.Fatal("a run above the threshold was claimed")
	}
	r := mustRun(t, fx.env, id)
	if r.Status != "queued" || r.ClaimGeneration != 0 || r.WorkerID.Valid {
		t.Fatalf("held run: status=%s gen=%d worker=%v, want an untouched queued run", r.Status, r.ClaimGeneration, r.WorkerID)
	}
	fx.reading(tok, 3, 10, "1 minute") // the 7-day reset: the poller reads 3%
	p := fx.claim()
	if p == nil || p.Secrets.AnthropicOAuthToken == "" {
		t.Fatal("the run was not claimed once the reading dropped below the threshold")
	}
	r = mustRun(t, fx.env, id)
	if r.Status != "claimed" || r.AnthropicSecretID != (pgtype.UUID{Bytes: tok, Valid: true}) {
		t.Fatalf("claimed run: status=%s credential=%v, want claimed on %s", r.Status, r.AnthropicSecretID, tok)
	}
	if r.ReserveHoldReason.Valid {
		t.Fatalf("a claimed run keeps a reserve reason: %q", r.ReserveHoldReason.String)
	}
}

// TestReserveAutoPoolSkipsToBelowThresholdTokenLiveDB: the auto pool never spends a pooled token at
// or above the line while another pooled token is below it.
func TestReserveAutoPoolSkipsToBelowThresholdTokenLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 99)
	hi := fx.token("pool-hi", false, true, 85)
	ok := fx.token("pool-ok", false, true, 20)
	fx.bindWorker(BindModeAuto, nil)
	// Make the blocked token look BETTER to the ranker: lower 5-hour use, so only the reserve can skip it.
	fx.reading(hi, 85, 0, "1 minute")
	fx.reading(ok, 20, 60, "1 minute")
	id := fx.queueRun(reserveRun{})
	if p := fx.claim(); p == nil {
		t.Fatal("a pooled token below the threshold existed but the run was not claimed")
	}
	if got := mustRun(t, fx.env, id).AnthropicSecretID; got != (pgtype.UUID{Bytes: ok, Valid: true}) {
		t.Fatalf("auto spent %v, want the below-threshold pooled token %s (never %s)", got, ok, hi)
	}
}

// TestReserveAssemblyHoldRequeuesWithReasonLiveDB pins the requeue/counter mechanics of an
// assembly-time hold, not a production race: assembleAndFinishRunClaim is called with a bare
// context, so it reads a FRESH snapshot that sees the moved reading (a real claim reuses the
// admission snapshot, so an assembly hold there arises only from a Go/SQL divergence or the
// open-failure floor). Assembly returns errReserveHeld and the claim settles claimed -> queued with
// the reason persisted, and touches no limit-park, requeue or wall accounting.
func TestReserveAssemblyHoldRequeuesWithReasonLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 10)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	fx.env.exec(`UPDATE runs SET started_at = now() - interval '1 hour', budget_paused_seconds = 321,
	             requeue_count = 2, limit_wait_count = 1 WHERE id = $1`, id)
	before := mustRun(t, fx.env, id)

	// Admission with the reserve parameters computed while the reading is still fine.
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	fx.reading(tok, 88, 10, "1 minute") // ... then the poller reports 88%
	payload, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false)
	if err != nil || payload != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v), want an idle requeue", payload != nil, err)
	}
	r := mustRun(t, fx.env, id)
	if r.Status != "queued" {
		t.Fatalf("status = %s, want queued", r.Status)
	}
	if r.ReserveHoldReason.String != usagereserve.HeldReason(88, 70) {
		t.Fatalf("reserve_hold_reason = %q, want %q", r.ReserveHoldReason.String, usagereserve.HeldReason(88, 70))
	}
	if r.LimitWaitCount != before.LimitWaitCount || r.RequeueCount != before.RequeueCount ||
		!r.StartedAt.Time.Equal(before.StartedAt.Time) || r.BudgetPausedSeconds != before.BudgetPausedSeconds ||
		r.LimitResetsAt != before.LimitResetsAt || r.RetryNotBefore != before.RetryNotBefore || r.RateLimitType != before.RateLimitType {
		t.Fatalf("a reserve hold touched usage-park/requeue/wall state:\nbefore %+v\nafter  %+v", before, r)
	}
	if r.AnthropicSecretID.Valid {
		t.Fatalf("a blocked credential was recorded as spent: %v", r.AnthropicSecretID)
	}
}

// TestReserveOpenFailureOfPermittedTokenHoldsLiveDB: the only permitted pooled token fails to
// decrypt; the open-failed retry would otherwise floor onto the blocked token. It must hold the
// run (queued, reserve reason) instead: no blocked token spent, no terminal failure.
func TestReserveOpenFailureOfPermittedTokenHoldsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 10)
	ok := fx.token("pool-ok", false, true, 10)
	fx.token("pool-hi", false, true, 91)
	fx.bindWorker(BindModeAuto, nil)
	id := fx.queueRun(reserveRun{})

	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	// The permitted token goes undecryptable after admission.
	fx.env.exec(`UPDATE user_secrets SET ciphertext = '\x0102030405060708090a0b0c0d0e0f10111213'::bytea WHERE id = $1`, ok)
	payload, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false)
	if err != nil || payload != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v)", payload != nil, err)
	}
	r := mustRun(t, fx.env, id)
	if r.Status != "queued" || r.FailOrigin.Valid || r.AnthropicSecretID.Valid {
		t.Fatalf("status=%s fail_origin=%v credential=%v, want a held queued run that spent nothing", r.Status, r.FailOrigin, r.AnthropicSecretID)
	}
	if r.ReserveHoldReason.String != usagereserve.HeldReason(91, 70) {
		t.Fatalf("reason = %q, want the blocked pooled token's reading", r.ReserveHoldReason.String)
	}
}

// TestReserveOpenFailureWithNoOtherPooledTokenStaysTerminalLiveDB is the control: with no OTHER
// pooled token at all, the open failure keeps its existing terminal outcome (the reserve did not
// empty the pool).
func TestReserveOpenFailureWithNoOtherPooledTokenStaysTerminalLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 10)
	ok := fx.token("pool-ok", false, true, 10)
	fx.bindWorker(BindModeAuto, nil)
	id := fx.queueRun(reserveRun{})
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	fx.env.exec(`UPDATE user_secrets SET ciphertext = '\x0102030405060708090a0b0c0d0e0f10111213'::bytea WHERE id = $1`, ok)
	if _, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false); err != nil {
		t.Fatal(err)
	}
	if r := mustRun(t, fx.env, id); r.Status != "failed" || r.FailOrigin.String != "credential_unavailable" {
		t.Fatalf("status=%s origin=%q, want the existing terminal credential failure", r.Status, r.FailOrigin.String)
	}
}

// TestReserveJudgeGatedLiveDB: a judge run follows the OWNER's judge binding, not the worker's.
func TestReserveJudgeGatedLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	def := fx.token("default", true, false, 85)
	fx.bindWorker(BindModeDefault, nil)
	fx.bindJudge(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{kind: runkind.Judge})
	if p := fx.claim(); p != nil {
		t.Fatal("a judge run on a blocked default was claimed")
	}
	fx.reading(def, 5, 10, "1 minute")
	if p := fx.claim(); p == nil || mustRun(t, fx.env, id).Status != "claimed" {
		t.Fatal("the judge run was not claimed below the threshold")
	}
}

// TestReserveCodexAliasGatedLiveDB: a Codex run is held on its alias and claims nothing (no capability
// is minted) while held; a held assembly (driven here with a fresh snapshot read, see
// TestReserveAssemblyHoldRequeuesWithReasonLiveDB) returns before the mint.
func TestReserveCodexAliasGatedLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 10)
	alias := fx.subAlias(weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 75)))
	id := fx.queueRun(reserveRun{codexAlias: &alias})
	if p := fx.claim(); p != nil {
		t.Fatal("a Codex run on an alias above the threshold was claimed")
	}
	// Requeue mechanics with a fresh snapshot read at assembly (bare ctx, not a production race):
	// admitted at a low reading, the assembly then sees a crossed reading. No capability may have
	// been minted by the time the claim is requeued.
	fx.codexReading(alias, weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 10)), "1 minute")
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	fx.codexReading(alias, weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 90)), "1 minute")
	if p, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false); err != nil || p != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v)", p != nil, err)
	}
	r := mustRun(t, fx.env, id)
	if r.Status != "queued" || r.ReserveHoldReason.String != usagereserve.HeldReason(90, 70) {
		t.Fatalf("status=%s reason=%q, want a held queued run", r.Status, r.ReserveHoldReason.String)
	}
	if len(r.CodexCapHash) != 0 {
		t.Fatalf("a held Codex claim left a minted capability: %x", r.CodexCapHash)
	}
}

// reserveCountingQueries counts the reserve reads a claim performs.
type reserveCountingQueries struct {
	*store.Queries
	settings, tokens, aliases, binding int
}

func (c *reserveCountingQueries) GetUserUsageReserve(ctx context.Context, id uuid.UUID) (store.GetUserUsageReserveRow, error) {
	c.settings++
	return c.Queries.GetUserUsageReserve(ctx, id)
}

func (c *reserveCountingQueries) ListReserveClaudeTokens(ctx context.Context, id uuid.UUID) ([]store.ListReserveClaudeTokensRow, error) {
	c.tokens++
	return c.Queries.ListReserveClaudeTokens(ctx, id)
}

func (c *reserveCountingQueries) ListReserveCodexAliases(ctx context.Context, id uuid.UUID) ([]store.ListReserveCodexAliasesRow, error) {
	c.aliases++
	return c.Queries.ListReserveCodexAliases(ctx, id)
}

func (c *reserveCountingQueries) ListReserveWorkers(ctx context.Context, id uuid.UUID) ([]store.ListReserveWorkersRow, error) {
	c.binding++
	return c.Queries.ListReserveWorkers(ctx, id)
}

// TestReserveOffCostsOneSettingsReadLiveDB: an owner with no threshold changes nothing for their
// claims and the reserve adds exactly ONE read (the settings row) to a claim, including on a
// deployment with the usage poller disabled.
func TestReserveOffCostsOneSettingsReadLiveDB(t *testing.T) {
	fx := newReserveFix(t, 0, true)
	fx.svc.p.UsagePollInterval = 0 // UZI_USAGE_POLL_INTERVAL=0: nothing refreshes any reading
	fx.svc.p.CodexUsagePollInterval = 0
	fx.token("default", true, false, -1) // a token with NO reading at all
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	cq := &reserveCountingQueries{Queries: fx.env.q}
	fx.svc.q = cq
	p := fx.claim()
	if p == nil || mustRun(t, fx.env, id).Status != "claimed" {
		t.Fatal("an owner with no threshold must be unaffected, readings or not")
	}
	if cq.settings != 1 || cq.tokens != 0 || cq.aliases != 0 || cq.binding != 0 {
		t.Fatalf("reserve reads on the off path: settings=%d tokens=%d aliases=%d workers=%d, want exactly 1/0/0/0",
			cq.settings, cq.tokens, cq.aliases, cq.binding)
	}
}

// TestReserveChatNeverGatedLiveDB: chat is the interactive use the reserve protects and is never
// gated; the decision core agrees for a chat-kind run on a blocked default (ClaimRun excludes chat
// from the run lane, so chat's own claim never reaches the reserve clause at all).
func TestReserveChatNeverGatedLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 99)
	snap, err := fx.svc.reserveSnapshot(fx.env.ctx, fx.userID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := reserveHeld(store.Run{Kind: runkind.Chat, UserID: fx.userID}, snap, reserveLane{mode: BindModeDefault}, time.Now()); d.held {
		t.Fatal("chat was held by the reserve")
	}
	if !snap.defaultBlocked() {
		t.Fatal("the control: the default must read as blocked")
	}
}

// TestReserveEphemeralRecheckLiveDB: the ephemeral provisioner's locked recheck holds a run on the
// lane a fresh ephemeral worker would get (auto with a pool, else default) and passes an exempt one.
func TestReserveEphemeralRecheckLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	def := fx.token("default", true, false, 85)
	pool := fx.token("pool-ok", false, true, 10)
	iv := ReserveIntervals{Claude: 5 * time.Minute, Codex: 5 * time.Minute}
	id := fx.queueRun(reserveRun{})
	held := func(runID uuid.UUID) bool {
		t.Helper()
		h, err := EphemeralReserveHeld(fx.env.ctx, fx.env.q, runID, iv, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if held(id) {
		t.Fatal("with a permitted pooled token the ephemeral auto lane can serve the run")
	}
	fx.reading(pool, 90, 10, "1 minute")
	if !held(id) {
		t.Fatal("with the whole pool blocked the ephemeral recheck must hold the run")
	}
	fx.env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE id = $1`, pool)
	if !held(id) {
		t.Fatal("no pool: the ephemeral worker binds `default`, which is blocked")
	}
	fx.reading(def, 5, 10, "1 minute")
	if held(id) {
		t.Fatal("a below-threshold default frees the run")
	}
	started := fx.queueRun(reserveRun{firstStarted: true})
	fx.reading(def, 85, 10, "1 minute")
	fx.setReserve(70, false)
	if held(started) {
		t.Fatal("a started run is exempt with the pause toggle off")
	}
	if _, err := EphemeralReserveHeld(fx.env.ctx, fx.env.q, uuid.New(), iv, time.Now()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a vanished run = %v, want pgx.ErrNoRows for the caller to skip", err)
	}
}

// ---- provider-bound runs are outside the VENDOR reserve (PRD #4322 M1a, issue #4658) ---------------

// claimOn runs ClaimRun with the reserve parameters claimLane derives for workerID and reports which
// run it claimed. The claim is undone so the next probe starts clean, and every queued run is
// re-dated first so the fleet spread (a run younger than @spread_cutoff) stays active.
func (fx *reserveFix) claimOn(workerID uuid.UUID) (uuid.UUID, bool) {
	fx.t.Helper()
	fx.env.exec(`UPDATE runs SET updated_at = now() WHERE user_id = $1 AND status = 'queued'`, fx.userID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(workerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false
	}
	if err != nil {
		fx.t.Fatalf("ClaimRun: %v", err)
	}
	fx.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = 0 WHERE id = $1`, run.ID)
	return run.ID, true
}

// wantProviderPayload asserts a delivered claim is the provider run's frozen binding and nothing vendor.
func (fx *reserveFix) wantProviderPayload(t *testing.T, p *ClaimPayload, runID uuid.UUID) {
	t.Helper()
	b := fx.providerBinding()
	if p == nil {
		t.Errorf("Claim delivered nothing, want the provider-bound run %s", runID)
		return
	}
	if p.RunID != runID.String() {
		t.Errorf("Claim delivered run %s, want %s", p.RunID, runID)
		return
	}
	got := p.Secrets.Provider
	if got == nil || got.ProviderID != b.provider.String() || got.Revision != 1 || got.Credential != b.plain || p.Secrets.AnthropicOAuthToken != "" {
		t.Errorf("claim secrets = %+v, want only the frozen provider binding with the seeded key", p.Secrets)
	}
	r := mustRun(t, fx.env, runID)
	if r.Status != "claimed" || r.AnthropicSecretID.Valid ||
		r.ModelProviderKeySecretID != (pgtype.UUID{Bytes: b.key, Valid: true}) || r.ModelProviderKeyRev.Int64 != b.keyRev {
		t.Errorf("persisted run: status=%s anthropic=%v key=%v rev=%v, want claimed with the frozen key and no vendor credential",
			r.Status, r.AnthropicSecretID, r.ModelProviderKeySecretID, r.ModelProviderKeyRev)
	}
}

// TestReserveProviderBoundExemptLiveDB: a run frozen to a custom model provider spends its own gateway
// key, never a vendor credential, so the VENDOR weekly reserve never holds, marks, pauses or keeps it
// parked; a vendor-bound run beside it stays subject. Every subtest asserts the SQL verdict (ClaimRun)
// and then the Go core, both reported.
//
// MUTATION: dropping the provider arm from ClaimRun's reserve clause, from the fleet-spread peer
// clause, from reserveEnforced or from reserveRequestStillWanted reddens the matching subtests.
func TestReserveProviderBoundExemptLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, kind string }{
		{"issue_blocked_worker_default", runkind.Issue},
		{"self_improve_judge_lane_blocked", runkind.SelfImprove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newReserveFix(t, 70, true)
			fx.token("default", true, false, 72) // the owner default: held
			fx.bindWorker(BindModeDefault, nil)
			fx.bindJudge(BindModeDefault, nil)

			vendor := fx.queueRun(reserveRun{kind: tc.kind})
			if fx.admits(vendor) {
				t.Errorf("SQL: the vendor control was admitted on a blocked default")
			}
			if !fx.goHeld(vendor).held {
				t.Errorf("Go: the vendor control was not held on a blocked default")
			}

			if tc.kind == runkind.SelfImprove {
				fx.dropRuns() // one active self_improve per owner and repo: the control is gone before the provider run
			}
			prov := fx.queueRun(reserveRun{kind: tc.kind, provider: true})
			if !fx.admits(prov) {
				t.Errorf("SQL: the provider-bound run was held by the vendor reserve")
			}
			if d := fx.goHeld(prov); d.held {
				t.Errorf("Go: the provider-bound run was held by the vendor reserve: %q", d.reason)
			}

			// The assembled claim delivers the frozen binding and nothing vendor.
			fx.wantProviderPayload(t, fx.claim(), prov)
			if tc.kind == runkind.SelfImprove {
				return
			}
			if r := mustRun(t, fx.env, vendor); r.Status != "queued" || r.WorkerID.Valid {
				t.Errorf("the vendor control was claimed: status=%s worker=%v", r.Status, r.WorkerID)
			}
		})
	}

	t.Run("judge_provider_vs_pinned_vendor", func(t *testing.T) {
		p := newPJFix(t)
		if _, err := p.env.q.SetUserUsageReserve(p.env.ctx, store.SetUserUsageReserveParams{
			ID: p.userID, WeeklyReservePct: pgtype.Int2{Int16: 70, Valid: true}, WeeklyReservePauseRunning: true,
		}); err != nil {
			t.Fatalf("SetUserUsageReserve: %v", err)
		}
		p.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at, seven_day_pct, seven_day_resets_at, source, synced_at)
		            VALUES ($1, $2, 10, now() + interval '1 hour', 90, now() + interval '48 hours', 'usage_endpoint', now() - interval '1 minute')`, p.token, p.userID)

		provJudge := p.createdJudge() // the default judge binding resolves to the selected provider
		snap, err := p.svc.reserveSnapshot(p.env.ctx, p.userID, time.Now())
		if err != nil {
			t.Fatalf("reserveSnapshot: %v", err)
		}
		if reserveEnforced(mustRun(t, p.env, provJudge.ID), snap, time.Now()) {
			t.Errorf("Go: the provider judge is subject to the vendor reserve")
		}
		// directClaim is ClaimRun's verdict with the reserve parameters claimLane derives for the worker.
		directClaim := func() (store.Run, error) {
			w := p.worker()
			now := time.Now()
			s, err := p.svc.reserveSnapshot(p.env.ctx, p.userID, now)
			if err != nil {
				t.Fatalf("reserveSnapshot: %v", err)
			}
			peers, err := p.svc.reserveBlockedWorkers(p.env.ctx, s)
			if err != nil {
				t.Fatalf("reserveBlockedWorkers: %v", err)
			}
			cp := claimRunParamsFor(w)
			reserveClaimParams(&cp, s, w, peers, now)
			return p.env.q.ClaimRun(p.env.ctx, cp)
		}
		got, err := directClaim()
		switch {
		case err != nil:
			t.Errorf("SQL: ClaimRun refused the provider judge on the blocked vendor token: %v", err)
		case got.ID != provJudge.ID:
			t.Errorf("SQL: ClaimRun claimed %s, want the provider judge %s", got.ID, provJudge.ID)
		default:
			// Back to queued, as reserveFix.admits does, so Service.Claim can claim it.
			p.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = 0 WHERE id = $1`, provJudge.ID)
		}
		payload, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if payload == nil || payload.RunID != provJudge.ID.String() {
			t.Errorf("Claim: the provider judge was not claimed despite the blocked vendor token: %+v", payload)
		}

		// A judge pinned to the held vendor token is vendor-frozen and stays queued.
		tok := p.token
		if _, err := p.judge.SetUserJudgeBinding(p.env.ctx, p.userID, BindModePinned, &tok, nil); err != nil {
			t.Fatalf("pin: %v", err)
		}
		target := p.newTarget()
		p.auto(target)
		vendorJudge := p.mustJudge(target)
		p.wantVendor(vendorJudge)
		if !reserveEnforced(mustRun(t, p.env, vendorJudge.ID), snap, time.Now()) {
			t.Errorf("Go: the vendor-frozen judge is not subject to the reserve")
		}
		genBefore := mustRun(t, p.env, vendorJudge.ID).ClaimGeneration
		if direct, err := directClaim(); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("SQL: ClaimRun on the vendor-frozen judge = (%v, %v), want pgx.ErrNoRows", direct.ID, err)
			if err == nil {
				p.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = $2 WHERE id = $1`, vendorJudge.ID, genBefore)
			}
		}
		again, err := p.svc.Claim(p.env.ctx, p.worker(), nil)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if again != nil {
			t.Errorf("Claim: the vendor-frozen judge on the held token was claimed: %s", again.RunID)
		}
		after := mustRun(t, p.env, vendorJudge.ID)
		if after.Status != "queued" {
			t.Errorf("vendor judge status = %s, want queued", after.Status)
		}
		if after.ClaimGeneration != genBefore {
			t.Errorf("vendor judge claim_generation advanced %d -> %d", genBefore, after.ClaimGeneration)
		}
	})

	t.Run("source_selected_unbound_row_held", func(t *testing.T) {
		fx := newReserveFix(t, 70, true)
		fx.token("default", true, false, 72)
		fx.bindWorker(BindModeDefault, nil)
		// The owner has a provider source selected, but this row froze no binding (a vendor run).
		selectClaudeSource(t, fx.env, fx.userID, fx.providerBinding().provider)
		id := fx.queueRun(reserveRun{})
		if fx.admits(id) {
			t.Errorf("SQL: an unbound run was admitted because the owner selected a source")
		}
		if !fx.goHeld(id).held {
			t.Errorf("Go: an unbound run was not held because the owner selected a source")
		}
		if p := fx.claim(); p != nil {
			t.Errorf("an unbound run was claimed: %s", p.RunID)
		}
	})

	t.Run("fleet_spread_peer", func(t *testing.T) {
		fx := newReserveFix(t, 70, true)
		fx.token("default", true, false, 72)
		permitted := fx.token("permitted", false, false, 5)
		claimant := fx.workerID
		peer := fx.extraWorker(BindModeDefault, nil) // its lane is the held default: a reserve-blocked peer
		fx.env.exec(`UPDATE workers SET max_concurrent_runs = 4 WHERE id = ANY($1)`, []uuid.UUID{claimant, peer})
		fx.env.exec(`UPDATE workers SET protocol_capabilities = (SELECT protocol_capabilities FROM workers WHERE id = $2) WHERE id = $1`, peer, claimant)
		fx.bindWorker(BindModePinned, &permitted)
		fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		             VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'running', $5)`, uuid.New(), fx.userID, fx.repoID, fx.nextIssueIID(), claimant)

		// Vendor control. With an ordinary (unblocked) peer the busy claimant defers: the spread is live.
		vendor := fx.queueRun(reserveRun{})
		fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'pinned', anthropic_secret_id = $2 WHERE id = $1`, peer, permitted)
		if id, ok := fx.claimOn(claimant); ok {
			t.Fatalf("control: the busy claimant claimed %s although an idle unblocked peer exists; the spread is not active", id)
		}
		// With the peer's lane blocked the deferral is suppressed (today's behaviour): the claimant claims.
		fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'default', anthropic_secret_id = NULL WHERE id = $1`, peer)
		if id, ok := fx.claimOn(claimant); !ok || id != vendor {
			t.Errorf("the vendor run was not claimed by the claimant while its only peer is reserve-blocked: (%v, %v)", id, ok)
		}
		fx.dropRuns()

		// A provider-bound run is not gated on the blocked peer, so the claimant defers to it and the peer claims.
		prov := fx.queueRun(reserveRun{provider: true})
		if id, ok := fx.claimOn(claimant); ok {
			t.Errorf("the claimant claimed the provider run %s instead of deferring to the idle peer", id)
		}
		if id, ok := fx.claimOn(peer); !ok || id != prov {
			t.Errorf("the idle peer did not claim the provider run: (%v, %v)", id, ok)
		}
	})

	t.Run("marker", func(t *testing.T) {
		fx := newReserveFix(t, 70, true)
		fx.token("default", true, false, 72)
		fx.bindWorker(BindModeDefault, nil)
		vendor := fx.queueRun(reserveRun{})
		prov := fx.queueRun(reserveRun{provider: true})
		provFresh := fx.queueRun(reserveRun{provider: true}) // starts unmarked
		fx.env.exec(`UPDATE runs SET reserve_hold_reason = $2 WHERE id = $1`, prov, usagereserve.HeldReason(72, 70))
		fx.mark()
		if got, ok := fx.reasonOf(prov); ok {
			t.Errorf("the pre-marked provider-bound run kept a reserve hold reason %q", got)
		}
		if got, ok := fx.reasonOf(provFresh); ok {
			t.Errorf("the unmarked provider-bound run gained a reserve hold reason %q", got)
		}
		if got, ok := fx.reasonOf(vendor); !ok || got != usagereserve.HeldReason(72, 70) {
			t.Errorf("vendor control reason = (%q, %v), want %q", got, ok, usagereserve.HeldReason(72, 70))
		}
	})

	t.Run("pending_request_withdrawn", func(t *testing.T) {
		fx := newReserveFix(t, 70, true)
		held := fx.token("default", true, false, 90)
		fx.bindWorker(BindModeDefault, nil)
		prov := fx.pauseRun(pauseRun{pauseMode: "reserve", completed: 1})
		fx.freezeProvider(prov)
		ownerNow := fx.pauseRun(pauseRun{pauseMode: "now"})
		fx.freezeProvider(ownerNow)
		vendor := fx.pauseRun(pauseRun{cred: &held, pauseMode: "reserve", completed: 1})
		fx.pass()
		if _, _, req := fx.pausePending(prov); req {
			t.Errorf("the provider run kept its pending reserve pause request")
		}
		if m, _, req := fx.pausePending(vendor); !req || m != "reserve" {
			t.Errorf("the vendor control lost its request: (%q, %v)", m, req)
		}
		if m, _, req := fx.pausePending(ownerNow); !req || m != "now" {
			t.Errorf("the owner's pause on the provider run was touched: (%q, %v)", m, req)
		}
	})

	t.Run("paused_release", func(t *testing.T) {
		fx := newReserveFix(t, 70, true)
		held := fx.token("default", true, false, 90)
		fx.bindWorker(BindModeDefault, nil)
		prov := fx.pauseRun(pauseRun{status: "paused", hold: holdWeeklyReserve, statusSince: "100 seconds", budgetPaused: 40})
		fx.freezeProvider(prov)
		vendor := fx.pauseRun(pauseRun{cred: &held, status: "paused", hold: holdWeeklyReserve, statusSince: "100 seconds"})
		before := mustRun(t, fx.env, prov)
		fx.pass()
		after := mustRun(t, fx.env, prov)
		if after.Status != "queued" || after.HoldReason.Valid {
			t.Errorf("the provider run stayed parked: status=%s hold=%v", after.Status, after.HoldReason)
		} else {
			if !after.StartedAt.Time.Equal(before.StartedAt.Time) {
				t.Errorf("started_at moved %v -> %v on release", before.StartedAt.Time, after.StartedAt.Time)
			}
			if gained := after.BudgetPausedSeconds - before.BudgetPausedSeconds; gained < 100 || gained > 130 {
				t.Errorf("banked %ds of the parked interval, want ~100", gained)
			}
		}
		if r := mustRun(t, fx.env, vendor); r.Status != "paused" {
			t.Errorf("the vendor control was released: %s", r.Status)
		}
	})
}
