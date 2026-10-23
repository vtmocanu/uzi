package workersvc

import (
	"bytes"
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

// Birth coverage comes from the public creation writer, never a source UPDATE.
// Only worker ownership and cross-check opt-in are fixture state.
func revisionInventoryFixture(t *testing.T) (codexTestEnv, *Service, store.Worker, uuid.UUID, PlanCrossCheckCandidate) {
	t.Helper()
	e, svc, w, old, c := newAutomaticRoundsHarnessFixture(t, 2, "codex")
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	t.Cleanup(cancel)
	e.ctx = ctx
	repo := uuid.UUID(mustRun(t, e, old).RepoID.Bytes)
	setDefaultHarness(t, e, w.UserID, "codex")
	r, err := svc.CreateTaskRun(ctx, w.UserID, repo, "inventory writer", "", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || source.PrimaryRevision != nil || len(source.AssociatedCrossChecks) != 0 {
		t.Fatalf("birth source=%s err=%v", r.PolicyRevisionSource, err)
	}
	e.exec("UPDATE runs SET status='running',worker_id=$2,claim_generation=1,auto_approve=true,plan_cross_check_required=true WHERE id=$1", r.ID, w.ID)
	return e, svc, w, r.ID, c
}

func inventorySource(t *testing.T, e codexTestEnv, lead uuid.UUID) apitypes.PolicyRevisionSourceV1 {
	t.Helper()
	r := mustRun(t, e, lead)
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil {
		t.Fatalf("source=%s err=%v", r.PolicyRevisionSource, err)
	}
	return source
}

func TestPolicyRevisionInventoryBirthPendingLiveDB(t *testing.T) {
	e, svc, w, lead, c := revisionInventoryFixture(t)
	cc, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c)
	if err != nil {
		t.Fatal(err)
	}
	source := inventorySource(t, e, lead)
	if source.PrimaryRevision != nil || len(source.AssociatedCrossChecks) != 1 ||
		*source.AssociatedCrossChecks[0].CrossCheckID != cc.ID.String() ||
		cc.ID == uuid.UUID(cc.CheckerRunID.Bytes) || cc.Verdict != "pending" || cc.DecidedAt.Valid {
		t.Fatalf("pending creation inventory=%+v row=%+v", source, cc)
	}
	before := policyOwnerStopSnapshot(t, e, lead)
	retry, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c)
	if err != nil || retry.ID != cc.ID || policyOwnerStopSnapshot(t, e, lead) != before {
		t.Fatalf("exact retry changed inventory: %v", err)
	}
}

func TestPolicyRevisionInventoryHumanAndHistoryLiveDB(t *testing.T) {
	e, svc, w, lead, c := revisionInventoryFixture(t)
	svc.p.PolicyHoldNegotiated = true
	svc.p.PolicyHoldSnapshotEnabled = true
	svc.p.TerminalPendingLease = time.Minute
	w.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1, capability.InputReceiptsV1,
		"cross_check_v1", "cross_check_rounds_v1", "cross_check_codex_lead_v1"}
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	input, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: lead, Kind: "revise_plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AckInputs(e.ctx, w, lead, 1, []int64{input.ID}); err != nil {
		t.Fatal(err)
	}
	source := inventorySource(t, e, lead)
	if err := svc.AppendMessagesForClaim(e.ctx, w, lead, []IncomingMessage{{
		Seq: mustRun(t, e, lead).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(input.ID)),
	}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	source = inventorySource(t, e, lead)
	if source.PrimaryRevision == nil || source.PrimaryRevision.InputID == nil {
		t.Fatal("human writer did not establish primary")
	}
	first, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	child := uuid.UUID(first.CheckerRunID.Bytes)
	e.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
	if _, err := svc.DecidePlanCrossCheck(e.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
		t.Fatal(err)
	}
	c.PlanMd = "a different historical candidate"
	second, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := inventorySource(t, e, lead)
	if got.PrimaryRevision == nil || *got.PrimaryRevision.InputID != *source.PrimaryRevision.InputID ||
		len(got.AssociatedCrossChecks) != 2 || *got.AssociatedCrossChecks[0].CrossCheckID != first.ID.String() ||
		*got.AssociatedCrossChecks[1].CrossCheckID != second.ID.String() {
		t.Fatalf("primary/history lost: %+v", got)
	}
}

func TestPolicyRevisionInventoryCrossCheckPrimaryLiveDB(t *testing.T) {
	e, svc, w, lead, c := revisionInventoryFixture(t)
	first, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c, 1)
	if err != nil {
		t.Fatal(err)
	}
	child := uuid.UUID(first.CheckerRunID.Bytes)
	e.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
	if _, err := svc.DecidePlanCrossCheck(e.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
		t.Fatal(err)
	}
	svc.p.PolicyHoldNegotiated = true
	svc.p.PolicyHoldSnapshotEnabled = true
	svc.p.TerminalPendingLease = time.Minute
	w.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1,
		"cross_check_v1", "cross_check_rounds_v1", "cross_check_codex_lead_v1"}
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	source := inventorySource(t, e, lead)
	id := first.ID.String()
	ref := apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}
	if err := svc.AppendMessagesForClaim(e.ctx, w, lead, []IncomingMessage{{
		Seq: mustRun(t, e, lead).LastSeq + 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, ref),
	}}, i64p(1)); err != nil {
		t.Fatal(err)
	}
	source = inventorySource(t, e, lead)
	if source.PrimaryRevision == nil || source.PrimaryRevision.CrossCheckID == nil ||
		*source.PrimaryRevision.CrossCheckID != id {
		t.Fatal("revision writer did not establish cross-check primary")
	}
	c.PlanMd = "next candidate"
	second, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := inventorySource(t, e, lead)
	if got.PrimaryRevision == nil || *got.PrimaryRevision.CrossCheckID != id ||
		len(got.AssociatedCrossChecks) != 1 || *got.AssociatedCrossChecks[0].CrossCheckID != second.ID.String() {
		t.Fatalf("creation replaced cross-check primary: %+v", got)
	}
}

func TestPolicyRevisionInventoryCreateRecoveryLiveDB(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "null", true: "mismatch"}[mismatch], func(t *testing.T) {
			e, svc, w, lead, c := revisionInventoryFixture(t)
			var inserted store.CrossCheck
			var baseline string
			b := &associationRecoveryBeginner{TxBeginner: e.pool}
			b.afterCommit = func(ctx context.Context) error {
				var err error
				inserted, err = svc.SubmitPlanCrossCheck(ctx, w, lead, 1, c)
				if err != nil {
					return err
				}
				var selected any
				if mismatch {
					selected = uuid.New()
				}
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", lead, selected)
				baseline = policyOwnerStopSnapshot(t, e, lead)
				return nil
			}
			svc.SetTxBeginner(b)
			retry, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c)
			if err != nil || retry.ID != inserted.ID || b.exactReads != 1 || baseline == "" ||
				policyOwnerStopSnapshot(t, e, lead) != baseline {
				t.Fatalf("creation recovery changed source/selection: reads=%d err=%v", b.exactReads, err)
			}
		})
	}
}

func TestPolicyRevisionInventoryUnknownLiveDB(t *testing.T) {
	for _, mode := range []string{"null", "malformed", "selection", "gate", "missing", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			e, svc, w, lead, c := revisionInventoryFixture(t)
			switch mode {
			case "null":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", lead)
			case "malformed":
				e.exec("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", lead)
			case "selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", lead, uuid.New())
			case "gate":
				e.exec(`UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{gate}',
					jsonb_build_object('revision','1','presentation_id',$2::text,'payload_digest',repeat('a',64))) WHERE id=$1`, lead, uuid.NewString())
			case "missing", "foreign":
				id := uuid.NewString()
				if mode == "foreign" {
					// An actual immutable row belonging to another lead is insufficient.
					otherEnv, otherSvc, otherWorker, other, otherCandidate := newAutomaticRoundsHarnessFixture(t, 2, "codex")
					check, err := otherSvc.SubmitPlanCrossCheck(otherEnv.ctx, otherWorker, other, 1, otherCandidate)
					if err != nil {
						t.Fatal(err)
					}
					id = check.ID.String()
				}
				raw, err := json.Marshal(apitypes.PolicyRevisionSourceV1{Version: 1,
					AssociatedCrossChecks: []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &id}}})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_revision_source=$2,policy_candidate_cross_check_id=$3 WHERE id=$1", lead, raw, id)
			}
			if _, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c); err != nil {
				t.Fatal(err)
			}
			var unknown bool
			if err := e.pool.QueryRow(e.ctx, "SELECT policy_revision_source IS NULL FROM runs WHERE id=$1", lead).Scan(&unknown); err != nil || !unknown {
				t.Fatalf("unknown coverage bootstrapped: %v %v", unknown, err)
			}
		})
	}
}

type inventoryFenceBeginner struct {
	TxBeginner
	mode string
	hits int
}
type inventoryFenceTx struct {
	pgx.Tx
	owner *inventoryFenceBeginner
}

func (b *inventoryFenceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &inventoryFenceTx{Tx: tx, owner: b}, nil
}
func (tx *inventoryFenceTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "-- name: SetPolicyRevisionSource :execrows") {
		tx.owner.hits++
		if tx.owner.mode == "error" {
			return pgconn.CommandTag{}, errors.New("injected inventory source failure")
		}
		if tx.owner.mode == "released" {
			if _, err := tx.Tx.Exec(ctx, "UPDATE runs SET claim_released_at=now() WHERE id=$1", args[1]); err != nil {
				return pgconn.CommandTag{}, err
			}
		}
		if tx.owner.mode == "zero" {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

func TestPolicyRevisionInventoryRollbackLiveDB(t *testing.T) {
	for _, mode := range []string{"error", "zero", "released", "association", "stop", "commit"} {
		t.Run(mode, func(t *testing.T) {
			e, svc, w, lead, c := revisionInventoryFixture(t)
			if mode == "stop" {
				// Model the persisted accepted-Stop fence with a real same-run scope.
				scope, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: lead, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", lead, scope.ID)
			}
			before := policyOwnerStopSnapshot(t, e, lead)
			b := &inventoryFenceBeginner{TxBeginner: e.pool, mode: mode}
			svc.SetTxBeginner(b)
			if mode == "association" || mode == "commit" {
				point := mode
				if mode == "association" {
					point = "zero"
				}
				svc.SetTxBeginner(&associationBeginner{TxBeginner: e.pool, point: point, failure: errors.New("injected create failure")})
			}
			if _, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c); err == nil {
				t.Fatal("failed creation succeeded")
			}
			if mode == "error" || mode == "zero" || mode == "released" {
				if b.hits != 1 {
					t.Fatalf("source fence not exercised: %d", b.hits)
				}
			}
			if policyOwnerStopSnapshot(t, e, lead) != before {
				t.Fatal("failed creation retained source/selection/check writes")
			}
			var children int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM runs WHERE target_run_id=$1", lead).Scan(&children); err != nil || children != 0 {
				t.Fatalf("rolled back children=%d err=%v", children, err)
			}
		})
	}
}

func TestPolicyRevisionInventoryLostACKLiveDB(t *testing.T) {
	e, svc, w, lead, c := revisionInventoryFixture(t)
	injected := errors.New("lost inventory commit ACK")
	b := &associationBeginner{TxBeginner: e.pool, point: "lost_ack", failure: injected}
	svc.SetTxBeginner(b)
	if _, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c); !errors.Is(err, injected) || b.hits != 1 {
		t.Fatalf("lost ACK hits=%d err=%v", b.hits, err)
	}
	source := inventorySource(t, e, lead)
	if len(source.AssociatedCrossChecks) != 1 {
		t.Fatal("lost ACK did not commit inventory")
	}
	before := append([]byte{}, mustRun(t, e, lead).PolicyRevisionSource...)
	retry, err := svc.SubmitPlanCrossCheck(e.ctx, w, lead, 1, c)
	if err != nil || retry.ID.String() != *source.AssociatedCrossChecks[0].CrossCheckID ||
		!bytes.Equal(before, mustRun(t, e, lead).PolicyRevisionSource) || b.hits != 1 {
		t.Fatalf("lost ACK retry changed source: %v", err)
	}
}
