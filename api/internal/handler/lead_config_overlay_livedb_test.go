package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// lead_config_overlay_livedb_test.go covers the GetRun lead_config overlay (PRD #3244 section 5)
// and the lead-lane agreement table against Postgres. Skipped unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh).

type leadOverlayEnv struct {
	h      *Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	viewer store.User
	repo   uuid.UUID
	worker uuid.UUID
	iid    int64
}

func newLeadOverlayEnv(t *testing.T) *leadOverlayEnv {
	t.Helper()
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	t.Cleanup(func() {
		// Cascades the runs and their lead-config rows.
		_, _ = pool.Exec(t.Context(), `DELETE FROM runs WHERE user_id = $1`, owner)
		_, _ = pool.Exec(t.Context(), `DELETE FROM egress_profiles WHERE name LIKE $1`, "lo-"+owner.String()[:8]+"%")
		_, _ = pool.Exec(t.Context(), `DELETE FROM workers WHERE user_id = $1`, owner)
		_, _ = pool.Exec(t.Context(), `DELETE FROM users WHERE id = $1`, owner)
	})
	repo := rmSeedRepo(t, pool, rmSeedConn(t, pool, owner), time.Now().UnixNano()&0x3fffffff, true)
	worker := uuid.New()
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	                      VALUES ($1, $2, $3, $4, 'online', now())`, worker, owner, "lo-"+worker.String(), worker[:])
	return &leadOverlayEnv{h: h, pool: pool, owner: owner, viewer: store.User{ID: owner}, repo: repo, worker: worker, iid: 900}
}

func (e *leadOverlayEnv) nextIID() int64 { e.iid++; return e.iid }

type overlayRun struct {
	kind    string
	harness string
	review  bool
	egress  bool
	model   string // runs.model (a schedule's frozen pin)
	gen     int64
}

// seed inserts a running run held by the env worker, failing the test when the schema refuses the
// combination. Zero values give a Claude issue run at generation 1.
func (e *leadOverlayEnv) seed(t *testing.T, o overlayRun) uuid.UUID {
	t.Helper()
	id, err := e.seedTry(t, o)
	if err != nil {
		t.Fatalf("seed %+v: %v", o, err)
	}
	return id
}

// seedTry is seed returning the final INSERT's error, so the lane table can tell a combination the
// schema cannot store (a CHECK violation) from a real failure.
func (e *leadOverlayEnv) seedTry(t *testing.T, o overlayRun) (uuid.UUID, error) {
	t.Helper()
	if o.kind == "" {
		o.kind = "issue"
	}
	if o.harness == "" {
		o.harness = "claude"
	}
	if o.gen == 0 {
		o.gen = 1
	}
	id := uuid.New()
	cols := `id, user_id, kind, status, harness, issue_title, issue_description, worker_id, claim_generation`
	vals := []any{id, e.owner, o.kind, "running", o.harness, "t", "d", e.worker, o.gen}
	add := func(col string, v any) {
		cols += ", " + col
		vals = append(vals, v)
	}
	if o.model != "" {
		add("model", o.model)
	}
	if o.egress {
		prof := uuid.New()
		cliMustExec(t, e.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, ARRAY['example.test'])`,
			prof, "lo-"+e.owner.String()[:8]+"-"+id.String()[:8])
		add("egress_profile_id", prof)
	}
	completedTarget := func(harness string) uuid.UUID {
		t.Helper()
		tid := uuid.New()
		cliMustExec(t, e.pool, `INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, harness)
		                        VALUES ($1, $2, $3, 'task', $4, 'main', 't', 'd', 'completed', $5)`, tid, e.owner, e.repo, "uzi/task/"+tid.String(), harness)
		return tid
	}
	if o.review {
		add("review_target_run_id", completedTarget(o.harness))
	}
	switch o.kind {
	case "issue", "self_improve":
		add("repo_id", e.repo)
		add("issue_iid", e.nextIID())
	case "ci_fix":
		add("repo_id", e.repo)
		add("pipeline_id", e.nextIID())
		add("pipeline_ref", "agent/issue-"+id.String())
	case "prompt":
		add("repo_id", e.repo)
	case "task":
		add("repo_id", e.repo)
		add("branch", "uzi/task/"+id.String())
	case "mr_rework":
		add("repo_id", e.repo)
		add("pipeline_ref", "agent/issue-"+id.String())
		add("mr_iid", e.nextIID())
		add("target_run_id", completedTarget("claude"))
	case "judge":
		add("target_run_id", completedTarget("claude"))
	case "job":
		add("job_type", "research")
	case "cross_check":
		add("repo_id", e.repo)
		add("target_run_id", completedTarget("codex"))
		add("report_only", true)
		add("budget_wall_seconds", 600)
	case "chat":
	default:
		t.Fatalf("overlayRun: unsupported kind %q", o.kind)
	}
	ph := ""
	for i := range vals {
		if i > 0 {
			ph += ", "
		}
		ph += fmt.Sprintf("$%d", i+1)
	}
	_, err := e.pool.Exec(t.Context(), `INSERT INTO runs (`+cols+`) VALUES (`+ph+`)`, vals...)
	return id, err
}

func sref(s string) *string { return &s }

// post records one row through the worker route (the real handler, fence and refresh path).
func (e *leadOverlayEnv) post(t *testing.T, run uuid.UUID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := workerReq(http.MethodPost, body, run)
	// workerReq stamps a random worker; the fence needs the run's holder.
	req = req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: e.worker, UserID: e.owner}))
	e.h.WorkerRecordLeadConfig(rec, req)
	return rec
}

func (e *leadOverlayEnv) mustPost(t *testing.T, run uuid.UUID, mutate map[string]any) {
	t.Helper()
	if rec := e.post(t, run, leadConfigBody(t, mutate)); rec.Code != http.StatusNoContent {
		t.Fatalf("post %v = %d %s", mutate, rec.Code, rec.Body.String())
	}
}

func (e *leadOverlayEnv) dto(t *testing.T, run uuid.UUID) apitypes.RunDTO {
	t.Helper()
	return getRunDTO(t, e.h, e.viewer, run)
}

func phaseOf(t *testing.T, lc *apitypes.RunLeadConfigDTO, phase string) apitypes.LeadPhaseConfigDTO {
	t.Helper()
	for _, p := range lc.Phases {
		if p.Phase == phase {
			return p
		}
	}
	t.Fatalf("no %s phase in %+v", phase, lc.Phases)
	return apitypes.LeadPhaseConfigDTO{}
}

func strVal(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestGetRunLeadConfigOverlayLiveDB: two generations, both phases, a second plan entry, a truncated
// final row. The overlay shows the highest-seq row per phase of the highest generation that has
// rows, plan first, with `current` following runs.claim_generation and fixed_at derived from the source.
func TestGetRunLeadConfigOverlayLiveDB(t *testing.T) {
	e := newLeadOverlayEnv(t)
	run := e.seed(t, overlayRun{})

	// No rows yet: present (lead lane), generation 0, no phases.
	lc := e.dto(t, run).LeadConfig
	if lc == nil || !lc.Applicable || lc.ClaimGeneration != 0 || len(lc.Phases) != 0 || lc.Current {
		t.Fatalf("run with no rows: %+v, want applicable, generation 0, no phases, not current", lc)
	}

	// Generation 1: plan entry 1 (seq 1), implement entry 1 (seq 1).
	e.mustPost(t, run, map[string]any{"phase": "implement", "seq": 1, "requested_model": "sonnet"})
	e.mustPost(t, run, map[string]any{"phase": "plan", "seq": 1, "requested_model": "opus"})
	lc = e.dto(t, run).LeadConfig
	if lc.ClaimGeneration != 1 || !lc.Current || len(lc.Phases) != 2 {
		t.Fatalf("gen 1 overlay: %+v", lc)
	}
	if lc.Phases[0].Phase != "plan" || lc.Phases[1].Phase != "implement" {
		t.Fatalf("phases ordered %s then %s, want plan first", lc.Phases[0].Phase, lc.Phases[1].Phase)
	}

	// Reclaim: generation 2 records a plan row, a second plan entry (seq 2) and an implement row.
	cliMustExec(t, e.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "plan", "seq": 1, "requested_model": "haiku"})
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "plan", "phase_entry": 2, "seq": 2, "requested_model": "sonnet",
		"requested_model_source": "schedule_pin", "requested_effort": "high", "requested_effort_source": "user_default"})
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "implement", "seq": 1, "requested_model": "opus"})

	lc = e.dto(t, run).LeadConfig
	if lc.ClaimGeneration != 2 || !lc.Current || len(lc.Phases) != 2 {
		t.Fatalf("gen 2 overlay: %+v, want generation 2, current, one row per phase", lc)
	}
	plan, impl := lc.Phases[0], lc.Phases[1]
	if plan.Phase != "plan" || impl.Phase != "implement" {
		t.Fatalf("order = %s, %s; want plan, implement", plan.Phase, impl.Phase)
	}
	if plan.Seq != 2 || plan.PhaseEntry != 2 || strVal(plan.RequestedModel) != "sonnet" {
		t.Errorf("plan row = seq %d entry %d model %s, want the second plan entry (seq 2, entry 2, sonnet)", plan.Seq, plan.PhaseEntry, strVal(plan.RequestedModel))
	}
	if plan.RequestedModelSource != "schedule_pin" || plan.RequestedModelFixedAt != "create" ||
		plan.RequestedEffortSource != "user_default" || plan.RequestedEffortFixedAt != "claim" {
		t.Errorf("plan sources/fixed_at = %s/%s %s/%s, want schedule_pin/create user_default/claim",
			plan.RequestedModelSource, plan.RequestedModelFixedAt, plan.RequestedEffortSource, plan.RequestedEffortFixedAt)
	}
	if impl.Seq != 1 || strVal(impl.RequestedModel) != "opus" || impl.RequestedModelFixedAt != "claim" || impl.Harness != "claude" {
		t.Errorf("implement row = %+v", impl)
	}
	if plan.ReportedAt.IsZero() {
		t.Error("reported_at not set")
	}

	// A truncated final row (seq 32) is the latest implement row and says so.
	e.mustPost(t, run, map[string]any{"claim_generation": 2, "phase": "implement", "seq": 32, "truncated": true, "requested_model": "opus"})
	impl = phaseOf(t, e.dto(t, run).LeadConfig, "implement")
	if impl.Seq != 32 || !impl.Truncated {
		t.Errorf("implement row = seq %d truncated %v, want the truncated seq-32 row", impl.Seq, impl.Truncated)
	}
	if phaseOf(t, e.dto(t, run).LeadConfig, "plan").Truncated {
		t.Error("plan row must not be truncated")
	}

	// A requeue and reclaim to generation 3 with no rows yet: the overlay still shows generation 2,
	// marked not current.
	cliMustExec(t, e.pool, `UPDATE runs SET claim_generation = 3 WHERE id = $1`, run)
	lc = e.dto(t, run).LeadConfig
	if lc.ClaimGeneration != 2 || lc.Current {
		t.Errorf("after reclaim to gen 3 with no rows: generation %d current %v, want 2 and not current", lc.ClaimGeneration, lc.Current)
	}
}

// TestGetRunLeadConfigAcceptanceExamplesLiveDB pins the requested side of PRD #3244 acceptance
// examples 1 to 3 through the real worker route and GetRun.
func TestGetRunLeadConfigAcceptanceExamplesLiveDB(t *testing.T) {
	t.Run("1 default Claude: opus (your default) and medium (built-in)", func(t *testing.T) {
		e := newLeadOverlayEnv(t)
		run := e.seed(t, overlayRun{})
		e.mustPost(t, run, map[string]any{"requested_model": "opus", "requested_model_source": "user_default",
			"requested_effort": "medium", "requested_effort_source": "builtin"})
		p := phaseOf(t, e.dto(t, run).LeadConfig, "plan")
		if strVal(p.RequestedModel) != "opus" || p.RequestedModelSource != "user_default" || p.RequestedModelFixedAt != "claim" ||
			strVal(p.RequestedEffort) != "medium" || p.RequestedEffortMode != "explicit" || p.RequestedEffortSource != "builtin" ||
			p.RequestedEffortFixedAt != "claim" || p.ObservedModel != nil || p.ObservedEffort != nil {
			t.Fatalf("example 1 row = %+v", p)
		}
		// The implement row after approval carries the same requested side.
		e.mustPost(t, run, map[string]any{"phase": "implement", "requested_model": "opus", "requested_model_source": "user_default",
			"requested_effort": "medium", "requested_effort_source": "builtin"})
		i := phaseOf(t, e.dto(t, run).LeadConfig, "implement")
		if strVal(i.RequestedModel) != "opus" || strVal(i.RequestedEffort) != "medium" {
			t.Fatalf("example 1 implement row = %+v", i)
		}
	})

	t.Run("2 Codex schedule pin: gpt-6-astra fixed at create, high from your Codex effort", func(t *testing.T) {
		e := newLeadOverlayEnv(t)
		run := e.seed(t, overlayRun{harness: "codex", model: "gpt-6-astra"})
		e.mustPost(t, run, map[string]any{"requested_model": "gpt-6-astra", "requested_model_source": "schedule_pin",
			"requested_effort": "high", "requested_effort_source": "user_default"})
		p := phaseOf(t, e.dto(t, run).LeadConfig, "plan")
		if p.Harness != "codex" || strVal(p.RequestedModel) != "gpt-6-astra" || p.RequestedModelSource != "schedule_pin" ||
			p.RequestedModelFixedAt != "create" || strVal(p.RequestedEffort) != "high" ||
			p.RequestedEffortSource != "user_default" || p.RequestedEffortFixedAt != "claim" {
			t.Fatalf("example 2 row = %+v", p)
		}
	})

	t.Run("3 reclaim after a settings change shows gen 2 over gen 1", func(t *testing.T) {
		e := newLeadOverlayEnv(t)
		run := e.seed(t, overlayRun{})
		e.mustPost(t, run, map[string]any{"requested_model": "opus", "requested_model_source": "template"})
		cliMustExec(t, e.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
		e.mustPost(t, run, map[string]any{"claim_generation": 2, "requested_model": "sonnet", "requested_model_source": "user_default"})
		lc := e.dto(t, run).LeadConfig
		p := phaseOf(t, lc, "plan")
		if lc.ClaimGeneration != 2 || !lc.Current || strVal(p.RequestedModel) != "sonnet" || p.RequestedModelSource != "user_default" {
			t.Fatalf("example 3 overlay = %+v row %+v, want generation 2 sonnet/user_default", lc, p)
		}
	})
}

// TestLeadLaneAgreementLiveDB crosses every kind of fixtures/run-kinds/registry.json with review
// target and egress binding and asserts, against the independent oracle of leadcfgtest.LaneMatrix,
// that the SQL fence (store.RecordRunLeadConfig), the GetRun overlay presence and leadcfg.LeadLane
// all agree. The claim-key leg of the same table is TestClaimLeadSourceKeysAgreeWithLeadLane in
// workersvc.
func TestLeadLaneAgreementLiveDB(t *testing.T) {
	visited := 0
	for _, lc := range leadcfgtest.LaneMatrix(t) {
		name := fmt.Sprintf("%s/review=%v/egress=%v", lc.Kind, lc.ReviewTarget, lc.EgressBound)
		t.Run(name, func(t *testing.T) {
			// A fresh owner per case: the schema admits one active self_improve run per user.
			e := newLeadOverlayEnv(t)
			harness := "claude"
			model := "opus"
			if lc.Kind == "cross_check" {
				harness, model = "codex", "gpt-6-sol"
			}
			run, serr := e.seedTry(t, overlayRun{kind: lc.Kind, harness: harness, review: lc.ReviewTarget, egress: lc.EgressBound})
			var pgErr *pgconn.PgError
			if errors.As(serr, &pgErr) && pgErr.Code == "23514" {
				// The schema cannot store this combination (a CHECK refuses it), so there is no run to
				// fence or read; the predicate still answers it.
				t.Logf("not storable (%s): asserting the predicate only", pgErr.ConstraintName)
				if got := leadcfg.LeadLane(lc.Kind, lc.ReviewTarget, lc.EgressBound); got != lc.Want {
					t.Errorf("LeadLane = %v, want %v", got, lc.Want)
				}
				return
			}
			if serr != nil {
				t.Fatalf("seed: %v", serr)
			}
			p := store.RecordRunLeadConfigParams{
				RunID: run, WorkerID: e.worker, ClaimGeneration: 1, Phase: "plan", PhaseEntry: 1, Seq: 1,
				RequestedModel: pgtype.Text{Valid: true, String: model}, RequestedModelSource: "user_default",
				RequestedEffort: pgtype.Text{Valid: true, String: "medium"}, RequestedEffortMode: "explicit", RequestedEffortSource: "builtin",
			}
			res, err := e.h.q.RecordRunLeadConfig(t.Context(), p)
			switch {
			case lc.Want && (err != nil || !res.Inserted):
				t.Errorf("fence refused a lead-lane run: %+v %v", res, err)
			case !lc.Want && !errors.Is(err, store.ErrLeadConfigClaimNotCurrent):
				t.Errorf("fence admitted a non-lead-lane run: %+v %v", res, err)
			}
			dto := e.dto(t, run)
			if (dto.LeadConfig != nil) != lc.Want {
				t.Errorf("overlay present = %v, want %v", dto.LeadConfig != nil, lc.Want)
			}
			if lc.Want && (len(dto.LeadConfig.Phases) != 1 || !dto.LeadConfig.Current) {
				t.Errorf("overlay = %+v, want one current row", dto.LeadConfig)
			}
			if got := leadcfg.LeadLane(lc.Kind, lc.ReviewTarget, lc.EgressBound); got != lc.Want {
				t.Errorf("LeadLane = %v, want %v", got, lc.Want)
			}
		})
		visited++
	}
	if visited == 0 {
		t.Fatal("no lane case ran")
	}
}
