package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// phase_models_mirror_livedb_test.go (PRD #3252): one matrix runs the same fixture runs through
// every query that mirrors the claude_phase_models_v1 claim clause and asserts identical
// eligibility: ClaimRun (claimant and fleet-spread peer clauses), CountOnlineWorkersClaimableForRun,
// ListUnplaceableQueuedRunsForEphemeral (through fn_placement_admitting_workers),
// ListSaturationQueuedRunsForEphemeral, the ListActiveRunsForHealth phase_models_required projection
// and CountOnlineWorkersSatisfyingPhaseModels. Skipped unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh).

const phaseCap = "claude_phase_models_v1"

type phaseMatrixCase struct {
	name     string
	kind     string // issue, ci_fix, self_improve, prompt, mr_rework, task
	review   bool   // a task review run (review_target_run_id set)
	harness  string
	model    any // users.planning_claude_model
	effort   any // users.planning_claude_effort
	required bool
	noCaps   bool // required_capabilities '{}' and no docker preference: a phase-only run (PRD example 5)
	egress   any  // runs.egress_profile_id (immutable after insert); honoured for issue and self_improve only
}

func phaseMatrixCases() []phaseMatrixCase {
	var out []phaseMatrixCase
	for _, k := range []string{"issue", "ci_fix", "self_improve", "prompt", "mr_rework", "task"} {
		out = append(out,
			phaseMatrixCase{name: "codex/" + k + "/model", kind: k, harness: "codex", model: "gpt-6-sol", required: true},
			phaseMatrixCase{name: "codex/" + k + "/effort", kind: k, harness: "codex", effort: "high", required: true},
			phaseMatrixCase{name: "codex/" + k + "/neither", kind: k, harness: "codex"})
		out = append(out,
			phaseMatrixCase{name: k + "/planning model", kind: k, harness: "claude", model: "opus", required: true},
			phaseMatrixCase{name: k + "/planning effort", kind: k, harness: "claude", effort: "high", required: true},
			phaseMatrixCase{name: k + "/nothing set", kind: k, harness: "claude"},
			phaseMatrixCase{name: k + "/planning model/no caps", kind: k, harness: "claude", model: "opus", required: true, noCaps: true},
			phaseMatrixCase{name: k + "/planning effort/no caps", kind: k, harness: "claude", effort: "high", required: true, noCaps: true},
			phaseMatrixCase{name: k + "/nothing set/no caps", kind: k, harness: "claude", noCaps: true})
	}
	out = append(out,
		phaseMatrixCase{name: "task review is not a planning run", kind: "task", review: true, harness: "claude", model: "opus", effort: "high"},
		phaseMatrixCase{name: "codex task review is not a planning run", kind: "task", review: true, harness: "codex", model: "private-plan", effort: "high"})
	return out
}

// phaseRun inserts a queued, unowned run of c.kind for the fixture user with required_capabilities
// {docker} (so the capability-gap trigger's demand gate admits it; '{}' for a noCaps case, where
// only the phase predicate can admit it) and an old status_since (so the saturation debounce admits
// it).
func phaseRun(fx *fleetFixture, c phaseMatrixCase) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	docker := []string{"docker"}
	if c.noCaps {
		docker = []string{}
	}
	switch {
	case c.review:
		target := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_title, issue_description, status, harness)
		    VALUES ($1, $2, $3, 'task', $4, 't', 'd', 'completed', $5)`, target, fx.userID, fx.repoID, "uzi/task/"+target.String(), c.harness)
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, harness, required_capabilities, status_since)
		    VALUES ($1, $2, $3, 'task', $4, $5, now(), 't', 'd', 'queued', $6, $7, now() - interval '1 hour')`, id, fx.userID, fx.repoID, "uzi/task/"+id.String(), target, c.harness, docker)
	case c.kind == "issue" || c.kind == "self_improve":
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness, required_capabilities, status_since, egress_profile_id)
		    VALUES ($1, $2, $3, $4, $5, 't', 'd', 'queued', $6, $7, now() - interval '1 hour', $8)`, id, fx.userID, fx.repoID, c.kind, fx.nextIID(), c.harness, docker, c.egress)
	case c.kind == "ci_fix":
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, pipeline_id, pipeline_ref, issue_title, issue_description, status, harness, required_capabilities, status_since)
		    VALUES ($1, $2, $3, 'ci_fix', $4, $5, 't', 'd', 'queued', $6, $7, now() - interval '1 hour')`, id, fx.userID, fx.repoID, fx.nextIID(), "ref-"+id.String(), c.harness, docker)
	case c.kind == "mr_rework":
		source := uuid.New()
		mr := fx.nextIID()
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, branch, mr_iid, mr_state, issue_title, issue_description, status, harness)
		    VALUES ($1, $2, $3, 'issue', $4, $5, $6, 'opened', 't', 'd', 'completed', $7)`, source, fx.userID, fx.repoID, fx.nextIID(), "agent/issue-"+source.String(), mr, c.harness)
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, pipeline_ref, mr_iid, target_run_id, issue_title, issue_description, status, harness, required_capabilities, status_since)
		    VALUES ($1, $2, $3, 'mr_rework', $4, $5, $6, 't', 'd', 'queued', $7, $8, now() - interval '1 hour')`, id, fx.userID, fx.repoID, "agent/issue-"+source.String(), mr, source, c.harness, docker)
	default: // prompt, task
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs (id, user_id, repo_id, kind, branch, dispatched_at, issue_title, issue_description, status, harness, required_capabilities, status_since)
		    VALUES ($1, $2, $3, $4, $5, now(), 't', 'd', 'queued', $6, $7, now() - interval '1 hour')`, id, fx.userID, fx.repoID, c.kind, "uzi/"+c.kind+"/"+id.String(), c.harness, docker)
	}
	return id
}

func phaseWorkerRow(fx *fleetFixture, name string, maxRuns *int32, protos []string) uuid.UUID {
	fx.t.Helper()
	id := fx.worker(name, maxRuns, true)
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, id, protos)
	return id
}

func phaseClaim(fx *fleetFixture, worker uuid.UUID, protos []string) (uuid.UUID, bool) {
	fx.t.Helper()
	run, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: worker, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      true,
		DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		CapabilityAware:     true,
		WorkerCaps:          []string{"docker"},
		WorkerProtocolCaps:  protos,
		CodexCuratedModels:  []string{"gpt-6-sol"},
	})
	if err != nil {
		if err.Error() == "no rows in result set" {
			return uuid.Nil, false
		}
		fx.t.Fatalf("ClaimRun: %v", err)
	}
	return run.ID, true
}

// phaseMaxRows is far above any leftover queued-run count on a shared database, so the trigger
// queries' ORDER BY ... LIMIT cannot crowd the fixture's run out of the listed batch (the shared
// listedUnplaceable / listedSaturation helpers cap at 50 rows).
const phaseMaxRows = 100000

func phaseListedUnplaceable(fx *fleetFixture, iv pgtype.Interval) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		CodexCuratedModels:    []string{"gpt-6-sol"},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
		EphemeralLease:        iv, MaxPerUser: 1000, MaxRows: phaseMaxRows,
		CrossCheckEvaluatedAt:    planCrossCheckTime(time.Now()),
		CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-2 * time.Minute))})
	if err != nil {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func phaseListedSaturation(fx *fleetFixture, iv pgtype.Interval) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		CodexCuratedModels:    []string{"gpt-6-sol"},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
		SaturationDelay:       leaseInterval(0), EphemeralLease: iv, MaxPerUser: 1000, MaxRows: phaseMaxRows,
		CrossCheckEvaluatedAt:    planCrossCheckTime(time.Now()),
		CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-2 * time.Minute))})
	if err != nil {
		fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func phaseHealthRow(fx *fleetFixture, run uuid.UUID) store.ListActiveRunsForHealthRow {
	fx.t.Helper()
	rows, err := fx.q.ListActiveRunsForHealth(fx.ctx, []string{"gpt-6-sol"})
	if err != nil {
		fx.t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if r.ID == run {
			return r
		}
	}
	fx.t.Fatalf("run %s not in ListActiveRunsForHealth", run)
	return store.ListActiveRunsForHealthRow{}
}

func TestPhaseModelsMirrorMatrixLiveDB(t *testing.T) {
	iv := leaseInterval(leaseTwoHours)
	cases := phaseMatrixCases()
	seen := make(map[string]bool, len(cases))
	for _, c := range cases {
		if seen[c.name] {
			t.Fatalf("duplicate phase matrix case %q", c.name)
		}
		seen[c.name] = true
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setup := func(withCapable bool, busyIncapable bool) (*fleetFixture, uuid.UUID, uuid.UUID, uuid.UUID) {
				fx := newFleetFixture(t)
				optInEphemeral(fx)
				mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_claude_model = CASE WHEN $4 = 'claude' THEN $2 END, planning_claude_effort = CASE WHEN $4 = 'claude' THEN $3 END, planning_codex_model = CASE WHEN $4 = 'codex' THEN $2 END, planning_codex_effort = CASE WHEN $4 = 'codex' THEN $3 END WHERE id = $1`, fx.userID, c.model, c.effort, c.harness)
				incapable := phaseWorkerRow(fx, "incapable", capOf(1), phaseProtocols(c.harness, false))
				if busyIncapable {
					fx.holdActive(incapable, 1)
				}
				capable := uuid.Nil
				if withCapable {
					capable = phaseWorkerRow(fx, "capable", capOf(1), phaseProtocols(c.harness, true))
				}
				return fx, phaseRun(fx, c), incapable, capable
			}
			want := func(cond bool) int64 {
				if cond {
					return 1
				}
				return 0
			}

			// Fleet A: only an incapable worker online.
			fx, run, incapable, _ := setup(false, false)
			if h := phaseHealthRow(fx, run); h.PhaseModelsRequired != c.required {
				t.Fatalf("health phase_models_required = %v, want %v", h.PhaseModelsRequired, c.required)
			}
			if n, err := fx.q.CountOnlineWorkersSatisfyingPhaseModels(fx.ctx, store.CountOnlineWorkersSatisfyingPhaseModelsParams{UserID: fx.userID, Harness: c.harness}); err != nil || n != 0 {
				t.Fatalf("satisfier count with no capable worker = (%d, %v), want 0", n, err)
			}
			if n := phaseClaimableCount(fx, run, iv); n != want(!c.required) {
				t.Fatalf("claimable by the incapable worker = %d, want %d", n, want(!c.required))
			}
			if gap := phaseListedUnplaceable(fx, iv); gap[run] != c.required {
				t.Fatalf("capability-gap trigger listed = %v, want %v", gap[run], c.required)
			}
			if id, ok := phaseClaim(fx, incapable, phaseProtocols(c.harness, false)); ok != !c.required || (ok && id != run) {
				t.Fatalf("ClaimRun by the incapable worker claimed = %v, want %v", ok, !c.required)
			}

			// Fleet B: an incapable worker and a capable one, both idle.
			fx, run, _, capable := setup(true, false)
			if n, err := fx.q.CountOnlineWorkersSatisfyingPhaseModels(fx.ctx, store.CountOnlineWorkersSatisfyingPhaseModelsParams{UserID: fx.userID, Harness: c.harness}); err != nil || n != 1 {
				t.Fatalf("satisfier count with one capable worker = (%d, %v), want 1", n, err)
			}
			if n := phaseClaimableCount(fx, run, iv); n != 2-want(c.required) {
				t.Fatalf("claimable = %d, want %d", n, 2-want(c.required))
			}
			if gap := phaseListedUnplaceable(fx, iv); gap[run] {
				t.Fatal("capability-gap trigger listed a run a capable worker can take")
			}
			if id, ok := phaseClaim(fx, capable, phaseProtocols(c.harness, true)); !ok || id != run {
				t.Fatalf("ClaimRun by the capable worker = (%v, %v), want it to claim", id, ok)
			}

			// Fleet C: only a SATURATED incapable worker. A required run has no capable worker to wait
			// for, so it is the gap trigger's, not the saturation trigger's; a free run is capable-but-full.
			fx, run, _, _ = setup(false, true)
			if sat := phaseListedSaturation(fx, iv); sat[run] != !c.required {
				t.Fatalf("saturation trigger listed = %v, want %v", sat[run], !c.required)
			}

			// Fleet D: only a SATURATED capable worker: capable-but-full for every run.
			fx = newFleetFixture(t)
			optInEphemeral(fx)
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_claude_model = CASE WHEN $4 = 'claude' THEN $2 END, planning_claude_effort = CASE WHEN $4 = 'claude' THEN $3 END, planning_codex_model = CASE WHEN $4 = 'codex' THEN $2 END, planning_codex_effort = CASE WHEN $4 = 'codex' THEN $3 END WHERE id = $1`, fx.userID, c.model, c.effort, c.harness)
			busyCapable := phaseWorkerRow(fx, "busy-capable", capOf(1), phaseProtocols(c.harness, true))
			fx.holdActive(busyCapable, 1)
			run = phaseRun(fx, c)
			if sat := phaseListedSaturation(fx, iv); !sat[run] {
				t.Fatal("saturation trigger did not list a run waiting on a saturated capable worker")
			}
			if gap := phaseListedUnplaceable(fx, iv); gap[run] {
				t.Fatal("capability-gap trigger listed a run a (saturated) capable worker could take")
			}

			// Fleet F: a SATURATED capable worker plus an IDLE incapable one. For a required run the idle
			// incapable worker has no free slot the run can use, so the run is still slot-blocked and the
			// saturation trigger lists it (the "capable worker with a free slot" NOT EXISTS must apply the
			// phase clause); for a free run the idle worker takes it, so it is not listed.
			fx = newFleetFixture(t)
			optInEphemeral(fx)
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_claude_model = CASE WHEN $4 = 'claude' THEN $2 END, planning_claude_effort = CASE WHEN $4 = 'claude' THEN $3 END, planning_codex_model = CASE WHEN $4 = 'codex' THEN $2 END, planning_codex_effort = CASE WHEN $4 = 'codex' THEN $3 END WHERE id = $1`, fx.userID, c.model, c.effort, c.harness)
			busyCapable = phaseWorkerRow(fx, "busy-capable", capOf(1), phaseProtocols(c.harness, true))
			fx.holdActive(busyCapable, 1)
			phaseWorkerRow(fx, "idle-incapable", capOf(1), phaseProtocols(c.harness, false))
			run = phaseRun(fx, c)
			if sat := phaseListedSaturation(fx, iv); sat[run] != c.required {
				t.Fatalf("saturation trigger with a busy capable and an idle incapable worker listed = %v, want %v", sat[run], c.required)
			}

			// Fleet E: the fleet-spread peer clause. The capable claimant already holds a run and an idle
			// incapable peer exists; a required run must not be deferred to a peer that cannot claim it.
			fx = newFleetFixture(t)
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_claude_model = CASE WHEN $4 = 'claude' THEN $2 END, planning_claude_effort = CASE WHEN $4 = 'claude' THEN $3 END, planning_codex_model = CASE WHEN $4 = 'codex' THEN $2 END, planning_codex_effort = CASE WHEN $4 = 'codex' THEN $3 END WHERE id = $1`, fx.userID, c.model, c.effort, c.harness)
			claimant := phaseWorkerRow(fx, "claimant", capOf(4), phaseProtocols(c.harness, true))
			phaseWorkerRow(fx, "idle-incapable-peer", capOf(4), phaseProtocols(c.harness, false))
			fx.holdActive(claimant, 1)
			run = phaseRun(fx, c)
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET updated_at = now(), status_since = now() WHERE id = $1`, run)
			id, ok := phaseClaim(fx, claimant, phaseProtocols(c.harness, true))
			if c.required && (!ok || id != run) {
				t.Fatalf("peer clause: the capable claimant was refused a required run (deferred to an incapable peer)")
			}
		})
	}
}

// TestPhaseModelsGapTriggerDemandGateEmptyFleetLiveDB pins the demand gate of the capability-gap
// trigger (ListUnplaceableQueuedRunsForEphemeral) in isolation. The matrix above always has an
// online worker that admits a non-required run, so a gate that wrongly admitted every phase-free run
// would still pass there. Each case here runs on its own freshly created user with NO worker at all,
// so nothing but the demand gate decides whether a '{}' run is listed.
func TestPhaseModelsGapTriggerDemandGateEmptyFleetLiveDB(t *testing.T) {
	iv := leaseInterval(leaseTwoHours)
	cases := []struct {
		name    string
		harness string
		model   any
		listed  bool
		review  bool // a task review run (review_target_run_id set)
		egress  bool // the run carries an egress profile (the query also excludes these at its top level, so only removing both guards turns this case red)
	}{
		{"claude lead-lane, no planning value, no caps: not demand", "claude", nil, false, false, false},
		{"codex lead planning model is demand", "codex", "gpt-6-sol", true, false, false},
		{"codex lead no planning is not demand", "codex", nil, false, false, false},
		{"control: claude lead-lane, planning value set, no caps: demand", "claude", "opus", true, false, false},
		{"task review run, planning value set, no caps: not demand", "claude", "opus", false, true, false},
		{"egress-profiled run, planning value set, no caps: not demand", "claude", "opus", false, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_claude_model = CASE WHEN $3 = 'claude' THEN $2 END, planning_codex_model = CASE WHEN $3 = 'codex' THEN $2 END WHERE id = $1`, fx.userID, c.model, c.harness)
			kind := "issue"
			if c.review {
				kind = "task"
			}
			var egress any
			if c.egress {
				profile := uuid.New()
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
					profile, "pm-"+profile.String()[:20])
				// Registered after the fixture's cleanup, so it runs first: the profile is RESTRICTed while a run uses it.
				t.Cleanup(func() {
					_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE egress_profile_id = $1`, profile)
					_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM egress_profiles WHERE id = $1`, profile)
				})
				egress = profile
			}
			run := phaseRun(fx, phaseMatrixCase{kind: kind, review: c.review, harness: c.harness, noCaps: true, egress: egress})
			if n := fx.onlineWorkerCount(); n != 0 {
				t.Fatalf("fixture user has %d workers, want an empty fleet", n)
			}
			if gap := phaseListedUnplaceable(fx, iv); gap[run] != c.listed {
				t.Fatalf("capability-gap trigger listed = %v, want %v", gap[run], c.listed)
			}
		})
	}
}

func (fx *fleetFixture) onlineWorkerCount() int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE user_id = $1`, fx.userID).Scan(&n); err != nil {
		fx.t.Fatalf("count workers: %v", err)
	}
	return n
}

func phaseProtocols(harness string, phase bool) []string {
	caps := []string{}
	if harness == "codex" {
		caps = append(caps, "codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1")
	}
	if phase {
		if harness == "codex" {
			caps = append(caps, "codex_phase_models_v1")
		} else {
			caps = append(caps, phaseCap)
		}
	}
	return caps
}

func phaseClaimableCount(fx *fleetFixture, run uuid.UUID, iv pgtype.Interval) int64 {
	fx.t.Helper()
	n, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: run, CodexCuratedModels: []string{"gpt-6-sol"},
		CrossCheckEvaluatedAt: planCrossCheckTime(time.Now()), CrossCheckAffinityCutoff: planCrossCheckTime(time.Now().Add(-2 * time.Minute)),
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true}, DockerRepoAllowlist: []uuid.UUID{fx.repoID}, EphemeralLease: iv,
	})
	if err != nil {
		fx.t.Fatal(err)
	}
	return n.Claimable
}

// These cases keep phase support present on the incapable worker, so only the custom
// planning-model predicate can exclude it from either saturation arm or peer spread.
func TestCodexCustomPlanningPeerAndSaturationLiveDB(t *testing.T) {
	for _, mode := range []string{"busy without custom", "busy both plus idle without custom", "peer without custom"} {
		t.Run(mode, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			mustExec(fx.ctx, t, fx.pool, "UPDATE users SET planning_codex_model = 'private-plan', default_codex_model = 'gpt-6-sol' WHERE id = $1", fx.userID)
			phase := []string{"codex_harness_v1", "codex_runtime_v2", "codex_phase_models_v1"}
			both := append(append([]string{}, phase...), "codex_custom_model_v1")
			caps := both
			if mode == "busy without custom" {
				caps = phase
			}
			slots := int32(1)
			if mode == "peer without custom" {
				slots = 4
			}
			claimant := phaseWorkerRow(fx, "busy", capOf(slots), caps)
			fx.holdActive(claimant, 1)
			if mode != "busy without custom" {
				phaseWorkerRow(fx, "idle-without-custom", capOf(4), phase)
			}
			run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex"})
			if mode == "peer without custom" {
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET updated_at = now(), status_since = now() WHERE id = $1", run)
				id, ok := phaseClaim(fx, claimant, both)
				if !ok || id != run {
					t.Fatal("custom-capable claimant deferred to a peer lacking custom planning support")
				}
				return
			}
			want := mode == "busy both plus idle without custom"
			if got := phaseListedSaturation(fx, leaseInterval(leaseTwoHours))[run]; got != want {
				t.Fatalf("custom planning saturation = %v, want %v", got, want)
			}
		})
	}
}

// A split fleet must not satisfy custom and phase requirements on different workers.
func TestCodexPlanningCustomCapabilitiesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   [][]string
		frozen bool
		want   int64
	}{
		{"phase without custom", [][]string{{"codex_phase_models_v1"}}, false, 0},
		{"custom without phase", [][]string{{"codex_custom_model_v1"}}, false, 0},
		{"split fleet", [][]string{{"codex_phase_models_v1"}, {"codex_custom_model_v1"}}, false, 0},
		{"same worker", [][]string{{"codex_phase_models_v1", "codex_custom_model_v1"}}, false, 1},
		{"frozen curated suppresses custom planning", [][]string{{"codex_phase_models_v1"}}, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			mustExec(fx.ctx, t, fx.pool, `UPDATE users SET planning_codex_model = 'private-plan', default_codex_model = 'gpt-6-sol' WHERE id = $1`, fx.userID)
			run := phaseRun(fx, phaseMatrixCase{kind: "issue", harness: "codex"})
			if tc.frozen {
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET model = 'gpt-6-sol' WHERE id = $1`, run)
			}
			var workers []uuid.UUID
			for _, extra := range tc.caps {
				caps := append([]string{"codex_harness_v1", "codex_runtime_v2"}, extra...)
				workers = append(workers, phaseWorkerRow(fx, "worker", capOf(1), caps))
			}
			h := phaseHealthRow(fx, run)
			if !h.PhaseModelsRequired || h.CodexCustomRoot == tc.frozen {
				t.Fatalf("health projection phase=%v custom=%v frozen=%v", h.PhaseModelsRequired, h.CodexCustomRoot, tc.frozen)
			}
			if n := phaseClaimableCount(fx, run, leaseInterval(leaseTwoHours)); n != tc.want {
				t.Fatalf("claimable=%d want=%d", n, tc.want)
			}
			if !tc.frozen {
				n, err := fx.q.CountOnlineWorkersSatisfyingCustomCodex(fx.ctx, store.CountOnlineWorkersSatisfyingCustomCodexParams{UserID: fx.userID, PhaseModelsRequired: true})
				if err != nil || n != tc.want {
					t.Fatalf("custom+phase count=(%d,%v) want=%d", n, err, tc.want)
				}
			}
			gap := phaseListedUnplaceable(fx, leaseInterval(leaseTwoHours))
			if gap[run] != (tc.want == 0) {
				t.Fatalf("gap=%v want=%v", gap[run], tc.want == 0)
			}
			for i, w := range workers {
				caps := append([]string{"codex_harness_v1", "codex_runtime_v2"}, tc.caps[i]...)
				id, ok := phaseClaim(fx, w, caps)
				if ok != (tc.want > 0) || (ok && id != run) {
					t.Fatalf("claim=(%v,%v) want=%v", id, ok, tc.want > 0)
				}
			}
		})
	}
}
