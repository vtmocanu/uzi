package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func capableCaps() []string {
	return []string{capability.WorkerMemoryPressureV1, capability.WorkerMemoryAuthorityClaimV1}
}

// TestMemoryAuthorityLostTruthTable pins the claim-time judgment (issue #3142).
func TestMemoryAuthorityLostTruthTable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caps   []string
		report *string
		want   bool
	}{
		{"held, capable", capableCaps(), strp("held"), false},
		{"held, no caps", nil, strp("held"), false},
		{"lost, capable", capableCaps(), strp("lost"), true},
		{"lost, no caps (explicit report trusted alone)", nil, strp("lost"), true},
		{"unknown value is lost", capableCaps(), strp("whatever"), true},
		{"empty value is lost", nil, strp(""), true},
		{"case-sensitive held", nil, strp("HELD"), true},
		{"absent, capable", capableCaps(), nil, true},
		{"absent, no caps (old worker)", nil, nil, false},
		{"absent, pressure only", []string{capability.WorkerMemoryPressureV1}, nil, false},
		{"absent, claim cap only", []string{capability.WorkerMemoryAuthorityClaimV1}, nil, false},
	} {
		if got := memoryAuthorityLost(tc.caps, MemoryAuthorityReport{Value: tc.report}); got != tc.want {
			t.Errorf("%s: memoryAuthorityLost = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func claimWith(t *testing.T, wkr store.Worker, report *MemoryAuthorityReport) (*Service, *fakeStore) {
	t.Helper()
	fs := &fakeStore{claimErr: pgx.ErrNoRows}
	svc := New(fs, newBox(t), testParams())
	var err error
	if report == nil {
		_, err = svc.Claim(context.Background(), wkr, nil)
	} else {
		_, err = svc.ClaimReported(context.Background(), wkr, nil, *report)
	}
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if fs.claimParams == nil {
		t.Fatal("ClaimRun was not called")
	}
	return svc, fs
}

func TestClaimMemoryAuthorityGateAndTracker(t *testing.T) {
	capable := func() store.Worker {
		w := worker()
		w.ProtocolCapabilities = capableCaps()
		return w
	}
	t.Run("capable worker, member absent (Claim): gated and recorded", func(t *testing.T) {
		w := capable()
		svc, fs := claimWith(t, w, nil)
		if !fs.claimParams.MemoryAuthorityLost {
			t.Error("MemoryAuthorityLost = false, want true")
		}
		if _, ok := svc.MemoryAuthorityLostFor(w.ID); !ok {
			t.Error("tracker has no entry for the gated worker")
		}
	})
	t.Run("capable worker, explicit lost", func(t *testing.T) {
		w := capable()
		svc, fs := claimWith(t, w, &MemoryAuthorityReport{Value: strp("lost")})
		if !fs.claimParams.MemoryAuthorityLost {
			t.Error("MemoryAuthorityLost = false, want true")
		}
		if _, ok := svc.MemoryAuthorityLostFor(w.ID); !ok {
			t.Error("tracker has no entry")
		}
	})
	t.Run("explicit lost without the capability is still gated", func(t *testing.T) {
		_, fs := claimWith(t, worker(), &MemoryAuthorityReport{Value: strp("lost")})
		if !fs.claimParams.MemoryAuthorityLost {
			t.Error("MemoryAuthorityLost = false, want true")
		}
	})
	t.Run("held claims normally and clears the entry", func(t *testing.T) {
		w := capable()
		fs := &fakeStore{claimErr: pgx.ErrNoRows}
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.Claim(context.Background(), w, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := svc.MemoryAuthorityLostFor(w.ID); !ok {
			t.Fatal("setup: entry not recorded")
		}
		if _, err := svc.ClaimReported(context.Background(), w, nil, MemoryAuthorityReport{Value: strp("held")}); err != nil {
			t.Fatal(err)
		}
		if fs.claimParams.MemoryAuthorityLost {
			t.Error("MemoryAuthorityLost = true for a held report")
		}
		if _, ok := svc.MemoryAuthorityLostFor(w.ID); ok {
			t.Error("held claim did not clear the tracker entry")
		}
	})
	t.Run("old worker sending nothing is not gated and not tracked", func(t *testing.T) {
		w := worker()
		svc, fs := claimWith(t, w, nil)
		if fs.claimParams.MemoryAuthorityLost {
			t.Error("MemoryAuthorityLost = true for a worker without the capability")
		}
		if _, ok := svc.MemoryAuthorityLostFor(w.ID); ok {
			t.Error("tracker recorded a worker that was never gated")
		}
	})
}

func TestMemoryAuthorityTracker(t *testing.T) {
	tr := newMemoryAuthorityTracker()
	id := uuid.New()
	t0 := time.Now()
	tr.recordLost(id, t0)
	tr.recordLost(id, t0.Add(time.Hour))
	if since, ok := tr.get(id); !ok || !since.Equal(t0) {
		t.Fatalf("get = %v,%v; want first-observed %v", since, ok, t0)
	}
	// The refresh keeps the entry alive past the TTL measured from the first observation.
	tr.prune(t0.Add(memoryAuthorityTrackerTTL + 30*time.Minute))
	if _, ok := tr.get(id); !ok {
		t.Fatal("pruned an entry refreshed within the TTL")
	}
	tr.prune(t0.Add(time.Hour + memoryAuthorityTrackerTTL + time.Minute))
	if _, ok := tr.get(id); ok {
		t.Fatal("did not prune an expired entry")
	}
	tr.recordLost(id, t0)
	tr.evictIfSet(id)
	if _, ok := tr.get(id); ok {
		t.Fatal("evictIfSet left the entry")
	}
	var nilTr *memoryAuthorityTracker
	nilTr.evictIfSet(id) // nil-safe
	nilTr.prune(t0)
	nilTr.recordLost(id, t0)
	if _, ok := nilTr.get(id); ok {
		t.Fatal("nil tracker reports an entry")
	}
	for i := 0; i < memoryAuthorityTrackerMaxWorkers+10; i++ {
		tr.recordLost(uuid.New(), t0)
	}
	if n := len(tr.workers); n != memoryAuthorityTrackerMaxWorkers {
		t.Fatalf("tracker size = %d, want cap %d", n, memoryAuthorityTrackerMaxWorkers)
	}
}

// A fresh registration evicts a gated worker's entry, and the next held claim is not gated
// (issue #3142). Register is the only path that makes a restarted worker report afresh.
func TestRegisterEvictsMemoryAuthorityEntry(t *testing.T) {
	w := worker()
	w.ProtocolCapabilities = capableCaps()
	fs := &fakeStore{claimErr: pgx.ErrNoRows, registerResult: store.Worker{ID: w.ID, Status: "online"}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.Claim(context.Background(), w, nil); err != nil {
		t.Fatal(err)
	}
	if !fs.claimParams.MemoryAuthorityLost {
		t.Fatal("setup: capable claim with the member absent was not gated")
	}
	if _, ok := svc.MemoryAuthorityLostFor(w.ID); !ok {
		t.Fatal("setup: entry not recorded")
	}
	if _, _, err := svc.Register(context.Background(), w, "1", "base", nil, nil, capableCaps(), nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := svc.MemoryAuthorityLostFor(w.ID); ok {
		t.Error("Register left the entry")
	}
	if _, err := svc.ClaimReported(context.Background(), w, nil, MemoryAuthorityReport{Value: strp("held")}); err != nil {
		t.Fatal(err)
	}
	if fs.claimParams.MemoryAuthorityLost {
		t.Error("held claim after Register was gated")
	}
}

func TestDeleteWorkerEvictsMemoryAuthorityEntry(t *testing.T) {
	w := worker()
	w.ProtocolCapabilities = capableCaps()
	fs := &fakeStore{claimErr: pgx.ErrNoRows, deleteWorkerRows: 1}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.Claim(context.Background(), w, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.MemoryAuthorityLostFor(w.ID); !ok {
		t.Fatal("setup: entry not recorded")
	}
	if err := svc.DeleteWorker(context.Background(), w.UserID, w.ID); err != nil {
		t.Fatalf("DeleteWorker: %v", err)
	}
	if _, ok := svc.MemoryAuthorityLostFor(w.ID); ok {
		t.Error("DeleteWorker left the entry")
	}
}
