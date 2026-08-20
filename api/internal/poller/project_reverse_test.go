package poller

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// fakeReverseSyncer records ReverseSync invocations so a test can assert the
// per-tick sibling gating without a live DB/forge (syncRepo itself needs both).
type fakeReverseSyncer struct {
	calls []uuid.UUID
	err   error
}

func (f *fakeReverseSyncer) ReverseSync(_ context.Context, repoID uuid.UUID) error {
	f.calls = append(f.calls, repoID)
	return f.err
}

// TestSetProjectReverseSyncWiresCollaborator confirms SetProjectReverseSync stores
// the optional collaborator (the reverse kill-switch is simply not wiring it) and
// that a fake satisfies the projectReverseSyncer interface.
func TestSetProjectReverseSyncWiresCollaborator(t *testing.T) {
	e := New(nil, nil, 0, 0)
	if e.projectReverse != nil {
		t.Fatalf("projectReverse must be nil until wired")
	}
	rs := &fakeReverseSyncer{}
	e.SetProjectReverseSync(rs)
	if e.projectReverse == nil {
		t.Fatalf("SetProjectReverseSync must wire the collaborator")
	}
}

// TestReverseSyncSiblingGate mirrors the exact gate the syncRepo sibling applies
// (github repo AND a wired syncer), so the intended fire/skip behaviour is pinned
// without standing up the DB-backed syncRepo path.
func TestReverseSyncSiblingGate(t *testing.T) {
	repoID := uuid.New()
	cases := []struct {
		name      string
		forgeType string
		wired     bool
		wantFire  bool
	}{
		{"github wired fires", string(forge.TypeGitHub), true, true},
		{"github unwired skips", string(forge.TypeGitHub), false, false},
		{"gitlab wired skips", string(forge.TypeGitLab), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := New(nil, nil, 0, 0)
			var rs *fakeReverseSyncer
			if tc.wired {
				rs = &fakeReverseSyncer{}
				e.SetProjectReverseSync(rs)
			}
			// Exercise the SAME predicate syncRepo uses (not a re-implementation), so a
			// regression in the production gate is caught here.
			if got := e.reverseSyncEligible(tc.forgeType); got != tc.wantFire {
				t.Fatalf("reverseSyncEligible(%q)=%v, want %v", tc.forgeType, got, tc.wantFire)
			}
			if e.reverseSyncEligible(tc.forgeType) {
				_ = e.projectReverse.ReverseSync(context.Background(), repoID)
			}
			fired := rs != nil && len(rs.calls) == 1
			if fired != tc.wantFire {
				t.Errorf("fired=%v, want %v", fired, tc.wantFire)
			}
		})
	}
}
