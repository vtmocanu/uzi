package workersvc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These tests exercise exact transaction readers after actual owner review and
// confirmation. Claim-like SQL ownership is an explicit fixture bridge, not
// Claim/ACK or writer permission proof. setupCodexLiveDB owns the configured DSN.
func stopExecutionConfirm(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID, review apitypes.PolicyStopReview) policyStopResolutionV1 {
	t.Helper()
	result, err := f.svc.SubmitInputWithOptions(e.ctx, f.userID, run, "stop", "owner audit reason", nil,
		SubmitInputOptions{ActionIfMatch: review.StopIfMatch})
	if err != nil || result.PolicyStop == nil {
		t.Fatalf("actual Stop confirmation: %+v %v", result, err)
	}
	row := mustRun(t, e, run)
	record, err := parsePolicyStopResolution(row.PolicyStopResolution, f.userID, run)
	if err != nil || record.ResolutionID.String() != result.PolicyStop.ResolutionID {
		t.Fatalf("persisted accepted receipt: %v", err)
	}
	return record
}

func stopExecutionBeginClaim(t *testing.T, e codexTestEnv, f *gateFix, run uuid.UUID, generation int64) (pgx.Tx, *store.Queries) {
	t.Helper()
	tx, err := e.pool.BeginTx(e.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(e.ctx) })
	if _, err := tx.Exec(e.ctx, "UPDATE runs SET status='claimed',worker_id=$2,claim_generation=$3,claim_released_at=NULL WHERE id=$1",
		run, f.wkr.ID, generation); err != nil {
		t.Fatal(err)
	}
	return tx, store.New(tx)
}

func TestPolicyStopExecutionLockedLiveDB(t *testing.T) {
	e, f, run := stopHistoryFixture(t)
	predecessor := stopHistoryInput(t, e, f, run, "immutable predecessor", false)
	active := stopHistoryInput(t, e, f, run, "immutable applied history", true)
	stopHistoryPublish(t, e, f, run, active, predecessor)
	pending := stopHistoryInput(t, e, f, run, "waived pending body", true)
	review := stopHistoryHold(t, e, f, run)
	record := stopExecutionConfirm(t, e, f, run, review)
	if record.Manifest.ReviewGeneration != "1" {
		t.Fatal("fixture did not retain review generation")
	}
	baseline := policyOwnerStopSnapshot(t, e, run)
	for _, mode := range []string{
		"reclaim_current_G", "stale_request_G", "wrong_worker", "wrong_user", "released", "terminal",
		"source_unknown", "source_generation", "selection", "gate_payload", "history_metadata",
		"pending_body", "pending_delivery", "history_body", "history_settlement", "new_pending",
		"scope_missing", "scope_kind", "scope_time", "scope_settled", "scope_consumed",
		"receipt_corrupt", "ordinary_absence", "store_error",
	} {
		t.Run(mode, func(t *testing.T) {
			tx, q := stopExecutionBeginClaim(t, e, f, run, 2)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := tx.Exec(e.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			w, generation := f.wkr, int64(2)
			switch mode {
			case "stale_request_G":
				generation = 1
			case "wrong_worker":
				w.ID = uuid.New()
			case "wrong_user":
				w.UserID = uuid.New()
			case "released":
				exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
			case "terminal":
				exec("UPDATE runs SET status='completed' WHERE id=$1", run)
			case "source_generation":
				exec("UPDATE runs SET policy_last_source_generation=2 WHERE id=$1", run)
			case "gate_payload":
				exec("UPDATE runs SET plan_md=plan_md || ' changed' WHERE id=$1", run)
			case "history_metadata":
				exec("UPDATE runs SET gate_presented_payload=gate_presented_payload - 'revision_retry_receipt' WHERE id=$1", run)
			case "history_body":
				exec("UPDATE run_user_inputs SET body=body || ' drift' WHERE id=$1", active)
			case "new_pending":
				exec("INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision) VALUES($1,'revise_plan','new feedback','bound',$2)", run, mustRun(t, e, run).GateRevision)
			case "scope_settled":
				exec("UPDATE run_user_inputs SET disposition='applied' WHERE id=$1", record.ScopeInputID)
			case "scope_consumed":
				exec("UPDATE run_user_inputs SET consumed_at=now(),consumed_claim_generation=2,consumed_worker_id=$2 WHERE id=$1", record.ScopeInputID, f.wkr.ID)
			}
			locked, err := q.GetRunByIDForUpdate(e.ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			before := mustStopJSON(t, locked)
			// The immutable SQL triggers forbid these corruptions. Inject them
			// only at the validator input/read seam, after reading real evidence.
			switch mode {
			case "source_unknown":
				locked.PolicyRevisionSource = nil
			case "selection":
				locked.PolicyCandidateCrossCheckID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			case "receipt_corrupt":
				locked.PolicyStopResolution = []byte("{}")
			case "ordinary_absence":
				locked.PolicyStopResolution = nil
			}
			reader := &stopExecutionFaultReader{Queries: q, mode: mode, pending: pending, active: active}
			if mode == "store_error" {
				_, err := tx.Exec(e.ctx, "SELECT 1/0")
				var sqlErr *pgconn.PgError
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
					t.Fatalf("failed to abort transaction: %v", err)
				}
			}
			got, err := policyStopExecutionLocked(e.ctx, reader, w, locked, generation)
			switch mode {
			case "reclaim_current_G":
				if err != nil || got == nil || got.record.ResolutionID != record.ResolutionID {
					t.Fatalf("reclaimed accepted context: %v %v", got, err)
				}
				if policyRevisionPlanApproved(e.ctx, q, locked, true) {
					t.Fatal("Stop waiver became ordinary candidate approval")
				}
			case "ordinary_absence":
				if got != nil || err != nil {
					t.Fatalf("ordinary absence: %v %v", got, err)
				}
			case "store_error":
				var sqlErr *pgconn.PgError
				if got != nil || !errors.As(err, &sqlErr) || sqlErr.Code != "25P02" {
					t.Fatalf("transaction failure swallowed: %v %v", got, err)
				}
			default:
				if got != nil || err == nil {
					t.Fatalf("drift accepted: %v %v", got, err)
				}
			}
			if mode != "store_error" {
				after, err := q.GetRunByIDForUpdate(e.ctx, run)
				if err != nil || !bytes.Equal(before, mustStopJSON(t, after)) {
					t.Fatalf("validator changed locked run: %v", err)
				}
			}
			if err := tx.Rollback(e.ctx); err != nil {
				t.Fatal(err)
			}
			if baseline != policyOwnerStopSnapshot(t, e, run) {
				t.Fatal("rollback changed accepted evidence")
			}
		})
	}
}

func TestPolicyStopExecutionLockedNoGateLiveDB(t *testing.T) {
	for _, checked := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "selected_approve"}[checked], func(t *testing.T) {
			e, f, run := policyStopNoGateFixture(t, false, checked, false)
			review := policyStopNoGateReview(t, e, f, run)
			record := stopExecutionConfirm(t, e, f, run, review)
			for _, mode := range []string{"valid", "provenance", "new_pending", "checker_body", "checker_settled"} {
				if !checked && (mode == "checker_body" || mode == "checker_settled") {
					continue
				}
				t.Run(mode, func(t *testing.T) {
					tx, q := stopExecutionBeginClaim(t, e, f, run, 2)
					exec := func(sql string, args ...any) {
						t.Helper()
						if _, err := tx.Exec(e.ctx, sql, args...); err != nil {
							t.Fatal(err)
						}
					}
					switch mode {
					case "provenance":
						exec("UPDATE runs SET auto_approve=false WHERE id=$1", run)
					case "new_pending":
						exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'revise_plan','later unbound feedback')", run)
					case "checker_body":
						exec("UPDATE cross_checks SET planning_diff=COALESCE(planning_diff,'') || ' drift' WHERE id=$1", uuid.MustParse(*record.Manifest.CandidateCrossCheckID))
					}
					locked, err := q.GetRunByIDForUpdate(e.ctx, run)
					if err != nil {
						t.Fatal(err)
					}
					reader := &stopExecutionFaultReader{Queries: q, mode: mode}
					got, err := policyStopExecutionLocked(e.ctx, reader, f.wkr, locked, 2)
					if (got != nil) != (mode == "valid") || (err == nil) != (mode == "valid") {
						t.Fatalf("no gate: context=%v err=%v", got, err)
					}
					if err := tx.Rollback(e.ctx); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

// Actual semantic compilation and automatic running freeze A; actual Stop
// review/confirmation records consent. B/C are compiled before A is frozen.
// The gated variant bridges an unapproved B presentation using SQL because the
// current frozen-report writer correctly refuses replacement plan fields.
// This tests validator composition, not reachability through future writers.
func TestPolicyStopExecutionLockedSemanticLiveDB(t *testing.T) {
	for _, noGate := range []bool{false, true} {
		t.Run(map[bool]string{false: "gate_B_frozen_A_mutable_C", true: "no_gate_A"}[noGate], func(t *testing.T) {
			e, f, c := semanticPolicySourceLive(t, true)
			f.svc.p.PolicyStopNegotiated = true
			originalPlan := c.structural.PlanMd
			c.structural.PlanMd = "unapproved B"
			c.submit(t)
			b, bFields := c.candidate, c.structural
			c.structural.PlanMd = "mutable C"
			c.submit(t)
			mutable := c.candidate
			c.structural.PlanMd = originalPlan
			c.submit(t)
			a := c.candidate
			publishGateCoverage(t, c)
			req := semanticPolicySourceReport(t, c)
			req.State, req.PresentationID = "running", nil
			autopilotRevisionReport(t, f, c.run, req)
			completed := []string{"m1"}
			autopilotRevisionReport(t, f, c.run, StateRequest{State: "running", ClaimGeneration: i64(7), MilestonesCompleted: &completed})
			if !noGate {
				current := mustRun(t, e, c.run)
				current.PlanMd.String = bFields.PlanMd
				current.MilestonesCandidate = bFields.Milestones
				presentation := semanticGateProjection(&semanticCoverageTx{candidate: b}, current)
				payload := gatePresentedPayload{PlanMd: &bFields.PlanMd, Milestones: bFields.Milestones,
					RequiredCapabilities: bFields.RequiredCapabilities, RequiredTools: bFields.RequiredTools,
					SizeClass: nonEmptyPtr(bFields.SizeClass), Semantic: &presentation}
				raw, digest, err := payload.digest()
				if err != nil {
					t.Fatal(err)
				}
				presentationID := uuid.New()
				source := acceptanceSource(t, e, c.run)
				source.Gate = &apitypes.PolicyStopGateContext{Revision: "1", PresentationID: presentationID.String(), PayloadDigest: hex.EncodeToString(digest)}
				e.exec(`UPDATE runs SET auto_approve=false,plan_md=$2,milestones_candidate=$3,gate_revision=1,
 gate_presentation_id=$4,gate_payload_digest=$5,gate_presented_payload=$6,policy_revision_source=$7 WHERE id=$1`,
					c.run, bFields.PlanMd, bFields.Milestones, presentationID, digest, raw, mustStopJSON(t, source))
				e.exec("UPDATE semantic_planning SET candidate=$2 WHERE run_id=$1", c.run, mustStopJSON(t, mutable))
			}
			hold := policyRequest()
			hold.ClaimGeneration, hold.SourceGeneration = 7, 7
			hold.Phase = strPtr("implementation")
			if !noGate {
				hold.ExpectedGateRevision = i64(1)
			}
			if _, disposition, err := f.svc.ReportPolicyHold(e.ctx, f.wkr, c.run, hold); err != nil || disposition != PolicyHoldApplied {
				t.Fatalf("semantic hold=%s err=%v", disposition, err)
			}
			review, err := f.svc.PolicyStopReviewForOwner(e.ctx, f.userID, c.run, PolicyHoldActionToken(mustRun(t, e, c.run)))
			if err != nil {
				t.Fatal(err)
			}
			record := stopExecutionConfirm(t, e, f, c.run, review)
			for _, mode := range []string{"valid", "missing_A", "contract_drift", "no_gate_plan_drift"} {
				if !noGate && mode == "no_gate_plan_drift" {
					continue
				}
				t.Run(mode, func(t *testing.T) {
					tx, q := stopExecutionBeginClaim(t, e, f, c.run, 8)
					var err error
					switch mode {
					case "missing_A":
						_, err = tx.Exec(e.ctx, "DELETE FROM semantic_candidate_history WHERE run_id=$1 AND candidate->>'digest'=$2", c.run, a.Digest)
					case "contract_drift":
						_, err = tx.Exec(e.ctx, "UPDATE runs SET completion_contract=jsonb_set(completion_contract,'{revision}','99') WHERE id=$1", c.run)
					case "no_gate_plan_drift":
						_, err = tx.Exec(e.ctx, "UPDATE runs SET plan_md='unapproved replacement' WHERE id=$1", c.run)
					}
					if err != nil {
						t.Fatal(err)
					}
					locked, err := q.GetRunByIDForUpdate(e.ctx, c.run)
					if err != nil {
						t.Fatal(err)
					}
					got, err := policyStopExecutionLocked(e.ctx, q, f.wkr, locked, 8)
					if (got != nil) != (mode == "valid") || (err == nil) != (mode == "valid") {
						t.Fatalf("semantic context=%v err=%v", got, err)
					}
					if got != nil {
						contract, err := parseCompletionContract(got.record.Manifest.Execution.Contract)
						if err != nil || contract.SemanticPresentation.Candidate.Digest != a.Digest || got.record.ResolutionID != record.ResolutionID {
							t.Fatalf("execution substituted B/C: %v", err)
						}
						if !noGate && policyRevisionPlanApproved(e.ctx, q, locked, true) {
							t.Fatal("validator approved B")
						}
						planning, err := q.GetSemanticPlanning(e.ctx, c.run)
						if err != nil {
							t.Fatal(err)
						}
						var candidate apitypes.SemanticCandidate
						if err := json.Unmarshal(planning.Candidate, &candidate); err != nil {
							t.Fatal(err)
						}
						want := a.Digest
						if !noGate {
							want = mutable.Digest
						}
						if candidate.Digest != want {
							t.Fatal("validator adopted or rewrote mutable candidate")
						}
					}
					if err := tx.Rollback(e.ctx); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

// Corruption injection is limited to negative cases that database constraints or
// immutability triggers prohibit. Every override first executes the generated
// exact reader in the same transaction; successful controls use unchanged rows.
type stopExecutionFaultReader struct {
	*store.Queries
	mode    string
	pending int64
	active  int64
}

func (q *stopExecutionFaultReader) GetPolicyStopScopeInput(ctx context.Context, p store.GetPolicyStopScopeInputParams) (store.RunUserInput, error) {
	row, err := q.Queries.GetPolicyStopScopeInput(ctx, p)
	if err != nil {
		return row, err
	}
	switch q.mode {
	case "scope_missing":
		return store.RunUserInput{}, pgx.ErrNoRows
	case "scope_kind":
		row.Kind = "follow_up"
	case "scope_time":
		row.CreatedAt.Time = row.CreatedAt.Time.Add(time.Second)
	}
	return row, nil
}

func (q *stopExecutionFaultReader) GetPolicyStopRevisionInput(ctx context.Context, p store.GetPolicyStopRevisionInputParams) (store.RunUserInput, error) {
	row, err := q.Queries.GetPolicyStopRevisionInput(ctx, p)
	if err != nil {
		return row, err
	}
	if row.ID == q.pending {
		switch q.mode {
		case "pending_body":
			row.Body.String += " drift"
		case "pending_delivery":
			row.ConsumedClaimGeneration.Int64++
		}
	}
	if row.ID == q.active && q.mode == "history_settlement" {
		row.SettledAt = row.CreatedAt
	}
	return row, nil
}

func (q *stopExecutionFaultReader) GetPolicyStopCrossCheck(ctx context.Context, id uuid.UUID) (store.CrossCheck, error) {
	row, err := q.Queries.GetPolicyStopCrossCheck(ctx, id)
	if err != nil {
		return row, err
	}
	if q.mode == "checker_settled" {
		row.RevisionDisposition = pgtype.Text{String: "superseded_by_stop", Valid: true}
	}
	return row, nil
}

// Keep the interface assertion local to the owned tests. It checks the generated
// exact-scope reader's signature when the tester compiles this slice.
var _ policyStopExecutionReader = (*store.Queries)(nil)
