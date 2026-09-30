package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRegisterTxLessRunsAttestedFinalizePass covers the tx-less Register path (a fake store, no
// pool): the snapshot is never persisted, but a valid finalize list is still applied before the
// ordinary orphan pass, and its ids reach the same publish fan-out.
func TestRegisterTxLessRunsAttestedFinalizePass(t *testing.T) {
	w := worker()
	run := uuid.New()
	requeuedRun := uuid.New()
	fs := &fakeStore{
		registerResult:       store.Worker{ID: w.ID, Status: "online"},
		attestedFailedRuns:   []uuid.UUID{run},
		attestedRequeuedRows: []store.RequeueAttestedFinalizeRunsRow{{ID: requeuedRun, AllowanceUsed: true}},
	}
	svc := New(fs, newBox(t), testParams())
	snap := &ActiveSnapshot{FinalizeResume: []FinalizeResumeEntry{{RunID: run.String(), ClaimGeneration: 3}}}

	if _, _, err := svc.Register(context.Background(), w, "1", "base", nil, nil, nil, snap); err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := []string{"register", "fail_attested", "requeue_attested", "fail_over_cap", "requeue_worker"}
	if strings.Join(fs.callOrder, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", fs.callOrder, want)
	}
	if fs.requeueAttested == nil || fs.requeueAttested.WorkerID.Bytes != w.ID || fs.requeueAttested.MaxRequeues != 1 ||
		len(fs.requeueAttested.RunIds) != 1 || fs.requeueAttested.RunIds[0] != run || fs.requeueAttested.ClaimGenerations[0] != 3 {
		t.Fatalf("requeue attested params wrong: %+v", fs.requeueAttested)
	}
	if fs.failAttested == nil || !fs.failAttested.FailureReason.Valid {
		t.Fatalf("fail attested params wrong: %+v", fs.failAttested)
	}
}

// TestRegisterIgnoresInvalidFinalizeList: an invalid list never fails Register and never runs the
// attested pass.
func TestRegisterIgnoresInvalidFinalizeList(t *testing.T) {
	w := worker()
	run := uuid.New().String()
	for name, list := range map[string][]FinalizeResumeEntry{
		"duplicate":  {{RunID: run, ClaimGeneration: 1}, {RunID: run, ClaimGeneration: 1}},
		"not a uuid": {{RunID: "nope", ClaimGeneration: 1}},
		"negative":   {{RunID: run, ClaimGeneration: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{registerResult: store.Worker{ID: w.ID, Status: "online"}}
			svc := New(fs, newBox(t), testParams())
			if _, _, err := svc.Register(context.Background(), w, "1", "base", nil, nil, nil, &ActiveSnapshot{FinalizeResume: list}); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if fs.failAttested != nil || fs.requeueAttested != nil {
				t.Fatal("attested pass ran for an invalid list")
			}
		})
	}
}

func TestValidateFinalizeResumeCeiling(t *testing.T) {
	p := testParams()
	p.ActiveSnapshotMaxEntries = 2
	svc := New(&fakeStore{}, newBox(t), p)
	mk := func(n int) *ActiveSnapshot {
		s := &ActiveSnapshot{}
		for i := 0; i < n; i++ {
			s.FinalizeResume = append(s.FinalizeResume, FinalizeResumeEntry{RunID: uuid.New().String(), ClaimGeneration: int64(i)})
		}
		return s
	}
	if _, ok := svc.validateFinalizeResume(store.Worker{}, mk(2)); !ok {
		t.Fatal("a list at the ceiling must be valid")
	}
	if _, ok := svc.validateFinalizeResume(store.Worker{}, mk(3)); ok {
		t.Fatal("a list over the ceiling must be rejected")
	}
	if _, ok := svc.validateFinalizeResume(store.Worker{}, nil); ok {
		t.Fatal("a nil snapshot has no list")
	}
}
