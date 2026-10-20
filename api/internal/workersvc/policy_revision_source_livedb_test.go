package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// This fixture seeds an initial complete V1 as an explicit precondition. These
// tests prove the public append writer, not new-run bootstrap or composition.
func revisionWriterHuman(t *testing.T) (usageTailEnv, apitypes.PolicyRevisionSourceV1, int64) {
	t.Helper()
	e := seedPolicyRevisionApprovalClaim(t, false, 3, true)
	ctx, cancel := context.WithTimeout(e.ctx, 20*time.Second)
	t.Cleanup(cancel)
	e.ctx = ctx
	r := mustRun(t, e.codexTestEnv, e.runID)
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := e.pool.QueryRow(e.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='revise_plan'", e.runID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	source.PrimaryRevision = nil
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET policy_revision_source=$2,policy_refusal_count=0 WHERE id=$1", e.runID, raw)
	return e, source, id
}

func revisionWriterPayload(t *testing.T, source apitypes.PolicyRevisionSourceV1, ref apitypes.PolicyRevisionRef) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"policy_revision": apitypes.PolicyRevisionCarrier{Gate: source.Gate, Revision: ref}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func revisionWriterHumanRef(id int64) apitypes.PolicyRevisionRef {
	value := apitypes.PolicyStopDecimal(fmt.Sprint(id))
	return apitypes.PolicyRevisionRef{Source: "run_user_input", InputID: &value}
}
func revisionWriterAppend(t *testing.T, e usageTailEnv, msgs ...IncomingMessage) {
	t.Helper()
	if err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, msgs, i64p(1)); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRevisionWriterHumanLiveDB(t *testing.T) {
	for _, mode := range []string{"bound_count_zero", "legacy", "unbound", "count_positive", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			e, source, id := revisionWriterHuman(t)
			if mode == "legacy" {
				e.exec("UPDATE run_user_inputs SET gate_binding=NULL,gate_revision=NULL WHERE id=$1", id)
			}
			if mode == "unbound" {
				e.exec("UPDATE run_user_inputs SET gate_binding='unbound',gate_revision=NULL WHERE id=$1", id)
			}
			if mode == "count_positive" {
				e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", e.runID)
			}
			payload := revisionWriterPayload(t, source, revisionWriterHumanRef(id))
			revisionWriterAppend(t, e, IncomingMessage{Seq: 1, Kind: "plan_revising", Payload: payload})
			r := mustRun(t, e.codexTestEnv, e.runID)
			got, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
			if err != nil || got.PrimaryRevision == nil || *got.PrimaryRevision.InputID != apitypes.PolicyStopDecimal(fmt.Sprint(id)) {
				t.Fatalf("positive source=%s err=%v", r.PolicyRevisionSource, err)
			}
			if mode == "duplicate" {
				revisionWriterAppend(t, e, IncomingMessage{Seq: 1, Kind: "plan_revising", Payload: []byte("{}")})
				if !bytes.Equal(r.PolicyRevisionSource, mustRun(t, e.codexTestEnv, e.runID).PolicyRevisionSource) {
					t.Fatal("duplicate rewrote source")
				}
			}
		})
	}
}

func TestPolicyRevisionWriterUnknownLiveDB(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "raw_nul", "duplicate_key", "wrong_input", "wrong_kind", "wrong_gate", "wrong_worker", "wrong_generation", "not_delivered", "settled", "applied", "included", "wrong_bound_gate", "unknown_source", "selection_without_inventory"} {
		t.Run(mode, func(t *testing.T) {
			e, source, id := revisionWriterHuman(t)
			payload := revisionWriterPayload(t, source, revisionWriterHumanRef(id))
			switch mode {
			case "missing":
				payload = []byte("{}")
			case "malformed":
				payload = []byte(`{"policy_revision":{}}`)
			case "raw_nul":
				payload = bytes.Replace(payload, []byte("run_user_input"), []byte(`run_user_\u0000input`), 1)
			case "duplicate_key":
				payload = append([]byte(`{"policy_revision":null,`), payload[1:]...)
			case "wrong_input":
				payload = revisionWriterPayload(t, source, revisionWriterHumanRef(id+1000000))
			case "wrong_kind":
				var approval int64
				if err := e.pool.QueryRow(e.ctx, "SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'", e.runID).Scan(&approval); err != nil {
					t.Fatal(err)
				}
				payload = revisionWriterPayload(t, source, revisionWriterHumanRef(approval))
			case "wrong_gate":
				changed := *source.Gate
				changed.PresentationID = uuid.NewString()
				source.Gate = &changed
				payload = revisionWriterPayload(t, source, revisionWriterHumanRef(id))
			case "wrong_worker":
				e.exec("UPDATE run_user_inputs SET consumed_worker_id=$2 WHERE id=$1", id, uuid.New())
			case "wrong_generation":
				e.exec("UPDATE run_user_inputs SET consumed_claim_generation=2 WHERE id=$1", id)
			case "not_delivered":
				e.exec("UPDATE run_user_inputs SET consumed_at=NULL,consumed_worker_id=NULL,consumed_claim_generation=NULL WHERE id=$1", id)
			case "settled":
				scope, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE run_user_inputs SET settled_at=now(),disposition='superseded_by_stop',superseded_by_stop_input_id=$2 WHERE id=$1", id, scope.ID)
			case "applied":
				e.exec("UPDATE run_user_inputs SET applied_at=now() WHERE id=$1", id)
			case "included":
				e.exec("UPDATE run_user_inputs SET included_at=now() WHERE id=$1", id)
			case "wrong_bound_gate":
				e.exec("UPDATE run_user_inputs SET gate_revision=2 WHERE id=$1", id)
			case "unknown_source":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", e.runID)
			case "selection_without_inventory":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", e.runID, uuid.New())
			}
			revisionWriterAppend(t, e, IncomingMessage{Seq: 1, Kind: "plan_revising", Payload: payload})
			if r := mustRun(t, e.codexTestEnv, e.runID); len(r.PolicyRevisionSource) != 0 {
				t.Fatalf("negative proof retained coverage: %s", r.PolicyRevisionSource)
			}
		})
	}
}

func TestPolicyRevisionWriterOrderingLiveDB(t *testing.T) {
	for _, mode := range []string{"array_order", "later_plan", "old_insert", "ordinary", "ineligible", "unsupported", "fallback", "accepted_stop"} {
		t.Run(mode, func(t *testing.T) {
			e, source, id := revisionWriterHuman(t)
			payload := revisionWriterPayload(t, source, revisionWriterHumanRef(id))
			before := mustRun(t, e.codexTestEnv, e.runID).PolicyRevisionSource
			switch mode {
			case "ineligible":
				e.exec("UPDATE runs SET interactive=true WHERE id=$1", e.runID)
			case "unsupported":
				e.svc.p.PolicyHoldNegotiated = false
			case "fallback":
				e.svc.SetTxBeginner(nil)
			case "accepted_stop":
				scope, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: e.runID, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				e.exec("UPDATE runs SET policy_stop_resolution=jsonb_build_object('version',1,'scope_input_id',$2::bigint) WHERE id=$1", e.runID, scope.ID)
			}
			msgs := []IncomingMessage{{Seq: 3, Kind: "plan_revising", Payload: payload}}
			switch mode {
			case "array_order":
				msgs = append(msgs, IncomingMessage{Seq: 2, Kind: "plan_revising", Payload: []byte("{}")})
			case "later_plan":
				msgs = append(msgs, IncomingMessage{Seq: 4, Kind: "plan", Payload: []byte("{}")})
			case "old_insert":
				revisionWriterAppend(t, e, IncomingMessage{Seq: 4, Kind: "plan", Payload: []byte("{}")})
			case "accepted_stop":
				msgs[0].Payload = []byte("{}")
			case "ordinary":
				msgs = []IncomingMessage{{Seq: 3, Kind: "status", Payload: []byte("{}")}}
			}
			revisionWriterAppend(t, e, msgs...)
			after := mustRun(t, e.codexTestEnv, e.runID)
			if mode == "array_order" {
				got, err := apitypes.ParsePolicyRevisionSourceV1(after.PolicyRevisionSource)
				if err != nil || got.PrimaryRevision == nil {
					t.Fatalf("highest seq lost: %s %v", after.PolicyRevisionSource, err)
				}
			} else if mode == "unsupported" || mode == "later_plan" {
				var isNull bool
				if err := e.pool.QueryRow(e.ctx, "SELECT policy_revision_source IS NULL FROM runs WHERE id=$1", e.runID).Scan(&isNull); err != nil || !isNull {
					t.Fatalf("unproven authoritative revision must invalidate coverage to SQL NULL: %v %v", isNull, err)
				}
			} else if !bytes.Equal(before, after.PolicyRevisionSource) {
				t.Fatal("source changed without authoritative eligible latest revision")
			}
		})
	}
}

// Faults execute no candidate command or SQL: they return a synthetic DB error
// at a fixed write, or fail commit after the real source write.
type revisionWriterBeginner struct {
	TxBeginner
	point string
	hits  int
}
type revisionWriterTx struct {
	pgx.Tx
	owner *revisionWriterBeginner
	wrote bool
}

func (b *revisionWriterBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &revisionWriterTx{Tx: tx, owner: b}, nil
}
func (tx *revisionWriterTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "-- name: SetPolicyRevisionSource :execrows") {
		if tx.owner.point == "writer" {
			tx.owner.hits++
			return pgconn.CommandTag{}, errors.New("injected source writer failure")
		}
		if tx.owner.point == "zero" {
			tx.owner.hits++
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		tx.wrote = true
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

type revisionWriterFailureRow struct{ err error }

func (r revisionWriterFailureRow) Scan(...any) error { return r.err }
func (tx *revisionWriterTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.owner.point == "witness" && (strings.HasPrefix(sql, "SELECT (") || strings.HasPrefix(sql, "-- name: GetPolicyStopRevisionInput :one")) {
		tx.owner.hits++
		return revisionWriterFailureRow{err: errors.New("injected witness read failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *revisionWriterTx) Commit(ctx context.Context) error {
	if tx.wrote && tx.owner.point == "commit" {
		tx.owner.hits++
		_ = tx.Rollback(ctx)
		return errors.New("injected later transaction failure")
	}
	return tx.Tx.Commit(ctx)
}

// Public Task birth and real checker child/decision/publication APIs exercise the
// source carrier component. Go supplies worker ownership and carrier messages;
// this is not a Task Stop-eligibility or composed-worker proof.
func TestPolicyRevisionWriterTaskPositiveGateLiveDB(t *testing.T) {
	e, f, run, candidate := acceptanceUnpublishedFixture(t)
	if mustRun(t, e, run).Kind != "task" {
		t.Fatal("expected public Task birth")
	}
	take := func(id uuid.UUID) {
		t.Helper()
		source := acceptanceSource(t, e, run)
		key := id.String()
		if err := f.svc.AppendMessagesForClaim(e.ctx, f.wkr, run, []IncomingMessage{{
			Seq: mustRun(t, e, run).LastSeq + 1, Kind: "plan_revising",
			Payload: revisionWriterPayload(t, source, apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &key}),
		}}, i64(1)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, candidate, 1)
	if err != nil {
		t.Fatal(err)
	}
	autopilotDecide(t, e, f, first, "revise")
	take(first.ID)
	atZero := acceptanceSource(t, e, run)
	if atZero.Gate != nil || atZero.PrimaryRevision == nil || atZero.PrimaryRevision.CrossCheckID == nil ||
		*atZero.PrimaryRevision.CrossCheckID != first.ID.String() {
		t.Fatalf("A was not taken at Gate 0: %+v", atZero)
	}
	candidate.PlanMd = "# Task candidate B"
	second, err := f.svc.SubmitPlanCrossCheck(e.ctx, f.wkr, run, 1, candidate, 2)
	if err != nil {
		t.Fatal(err)
	}
	autopilotDecide(t, e, f, second, "revise")
	req := acceptanceGateRequest()
	req.PlanMd = &candidate.PlanMd
	req.RequiredCapabilities, req.RequiredTools = &candidate.RequiredCapabilities, &candidate.RequiredTools
	req.SizeClass = &candidate.SizeClass
	f.mustPublish(run, req, 1)
	take(second.ID)
	if len(mustRun(t, e, run).PolicyRevisionSource) == 0 {
		t.Fatal("Task positive-gate carrier cleared verified source coverage")
	}
	got := acceptanceSource(t, e, run)
	if got.Gate == nil || got.Gate.Revision != "1" || got.PrimaryRevision == nil ||
		got.PrimaryRevision.CrossCheckID == nil || *got.PrimaryRevision.CrossCheckID != second.ID.String() ||
		len(got.AssociatedCrossChecks) != 1 || got.AssociatedCrossChecks[0].CrossCheckID == nil ||
		*got.AssociatedCrossChecks[0].CrossCheckID != first.ID.String() {
		t.Fatalf("B did not retain closed source with A: %+v", got)
	}
}

func TestPolicyRevisionWriterRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"writer", "zero", "witness", "commit"} {
		t.Run(point, func(t *testing.T) {
			e, source, id := revisionWriterHuman(t)
			before := mustRun(t, e.codexTestEnv, e.runID)
			b := &revisionWriterBeginner{TxBeginner: e.pool, point: point}
			e.svc.SetTxBeginner(b)
			err := e.svc.AppendMessagesForClaim(e.ctx, e.wkr, e.runID, []IncomingMessage{{Seq: 1, Kind: "plan_revising", Payload: revisionWriterPayload(t, source, revisionWriterHumanRef(id))}}, i64p(1))
			if err == nil || b.hits != 1 {
				t.Fatalf("fault err=%v hits=%d", err, b.hits)
			}
			after := mustRun(t, e.codexTestEnv, e.runID)
			if after.LastSeq != before.LastSeq || !bytes.Equal(after.PolicyRevisionSource, before.PolicyRevisionSource) {
				t.Fatal("failed transaction retained source or highwater")
			}
			var count int
			if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM run_messages WHERE run_id=$1", e.runID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("messages=%d err=%v", count, err)
			}
		})
	}
}

func TestPolicyRevisionWriterAutomaticLiveDB(t *testing.T) {
	for _, mode := range []string{"null_gate", "current_gate", "wrong_selected", "wrong_generation", "settled", "bad_digest", "preserve_former_primary", "preserve_finite_associations"} {
		t.Run(mode, func(t *testing.T) {
			env, svc, w, lead, candidate := newAutomaticRoundsHarnessFixture(t, 2, "codex")
			ctx, cancel := context.WithTimeout(env.ctx, 20*time.Second)
			t.Cleanup(cancel)
			env.ctx = ctx
			svc.p.PolicyHoldNegotiated = true
			svc.p.PolicyHoldSnapshotEnabled = true
			svc.p.TerminalPendingLease = time.Hour
			w.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1, capability.CrossCheckV1, capability.CrossCheckRoundsV1, capability.CrossCheckCodexLeadV1}
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
			cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate, 1)
			if err != nil {
				t.Fatal(err)
			}
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
			if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
				t.Fatal(err)
			}
			env.exec("DELETE FROM runs WHERE id=$1", child)
			id := cc.ID.String()
			ref := apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id}
			source := apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{ref}}
			if mode == "current_gate" {
				env.exec("UPDATE runs SET plan_md=$2,milestones_candidate=$3,required_capabilities=$4,required_tools=$5,size_class=$6,gate_revision=1,gate_presentation_id=$7 WHERE id=$1", lead, candidate.PlanMd, candidate.Milestones, candidate.RequiredCapabilities, candidate.RequiredTools, candidate.SizeClass, uuid.New())
				r := mustRun(t, env, lead)
				raw, digest, err := gatePayloadFromRow(r).digest()
				if err != nil {
					t.Fatal(err)
				}
				env.exec("UPDATE runs SET gate_presented_payload=$2,gate_payload_digest=$3 WHERE id=$1", lead, raw, digest)
				// Use the same persisted gate context shape as the public wire contract.
				source.Gate = &apitypes.PolicyStopGateContext{Revision: "1", PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(), PayloadDigest: fmt.Sprintf("%x", digest)}
			}
			if mode == "preserve_former_primary" || mode == "preserve_finite_associations" {
				source.PrimaryRevision = &ref
				source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{}
			}
			if mode == "preserve_finite_associations" {
				second, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate, 2)
				if err != nil {
					t.Fatal(err)
				}
				child := uuid.UUID(second.CheckerRunID.Bytes)
				env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, w.ID)
				if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
					t.Fatal(err)
				}
				env.exec("DELETE FROM runs WHERE id=$1", child)
				secondID := second.ID.String()
				source.AssociatedCrossChecks = []apitypes.PolicyRevisionRef{{Source: "plan_cross_check", CrossCheckID: &secondID}}
			}
			raw, err := json.Marshal(source)
			if err != nil {
				t.Fatal(err)
			}
			env.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", lead, raw)
			payload := revisionWriterPayload(t, source, ref)
			if mode == "preserve_former_primary" || mode == "preserve_finite_associations" {
				// The human witness uses actual ACK delivery; the previous automatic
				// primary must survive in the finite association inventory.
				w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.InputReceiptsV1)
				var input store.RunUserInput
				input, err = env.q.CreateRunInput(env.ctx, store.CreateRunInputParams{RunID: lead, Kind: "revise_plan"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = svc.AckInputs(env.ctx, w, lead, 1, []int64{input.ID}); err != nil {
					t.Fatal(err)
				}
				payload = revisionWriterPayload(t, source, revisionWriterHumanRef(input.ID))
			}
			switch mode {
			case "bad_digest":
				env.exec("UPDATE cross_checks SET candidate_digest=$2 WHERE id=$1", cc.ID, bytes.Repeat([]byte{1}, 32))
			case "wrong_selected":
				env.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", lead, uuid.New())
			case "wrong_generation":
				env.exec("UPDATE cross_checks SET lead_claim_generation=2 WHERE id=$1", cc.ID)
			case "settled":
				scope, err := env.q.CreateRunInput(env.ctx, store.CreateRunInputParams{RunID: lead, Kind: "scope"})
				if err != nil {
					t.Fatal(err)
				}
				env.exec("UPDATE cross_checks SET revision_disposition='superseded_by_stop',revision_settled_at=now(),revision_superseded_by_stop_input_id=$2 WHERE id=$1", cc.ID, scope.ID)
			}
			beforeAppend := mustRun(t, env, lead)
			seq := beforeAppend.LastSeq + 1
			if err := svc.AppendMessagesForClaim(env.ctx, w, lead, []IncomingMessage{{Seq: seq, Kind: "plan_revising", Payload: payload}}, i64p(1)); err != nil {
				t.Fatal(err)
			}
			r := mustRun(t, env, lead)
			if r.PolicyCandidateCrossCheckID != beforeAppend.PolicyCandidateCrossCheckID {
				t.Fatal("writer changed selected identity")
			}
			if strings.HasPrefix(mode, "wrong_") || mode == "settled" || mode == "bad_digest" {
				if len(r.PolicyRevisionSource) != 0 {
					t.Fatalf("invalid automatic proof retained source: %s", r.PolicyRevisionSource)
				}
				return
			}
			got, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
			if err != nil || got.PrimaryRevision == nil {
				t.Fatalf("automatic source=%s err=%v", r.PolicyRevisionSource, err)
			}
			if mode == "preserve_finite_associations" {
				if got.PrimaryRevision.Source != "run_user_input" || len(got.AssociatedCrossChecks) != 2 ||
					*got.AssociatedCrossChecks[0].CrossCheckID != *source.AssociatedCrossChecks[0].CrossCheckID ||
					*got.AssociatedCrossChecks[1].CrossCheckID != id {
					t.Fatal("lost finite inventory or former primary")
				}
			} else if mode == "preserve_former_primary" {
				if got.PrimaryRevision.Source != "run_user_input" || len(got.AssociatedCrossChecks) != 1 || *got.AssociatedCrossChecks[0].CrossCheckID != id {
					t.Fatal("lost former automatic primary")
				}
			} else if got.PrimaryRevision.CrossCheckID == nil || *got.PrimaryRevision.CrossCheckID != id {
				t.Fatal("selected automatic identity changed")
			}
		})
	}
}
