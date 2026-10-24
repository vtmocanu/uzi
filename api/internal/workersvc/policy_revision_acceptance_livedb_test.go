package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func acceptanceGateRequest() StateRequest {
	req := gateReq(i64(1), uid())
	req.RequiredCapabilities = &[]string{}
	req.RequiredTools = &[]string{}
	req.Milestones = &[]Milestone{}
	size := "s"
	req.SizeClass = &size
	return req
}

func acceptanceUnpublishedFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID, PlanCrossCheckCandidate) {
	t.Helper()
	// Public creation supplies birth coverage; the checker is submitted before
	// the first plan or gate is persisted, as production admission requires.
	e, svc, w, run, candidate := revisionInventoryFixture(t)
	f := &gateFix{t: t, ctx: e.ctx, pool: e.pool, q: e.q, svc: svc, wkr: w, userID: w.UserID}
	f.wkr.ProtocolCapabilities = append(f.wkr.ProtocolCapabilities, capability.GateRevisionV1, capability.InputReceiptsV1,
		capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1,
		"cross_check_v1", "cross_check_rounds_v1", "cross_check_codex_lead_v1")
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.wkr.ID, f.wkr.ProtocolCapabilities)
	f.svc.p.PolicyHoldNegotiated = true
	f.svc.p.PolicyHoldSnapshotEnabled = true
	f.svc.p.TerminalPendingLease = time.Minute
	return e, f, run, candidate
}

func acceptanceFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID) {
	t.Helper()
	e, f, run, _ := acceptanceUnpublishedFixture(t)
	e.exec("UPDATE runs SET plan_cross_check_required=false,auto_approve=false WHERE id=$1", run)
	f.mustPublish(run, acceptanceGateRequest(), 1)
	return e, f, run
}

func acceptanceInput(t *testing.T, e codexTestEnv, run uuid.UUID, kind string, revision int64) int64 {
	t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO run_user_inputs(run_id,kind,body,gate_binding,gate_revision)
 VALUES($1,$2,'fixture','bound',$3) RETURNING id`, run, kind, revision).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func acceptanceAck(t *testing.T, f *gateFix, run uuid.UUID, ids ...int64) {
	t.Helper()
	if _, err := f.svc.AckInputs(f.ctx, f.wkr, run, 1, ids); err != nil {
		t.Fatal(err)
	}
}

func acceptanceApply(t *testing.T, f *gateFix, run uuid.UUID, ids ...int64) {
	t.Helper()
	if _, err := f.svc.ApplyInputs(f.ctx, f.wkr, run, 1, ids); err != nil {
		t.Fatal(err)
	}
}

func acceptancePair(t *testing.T, e codexTestEnv, run uuid.UUID) string {
	t.Helper()
	var value string
	if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_array(policy_revision_source,policy_candidate_cross_check_id)::text FROM runs WHERE id=$1`, run).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func acceptanceHistory(t *testing.T, e codexTestEnv, run uuid.UUID) string {
	t.Helper()
	var value string
	if err := e.pool.QueryRow(e.ctx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM run_user_inputs i WHERE run_id=$1 AND kind='revise_plan'),
 (SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM cross_checks c WHERE lead_run_id=$1))::text`, run).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func acceptanceSource(t *testing.T, e codexTestEnv, run uuid.UUID) apitypes.PolicyRevisionSourceV1 {
	t.Helper()
	source, err := apitypes.ParsePolicyRevisionSourceV1(mustRun(t, e, run).PolicyRevisionSource)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestPolicyRevisionAcceptanceHumanSettlementLiveDB(t *testing.T) {
	for _, status := range []string{"awaiting_approval", "awaiting_input", "paused"} {
		t.Run(status, func(t *testing.T) {
			e, f, run := acceptanceFixture(t)
			revision, err := f.svc.SubmitInputWithOptions(e.ctx, f.wkr.UserID, run, "revise_plan", "revise", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
			if err != nil {
				t.Fatal(err)
			}
			acceptanceAck(t, f, run, revision.ID)
			source := acceptanceSource(t, e, run)
			ref := revisionWriterHumanRef(revision.ID)
			// Go simulates the agent carrier bridge; Append executes the real writer.
			if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
				Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, ref),
			}}, i64(1)); err != nil {
				t.Fatal(err)
			}
			before := acceptancePair(t, e, run)
			req := acceptanceGateRequest()
			plan := "# Revised plan"
			req.PlanMd, req.ExpectedGateRevision, req.RevisionInputID = &plan, i64(1), &revision.ID
			f.mustPublish(run, req, 2) // Actual #3001 settlement marks the old origin APPLIED.
			if acceptanceSource(t, e, run).PrimaryRevision == nil {
				t.Fatal("settlement accepted primary")
			}
			acceptanceApply(t, f, run, revision.ID)
			settledPair := acceptancePair(t, e, run)
			approval, err := f.svc.SubmitInputWithOptions(e.ctx, f.wkr.UserID, run, "approve_plan", "", nil, SubmitInputOptions{ExpectedGateRevision: i64(2)})
			if err != nil {
				t.Fatal(err)
			}
			published := acceptancePair(t, e, run)
			if published != settledPair {
				t.Fatal("enqueue accepted source")
			}
			acceptanceAck(t, f, run, approval.ID)
			if acceptancePair(t, e, run) != published {
				t.Fatal("enqueue or ACK accepted source")
			}
			history := acceptanceHistory(t, e, run)
			e.exec("UPDATE runs SET status=$2 WHERE id=$1", run, status)
			acceptanceApply(t, f, run, approval.ID)
			got := acceptanceSource(t, e, run)
			if got.PrimaryRevision != nil || len(got.AssociatedCrossChecks) != 0 || got.Gate.Revision != "2" {
				t.Fatalf("not accepted: %+v", got)
			}
			if acceptanceHistory(t, e, run) != history {
				t.Fatal("acceptance changed feedback history")
			}
			accepted := acceptancePair(t, e, run)
			acceptanceApply(t, f, run, approval.ID) // Lost reply: exact retry is read-only.
			if acceptancePair(t, e, run) != accepted {
				t.Fatal("retry changed source")
			}
			e.exec("UPDATE runs SET status='awaiting_approval' WHERE id=$1", run)
			f.mustPublish(run, acceptanceGateRequest(), 3)
			newer := acceptancePair(t, e, run)
			acceptanceApply(t, f, run, approval.ID)
			if acceptancePair(t, e, run) != newer || before == published {
				t.Fatal("old APPLIED restored source or publication did not rebind")
			}
		})
	}
}

func TestPolicyRevisionAcceptanceNullableSwitchLiveDB(t *testing.T) {
	e, f, run := acceptanceFixture(t)
	id := acceptanceInput(t, e, run, "approve_plan", 1)
	acceptanceAck(t, f, run, id)
	// Match receiptInactiveReason: a missing generation does not describe a
	// switch pending at this claim. The source writer must use the same fence.
	e.exec("UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=NULL WHERE id=$1", run)
	acceptanceApply(t, f, run, id)
}

func TestPolicyRevisionAcceptanceNegativeProofLiveDB(t *testing.T) {
	for _, mode := range []string{"pending_current", "pending_unbound", "pending_future", "mixed", "other_gate", "unknown", "malformed", "gate", "selection", "old", "unbound", "old_applied_mixed", "missing_primary", "foreign_primary", "future_primary_gate", "future_primary_generation", "linkage", "accepted_stop"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := acceptanceFixture(t)
			currentGate := int64(1)
			if mode == "other_gate" || mode == "old" {
				f.mustPublish(run, acceptanceGateRequest(), 2)
				currentGate = 2
			}
			approval := acceptanceInput(t, e, run, "approve_plan", currentGate)
			ids := []int64{approval}
			switch mode {
			case "pending_current", "pending_unbound", "pending_future", "mixed", "other_gate":
				gate := currentGate
				if mode == "other_gate" {
					gate = 1
				}
				if mode == "pending_future" {
					gate = currentGate + 1
				}
				revision := acceptanceInput(t, e, run, "revise_plan", gate)
				if mode == "pending_unbound" {
					e.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", revision)
				}
				if mode == "mixed" {
					ids = append(ids, revision)
				}
			case "unknown":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "malformed":
				e.exec("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", run)
			case "gate":
				e.exec("UPDATE runs SET gate_payload_digest=decode(repeat('01',32),'hex') WHERE id=$1", run)
			case "selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			case "old":
				e.exec("UPDATE run_user_inputs SET gate_revision=1 WHERE id=$1", approval)
			case "unbound":
				e.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", approval)
			case "old_applied_mixed":
				e.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", approval)
				follow, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: run, Kind: "follow_up"})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, follow.ID)
			case "missing_primary", "foreign_primary", "future_primary_gate", "future_primary_generation", "linkage":
				source := acceptanceSource(t, e, run)
				id := int64(9223372036854775806)
				if mode != "missing_primary" {
					id = acceptanceInput(t, e, run, "revise_plan", 1)
					e.exec("UPDATE run_user_inputs SET consumed_at=now(),consumed_worker_id=$2,consumed_claim_generation=1,applied_at=now() WHERE id=$1", id, f.wkr.ID)
					if mode == "future_primary_gate" {
						e.exec("UPDATE run_user_inputs SET gate_revision=2 WHERE id=$1", id)
					}
					if mode == "future_primary_generation" {
						e.exec("UPDATE run_user_inputs SET consumed_claim_generation=2 WHERE id=$1", id)
					}
					if mode == "foreign_primary" {
						_, _, other := acceptanceFixture(t)
						e.exec("UPDATE run_user_inputs SET run_id=$2 WHERE id=$1", id, other)
					}
				}
				ref := revisionWriterHumanRef(id)
				source.PrimaryRevision = &ref
				raw, err := json.Marshal(source)
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run, raw)
				if mode == "linkage" {
					e.exec(`UPDATE runs SET gate_presented_payload=gate_presented_payload || '{"revision_retry_receipt":{"source_revision":1,"revision_input_id":1}}'::jsonb WHERE id=$1`, run)
				}
			case "accepted_stop":
				// This is a constraint-valid retained-resolution marker fixture;
				// actual reviewed Stop composition is exercised separately.
				acceptanceAck(t, f, run, approval)
				scope, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: run, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", run, scope.ID)
			}
			// An exact historical APPLIED row must already be owned before its retry.
			if mode == "old_applied_mixed" {
				e.exec("UPDATE run_user_inputs SET consumed_at=now(),consumed_worker_id=$2,consumed_claim_generation=1 WHERE id=$1", approval, f.wkr.ID)
				acceptanceAck(t, f, run, ids[1])
			} else if mode != "accepted_stop" {
				acceptanceAck(t, f, run, ids...)
			}
			before := acceptancePair(t, e, run)
			observer := &acceptanceBeginner{TxBeginner: e.pool}
			f.svc.SetTxBeginner(observer)
			_, err := f.svc.ApplyInputs(e.ctx, f.wkr, run, 1, ids)
			if mode == "accepted_stop" {
				if !errors.Is(err, ErrInputReceiptConflict) {
					t.Fatalf("accepted Stop allowed approval or returned another failure: %v", err)
				}
				var unapplied bool
				if readErr := e.pool.QueryRow(e.ctx, "SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1", approval).Scan(&unapplied); readErr != nil || !unapplied {
					t.Fatalf("Stop refusal mutated receipt: %v %v", unapplied, readErr)
				}
				if observer.hits != 0 || acceptancePair(t, e, run) != before {
					t.Fatal("Stop refusal changed source or selection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "other_gate" && observer.hits != 1 {
				t.Fatal("old pending gate blocked new current-gate acceptance")
			}
			if mode != "other_gate" && observer.hits != 0 {
				t.Fatal("negative proof reached acceptance SQL")
			}
			if mode != "other_gate" && acceptancePair(t, e, run) != before {
				t.Fatal("negative proof changed source or selection")
			}
		})
	}
}

// These rows explicitly seed declared witness preconditions. The receipt service
// and acceptance SQL remain real; feedback must stay byte-for-byte unchanged.
func TestPolicyRevisionAcceptanceCrossCheckLiveDB(t *testing.T) {
	for _, mode := range []string{"approve", "revise", "block", "failed", "history", "pending", "historical_pending", "historical_pending_unfenced", "generation", "clear_keep", "digest", "foreign", "missing", "primary"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := acceptanceFixture(t)
			r := mustRun(t, e, run)
			candidate := PlanCrossCheckCandidate{PlanMd: r.PlanMd.String, Milestones: r.MilestonesCandidate,
				RequiredCapabilities: r.RequiredCapabilities, RequiredTools: r.RequiredTools, SizeClass: r.SizeClass,
				BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
			if len(candidate.Milestones) == 0 {
				candidate.Milestones = []byte("[]")
			}
			if mode == "history" || strings.HasPrefix(mode, "historical_pending") {
				candidate.PlanMd = "historical"
			}
			digest, err := candidate.Digest()
			if err != nil {
				t.Fatal(err)
			}
			verdict := mode
			switch mode {
			case "history", "generation", "digest", "foreign", "missing":
				verdict = "approve"
			case "historical_pending", "historical_pending_unfenced":
				verdict = "pending"
			case "clear_keep":
				verdict = "revise"
			case "primary":
				verdict = "revise"
			}
			id := uuid.New()
			e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
    required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,decided_at,deadline_at)
    VALUES($1,$2,'plan',1,1,$3,$4,$5,$6,$7,$8,$9,$10,$11,CASE WHEN $11='pending' THEN NULL ELSE now() END,now()+interval '1 hour')`,
				id, run, candidate.PlanMd, candidate.Milestones, candidate.RequiredCapabilities, candidate.RequiredTools,
				candidate.SizeClass, candidate.BaseCommit, candidate.PlanningDiff, digest, verdict)
			source := acceptanceSource(t, e, run)
			idText := id.String()
			ref := apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &idText}
			source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{ref}
			if mode == "primary" {
				source.PrimaryRevision = &ref
				source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{}
			}
			if mode == "clear_keep" {
				other := uuid.New()
				e.exec(`INSERT INTO cross_checks(id,lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
 required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,verdict,decided_at,deadline_at)
 SELECT $2,lead_run_id,stage,2,lead_claim_generation,plan_md,milestones,required_capabilities,
 required_tools,size_class,base_commit,planning_diff,candidate_digest,'approve',now(),deadline_at
 FROM cross_checks WHERE id=$1`, id, other)
				otherText := other.String()
				source.AssociatedCrossChecks = append(source.AssociatedCrossChecks, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &otherText})
			}
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			e.exec("UPDATE runs SET policy_revision_source=$2,policy_candidate_cross_check_id=$3 WHERE id=$1", run, raw, id)
			if mode == "generation" {
				e.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE id=$1", id)
			}
			if mode == "historical_pending_unfenced" {
				e.exec("UPDATE runs SET status='running' WHERE id=$1", run)
			}
			if mode == "digest" {
				e.exec("UPDATE cross_checks SET candidate_digest=decode('01','hex') WHERE id=$1", id)
			}
			if mode == "foreign" {
				_, _, other := acceptanceFixture(t)
				e.exec("UPDATE cross_checks SET lead_run_id=$2 WHERE id=$1", id, other)
			}
			if mode == "missing" {
				e.exec("DELETE FROM cross_checks WHERE id=$1", id)
			}
			approval := acceptanceInput(t, e, run, "approve_plan", 1)
			acceptanceAck(t, f, run, approval)
			before, history := acceptancePair(t, e, run), acceptanceHistory(t, e, run)
			acceptanceApply(t, f, run, approval)
			if acceptanceHistory(t, e, run) != history {
				t.Fatal("receipt mutated feedback")
			}
			if mode == "pending" || mode == "historical_pending_unfenced" || mode == "generation" || mode == "digest" || mode == "foreign" || mode == "missing" {
				if acceptancePair(t, e, run) != before {
					t.Fatal("invalid evidence accepted")
				}
				return
			}
			got := mustRun(t, e, run)
			accepted := acceptanceSource(t, e, run)
			if !policyStopSelectionValid(accepted, stopUUID(got.PolicyCandidateCrossCheckID)) {
				t.Fatal("accepted source and selection are inconsistent")
			}
			if accepted.PrimaryRevision != nil {
				t.Fatal("primary survived acceptance")
			}
			if mode == "approve" {
				if !got.PolicyCandidateCrossCheckID.Valid || uuid.UUID(got.PolicyCandidateCrossCheckID.Bytes) != id || len(accepted.AssociatedCrossChecks) != 1 {
					t.Fatal("current APPROVE not retained")
				}
			} else if mode == "clear_keep" {
				if got.PolicyCandidateCrossCheckID.Valid || len(accepted.AssociatedCrossChecks) != 0 {
					t.Fatal("waived selection was replaced or left unselected live references")
				}
			} else if got.PolicyCandidateCrossCheckID.Valid || len(accepted.AssociatedCrossChecks) != 0 {
				t.Fatal("waived/historical reference survived")
			}
		})
	}
}

type acceptanceBeginner struct {
	TxBeginner
	point        string
	hits         int
	beforeAccept func(context.Context) error
}
type acceptanceTx struct {
	pgx.Tx
	owner *acceptanceBeginner
}

func (b *acceptanceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &acceptanceTx{Tx: tx, owner: b}, nil
}
func (tx *acceptanceTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	target := strings.HasPrefix(sql, "-- name: AcceptPolicyRevisionSource :execrows")
	if target {
		tx.owner.hits++
		if tx.owner.beforeAccept != nil {
			if err := tx.owner.beforeAccept(ctx); err != nil {
				return pgconn.CommandTag{}, err
			}
		}
	}
	if target && tx.owner.point == "source_error" {
		return pgconn.CommandTag{}, errors.New("injected acceptance failure")
	}
	if target && tx.owner.point == "source_zero" {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	if strings.HasPrefix(sql, "-- name: ApplyRunInputRows :execrows") && tx.owner.point == "count" {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx *acceptanceTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx.owner.point == "pending_read" && strings.HasPrefix(sql, "-- name: ListPolicyStopRevisionInputs :many") {
		return nil, errors.New("injected pending read failure")
	}
	return tx.Tx.Query(ctx, sql, args...)
}
func (tx *acceptanceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.owner.point == "source_read" && strings.HasPrefix(sql, "-- name: GetRunByID :one") {
		return revisionWriterFailureRow{err: errors.New("injected source read failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *acceptanceTx) Commit(ctx context.Context) error {
	if tx.owner.point == "commit" {
		_ = tx.Rollback(ctx)
		return errors.New("injected commit failure")
	}
	return tx.Tx.Commit(ctx)
}
func TestPolicyRevisionAcceptanceRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"source_error", "source_zero", "count", "commit", "pending_read", "source_read"} {
		t.Run(point, func(t *testing.T) {
			e, f, run := acceptanceFixture(t)
			id := acceptanceInput(t, e, run, "approve_plan", 1)
			acceptanceAck(t, f, run, id)
			before := acceptancePair(t, e, run)
			injection := &acceptanceBeginner{TxBeginner: e.pool, point: point}
			f.svc.SetTxBeginner(injection)
			if _, err := f.svc.ApplyInputs(e.ctx, f.wkr, run, 1, []int64{id}); err == nil {
				t.Fatal("injected failure succeeded")
			}
			prewriteFailure := point == "count" || strings.HasSuffix(point, "_read")
			if prewriteFailure && injection.hits != 0 {
				t.Fatal("source write before receipt proof")
			}
			if !prewriteFailure && injection.hits != 1 {
				t.Fatalf("acceptance attempts=%d", injection.hits)
			}
			var pending bool
			if err := e.pool.QueryRow(e.ctx, "SELECT applied_at IS NULL FROM run_user_inputs WHERE id=$1", id).Scan(&pending); err != nil || !pending {
				t.Fatalf("receipt did not roll back: %v %v", pending, err)
			}
			if acceptancePair(t, e, run) != before {
				t.Fatal("source pair did not roll back")
			}
		})
	}
}

func TestPolicyRevisionAcceptanceLateVerdictInterleavingLiveDB(t *testing.T) {
	e, f, run, candidate := acceptanceUnpublishedFixture(t)
	cc, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	e.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", child, f.wkr.ID)
	if _, err := f.svc.DecidePlanCrossCheck(e.ctx, f.wkr, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
		t.Fatal(err)
	}
	req := acceptanceGateRequest()
	plan := "# Different human candidate"
	req.PlanMd = &plan
	f.mustPublish(run, req, 1)
	// Publication normally settles the exited checker. Explicitly restore a
	// historical pending witness to exercise the late writer's independent fence.
	e.exec("UPDATE cross_checks SET verdict='pending',reason_class=NULL,findings=NULL,decided_at=NULL WHERE id=$1", cc.ID)
	e.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1,claim_released_at=NULL WHERE id=$1", child, f.wkr.ID)
	approval := acceptanceInput(t, e, run, "approve_plan", 1)
	acceptanceAck(t, f, run, approval)
	decider := New(e.q, e.box, f.svc.p)
	decider.SetTxBeginner(e.pool)
	attempts := 0
	injection := &acceptanceBeginner{TxBeginner: e.pool}
	injection.beforeAccept = func(ctx context.Context) error {
		attempts++
		// The receipt transaction owns the run lock and has already stamped its
		// approval. A second real transaction tries the actual verdict writer
		// before the source update. The fixture context bounds this overlap.
		_, err := decider.DecidePlanCrossCheck(ctx, f.wkr, child, 1, "revise", "revise", []byte(`{"summary":"late","items":[]}`))
		if !errors.Is(err, ErrCrossCheckRefused) {
			return errors.New("late verdict was not refused")
		}
		return nil
	}
	f.svc.SetTxBeginner(injection)
	history := acceptanceHistory(t, e, run)
	acceptanceApply(t, f, run, approval)
	if attempts != 1 || acceptanceHistory(t, e, run) != history {
		t.Fatal("late writer changed feedback or was not exercised")
	}
	if len(acceptanceSource(t, e, run).AssociatedCrossChecks) != 0 || mustRun(t, e, run).PolicyCandidateCrossCheckID.Valid {
		t.Fatal("fenced historical pending reference survived acceptance")
	}
}

func TestPolicyRevisionAcceptanceSubmitRecoveryLiveDB(t *testing.T) {
	for _, creationRace := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "creation_race"}[creationRace], func(t *testing.T) {
			e, f, run, candidate := acceptanceUnpublishedFixture(t)
			var created store.CrossCheck
			var baseline, history string
			accept := func(ctx context.Context) error {
				var err error
				created, err = f.svc.SubmitPlanCrossCheck(ctx, f.wkr, run, 1, candidate)
				if err != nil {
					return err
				}
				child := uuid.UUID(created.CheckerRunID.Bytes)
				e.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1 WHERE id=$1", child, f.wkr.ID)
				if _, err := f.svc.DecidePlanCrossCheck(ctx, f.wkr, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
					return err
				}
				f.mustPublish(run, acceptanceGateRequest(), 1)
				approval := acceptanceInput(t, e, run, "approve_plan", 1)
				acceptanceAck(t, f, run, approval)
				acceptanceApply(t, f, run, approval)
				accepted := acceptanceSource(t, e, run)
				if len(accepted.AssociatedCrossChecks) != 0 || mustRun(t, e, run).PolicyCandidateCrossCheckID.Valid {
					t.Fatal("human approval did not waive real decided feedback")
				}
				// Exact recovery retains its original auto-approved running admission.
				// This state bridge does not claim the complete resumed worker flow.
				e.exec("UPDATE runs SET status='running',auto_approve=true,plan_cross_check_required=true WHERE id=$1", run)
				baseline, history = acceptancePair(t, e, run), acceptanceHistory(t, e, run)
				return nil
			}
			var injected *associationRecoveryBeginner
			if creationRace {
				injected = &associationRecoveryBeginner{TxBeginner: e.pool, afterCommit: accept}
				f.svc.SetTxBeginner(injected)
			} else if err := accept(e.ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, candidate)
			if err != nil || recovered.ID != created.ID || baseline == "" {
				t.Fatalf("recovery: %v", err)
			}
			if creationRace && (injected.hookCalls != 1 || injected.exactReads != 1) {
				t.Fatal("did not exercise creation recovery")
			}
			if acceptancePair(t, e, run) != baseline || acceptanceHistory(t, e, run) != history {
				t.Fatal("exact recovery restored waived authority or changed history")
			}
		})
	}
}

func TestPolicyRevisionAcceptanceAuthorityLiveDB(t *testing.T) {
	for _, mode := range []string{"owner", "generation", "zero_generation", "released", "terminal", "switch", "claude", "interactive", "discard"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := acceptanceFixture(t)
			// Declared consumed historical human evidence makes an accidental
			// acceptance observable without invoking the new production helper.
			revision := acceptanceInput(t, e, run, "revise_plan", 1)
			e.exec("UPDATE run_user_inputs SET consumed_at=now(),consumed_worker_id=$2,consumed_claim_generation=1,applied_at=now() WHERE id=$1", revision, f.wkr.ID)
			source := acceptanceSource(t, e, run)
			ref := revisionWriterHumanRef(revision)
			source.PrimaryRevision = &ref
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run, raw)
			approval := acceptanceInput(t, e, run, "approve_plan", 1)
			acceptanceAck(t, f, run, approval)
			generation := int64(1)
			switch mode {
			case "owner":
				f.wkr.UserID = uuid.New()
			case "generation":
				e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", run)
			case "zero_generation":
				generation = 0
				e.exec("UPDATE runs SET claim_generation=0 WHERE id=$1", run)
				e.exec("UPDATE run_user_inputs SET consumed_claim_generation=0 WHERE id=$1", approval)
			case "released":
				e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", run)
			case "terminal":
				e.exec("UPDATE runs SET status='completed' WHERE id=$1", run)
			case "switch":
				e.exec("UPDATE runs SET credential_switch_requested_at=now(),credential_switch_generation=1 WHERE id=$1", run)
			case "claude":
				e.exec(`UPDATE runs SET harness='claude',codex_secret_id=NULL,codex_auth_mode=NULL,
 codex_secret_label=NULL,codex_account_key=NULL,codex_material_revision=NULL,
 codex_account_revision=NULL WHERE id=$1`, run)
			case "interactive":
				e.exec("UPDATE runs SET interactive=true WHERE id=$1", run)
			}
			before := acceptancePair(t, e, run)
			if mode == "discard" {
				_, err = f.svc.DiscardInputs(e.ctx, f.wkr, run, generation, []int64{approval})
			} else {
				_, err = f.svc.ApplyInputs(e.ctx, f.wkr, run, generation, []int64{approval})
			}
			mustConflict := mode == "generation" || mode == "released" || mode == "terminal" || mode == "switch"
			if mustConflict && !errors.Is(err, ErrInputReceiptConflict) {
				t.Fatalf("expected claim conflict: %v", err)
			}
			if !mustConflict && err != nil {
				t.Fatal(err)
			}
			if acceptancePair(t, e, run) != before {
				t.Fatal("inactive/unsupported/discarded receipt gained source authority")
			}
		})
	}
}

// No production typed acceptance helper is called: this file can be overlaid on
// the clean base to demonstrate missing behavior through ApplyInputs.
