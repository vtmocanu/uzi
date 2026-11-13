package hostedsvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4162: custody admission counts units, but the desired-fleet projection of
// custody_held (any OPEN hold with this worker as live holder) is unchanged, so a worker whose
// only open holds are grouped holds is still reported held to the controller.
func TestPollProjectsCustodyHeld(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "free", true: "held"}[held], func(t *testing.T) {
			st := newFakeStore()
			st.workers = []store.ListHostedWorkersForControllerRow{{ID: uuid.New(), UserID: uuid.New(),
				HostedSize: pgtype.Text{String: "m", Valid: true}, TemplateDeclared: pgtype.Text{String: "base", Valid: true},
				HostedGeneration: 1, CustodyHeld: held}}
			response, err := newTestService(t, st).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Workers) != 1 || response.Workers[0].CustodyHeld != held {
				t.Fatalf("poll custody_held = %+v, want %v", response.Workers, held)
			}
		})
	}
}
