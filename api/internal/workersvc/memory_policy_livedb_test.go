package workersvc

import (
	"errors"
	"github.com/google/uuid"
	"testing"
)

func TestMemoryReservationExplicitPolicyLiveDB(t *testing.T) {
	f := newMemoryFixture(t)
	request := MemoryReservationRequest{MemoryBinding: f.b}
	for _, policy := range []MemoryPolicy{{}, {Version: 1}, {Version: 2, MaxInterventions: 1}, {Version: 1, MaxInterventions: -1}, {Version: 1, MaxInterventions: 10000}} {
		request.Policy = policy
		if _, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("policy=%+v err=%v", policy, err)
		}
		if r := f.run(t); r.MemoryInterventionCount != 0 || len(r.MemoryPolicy) != 0 {
			t.Fatal("invalid policy charged or froze episode")
		}
	}
	request.Policy = MemoryPolicy{Version: 1, MaxInterventions: 1}
	first, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request)
	if err != nil || !first.Authorizing || first.Policy != request.Policy || first.Allowance.Limit != 1 {
		t.Fatalf("explicit policy=%+v %v", first, err)
	}
	request.Policy.MaxInterventions = 2
	if _, err = f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request); !errors.Is(err, ErrMemoryBinding) {
		t.Fatalf("same ID changed policy=%v", err)
	}
	request.InterventionID = uuid.New()
	if _, err = f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request); !errors.Is(err, ErrMemoryBinding) {
		t.Fatalf("new ID changed frozen policy=%v", err)
	}
	request.Policy.MaxInterventions = 1
	denied, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request)
	if err != nil || denied.Admitted || denied.Authorizing || denied.Policy != first.Policy || denied.Allowance.Used != 1 {
		t.Fatalf("cap=%+v %v", denied, err)
	}
	request.MemoryBinding = f.b
	f.hold(t)
	historical, err := f.s.ReserveMemoryIntervention(f.e.ctx, f.w, request)
	if err != nil || historical.Authorizing || historical.Policy != first.Policy || historical.MemoryBinding != first.MemoryBinding {
		t.Fatalf("history=%+v %v", historical, err)
	}
}
