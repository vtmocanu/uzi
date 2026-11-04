package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Install exactly the delivered lead rows used by the routing regression. Global
// defaults are disabled for this user so the test does not depend on the roster.
func setRoutingLeadTemplate(f crossCheckContentionFixture, model any, nullFirst bool) {
	f.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
    SELECT id,$1,false FROM agent_templates WHERE true
    ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid)))
    DO UPDATE SET enabled=false`, f.userID)
	id := uuid.New()
	f.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,codex_model)
    VALUES($1,'ORCHESTRATOR','d','b','user',$2,$3)`, id, f.userID, model)
	f.env.exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)", id, f.userID)
	if nullFirst {
		id = uuid.New()
		f.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id)
        VALUES($1,'LEAD','d','b','user',$2)`, id, f.userID)
		f.env.exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)", id, f.userID)
	}
}

func assertTemplateEphemeralRouting(t *testing.T, f crossCheckContentionFixture, allowed bool) {
	t.Helper()
	e := checkerPlacementEnv(t, f)
	f.env.exec("UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.userID)
	f.env.exec("UPDATE workers SET max_concurrent_runs=1 WHERE id=$1", f.workerID)
	f.env.exec("UPDATE runs SET status_since=now()-interval '5 minutes' WHERE id=$1", f.runID)
	check := func(saturated bool) {
		t.Helper()
		now := time.Now()
		u, err := e.q.ListUnplaceableQueuedRunsForEphemeral(e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: now, Valid: true}, CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
			EphemeralLease: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		unplaceable := false
		for _, r := range u {
			unplaceable = unplaceable || r.ID == f.runID
		}
		s, err := e.q.ListSaturationQueuedRunsForEphemeral(e.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: now, Valid: true}, CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
			EphemeralLease: pgtype.Interval{Valid: true}, SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		foundSaturated := false
		for _, r := range s {
			foundSaturated = foundSaturated || r.ID == f.runID
		}
		if unplaceable != !allowed || foundSaturated != saturated {
			t.Errorf("ephemeral unplaceable/saturated=%v/%v, want %v/%v", unplaceable, foundSaturated, !allowed, saturated)
		}
	}
	check(false)
	busy := e.seedActiveRunOwnedBy(t, f.workerID)
	check(allowed)
	f.env.exec("UPDATE runs SET status='completed' WHERE id=$1", busy)
}

func TestLeadTemplateRoutingLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name                  string
			template, lane, pin   any
			customCap, customRoot bool
			nullFirst, disabled   bool
		}{
			{"template-luna-incapable", "gpt-6-luna", nil, nil, false, true, false, false},
			{"template-luna-capable", "gpt-6-luna", nil, nil, true, true, false, false},
			{"curated-template", "gpt-6-astra", nil, nil, false, false, false, false},
			{"null-template", nil, nil, nil, false, false, false, false},
			{"invalid-template", "not-a-role-model", nil, nil, false, false, false, false},
			{"noncanonical-template", " gpt-6-luna ", nil, nil, false, false, false, false},
			{"lane-outranks-template", "gpt-6-luna", "gpt-6-sol", nil, false, false, false, false},
			{"checker-pin-outranks-template", "gpt-6-luna", nil, "gpt-6-astra", false, false, false, false},
			{"first-null-does-not-skip", "gpt-6-luna", nil, nil, false, false, true, false},
			{"disabled-template", "gpt-6-luna", nil, nil, false, false, false, true},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				var f crossCheckContentionFixture
				if stage == "code" {
					f = codeFixture(t)
					cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
					if err != nil || !cc.CheckerRunID.Valid {
						t.Fatalf("submit routing code checker: %+v %v", cc, err)
					}
					f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
				} else {
					f = newCrossCheckContentionFixture(t)
				}
				setRoutingLeadTemplate(f, tc.template, tc.nullFirst)
				if tc.disabled {
					f.env.exec("UPDATE agent_template_allocations SET enabled=false WHERE user_id=$1", f.userID)
				}
				f.env.exec("UPDATE users SET default_codex_model=$2 WHERE id=$1", f.userID, tc.lane)
				f.env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,$2,'codex',$3)", f.userID, stage, tc.pin)
				caps := checkerPinCaps(true, tc.customCap)
				if stage == "code" {
					caps = append(caps, capability.CrossCheckCodeV1, capability.CrossCheckLaneV1)
				}
				f.env.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1", f.workerID, caps)
				e := checkerPlacementEnv(t, f)
				if h := e.healthRowFor(t, f.runID); h.CodexCustomRoot != tc.customRoot {
					t.Errorf("health custom root=%v, want %v", h.CodexCustomRoot, tc.customRoot)
				}
				allowed := !tc.customRoot || tc.customCap
				wantCount := int64(0)
				if allowed {
					wantCount = 1
				}
				if n := e.claimableForRun(t, f.runID); n != wantCount {
					t.Errorf("claimable workers=%d, want %d", n, wantCount)
				}
				if stage == "plan" {
					assertTemplateEphemeralRouting(t, f, allowed)
				}
				params := e.claimParams(f.workerID, caps, false)
				if stage == "code" {
					params.Lane = "cross_check"
				}
				r, err := e.q.ClaimRun(e.ctx, params)
				if allowed {
					if err != nil || r.ID != f.runID {
						t.Fatalf("eligible worker did not claim checker: id=%v err=%v", r.ID, err)
					}
				} else if !errors.Is(err, pgx.ErrNoRows) || mustRun(t, f.env, f.runID).Status != "queued" {
					t.Fatalf("incapable worker claimed template-derived root: id=%v err=%v", r.ID, err)
				}
			})
		}
	}
}

func TestLeadTemplateSpreadPeerRoutingLiveDB(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("peer-custom-capability=%v", custom), func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			setRoutingLeadTemplate(f, "gpt-6-luna", false)
			f.env.exec("UPDATE users SET default_codex_model=NULL WHERE id=$1", f.userID)
			caps := checkerPinCaps(true, true)
			f.env.exec("UPDATE workers SET protocol_capabilities=$2,max_concurrent_runs=2,last_heartbeat_at=now() WHERE id=$1", f.workerID, caps)
			e := checkerPlacementEnv(t, f)
			e.seedActiveRunOwnedBy(t, f.workerID)
			e.seedSpreadWorker(t, checkerPinCaps(true, custom), 2)
			r, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, caps, false))
			if custom {
				if !errors.Is(err, pgx.ErrNoRows) || mustRun(t, f.env, f.runID).Status != "queued" {
					t.Fatalf("claimant did not defer to capable idle peer: id=%v err=%v", r.ID, err)
				}
			} else if err != nil || r.ID != f.runID {
				t.Fatalf("claimant deferred to incapable template-root peer: id=%v err=%v", r.ID, err)
			}
		})
	}
}

func TestLeadTemplateSelectionLiveDB(t *testing.T) {
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	raw, err := os.ReadFile("../../../fixtures/lead-template-selection/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			ID            string   `json:"id"`
			Names         []string `json:"names"`
			SelectedIndex *int     `json:"selected_index"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("empty lead selection corpus")
	}
	exercised := 0
	for _, c := range corpus.Cases {
		for _, stage := range []string{"plan", "code"} {
			t.Run(c.ID+"/"+stage, func(t *testing.T) {
				var f crossCheckContentionFixture
				if stage == "plan" {
					f = newCrossCheckContentionFixture(t)
				} else {
					f = codeFixture(t)
					cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
					if err != nil || !cc.CheckerRunID.Valid {
						t.Fatalf("submit selection code checker: %+v %v", cc, err)
					}
					f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
				}
				ctx, cancel := context.WithTimeout(f.env.ctx, 15*time.Second)
				defer cancel()
				tx, err := f.env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					rollbackCtx, done := context.WithTimeout(context.Background(), 3*time.Second)
					defer done()
					if err := tx.Rollback(rollbackCtx); err != nil {
						t.Errorf("selection rollback: %v", err)
					}
				}()
				q := f.env.q.WithTx(tx)
				exec := func(sql string, args ...any) {
					t.Helper()
					if _, err := tx.Exec(ctx, sql, args...); err != nil {
						t.Fatal(err)
					}
				}
				// Disable every existing allocation for this viewer. Shared rows are reused
				// only inside this transaction, whose rollback restores their original pins.
				exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
    SELECT id,$1,false FROM agent_templates WHERE true ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid))) DO UPDATE SET enabled=false`, f.userID)
				for i, name := range c.Names {
					id := uuid.New()
					var shared uuid.UUID
					err := tx.QueryRow(ctx, "SELECT id FROM agent_templates WHERE scope <> 'user' AND name=$1", name).Scan(&shared)
					switch err {
					case nil:
						id = shared
					case pgx.ErrNoRows:
						exec("INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id) VALUES($1,$2,'d','b','user',$3)", id, name, f.userID)
					default:
						t.Fatal(err)
					}
					models := []string{"gpt-6-luna", "gpt-6-astra", "gpt-6-sol", "gpt-5.6-sol", "gpt-6.1-sol"}
					exec("UPDATE agent_templates SET model=$2,codex_model=$3 WHERE id=$1", id, fmt.Sprintf("claude-selection-%d", i), models[i%len(models)])
					exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)
     ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid))) DO UPDATE SET enabled=true`, id, f.userID)
				}
				exec("UPDATE runs SET status='claimed',worker_id=$2,claim_generation=1,claim_released_at=NULL WHERE id=$1", f.runID, f.workerID)
				delivered, err := q.ListClaimAgentTemplates(ctx, pgconv.UUID(f.userID))
				if err != nil {
					t.Fatal(err)
				}
				names := make([]string, len(delivered))
				for i := range delivered {
					names[i] = delivered[i].Name
				}
				idx, found := agenttmpl.SelectLead(names)
				if len(delivered) != len(c.Names) || found != (c.SelectedIndex != nil) {
					t.Fatalf("fixture delivery=%v found=%v; supplied=%v selected=%v", names, found, c.Names, c.SelectedIndex)
				}
				// selected_index belongs to supplied-order unit tests. SQL expectations
				// come from the actual native delivery order, including the database locale.
				t.Logf("case=%s supplied_index=%v native_order=%v selected=%d found=%v", c.ID, c.SelectedIndex, names, idx, found)
				check := func(t *testing.T) {
					t.Helper()
					var wantClaude, wantCodex pgtype.Text
					if found {
						wantClaude, wantCodex = delivered[idx].Model, delivered[idx].CodexModel
					}
					rows, err := q.ListUserCrossCheckPins(ctx, f.userID)
					if err != nil {
						t.Fatal(err)
					}
					if len(rows) != 4 {
						t.Fatalf("settings cells=%d, want four", len(rows))
					}
					for _, row := range rows {
						if row.TemplateModel != wantClaude || row.TemplateCodexModel != wantCodex {
							t.Fatalf("%s/%s lateral=(%+v,%+v), delivered=(%+v,%+v)", row.Stage, row.Harness, row.TemplateModel, row.TemplateCodexModel, wantClaude, wantCodex)
						}
					}
					snap, err := q.GetCrossCheckPinSnapshot(ctx, store.GetCrossCheckPinSnapshotParams{
						ChildID: f.runID, UserID: f.userID, WorkerID: pgconv.UUID(f.workerID), ClaimGeneration: 1})
					if err != nil {
						t.Fatal(err)
					}
					if snap.Stage != stage || snap.TemplateModel != wantClaude || snap.TemplateCodexModel != wantCodex {
						t.Fatalf("claim snapshot=%+v, delivered pins=(%+v,%+v)", snap, wantClaude, wantCodex)
					}
				}
				check(t)
				leads := 0
				for _, name := range names {
					if agenttmpl.IsLeadName(name) {
						leads++
					}
				}
				if leads >= 2 {
					t.Run("null-first", func(t *testing.T) {
						if _, err := tx.Exec(ctx, "UPDATE agent_templates SET model=NULL,codex_model=NULL WHERE id=$1", delivered[idx].ID); err != nil {
							t.Fatal(err)
						}
						delivered[idx].Model, delivered[idx].CodexModel = pgtype.Text{}, pgtype.Text{}
						check(t)
						t.Logf("case=%s stage=%s first=%s both pins NULL; %d later lead candidates remain", c.ID, stage, delivered[idx].Name, leads-1)
					})
				}
				exercised++
			})
		}
	}
	if exercised != 2*len(corpus.Cases) || exercised == 0 {
		t.Fatalf("exercised=%d stage cases=%d", exercised, 2*len(corpus.Cases))
	}
	t.Logf("exercised %d shared selection stage cases", exercised)
}

// Claude has no Codex capability mint. Edit after its real SQL snapshot read,
// before returning that row to assembly, to test the same immutable input boundary.
type leadTemplateSnapshotEditStore struct {
	*store.Queries
	edit func()
}

func (q *leadTemplateSnapshotEditStore) GetCrossCheckPinSnapshot(ctx context.Context, p store.GetCrossCheckPinSnapshotParams) (store.GetCrossCheckPinSnapshotRow, error) {
	row, err := q.Queries.GetCrossCheckPinSnapshot(ctx, p)
	if err == nil {
		q.edit()
	}
	return row, err
}

func TestLeadTemplatePinsLiveDB(t *testing.T) {
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	exercised := 0
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name, harness, lane, effort, wantModel, wantEffort, effortSource string
		}{
			{"codex-template", "codex", "", "", "gpt-6-luna", "high", "cross-check default"},
			{"codex-lane", "codex", "gpt-6-sol", "low", "gpt-6-sol", "low", "pin"},
			{"claude-legacy-luna", "claude", "", "low", "", "low", "pin"},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				var f crossCheckContentionFixture
				var claim func() *ClaimPayload
				if stage == "plan" && tc.harness == "claude" {
					fx := newClaudeCheckerFix(t, BindModePinned)
					f = crossCheckContentionFixture{codexRunFixture: codexRunFixture{svc: fx.svc, userID: fx.userID, workerID: fx.workerID, runID: fx.kid}, env: fx.env, lead: fx.lead}
					claim = func() *ClaimPayload { return fx.claim(t) }
				} else if stage == "plan" {
					f = newCrossCheckContentionFixture(t)
					claim = func() *ClaimPayload { return f.claim(t, false, "", uuid.Nil) }
				} else {
					if tc.harness == "claude" {
						f, _ = newClaudeCodeCheckerFix(t)
					} else {
						f = codeFixture(t)
						cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
						if err != nil || !cc.CheckerRunID.Valid {
							t.Fatalf("submit code checker: %+v %v", cc, err)
						}
						f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
					}
					claim = func() *ClaimPayload { return laneClaim(t, f, "cross_check", nil) }
				}
				f.env.exec(`UPDATE workers SET protocol_capabilities=protocol_capabilities || $2::text[] WHERE id=$1`,
					f.workerID, append(checkerPinCaps(true, true), capability.CrossCheckCodexLeadV1))
				f.env.exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled)
     SELECT id,$1,false FROM agent_templates WHERE lower(name) IN ('lead','orchestrator')
     ON CONFLICT (template_id,(COALESCE(user_id,'00000000-0000-0000-0000-000000000000'::uuid))) DO UPDATE SET enabled=false`, f.userID)
				id := uuid.New()
				claudePin := "claude-custom-template"
				if tc.harness == "claude" {
					claudePin = "gpt-6-luna"
				}
				f.env.exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model,codex_model)
     VALUES($1,'ORCHESTRATOR','d','b','user',$2,$3,'gpt-6-luna')`, id, f.userID, claudePin)
				f.env.exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)", id, f.userID)
				f.env.exec("UPDATE users SET default_claude_model=NULL,default_codex_model=NULL,default_effort='max',default_codex_effort='medium' WHERE id=$1", f.userID)
				if tc.lane != "" {
					f.env.exec("UPDATE users SET default_codex_model=$2 WHERE id=$1", f.userID, tc.lane)
				}
				if tc.effort != "" {
					f.env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,$2,$3,$4)", f.userID, stage, tc.harness, tc.effort)
				}
				edited := 0
				edit := func() {
					edited++
					f.env.exec("UPDATE agent_templates SET model='haiku',codex_model='gpt-6-astra' WHERE id=$1", id)
					f.env.exec("UPDATE users SET default_claude_model='sonnet',default_codex_model='gpt-5.6-sol' WHERE id=$1", f.userID)
				}
				if tc.harness == "codex" {
					f.svc.claimHooks = &claimTestHooks{afterMint: func(context.Context, store.Run) { edit() }}
				} else {
					f.svc.q = &leadTemplateSnapshotEditStore{Queries: f.env.q, edit: edit}
				}
				p := claim()
				if edited != 1 || p == nil || p.RunID != f.runID.String() || p.CrossCheck == nil || p.CrossCheck.Stage != stage {
					t.Fatalf("edit count=%d payload=%+v", edited, p)
				}
				if tc.wantModel == "" {
					if p.Config.DefaultModel != nil {
						t.Fatalf("legacy Luna reached Claude: %q", *p.Config.DefaultModel)
					}
				} else if p.Config.DefaultModel == nil || *p.Config.DefaultModel != tc.wantModel {
					t.Fatalf("delivered model=%v want=%s", p.Config.DefaultModel, tc.wantModel)
				}
				if p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != tc.wantEffort ||
					p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != "worker default" ||
					p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != tc.effortSource {
					t.Fatalf("effort/provenance changed: config=%+v cross-check=%+v", p.Config, p.CrossCheck)
				}
				var model, effort, modelSource, effortSource pgtype.Text
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT checker_model,checker_effort,checker_model_source,checker_effort_source
     FROM cross_checks WHERE checker_run_id=$1 AND stage=$2`, f.runID, stage).Scan(&model, &effort, &modelSource, &effortSource); err != nil {
					t.Fatal(err)
				}
				if model.Valid != (tc.wantModel != "") || model.String != tc.wantModel || !effort.Valid || effort.String != tc.wantEffort ||
					!modelSource.Valid || modelSource.String != "worker default" || !effortSource.Valid || effortSource.String != tc.effortSource {
					t.Fatalf("stored snapshot=(%+v,%+v,%+v,%+v)", model, effort, modelSource, effortSource)
				}
				r := mustRun(t, f.env, f.runID)
				if r.Status != "claimed" || r.Harness != tc.harness || !r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.workerID ||
					r.CrossCheckLane != (stage == "code") {
					t.Fatalf("placement changed: %+v", r)
				}
				t.Logf("stage=%s case=%s immutable model=%q effort=%s", stage, tc.name, tc.wantModel, tc.wantEffort)
				exercised++
			})
		}
	}
	if exercised != 6 {
		t.Fatalf("exercised %d claim cases, want six", exercised)
	}
	t.Logf("exercised %d stage-local service claims", exercised)
}
