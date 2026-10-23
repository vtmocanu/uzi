package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func heldTaskDescriptor(t *testing.T, run store.Run) []byte {
	t.Helper()
	raw, err := json.Marshal(CheckpointCoordinates{
		Version: 1, RunID: run.ID, UserID: run.UserID, RepoID: uuid.UUID(run.RepoID.Bytes),
		Kind: "task", WorkBranch: run.Branch.String, CheckpointKey: "uzi/task/" + run.ID.String(),
		Provenance: []uuid.UUID{run.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type heldCoordinateStore struct {
	terminalHeldStore
	claim store.GetRunClaimContextRow
}

func (s *heldCoordinateStore) GetRunClaimContext(context.Context, uuid.UUID) (store.GetRunClaimContextRow, error) {
	return s.claim, nil
}

// Exercise PublishHeld's broker boundary without transactions or a live database.
func TestPublishHeldTaskCoordinateAdmission(t *testing.T) {
	for _, mode := range []string{"legacy", "enrolled", "mismatch", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			w := store.Worker{ID: uuid.New(), UserID: uuid.New(), ProtocolCapabilities: []string{capability.RecoveryHeldPublicationV1}}
			r := store.Run{ID: uuid.New(), UserID: w.UserID, RepoID: pgconv.UUID(uuid.New()),
				WorkerID: pgconv.UUID(w.ID), Kind: "task", Status: "failed", ClaimGeneration: 1,
				Branch: pgconv.Text("uzi/task/ancestor")}
			if mode != "legacy" {
				r.CheckpointCoordinates = heldTaskDescriptor(t, r)
			}
			switch mode {
			case "mismatch":
				r.Branch = pgconv.Text("wrong/branch")
			case "invalid":
				r.CheckpointCoordinates = []byte("{")
			}
			box := newBox(t)
			sealed, err := box.Seal([]byte("fixture-pat"))
			if err != nil {
				t.Fatal(err)
			}
			q := &heldCoordinateStore{terminalHeldStore: terminalHeldStore{
				run: r, hold: store.RecoveryCustodyHold{ID: uuid.New(), RunID: r.ID, UserID: w.UserID,
					RepoID: r.RepoID, Generation: 1, OriginalWorkerID: w.ID,
					LiveWorkerID: pgconv.UUID(w.ID), LiveRunID: pgconv.UUID(r.ID), State: "open", InventoryGuarded: true},
			}, claim: store.GetRunClaimContextRow{RepoWebUrl: "https://forge.example/team/repo",
				BaseUrl: "https://forge.example", DefaultBranch: pgconv.Text("main"), TokenCiphertext: sealed}}
			p := testParams()
			p.HeldPublication = true
			svc := New(q, box, p)
			svc.SetForgeBaseURLAllowed(func(string) bool { return true })
			calls := 0
			svc.heldPrepareFn = func(_ context.Context, o pushbroker.HeldPackOptions) (*pushbroker.PreparedHeldPack, error) {
				calls++
				wantWork, wantKey := "", ""
				if mode == "enrolled" {
					wantWork, wantKey = "uzi/task/ancestor", "uzi/task/"+r.ID.String()
				}
				if o.WorkBranch != wantWork || o.CheckpointKey != wantKey || o.Branch != "" || o.DefaultBranch != "main" {
					t.Fatalf("broker bases=(%q,%q,%q,%q)", o.WorkBranch, o.CheckpointKey, o.Branch, o.DefaultBranch)
				}
				return nil, pushbroker.ErrTipMissing
			}
			_, err = svc.PublishHeld(context.Background(), w, r.ID,
				HeldPublishRequest{Generation: 1, Tip: heldTestTip, Coverage: heldTestCov}, []byte("pack"))
			var refusal *HeldRefusal
			wantReason, wantCalls := HeldReasonTipMissing, 1
			if mode == "mismatch" || mode == "invalid" {
				wantReason, wantCalls = HeldReasonIdentityChanged, 0
			}
			if !errors.As(err, &refusal) || refusal.Reason != wantReason || calls != wantCalls {
				t.Fatalf("err=%v broker calls=%d; want %s calls=%d", err, calls, wantReason, wantCalls)
			}
		})
	}
}

type liveCoordinateStore struct {
	*settleUnitStore
	releasedLive *store.ReleasePredecessorCustodyHoldByLiveAncestryParams
}

func (s *liveCoordinateStore) ReleasePredecessorCustodyHoldByLiveAncestry(_ context.Context, p store.ReleasePredecessorCustodyHoldByLiveAncestryParams) (int64, error) {
	s.releasedLive = &p
	return 1, nil
}

func TestLiveSettleTaskCoordinateAdmission(t *testing.T) {
	for _, mode := range []string{"legacy", "enrolled", "mismatch", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			u := newSettleUnit()
			q := &liveCoordinateStore{settleUnitStore: u.st}
			q.run.Kind, q.run.Status = "task", "running"
			q.run.RepoID = pgconv.UUID(uuid.New())
			q.run.Branch = pgconv.Text("uzi/task/ancestor")
			q.hold.UserID = u.wkr.UserID
			if mode != "legacy" {
				q.run.CheckpointCoordinates = heldTaskDescriptor(t, q.run)
			}
			switch mode {
			case "mismatch":
				q.run.Branch = pgconv.Text("wrong/branch")
			case "invalid":
				q.run.CheckpointCoordinates = []byte("{")
			}
			svc := New(q, nil, Params{})
			svc.SetForges(settleUnitBuilder{f: u.fk})
			req := apitypes.RecoveryLiveSettleRequest{PredecessorGeneration: 1, SuccessorGeneration: 2,
				PublishedSha: uA, SourceSha: uB, AdoptedSha: uC}
			// Unenrolled tasks cannot settle against a synthesized checkpoint key.
			if mode == "legacy" {
				req.Target = "checkpoint"
				res, err := svc.SettlePredecessorHoldLive(context.Background(), u.wkr, u.run, u.hold, req)
				if err != nil {
					t.Fatal(err)
				}
				assertSettleRetained(t, res, "not_eligible")
				if u.fk.calls() != 0 || q.releasedLive != nil {
					t.Fatal("unenrolled checkpoint attempted proof or release")
				}
			}
			req.Target = "branch"
			res, err := svc.SettlePredecessorHoldLive(context.Background(), u.wkr, u.run, u.hold, req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "mismatch" || mode == "invalid" {
				assertSettleRetained(t, res, "not_eligible")
				if u.fk.calls() != 0 || q.releasedLive != nil {
					t.Fatal("invalid descriptor attempted proof or release")
				}
				return
			}
			if res.Outcome != "released" || q.releasedLive == nil {
				t.Fatalf("result=%+v release=%+v", res, q.releasedLive)
			}
			p := q.releasedLive
			if p.CapturedBranch != q.run.Branch || string(p.CapturedCoordinates) != string(q.run.CheckpointCoordinates) ||
				p.ProvenBranch != "uzi/task/ancestor" || len(u.fk.candidates) != 3 {
				t.Fatalf("release=%+v candidates=%v", p, u.fk.candidates)
			}
		})
	}
}
