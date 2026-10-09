package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These SHAs are fixture commit identities used only to exercise the DB shape.
var codeFixtureHead = strings.Repeat("a", 40)
var codeFixtureBase = strings.Repeat("b", 40)

func codeCrossCheckFixture(t *testing.T) *planCrossCheckDeletionFixture {
	t.Helper()
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	mustExec(ctx, t, f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
	mustExec(ctx, t, f.pool, "UPDATE runs SET code_cross_check_required=true WHERE id=$1", f.runID)
	mustExec(ctx, t, f.pool, `UPDATE workers SET protocol_capabilities = $2, max_cross_check_slots = 1 WHERE id=$1`,
		f.workerID, []string{"cross_check_v1", "cross_check_lane_v1", "cross_check_code_v1", "codex_harness_v1", "codex_runtime_v2"})
	mustExec(ctx, t, f.pool, "UPDATE runs SET worker_id=$2 WHERE id=$1", fx.checkerID, f.workerID)
	return fx
}

func insertCodeFixture(t *testing.T, fx *planCrossCheckDeletionFixture, head, base any, outcome string, reason any) error {
	t.Helper()
	_, err := fx.f.pool.Exec(context.Background(), `INSERT INTO cross_checks
 (id,lead_run_id,checker_run_id,stage,round,lead_claim_generation,verdict,
 head_commit,base_commit,candidate_digest,outcome,reason_class,code_context,
 guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,
 checker_harness,created_at,deadline_at,decided_at)
 VALUES ($1,$2,$3,'code',1,1,'failed',$4,$5,$6,$7,$8,'{}','',
 '',sha256(convert_to('', 'UTF8')),'',sha256(convert_to('', 'UTF8')),'codex',
 now()-interval '10 seconds',now()+interval '5 minutes',
 CASE WHEN $7::text='pending' THEN NULL ELSE now() END)`,
		fx.crossCheckID, fx.f.runID, fx.checkerID, head, base, []byte("fixture digest"), outcome, reason)
	return err
}

func requireCodeCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23514" {
		t.Fatalf("want CHECK violation, got %v", err)
	}
}

func TestCodeCrossCheckIdentityNullShapeLiveDB(t *testing.T) {
	fx := codeCrossCheckFixture(t)
	cases := []struct {
		name       string
		head, base any
		outcome    string
		reason     any
		valid      bool
	}{
		{"pending both null", nil, nil, "pending", nil, false},
		{"completed both null", nil, nil, "completed", nil, false},
		{"pending null head", nil, codeFixtureBase, "pending", nil, false},
		{"pending null base", codeFixtureHead, nil, "pending", nil, false},
		{"completed null head", nil, codeFixtureBase, "completed", nil, false},
		{"completed null base", codeFixtureHead, nil, "completed", nil, false},
		{"null reason", nil, nil, "failed", nil, false},
		{"unknown reason", nil, nil, "failed", "unknown", false},
		{"other terminal reason", nil, nil, "failed", "superseded", false},
		{"pending valid", codeFixtureHead, codeFixtureBase, "pending", nil, true},
		{"completed valid", codeFixtureHead, codeFixtureBase, "completed", nil, true},
		{"failed valid", codeFixtureHead, codeFixtureBase, "failed", "model_error", true},
		{"short head", "abc", codeFixtureBase, "pending", nil, false},
		{"short base", codeFixtureHead, "abc", "pending", nil, false},
		{"nonhex head", strings.Repeat("g", 40), codeFixtureBase, "pending", nil, false},
		{"nonhex base", codeFixtureHead, strings.Repeat("g", 40), "pending", nil, false},
		{"uppercase head", strings.ToUpper(codeFixtureHead), codeFixtureBase, "pending", nil, false},
		{"uppercase base", codeFixtureHead, strings.ToUpper(codeFixtureBase), "pending", nil, false},
	}
	for _, reason := range []string{"worker_unsupported", "snapshot_failed"} {
		cases = append(cases,
			struct {
				name       string
				head, base any
				outcome    string
				reason     any
				valid      bool
			}{reason + " both null", nil, nil, "failed", reason, true},
			struct {
				name       string
				head, base any
				outcome    string
				reason     any
				valid      bool
			}{reason + " null head", nil, codeFixtureBase, "failed", reason, false},
			struct {
				name       string
				head, base any
				outcome    string
				reason     any
				valid      bool
			}{reason + " null base", codeFixtureHead, nil, "failed", reason, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := insertCodeFixture(t, fx, tc.head, tc.base, tc.outcome, tc.reason)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireCodeCheckViolation(t, err)
			}
			mustExec(context.Background(), t, fx.f.pool, "DELETE FROM cross_checks WHERE id=$1", fx.crossCheckID)
		})
	}
}

func TestCodeCrossCheckIdentityRetentionLiveDB(t *testing.T) {
	fx := codeCrossCheckFixture(t)
	ctx := context.Background()
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, "pending", nil); err != nil {
		t.Fatal(err)
	}
	for _, assignment := range []string{
		"head_commit=NULL", "base_commit=NULL", "head_commit=repeat('c',40)", "base_commit=repeat('d',40)",
		"guidance_text='rebound'", "guidance_digest=sha256(convert_to('rebound','UTF8'))",
		"repo_instructions_enabled=true", "repo_instructions_text='rebound'",
		"repo_instructions_digest=sha256(convert_to('rebound','UTF8'))",
		"code_context='{\"rebound\":true}'", "guidance_snapshot='rebound'",
		"candidate_digest=convert_to('rebound','UTF8')", "lead_claim_generation=2",
	} {
		// Each independent rejected write leaves the snapshot for the next assertion.
		_, err := fx.f.pool.Exec(ctx, "UPDATE cross_checks SET "+assignment+" WHERE id=$1", fx.crossCheckID)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "P0001" || !strings.Contains(pe.Message, "immutable code cross-check snapshot") {
			t.Fatalf("%s: expected immutable snapshot, got %v", assignment, err)
		}
	}
	mustExec(ctx, t, fx.f.pool, "UPDATE cross_checks SET outcome='failed',reason_class='model_error',decided_at=now() WHERE id=$1", fx.crossCheckID)
	var head, base string
	if err := fx.f.pool.QueryRow(ctx, "SELECT head_commit,base_commit FROM cross_checks WHERE id=$1", fx.crossCheckID).Scan(&head, &base); err != nil {
		t.Fatal(err)
	}
	if head != codeFixtureHead || base != codeFixtureBase {
		t.Fatalf("failure lost identity: %s %s", head, base)
	}
}

func TestCodeCrossCheckInterruptedAttemptLiveDB(t *testing.T) {
	for _, outcome := range []string{"pending", "completed", "failed", "no snapshot"} {
		for _, exit := range []string{"generation", "worker", "requeue", "failed", "complete"} {
			t.Run(outcome+"/"+exit, func(t *testing.T) {
				ctx := context.Background()
				fx := codeCrossCheckFixture(t)
				f := fx.f
				var head, base any = codeFixtureHead, codeFixtureBase
				var reason any
				state := outcome
				if outcome == "failed" {
					reason = "model_error"
				}
				if outcome == "no snapshot" {
					state = "failed"
					head = nil
					base = nil
					reason = "snapshot_failed"
				}
				if err := insertCodeFixture(t, fx, head, base, state, reason); err != nil {
					t.Fatal(err)
				}
				if state == "completed" {
					mustExec(ctx, t, f.pool, `UPDATE cross_checks SET findings='[{"id":"fixture-finding"}]' WHERE id=$1`, fx.crossCheckID)
				}
				mustExec(ctx, t, f.pool, "UPDATE runs SET status='running',claim_generation=1 WHERE id=$1", fx.checkerID)
				statements := map[string]string{
					"generation": "UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1",
					"worker":     "UPDATE runs SET worker_id=NULL WHERE id=$1",
					"requeue":    "UPDATE runs SET status='queued' WHERE id=$1",
					"failed":     "UPDATE runs SET status='failed' WHERE id=$1",
					"complete":   "UPDATE runs SET status='completed' WHERE id=$1",
				}
				mustExec(ctx, t, f.pool, statements[exit], f.runID)
				var got, child string
				var interrupted, identity bool
				var credit int32
				if err := f.pool.QueryRow(ctx, `SELECT cc.outcome,cc.interrupted_at IS NOT NULL,child.status,
    cc.head_commit IS NOT DISTINCT FROM $2::text AND cc.base_commit IS NOT DISTINCT FROM $3::text,
    lead.budget_paused_seconds FROM cross_checks cc JOIN runs child ON child.id=cc.checker_run_id
    JOIN runs lead ON lead.id=cc.lead_run_id WHERE cc.id=$1`,
					fx.crossCheckID, head, base).Scan(&got, &interrupted, &child, &identity, &credit); err != nil {
					t.Fatal(err)
				}
				preserved := exit == "complete" && state != "pending"
				if preserved {
					if interrupted || got != state || !identity {
						t.Fatalf("successful completion lost evidence: %s interrupted=%v identity=%v", got, interrupted, identity)
					}
					if state == "completed" {
						var evidence bool
						if err := f.pool.QueryRow(ctx, `SELECT findings = '[{"id":"fixture-finding"}]'::jsonb FROM cross_checks WHERE id=$1`, fx.crossCheckID).Scan(&evidence); err != nil {
							t.Fatal(err)
						}
						if !evidence {
							t.Fatal("completion lost findings")
						}
					}
				} else {
					if !interrupted || got != "failed" || !identity {
						t.Fatalf("interruption: outcome=%s interrupted=%v identity=%v", got, interrupted, identity)
					}
					if child != "cancelled" {
						t.Fatalf("pending child not cancelled: %s", child)
					}
					if credit < 10 {
						t.Fatalf("wait not credited: %d", credit)
					}
					_, err := f.q.DecideCodeCrossCheck(ctx, store.DecideCodeCrossCheckParams{
						ChildID: fx.checkerID, WorkerID: pgU(f.workerID), ClaimGeneration: 1, Outcome: "completed", Findings: []byte("[]"),
					})
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("late verdict: %v", err)
					}
					_, err = f.q.BankCodeCrossCheckWait(ctx, fx.crossCheckID)
					if err != nil {
						t.Fatal(err)
					}
					run, err := f.q.GetRunByID(ctx, f.runID)
					if err != nil {
						t.Fatal(err)
					}
					if run.BudgetPausedSeconds != credit {
						t.Fatalf("duplicate wait: %d -> %d", credit, run.BudgetPausedSeconds)
					}
					// A reclaim still cannot submit another attempt, including a new child.
					mustExec(ctx, t, f.pool, "UPDATE runs SET status='running',worker_id=$2,claim_released_at=NULL,claim_generation=claim_generation+1,code_cross_check_required=true WHERE id=$1", f.runID, f.workerID)
					_, err = f.q.CreateCodeCrossCheckChild(ctx, store.CreateCodeCrossCheckChildParams{
						ChildID: fx.checkerID, ChildHarness: "codex", BudgetWallSeconds: 300, LeadRunID: f.runID,
						UserID: f.userID, WorkerID: pgU(f.workerID), ClaimGeneration: run.ClaimGeneration + 1,
					})
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("second attempt child: %v", err)
					}
				}
				if outcome == "no snapshot" {
					var gotReason string
					if err := f.pool.QueryRow(ctx, "SELECT reason_class FROM cross_checks WHERE id=$1", fx.crossCheckID).Scan(&gotReason); err != nil {
						t.Fatal(err)
					}
					if gotReason != "snapshot_failed" {
						t.Fatalf("no-SHA reason replaced: %s", gotReason)
					}
				}
			})
		}
	}
}

func TestCodeCrossCheckLegacyPlanRowsRemainValidLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := setupPlanCrossCheckDeletion(ctx, t)
	f := fx.f
	for _, verdict := range []string{"approve", "revise", "block", "failed"} {
		reason := verdict
		if verdict == "failed" {
			reason = "model_error"
		}
		mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict=$2,reason_class=$3,findings='[]' WHERE id=$1", fx.crossCheckID, verdict, reason)
	}
	for _, assignment := range []string{
		"outcome='failed'", "head_commit=repeat('a',40)", "code_context='{}'",
		"guidance_snapshot=''", "guidance_text=''", "guidance_digest=sha256(convert_to('','UTF8'))",
		"repo_instructions_enabled=true", "repo_instructions_text=''", "repo_instructions_digest=sha256(convert_to('','UTF8'))",
		"dispositions='[]'", "finalized_at=now()",
	} {
		_, err := f.pool.Exec(ctx, "UPDATE cross_checks SET "+assignment+" WHERE id=$1", fx.crossCheckID)
		requireCodeCheckViolation(t, err)
	}
	// The pre-code schema and consent defaults continue accepting ordinary leads.
	run, err := f.q.GetRunByID(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CodeCrossCheckRequired {
		t.Fatal("legacy lead inherited code consent")
	}
}

func TestCodeCrossCheckPendingWaitReadersLiveDB(t *testing.T) {
	ctx := context.Background()
	fx := codeCrossCheckFixture(t)
	f := fx.f
	if err := insertCodeFixture(t, fx, codeFixtureHead, codeFixtureBase, "pending", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	mustExec(ctx, t, f.pool, "UPDATE runs SET started_at=$2,budget_wall_seconds=60,completion_attempts=0 WHERE id=$1", f.runID, now.Add(-65*time.Second))
	params := store.SetRunWallParkParams{ID: f.runID, WorkerID: pgU(f.workerID), Now: pgtype.Timestamptz{Time: now, Valid: true}, GlobalTimeoutSeconds: 60, ClaimGeneration: pgtype.Int8{Int64: 1, Valid: true}}
	if _, err := f.q.SetRunWallPark(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pending code wait must exclude wall park: %v", err)
	}
	// Both stage records may coexist; a scalar wait subquery must not fail.
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,size_class,base_commit,candidate_digest,created_at,deadline_at)
 VALUES($1,'plan',1,1,'plan','[]','m',$2,$3,now()-interval '20 seconds',now()+interval '5 minutes')`, f.runID, codeFixtureBase, []byte("plan fixture digest"))
	if _, err := f.q.SetRunWallPark(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pending waits in both stages must remain readable: %v", err)
	}
}
