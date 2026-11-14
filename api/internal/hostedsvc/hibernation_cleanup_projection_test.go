package hostedsvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4206: an unsettled cache prune attempt is projected as state "cleaning" with the cleanup
// block, whether the database state is preparing or waking; the stop-proof pod uid rides only an
// unknown attempt, and a settled or absent attempt leaves the PRD #4205 projection untouched.
func TestPollProjectsHibernationCleaning(t *testing.T) {
	op, cleanupOp, pod := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, dbState, phase string
		wantState            string
		wantStopProof        bool
		wantCleanup          bool
	}{
		{"requested preparing", "preparing", "requested", "cleaning", false, true},
		{"started preparing", "preparing", "started", "cleaning", false, true},
		{"unknown preparing", "preparing", "unknown", "cleaning", true, true},
		{"unknown waking", "waking", "unknown", "cleaning", true, true},
		{"started waking", "waking", "started", "cleaning", false, true},
		{"settled preparing", "preparing", "settled", "preparing", false, false},
		{"none preparing", "preparing", "none", "preparing", false, false},
		{"legacy empty phase", "preparing", "", "preparing", false, false},
		{"settled waking", "waking", "settled", "waking", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.workers = []store.ListHostedWorkersForControllerRow{{
				ID: uuid.New(), UserID: uuid.New(), HostedSize: pgtype.Text{String: "m", Valid: true},
				TemplateDeclared: pgtype.Text{String: "base", Valid: true}, HostedGeneration: 1,
				HibernationState: tc.dbState, HibernationOpID: pgtype.UUID{Bytes: op, Valid: true},
				CleanupPhase: tc.phase, CleanupOpID: pgtype.UUID{Bytes: cleanupOp, Valid: tc.phase != "" && tc.phase != "none"},
				CleanupPodUid: pgtype.UUID{Bytes: pod, Valid: true}, CleanupReconcileSeq: 4,
			}}
			response, err := newTestService(t, st).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			h := response.Workers[0].Hibernation
			if h == nil || h.State != tc.wantState || h.OpID != op.String() {
				t.Fatalf("hibernation = %+v, want state %q op %s", h, tc.wantState, op)
			}
			if !tc.wantCleanup {
				if h.Cleanup != nil {
					t.Fatalf("cleanup block on %q: %+v", tc.wantState, h.Cleanup)
				}
				return
			}
			c := h.Cleanup
			if c == nil || c.OpID != cleanupOp.String() || c.ReconcileSeq != 4 {
				t.Fatalf("cleanup = %+v", c)
			}
			if tc.wantStopProof != (c.StopProofPodUID == pod.String()) || (!tc.wantStopProof && c.StopProofPodUID != "") {
				t.Fatalf("stop_proof_pod_uid = %q, want present=%v", c.StopProofPodUID, tc.wantStopProof)
			}
		})
	}
}
