package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Capture the statement sqlc actually executes, including its ordered arguments.
// This avoids a second hand-written copy of the production SQL in the plan test.
type templateRoutingPlanCapture struct {
	store.DBTX
	statement string
	arguments []any
}

func (c *templateRoutingPlanCapture) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.statement, c.arguments = sql, args
	return c.DBTX.Query(ctx, sql, args...)
}

func (c *templateRoutingPlanCapture) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.statement, c.arguments = sql, args
	return c.DBTX.QueryRow(ctx, sql, args...)
}

func TestLeadTemplateRoutingPlanLiveDB(t *testing.T) {
	first := newCrossCheckContentionFixture(t)
	second := newCrossCheckContentionFixture(t)
	var priorOwners int
	if err := first.env.pool.QueryRow(first.env.ctx, `SELECT count(DISTINCT user_id) FROM runs
        WHERE kind='cross_check' AND harness='codex' AND status IN ('queued','claimed','running','awaiting_approval')
          AND user_id<>$1 AND user_id<>$2`, first.userID, second.userID).Scan(&priorOwners); err != nil {
		t.Fatal(err)
	}
	// Shared sweeps retain other fixtures. Ensure the old per-child scan cannot
	// hide behind their owners, even when those owners have higher-precedence pins.
	fixtureChildren := max(32, priorOwners+16)
	for i, f := range []crossCheckContentionFixture{first, second} {
		model := "gpt-6-luna"
		if i == 1 {
			model = "gpt-6-astra"
		}
		setRoutingLeadTemplate(f, model, false)
		f.env.exec("UPDATE users SET default_codex_model=NULL WHERE id=$1", f.userID)
		f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, checkerPinCaps(true, false))
		e := checkerPlacementEnv(t, f)
		leadWorker := uuid.UUID(mustRun(t, f.env, f.lead).WorkerID.Bytes)
		for i := 1; i < fixtureChildren; i++ {
			lead := e.seedActiveRunOwnedBy(t, leadWorker)
			f.env.exec("UPDATE runs SET auto_approve=true,plan_cross_check_required=true,claim_generation=1 WHERE id=$1", lead)
			child := uuid.New()
			f.env.exec(`INSERT INTO runs(id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,harness,target_run_id,report_only,budget_wall_seconds)
                VALUES($1,$2,$3,NULL,'checker plan','body','cross_check','queued','codex',$4,true,1800)`, child, f.userID, e.repoID, lead)
			f.env.exec(`INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,checker_run_id,checker_harness,deadline_at)
                VALUES($1,'plan',1,1,'plan','[]','s',$2,$3,$4,'codex',now()+interval '1 hour')`, lead, strings.Repeat("a", 40), []byte(strings.Repeat("a", 32)), child)
		}
	}
	if _, err := first.env.pool.Exec(first.env.ctx, "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"health", "claim"} {
		t.Run(query, func(t *testing.T) {
			tx, err := first.env.pool.BeginTx(first.env.ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			exec := func(sql string) {
				t.Helper()
				if _, err := tx.Exec(first.env.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
					t.Fatal(err)
				}
			}
			exec("SET LOCAL plan_cache_mode = force_generic_plan")
			// This is a row-work assertion, not a timing/JIT test. Avoid spending
			// CPU compiling the deliberately bad control plan in mutation checks.
			exec("SET LOCAL jit = off")
			exec("SAVEPOINT capture")
			capture := &templateRoutingPlanCapture{DBTX: tx}
			q := store.New(capture)
			if query == "health" {
				rows, err := q.ListActiveRunsForHealth(first.env.ctx, codexCuratedModelsSlice())
				if err != nil {
					t.Fatal(err)
				}
				seen := 0
				for _, row := range rows {
					if row.ID == first.runID || row.ID == second.runID {
						if row.CodexCustomRoot != (row.ID == first.runID) {
							t.Fatal("health lost the distinct owners' template pins")
						}
						seen++
					}
				}
				if seen != 2 {
					t.Fatalf("health returned %d fixture checkers, want both owners", seen)
				}
				exec("SAVEPOINT status_edges")
				for _, status := range []string{"claimed", "running", "awaiting_approval"} {
					if _, err := tx.Exec(first.env.ctx, `UPDATE runs SET status=$1,claim_generation=1,
                        worker_id=CASE WHEN user_id=$2 THEN $3::uuid ELSE $4::uuid END
                        WHERE kind='cross_check' AND user_id IN ($2,$5)`, status, first.userID, first.workerID, second.workerID, second.userID); err != nil {
						t.Fatal(err)
					}
					edges, err := q.ListActiveRunsForHealth(first.env.ctx, codexCuratedModelsSlice())
					if err != nil {
						t.Fatal(err)
					}
					seen = 0
					for _, row := range edges {
						if row.ID == first.runID || row.ID == second.runID {
							if row.CodexCustomRoot != (row.ID == first.runID) {
								t.Fatalf("%s health lost owner template model", status)
							}
							seen++
						}
					}
					if seen != 2 {
						t.Fatalf("%s health returned %d fixture checkers", status, seen)
					}
				}
				exec("ROLLBACK TO SAVEPOINT status_edges")
			} else {
				e := checkerPlacementEnv(t, first)
				_, err := q.ClaimRun(first.env.ctx, e.claimParams(first.workerID, checkerPinCaps(true, false), false))
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("incapable worker must not claim a template-only Luna root: %v", err)
				}
			}
			exec("ROLLBACK TO SAVEPOINT capture")
			var ownerScope pgtype.UUID
			if query == "claim" {
				ownerScope = pgtype.UUID{Bytes: first.userID, Valid: true}
			}
			var owners, templates int64
			if err := tx.QueryRow(first.env.ctx, `SELECT count(DISTINCT user_id) FROM runs
                WHERE kind='cross_check' AND harness='codex' AND status IN ('queued','claimed','running','awaiting_approval')
                  AND ($1::uuid IS NULL OR user_id=$1)`, ownerScope).Scan(&owners); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(first.env.ctx, "SELECT count(*) FROM agent_templates").Scan(&templates); err != nil {
				t.Fatal(err)
			}
			if owners == 0 || templates == 0 || capture.statement == "" {
				t.Fatal("empty plan fixture or uncaptured SQL")
			}
			desc, err := tx.Conn().Prepare(first.env.ctx, "template_routing_plan", capture.statement)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Conn().Deallocate(context.Background(), "template_routing_plan") }()
			if len(desc.ParamOIDs) != len(capture.arguments) {
				t.Fatal("captured SQL argument count mismatch")
			}
			var literals []string
			for i, arg := range capture.arguments {
				encoded, err := tx.Conn().TypeMap().Encode(desc.ParamOIDs[i], pgtype.TextFormatCode, arg, nil)
				if err != nil {
					t.Fatalf("encode argument %d: %v", i, err)
				}
				if encoded == nil {
					literals = append(literals, "NULL")
				} else {
					literals = append(literals, "'"+strings.ReplaceAll(string(encoded), "'", "''")+"'")
				}
			}
			var raw []byte
			if err := tx.QueryRow(first.env.ctx, "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE template_routing_plan("+strings.Join(literals, ",")+")", pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var generic int64
			if err := tx.QueryRow(first.env.ctx, "SELECT generic_plans FROM pg_prepared_statements WHERE name='template_routing_plan'").Scan(&generic); err != nil || generic == 0 {
				t.Fatalf("generic plan not executed: count=%d err=%v", generic, err)
			}
			var doc []struct {
				Plan map[string]any `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
				t.Fatalf("invalid EXPLAIN: %v", err)
			}
			selectionNodes, selectionLoops := 0, float64(0)
			var walk func(map[string]any)
			walk = func(node map[string]any) {
				if node["Relation Name"] == "agent_templates" {
					alias, _ := node["Alias"].(string)
					loops, _ := node["Actual Loops"].(float64)
					if alias == "t" || strings.HasPrefix(alias, "t_") {
						selectionNodes++
						selectionLoops += loops
					} else {
						rows, _ := node["Actual Rows"].(float64)
						filtered, _ := node["Rows Removed by Filter"].(float64)
						if work := (rows + filtered) * loops; work > float64(owners*templates) {
							t.Errorf("shadow scan work %.0f exceeds owners x templates %d", work, owners*templates)
						}
					}
				}
				children, _ := node["Plans"].([]any)
				for _, child := range children {
					walk(child.(map[string]any))
				}
			}
			walk(doc[0].Plan)
			if selectionNodes == 0 || selectionLoops == 0 || selectionLoops > float64(owners) || selectionLoops >= float64(fixtureChildren) {
				t.Fatalf("template selection nodes=%d loops=%.0f exceeds distinct scoped owners=%d", selectionNodes, selectionLoops, owners)
			}
			t.Logf("%s: template selection loops=%.0f scoped owners=%d, prior owners=%d children per fixture=%d generic plans=%d", query, selectionLoops, owners, priorOwners, fixtureChildren, generic)
		})
	}
}
