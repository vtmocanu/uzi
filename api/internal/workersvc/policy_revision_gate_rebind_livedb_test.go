package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Public task creation supplies birth coverage. Ownership and gate capability
// are fixture state; SetStateReport executes the real publication transaction.
func revisionGateFixture(t *testing.T) (codexTestEnv, *gateFix, uuid.UUID) {
	t.Helper()
	e, svc, w, run, _ := revisionInventoryFixture(t)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.GateRevisionV1)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	e.exec("UPDATE runs SET plan_cross_check_required=false,auto_approve=false WHERE id=$1", run)
	f := &gateFix{t: t, ctx: e.ctx, pool: e.pool, q: e.q, svc: svc, wkr: w, userID: w.UserID}
	return e, f, run
}

func assertRevisionGateBound(t *testing.T, e codexTestEnv, run uuid.UUID, revision int64) apitypes.PolicyRevisionSourceV1 {
	t.Helper()
	r := mustRun(t, e, run)
	source := inventorySource(t, e, run)
	matched, matchErr := policyRevisionGateMatches(context.Background(), e.q, r, source)
	if r.GateRevision != revision || matchErr != nil || !matched {
		t.Fatalf("source does not authenticate published revision %d: %s", revision, r.PolicyRevisionSource)
	}
	return source
}

func TestPolicyRevisionGatePublicationLiveDB(t *testing.T) {
	e, f, run := revisionGateFixture(t)
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	assertRevisionGateBound(t, e, run, 1)
	f.mustPublish(run, gateReq(i64(1), uid()), 2)
	source := assertRevisionGateBound(t, e, run, 2)
	if source.PrimaryRevision != nil || len(source.AssociatedCrossChecks) != 0 {
		t.Fatal("publication fabricated revision inventory")
	}
	e.exec("UPDATE runs SET policy_refusal_count=1 WHERE id=$1", run)
	if policyRevisionPlanApproved(e.ctx, e.q, mustRun(t, e, run), true) {
		t.Fatal("publication minted approval")
	}
}

func TestPolicyRevisionGateAwaitingInputLiveDB(t *testing.T) {
	e, f, run := revisionGateFixture(t)
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	before := append([]byte{}, mustRun(t, e, run).PolicyRevisionSource...)
	question := "policy-source-question"
	_, applied, _, err := f.report(run, StateRequest{
		State: "awaiting_input", ClaimGeneration: i64(1), OpenQuestionID: &question,
	})
	if err != nil || !applied {
		t.Fatalf("question park applied=%v err=%v", applied, err)
	}
	parked := mustRun(t, e, run)
	if parked.Status != "awaiting_input" || !bytes.Equal(before, parked.PolicyRevisionSource) {
		t.Fatal("question park changed source or did not park")
	}
	f.mustPublish(run, gateReq(i64(1), uid()), 2)
	assertRevisionGateBound(t, e, run, 2)
}

func TestPolicyRevisionGatePreservesInventoryLiveDB(t *testing.T) {
	e, svc, w, run, candidate := revisionInventoryFixture(t)
	first, err := svc.SubmitPlanCrossCheck(e.ctx, w, run, 1, candidate)
	if err != nil {
		t.Fatal(err)
	}
	// Historical feedback and primary are explicit source preconditions here.
	// The cross-check and its inventory were created through Submit.
	source := inventorySource(t, e, run)
	input, err := e.q.CreateRunInput(e.ctx, store.CreateRunInputParams{RunID: run, Kind: "revise_plan"})
	if err != nil {
		t.Fatal(err)
	}
	primary := revisionWriterHumanRef(input.ID)
	source.PrimaryRevision = &primary
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	e.exec("UPDATE runs SET policy_revision_source=$2,plan_cross_check_required=false,auto_approve=false WHERE id=$1", run, raw)
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.GateRevisionV1)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
	f := &gateFix{t: t, ctx: e.ctx, pool: e.pool, q: e.q, svc: svc, wkr: w}
	for revision := int64(1); revision <= 2; revision++ {
		f.mustPublish(run, gateReq(i64(1), uid()), revision)
		got := assertRevisionGateBound(t, e, run, revision)
		if !reflect.DeepEqual(got.PrimaryRevision, source.PrimaryRevision) ||
			!reflect.DeepEqual(got.AssociatedCrossChecks, source.AssociatedCrossChecks) {
			t.Fatal("publication dropped historical revision inventory")
		}
		cc, err := e.q.GetPolicyStopCrossCheck(e.ctx, first.ID)
		if err != nil || cc.ID != first.ID {
			t.Fatalf("publication removed historical feedback: %v", err)
		}
	}
}

func TestPolicyRevisionGateInvalidCoverageLiveDB(t *testing.T) {
	for _, mode := range []string{"unknown", "malformed", "wrong_gate", "selection", "idless_initial", "idless_replacement"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := revisionGateFixture(t)
			if mode != "idless_initial" {
				f.mustPublish(run, gateReq(i64(1), uid()), 1)
			}
			switch mode {
			case "unknown":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "malformed":
				e.exec("UPDATE runs SET policy_revision_source='{}' WHERE id=$1", run)
			case "wrong_gate":
				e.exec("UPDATE runs SET policy_revision_source=jsonb_set(policy_revision_source,'{gate,presentation_id}',to_jsonb($2::text)) WHERE id=$1", run, uuid.NewString())
			case "selection":
				e.exec("UPDATE runs SET policy_candidate_cross_check_id=$2 WHERE id=$1", run, uuid.New())
			}
			req := gateReq(i64(1), uid())
			if strings.HasPrefix(mode, "idless") {
				req.PresentationID = nil
			}
			want := int64(2)
			if mode == "idless_initial" {
				want = 1
			}
			f.mustPublish(run, req, want)
			if r := mustRun(t, e, run); len(r.PolicyRevisionSource) != 0 {
				t.Fatalf("invalid coverage survived publication: %s", r.PolicyRevisionSource)
			}
		})
	}
}

type revisionGateBeginner struct {
	TxBeginner
	point string
	hits  int
}
type revisionGateTx struct {
	pgx.Tx
	owner     *revisionGateBeginner
	published bool
}

func (b *revisionGateBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &revisionGateTx{Tx: tx, owner: b}, nil
}
func (tx *revisionGateTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: PublishRunGatePresentation :one") {
		tx.published = true
	}
	if tx.published && strings.HasPrefix(sql, "-- name: GetRunByID :one") && tx.owner.point == "read_error" {
		return revisionWriterFailureRow{err: errors.New("injected published row read failure")}
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}
func (tx *revisionGateTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	point := tx.owner.point
	target := strings.HasPrefix(sql, "-- name: SetPolicyRevisionSource :execrows")
	if target {
		tx.owner.hits++
	}
	if target && point == "source_error" {
		return pgconn.CommandTag{}, errors.New("injected gate source error")
	}
	if target && point == "source_zero" {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	if point == "update_zero" && strings.HasPrefix(sql, "-- name: SetRunAwaitingApproval :execrows") {
		return pgconn.NewCommandTag("UPDATE 0"), nil
	}
	if point == "settlement" && strings.Contains(sql, "WITH settled AS") {
		return pgconn.CommandTag{}, errors.New("injected settlement failure")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}
func (tx *revisionGateTx) Commit(ctx context.Context) error {
	if tx.owner.point == "commit" {
		_ = tx.Rollback(ctx)
		return errors.New("injected gate commit failure")
	}
	return tx.Tx.Commit(ctx)
}

func revisionGateSnapshot(t *testing.T, e codexTestEnv, run uuid.UUID) string {
	t.Helper()
	var history string
	if err := e.pool.QueryRow(e.ctx, "SELECT coalesce(jsonb_agg(to_jsonb(p) ORDER BY revision),'[]'::jsonb)::text FROM run_gate_presentations p WHERE run_id=$1", run).Scan(&history); err != nil {
		t.Fatal(err)
	}
	return policyOwnerStopSnapshot(t, e, run) + history
}

func TestPolicyRevisionGateRollbackLiveDB(t *testing.T) {
	for _, point := range []string{"source_error", "source_zero", "read_error", "settlement", "commit", "update_zero"} {
		t.Run(point, func(t *testing.T) {
			e, f, run := revisionGateFixture(t)
			f.mustPublish(run, gateReq(i64(1), uid()), 1)
			active := seedRevisionTarget(t, f, run, 1)
			req := gateReq(i64(1), uid())
			req.ExpectedGateRevision, req.RevisionInputID = i64(1), &active
			before := revisionGateSnapshot(t, e, run)
			b := &revisionGateBeginner{TxBeginner: e.pool, point: point}
			f.svc.SetTxBeginner(b)
			_, applied, _, err := f.report(run, req)
			if applied || (point != "update_zero" && err == nil) {
				t.Fatalf("fault report applied=%v err=%v", applied, err)
			}
			wantHits := 1
			if point == "update_zero" || point == "read_error" {
				wantHits = 0
			}
			if b.hits != wantHits {
				t.Fatalf("source writes=%d want=%d", b.hits, wantHits)
			}
			if revisionGateSnapshot(t, e, run) != before {
				t.Fatal("rollback changed run/source/history/input/receipt")
			}
		})
	}
}

func TestPolicyRevisionGateControlsLiveDB(t *testing.T) {
	for _, mode := range []string{"retain", "adopt", "refused", "stale", "declined", "ineligible", "exact_retry"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := revisionGateFixture(t)
			first := gateReq(i64(1), uid())
			if mode == "adopt" {
				first.PresentationID = nil
			}
			f.mustPublish(run, first, 1)
			req := first
			switch mode {
			case "adopt":
				req.PresentationID, req.AdoptGateRevision = uid(), i64(1)
			case "refused":
				changed := "# conflicting plan"
				req.PlanMd = &changed
			case "stale":
				req.ClaimGeneration = i64(2)
			case "declined":
				e.exec("UPDATE runs SET status='paused' WHERE id=$1", run)
			case "ineligible":
				e.exec(`UPDATE runs SET harness='claude', codex_secret_id=NULL, codex_auth_mode=NULL,
					codex_secret_label=NULL, codex_account_key=NULL, codex_material_revision=NULL,
					codex_account_revision=NULL WHERE id=$1`, run)
				req.PresentationID = uid()
			case "exact_retry":
				active := seedRevisionTarget(t, f, run, 1)
				req = gateReq(i64(1), uid())
				req.ExpectedGateRevision, req.RevisionInputID = i64(1), &active
				f.mustPublish(run, req, 2)
			}
			before := append([]byte{}, mustRun(t, e, run).PolicyRevisionSource...)
			b := &revisionGateBeginner{TxBeginner: e.pool, point: "source_error"}
			f.svc.SetTxBeginner(b)
			_, applied, _, err := f.report(run, req)
			wantApplied := mode != "refused" && mode != "stale" && mode != "declined"
			if applied != wantApplied || (wantApplied && err != nil) {
				t.Fatalf("control applied=%v err=%v", applied, err)
			}
			if b.hits != 0 || !bytes.Equal(before, mustRun(t, e, run).PolicyRevisionSource) {
				t.Fatal("control rebound source")
			}
		})
	}
}
