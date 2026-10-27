package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Observes generated queries while forwarding every read to the real transaction.
// It does not fabricate enrollment, history, or query results.
type stopFrozenObservedTx struct {
	pgx.Tx
	enrollmentIDs []uuid.UUID
	history       []store.GetSemanticCandidateByDigestParams
}

func (tx *stopFrozenObservedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: GetSemanticPlanning :one") {
		tx.enrollmentIDs = append(tx.enrollmentIDs, args[0].(uuid.UUID))
	}
	if strings.HasPrefix(sql, "-- name: GetSemanticCandidateByDigest :one") {
		tx.history = append(tx.history, store.GetSemanticCandidateByDigestParams{RunID: args[0].(uuid.UUID), Column2: args[1].(string)})
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// Identity component only: saved execution is supplied directly. No fixture row
// claims API acceptance, authenticated policy source, or Claim/ACK permission.
// The caller's receipt/source/gate authentication remains a separate required seam.
func TestPolicyStopFrozenExecutionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	owner := seedReevalOwner(t, env, BindModeAuto, false)
	runID := seedRunningRun(t, env, owner, 9681, 0)
	for _, mode := range []string{"gated_B_frozen_A", "missing_A_history", "query_failure"} {
		t.Run(mode, func(t *testing.T) {
			r, saved, identity := stopFrozenFixture(t, "semantic")
			contract, err := parseCompletionContract(r.CompletionContract)
			if err != nil {
				t.Fatal(err)
			}
			a := contract.SemanticPresentation.Candidate
			b := stopFrozenDifferentCandidate(t, 2)
			// Mutable C is not history A and must never be used as execution evidence.
			c := stopFrozenDifferentCandidate(t, 3)
			r.PlanMd.String = "different unapproved B"
			r.MilestonesCandidate = []byte(`[{"id":"b1","title":"Different gate"}]`)
			p := semanticGateProjection(&semanticCoverageTx{candidate: b}, r)
			r.GatePresentedPayload = semanticJSON(t, gatePresentedPayload{PlanMd: &r.PlanMd.String, Milestones: r.MilestonesCandidate, Semantic: &p})
			rawTx, err := env.pool.BeginTx(env.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rawTx.Rollback(env.ctx) }()
			tx := &stopFrozenObservedTx{Tx: rawTx}
			q := store.New(tx)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := tx.Exec(env.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			// These are contract identity fixtures, not an owner approval receipt.
			exec(`UPDATE runs SET status='awaiting_approval',auto_approve=false,claim_generation=1,
    completion_contract_version=1,contract_revision=1,completion_contract=$2,
    milestones_frozen=$3,milestones_completed=$4,scope_ceiling=1,
    plan_md=$5,milestones_candidate=$6,gate_revision=2,gate_presented_payload=$7
    WHERE id=$1`, runID, r.CompletionContract, r.MilestonesFrozen, r.MilestonesCompleted, r.PlanMd.String, r.MilestonesCandidate, r.GatePresentedPayload)
			exec(`INSERT INTO semantic_planning(run_id,enrollment,revision,candidate) VALUES($1,$2,3,$3)`, runID, identity.planning.Enrollment, semanticJSON(t, c))
			if mode != "missing_A_history" {
				if err := q.InsertSemanticCandidateHistory(env.ctx, store.InsertSemanticCandidateHistoryParams{RunID: runID, Revision: a.Revision, Candidate: semanticJSON(t, a)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.InsertSemanticCandidateHistory(env.ctx, store.InsertSemanticCandidateHistoryParams{RunID: runID, Revision: b.Revision, Candidate: semanticJSON(t, b)}); err != nil {
				t.Fatal(err)
			}
			locked, err := q.GetRunByIDForUpdate(env.ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			before := semanticJSON(t, locked)
			// Read the real lack of approval; a false projection is fixture state, not
			// evidence that this helper has authenticated the current gate.
			approved, err := q.HasPolicyCurrentGateApproval(env.ctx, store.HasPolicyCurrentGateApprovalParams{RunID: runID, GateRevision: locked.GateRevision})
			if err != nil || approved {
				t.Fatalf("unapproved fixture: approved=%v err=%v", approved, err)
			}
			if mode == "query_failure" {
				_, err = tx.Exec(env.ctx, "SELECT 1/0")
				var sqlErr *pgconn.PgError
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
					t.Fatalf("failed to establish aborted transaction: %v", err)
				}
			}
			target, valid, err := policyStopFrozenExecutionTarget(env.ctx, q, locked, saved, false)
			if mode == "query_failure" {
				var sqlErr *pgconn.PgError
				if valid || target != nil || !errors.As(err, &sqlErr) || sqlErr.Code != "25P02" {
					t.Fatalf("query failure lost: target=%v valid=%v err=%v", target, valid, err)
				}
				return
			}
			if err != nil || valid != (mode == "gated_B_frozen_A") {
				t.Fatalf("identity valid=%v err=%v", valid, err)
			}
			if mode == "gated_B_frozen_A" && (target == nil || target.Digest != a.Digest) {
				t.Fatal("execution substituted B or C")
			}
			if mode == "missing_A_history" && target != nil {
				t.Fatal("missing A recovered from mutable planning")
			}
			if !reflect.DeepEqual(tx.enrollmentIDs, []uuid.UUID{runID}) ||
				!reflect.DeepEqual(tx.history, []store.GetSemanticCandidateByDigestParams{{RunID: runID, Column2: a.Digest}}) {
				t.Fatalf("identity reads: enrollment=%v history=%+v", tx.enrollmentIDs, tx.history)
			}
			after, err := q.GetRunByIDForUpdate(env.ctx, runID)
			if err != nil || !reflect.DeepEqual(before, semanticJSON(t, after)) {
				t.Fatalf("identity check changed locked row: %v", err)
			}
			planning, err := q.GetSemanticPlanning(env.ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			var current apitypes.SemanticCandidate
			// jsonb can reorder keys; compare typed candidates, not storage formatting.
			if err := strictSemanticJSON(planning.Candidate, &current); err != nil {
				t.Fatal(err)
			}
			if !sameSemanticJSON(current, c) {
				t.Fatal("mutable C was adopted or rewritten")
			}
			approved, err = q.HasPolicyCurrentGateApproval(env.ctx, store.HasPolicyCurrentGateApprovalParams{RunID: runID, GateRevision: after.GateRevision})
			if err != nil || approved {
				t.Fatalf("identity granted approval: %v %v", approved, err)
			}
		})
	}
}
