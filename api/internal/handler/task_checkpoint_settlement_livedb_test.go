package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Pin actual proof operands; the shared forge otherwise accepts unknown SHAs.
type taskCheckpointSettlementForge struct {
	*liveFakeForge
	candidates map[string]int
}

func (f *taskCheckpointSettlementForge) CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (forge.Ancestry, error) {
	if head != settleHead || (candidate != settlePushed && candidate != settleSource && candidate != settleAdopted) {
		return forge.AncestryUnknown, fmt.Errorf("unexpected settlement proof: head=%q candidate=%q", head, candidate)
	}
	f.candidates[candidate]++
	return f.liveFakeForge.CompareAncestry(ctx, projectID, head, candidate)
}

func TestTaskCheckpointDescendantLiveSettlementLiveDB(t *testing.T) {
	f := newTaskCheckpointHandlerFixture(t)
	proof := &taskCheckpointSettlementForge{liveFakeForge: f.lf, candidates: make(map[string]int)}
	f.wsvc.SetForges(settleForgeBuilder{f: proof})
	succ := f.successor()
	before := f.authoritySnapshot()
	body := liveBody(f.firstGeneration, succ, settlePushed, settleSource, settleAdopted, apitypes.RecoverySettleTargetCheckpoint)
	code, res, raw := f.settleLive(f.tokenA, f.run, f.pred, body)
	f.assertReleased(code, res, raw)
	want := holdRow{state: "released", evidence: "live_ancestry", pushed: settlePushed, source: settleSource,
		adopted: settleAdopted, finalHead: settleHead, bran: f.childKey, successor: succ, liveWorkerNull: true, liveRunNull: true}
	if got := f.hold(f.pred); got != want {
		t.Fatalf("descendant release=%+v want=%+v", got, want)
	}
	if got := f.releaseTarget(f.pred); got != "checkpoint" {
		t.Fatalf("release target=%q", got)
	}
	f.assertOpen(f.rootHold, f.sibGen, f.sibWork)
	refs, branches := f.lf.asked()
	hc, cc := f.fake.calls()
	if len(refs) != 1 || refs[0] != "refs/uzi-checkpoints/"+f.childKey || len(branches) != 0 || hc != 0 || cc != 3 {
		t.Fatalf("proof refs=%v branches=%v BranchHead=%d CompareAncestry=%d", refs, branches, hc, cc)
	}
	for _, sha := range []string{settlePushed, settleSource, settleAdopted} {
		if proof.candidates[sha] != 1 {
			t.Fatalf("proof candidate %s called %d times, want once", sha, proof.candidates[sha])
		}
	}
	// Compare all run/attempt/input fields and all other hold rows, including
	// timestamps. The sole allowed mutation is the predecessor's release row.
	after := f.authoritySnapshot()
	var oldState, newState map[string]json.RawMessage
	if err := json.Unmarshal([]byte(before), &oldState); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(after), &newState); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run", "attempts", "inputs"} {
		if string(oldState[key]) != string(newState[key]) {
			t.Fatalf("settlement changed %s", key)
		}
	}
	var oldHolds, newHolds []map[string]any
	if err := json.Unmarshal(oldState["holds"], &oldHolds); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(newState["holds"], &newHolds); err != nil {
		t.Fatal(err)
	}
	if len(oldHolds) != len(newHolds) {
		t.Fatal("settlement changed custody identities")
	}
	for i := range oldHolds {
		if oldHolds[i]["id"] == f.pred.String() {
			continue
		}
		a, err := json.Marshal(oldHolds[i])
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(newHolds[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("settlement changed sibling hold %v", oldHolds[i]["id"])
		}
	}
	// Identical retries before and after a router/service restart acknowledge
	// the stored identity without reading the forge or touching any audit row.
	for _, restart := range []bool{false, true} {
		if restart {
			f.wire()
		}
		code, res, raw = f.settleLive(f.tokenA, f.run, f.pred, body)
		f.assertReleased(code, res, raw)
		if f.lf.total() != 4 {
			t.Fatalf("retry restart=%v made extra forge calls: %d", restart, f.lf.total())
		}
		if f.authoritySnapshot() != after {
			t.Fatalf("retry restart=%v changed durable evidence", restart)
		}
	}
}

// Change only query operands. Mutating the immutable descriptor row would hit
// the trigger before exercising this final service-to-SQL proof fence.
func TestTaskCheckpointLiveSettlementFinalSQLFenceLiveDB(t *testing.T) {
	f := newTaskCheckpointHandlerFixture(t)
	succ := f.successor()
	q := store.New(f.pool)
	valid := store.ReleasePredecessorCustodyHoldByLiveAncestryParams{
		PublishedSha: settlePushed, SourceSha: settleSource, AdoptedSha: settleAdopted, FinalHeadSha: settleHead,
		SuccessorGeneration: succ, ProvenBranch: f.childKey, Target: apitypes.RecoverySettleTargetCheckpoint,
		HoldID: f.pred, RunID: f.run, UserID: f.user, PredecessorGeneration: f.firstGeneration, WorkerID: f.workerA,
		CapturedBranch: pgtype.Text{String: f.rootKey, Valid: true}, Kind: "task", CapturedCoordinates: f.coordinates,
	}
	for _, tc := range []struct {
		name   string
		change func(*store.ReleasePredecessorCustodyHoldByLiveAncestryParams)
	}{
		{"altered frozen descriptor", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) {
			var altered map[string]any
			if err := json.Unmarshal(f.coordinates, &altered); err != nil {
				t.Fatal(err)
			}
			altered["checkpoint_key"] = f.rootKey
			var err error
			p.CapturedCoordinates, err = json.Marshal(altered)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong captured branch", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) { p.CapturedBranch.String = f.childKey }},
		{"wrong kind", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) { p.Kind = "issue" }},
		{"wrong successor generation", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) { p.SuccessorGeneration = succ + 1 }},
		{"root key is not child checkpoint", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) { p.ProvenBranch = f.rootKey }},
		{"wrong server-held source", func(p *store.ReleasePredecessorCustodyHoldByLiveAncestryParams) { p.SourceSha = settleOther }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.change(&p)
			before := f.authoritySnapshot()
			n, err := q.ReleasePredecessorCustodyHoldByLiveAncestry(f.ctx, p)
			if err != nil || n != 0 {
				t.Fatalf("final fence rows=%d err=%v, want 0", n, err)
			}
			if f.authoritySnapshot() != before {
				t.Fatal("refused final SQL changed authority/custody audit")
			}
		})
	}
	n, err := q.ReleasePredecessorCustodyHoldByLiveAncestry(f.ctx, valid)
	if err != nil || n != 1 {
		t.Fatalf("valid final SQL positive control rows=%d err=%v", n, err)
	}
	want := holdRow{state: "released", evidence: "live_ancestry", pushed: settlePushed, source: settleSource,
		adopted: settleAdopted, finalHead: settleHead, bran: f.childKey, successor: succ, liveWorkerNull: true, liveRunNull: true}
	if got := f.hold(f.pred); got != want || f.releaseTarget(f.pred) != "checkpoint" {
		t.Fatalf("positive control audit=%+v", got)
	}
	f.assertOpen(f.rootHold, f.sibGen, f.sibWork)
	if f.runStatus() != "running" {
		t.Fatal("SQL release changed descendant execution")
	}
	if f.lf.total() != 0 {
		t.Fatal("direct final SQL fence unexpectedly invoked forge")
	}
}
