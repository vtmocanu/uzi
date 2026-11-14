package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The current_flight scope refuses run kinds that never publish code (checker and chat runs) and
// egress-profile-bound runs (a profile-bound run publishes no code), as unsupported. This pins both
// halves of terminalFlightKindRefused, which the gate and the tx1 re-check share, without a database.
func TestTerminalFlightScopeRefusesKindAndEgress(t *testing.T) {
	wkr := store.Worker{ID: uuid.New()}
	base := store.Run{
		ID:              uuid.New(),
		Kind:            runkind.Issue,
		Status:          "running",
		WorkerID:        pgconv.UUID(wkr.ID),
		ClaimGeneration: 1,
	}
	if ok, err := terminalFlightEligible(wkr, base, 1); !ok || err != nil {
		t.Fatalf("baseline issue flight: ok=%v err=%v, want eligible", ok, err)
	}
	cases := map[string]func(*store.Run){
		"cross_check": func(r *store.Run) { r.Kind = runkind.CrossCheck },
		"chat":        func(r *store.Run) { r.Kind = runkind.Chat },
		"egress profile bound": func(r *store.Run) {
			r.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			run := base
			mutate(&run)
			if !terminalFlightKindRefused(run) {
				t.Fatal("terminalFlightKindRefused = false, want true")
			}
			ok, err := terminalFlightEligible(wkr, run, 1)
			if ok {
				t.Fatal("eligible, want refused")
			}
			r, isRefusal := err.(*TerminalRefusal)
			if !isRefusal || r.Reason != apitypes.TerminalReasonUnsupported {
				t.Fatalf("err = %v, want unsupported refusal", err)
			}
		})
	}
}
