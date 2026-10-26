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
	svc.SetTxBeginner(env.pool)
	fx := &reserveFix{t: t, env: env, svc: svc, userID: userID, repoID: repoID, workerID: workerID, targetID: uuid.New()}
	env.exec(`UPDATE workers SET last_heartbeat_at = now(), protocol_capabilities = $2 WHERE id = $1`,
		workerID, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2})
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
}

func (fx *reserveFix) queueRun(o reserveRun) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	iid := 3000 + int64(id[0])*256 + int64(id[1])
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
	fx.token("default", true, false, 72)           // owner default, held
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

// TestReserveForcedRaceRequeuesWithReasonLiveDB: the reading crosses the threshold AFTER ClaimRun
// admitted the run (a fresh assembly sees the new reading). Assembly returns errReserveHeld and the
// claim settles claimed -> queued with the reason persisted, and touches no limit-park, requeue or
// wall accounting.
func TestReserveForcedRaceRequeuesWithReasonLiveDB(t *testing.T) {
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
// is minted) while held; a held assembly returns before the mint.
func TestReserveCodexAliasGatedLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 10)
	alias := fx.subAlias(weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 75)))
	id := fx.queueRun(reserveRun{codexAlias: &alias})
	if p := fx.claim(); p != nil {
		t.Fatal("a Codex run on an alias above the threshold was claimed")
	}
	// Race: admitted at a low reading, the account reading crosses before assembly. No capability
	// may have been minted by the time the claim is requeued.
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
