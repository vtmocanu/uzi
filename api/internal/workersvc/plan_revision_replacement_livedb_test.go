package workersvc

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
)

// replacementSnapshot includes receipt timestamps and dispositions, the persisted
// plan and receipt proof, and all presentation allocations.
func replacementSnapshot(t *testing.T, f *gateFix, run uuid.UUID) string {
	t.Helper()
	var snapshot string
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
 'run', jsonb_build_object('status', status, 'plan', plan_md,
   'revision', gate_revision, 'count', revise_count, 'payload', gate_presented_payload,
   'presentation', gate_presentation_id, 'frozen', milestones_frozen,
   'stop', stop_kind, 'refusals', gate_refusal_count),
 'inputs', (SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM run_user_inputs i WHERE run_id = r.id),
 'presentations', (SELECT jsonb_agg(to_jsonb(p) ORDER BY revision) FROM run_gate_presentations p WHERE run_id = r.id)
 )::text FROM runs r WHERE id = $1`, run).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func replacementIDs(t *testing.T, f *gateFix, run uuid.UUID) []int64 {
	t.Helper()
	rows, err := f.pool.Query(f.ctx, `SELECT id FROM run_user_inputs WHERE run_id=$1 ORDER BY id`, run)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func assertReplacementPending(t *testing.T, f *gateFix, run uuid.UUID, ids []int64) {
	t.Helper()
	var pending bool
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*)=$2 AND bool_and(
 kind='revise_plan' AND gate_binding='bound' AND gate_revision=1
 AND consumed_at IS NOT NULL AND consumed_worker_id=$3 AND consumed_claim_generation=1
 AND applied_at IS NULL AND disposition IS NULL)
 FROM run_user_inputs WHERE run_id=$1`, run, len(ids), f.wkr.ID).Scan(&pending); err != nil || !pending {
		t.Fatalf("accepted revisions must remain pending under G1: pending=%v err=%v", pending, err)
	}
	if got := replacementIDs(t, f, run); !slices.Equal(got, ids) {
		t.Fatalf("input IDs=%v want=%v", got, ids)
	}
	if s := f.sideEffects(t, run); int(s.reviseCount) != len(ids) || s.inputRows != len(ids) ||
		s.frozen != nil || s.contractFrozen || s.budgetIters != nil || s.stopKind != nil {
		t.Fatalf("revision acceptance changed count or granted approval: %+v", s)
	}
	if r := f.row(run); r.revision != 1 || r.status != "awaiting_approval" ||
		r.planMd == nil || *r.planMd != gatePlanA {
		t.Fatalf("delivery/ACK must not persist a replacement plan: %+v", r)
	}
}

// acceptReplacementInputs uses only ordinary Service submissions, with the normal
// three-revision cap. Each successor arrives while all predecessors remain pending.
func acceptReplacementInputs(t *testing.T) (*gateFix, uuid.UUID, []int64) {
	t.Helper()
	f := newGateFix(t, 1)
	run := f.gatedRun()
	f.liveWorker()
	if f.svc.p.PlanMaxRevisions != 3 {
		t.Fatalf("fixture revision cap=%d want=3", f.svc.p.PlanMaxRevisions)
	}
	var ids []int64
	for n := 1; n <= 3; n++ {
		if _, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
			t.Fatalf("ordinary revision %d: %v", n, err)
		}
		// Read the actual inserted rows rather than relying on SubmitInputResult.ID.
		ids = replacementIDs(t, f, run)
		if len(ids) != n {
			t.Fatalf("revision %d produced IDs %v", n, ids)
		}
		deliver := func() {
			t.Helper()
			got, err := f.svc.ConsumeInputs(f.ctx, f.wkr, run)
			if err != nil || !got.Receipts || len(got.Inputs) != n {
				t.Fatalf("delivery %d: %+v err=%v", n, got, err)
			}
			for j, input := range got.Inputs {
				if input.ID != ids[j] || input.Kind != "revise_plan" || input.GateBinding == nil ||
					*input.GateBinding != "bound" || input.GateRevision == nil || *input.GateRevision != 1 {
					t.Fatalf("delivery lost revision identity: %+v", input)
				}
			}
			ack, err := f.svc.AckInputs(f.ctx, f.wkr, run, 1, ids)
			if err != nil || !ack.Active || len(ack.Inputs) != n {
				t.Fatalf("G1 ACK: %+v err=%v", ack, err)
			}
		}
		deliver()
		assertReplacementPending(t, f, run, ids)
		before := replacementSnapshot(t, f, run)
		deliver()
		if after := replacementSnapshot(t, f, run); after != before {
			t.Fatalf("duplicate delivery/ACK changed rows, counter or settlement:\nbefore=%s\nafter=%s", before, after)
		}
	}
	before := replacementSnapshot(t, f, run)
	if _, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); !errors.Is(err, ErrReviseCapReached) {
		t.Fatalf("fourth ordinary revise: %v, want ErrReviseCapReached", err)
	}
	if after := replacementSnapshot(t, f, run); after != before {
		t.Fatalf("capped revise mutated lifecycle:\nbefore=%s\nafter=%s", before, after)
	}
	_, err := f.submit(run, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(0)})
	assertMismatch(t, err, 1)
	if after := replacementSnapshot(t, f, run); after != before {
		t.Fatalf("stale revision submission mutated lifecycle:\nbefore=%s\nafter=%s", before, after)
	}
	return f, run, ids
}

func TestPlanRevisionReplacementSubmissionLifecycleLiveDB(t *testing.T) {
	f, run, ids := acceptReplacementInputs(t)
	plan := "# Successor plan answering K"
	req := gateReq(i64(1), uid())
	req.PlanMd, req.ExpectedGateRevision, req.RevisionInputID = &plan, i64(1), &ids[2]
	req.SupersededRevisionInputIDs = ids[:2]
	result, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, run, req)
	if err != nil || !result.Applied || result.GateRevision != 2 ||
		result.RevisionInputID == nil || *result.RevisionInputID != ids[2] ||
		!slices.Equal(result.SupersededRevisionInputIDs, ids[:2]) {
		t.Fatalf("K publication: %+v err=%v", result, err)
	}
	if r := f.row(run); r.revision != 2 || r.status != "awaiting_approval" || r.planMd == nil ||
		*r.planMd != plan || r.generation != 1 || r.refusals != 0 {
		t.Fatalf("successor plan was not persisted at R2 under G1: %+v", r)
	}
	var settled bool
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*)=3 AND bool_and(
 kind='revise_plan' AND gate_binding='bound' AND gate_revision=1
 AND consumed_worker_id=$2 AND consumed_claim_generation=1
 AND applied_at IS NOT NULL
 AND CASE WHEN id=$3 THEN disposition IS NULL ELSE disposition IS NOT DISTINCT FROM 'superseded' END)
 FROM run_user_inputs WHERE run_id=$1`, run, f.wkr.ID, ids[2]).Scan(&settled); err != nil || !settled {
		t.Fatalf("I/J must be explicitly superseded and K normally applied: settled=%v err=%v", settled, err)
	}
	if s := f.sideEffects(t, run); s.reviseCount != 3 || s.inputRows != 3 || s.frozen != nil || s.contractFrozen || s.budgetIters != nil {
		t.Fatalf("replacement changed revision count or granted approval: %+v", s)
	}
	presentations := f.presentations(run)
	if len(presentations) != 2 || presentations[*req.PresentationID] != 2 {
		t.Fatalf("presentation allocations=%v, want only R1 and R2", presentations)
	}
	before := replacementSnapshot(t, f, run)
	replay, err := f.svc.SetStateReportWithReconciliation(f.ctx, f.wkr, run, req)
	if err != nil || !replay.Applied || replay.GateRevision != 2 ||
		replay.RevisionInputID == nil || *replay.RevisionInputID != ids[2] ||
		!slices.Equal(replay.SupersededRevisionInputIDs, ids[:2]) {
		t.Fatalf("exact K replay: %+v err=%v", replay, err)
	}
	if after := replacementSnapshot(t, f, run); after != before {
		t.Fatalf("exact replay changed plan, disposition timestamps or allocations:\nbefore=%s\nafter=%s", before, after)
	}
	if !reflect.DeepEqual(f.presentations(run), presentations) {
		t.Fatal("exact replay allocated an extra revision")
	}
	got, err := f.svc.ConsumeInputs(f.ctx, f.wkr, run)
	if err != nil || len(got.Inputs) != 0 {
		t.Fatalf("settled inputs replayed: %+v err=%v", got, err)
	}
	_, err = f.submit(run, "revise_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)})
	assertMismatch(t, err, 2)
	if after := replacementSnapshot(t, f, run); after != before {
		t.Fatal("delivery or stale R1 submission changed the published replacement")
	}
}

func TestPlanRevisionReplacementCappedOwnerControlsLiveDB(t *testing.T) {
	for _, kind := range []string{"cancel", "reject_plan"} {
		t.Run(kind, func(t *testing.T) {
			f, run, ids := acceptReplacementInputs(t)
			f.staleWorker()
			opts := SubmitInputOptions{}
			wantStatus, wantStop := "cancelled", "cancelled"
			if kind == "reject_plan" {
				opts.ExpectedGateRevision = i64(1)
				wantStatus, wantStop = "failed", "plan_rejected"
				before := replacementSnapshot(t, f, run)
				_, err := f.submit(run, kind, nil, SubmitInputOptions{ExpectedGateRevision: i64(0)})
				assertMismatch(t, err, 1)
				if after := replacementSnapshot(t, f, run); after != before {
					t.Fatal("stale bound reject changed the capped run")
				}
			}
			result, err := f.submit(run, kind, nil, opts)
			if err != nil || !result.ServerSide {
				t.Fatalf("capped owner %s: %+v err=%v", kind, result, err)
			}
			r, s := f.row(run), f.sideEffects(t, run)
			if r.status != wantStatus || s.stopKind == nil || *s.stopKind != wantStop ||
				r.revision != 1 || r.planMd == nil || *r.planMd != gatePlanA ||
				s.reviseCount != 3 || s.frozen != nil || s.contractFrozen || s.budgetIters != nil {
				t.Fatalf("owner control must terminate without publishing or approving: run=%+v effects=%+v", r, s)
			}
			if kind == "reject_plan" && (r.failureReason == nil || *r.failureReason != "because") {
				t.Fatalf("reject reason=%v want because", r.failureReason)
			}
			// Terminal cleanup may dispose inputs. Only persisted plan/revision and
			// presentation rows prove publication; disposal alone is no such proof.
			if got := f.presentations(run); len(got) != 1 {
				t.Fatalf("owner control allocated a replacement: %v", got)
			}
			if got := replacementIDs(t, f, run); !slices.Equal(got, ids) {
				t.Fatalf("owner control inserted or removed revision rows: %v want=%v", got, ids)
			}
		})
	}
}
