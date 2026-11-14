package hostedsvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4208 M1: the poll's inventory gate is a pure function of the worker's state and the
// recorded inventory status of its current op. It never reads the clock.
func TestPollMapsInventoryGate(t *testing.T) {
	status := func(s string) pgtype.Text {
		if s == "-" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	cases := []struct {
		name   string
		state  string
		status string // "-" = no inventory row
		want   string
	}{
		{"preparing without a row observes", "preparing", "-", InventoryGateObserve},
		{"preparing awaiting awaits", "preparing", "awaiting", InventoryGateAwait},
		{"preparing accepted permits", "preparing", "accepted", InventoryGatePermit},
		{"preparing timed_out permits", "preparing", "timed_out", InventoryGatePermit},
		{"preparing no_capability permits", "preparing", "no_capability", InventoryGatePermit},
		{"preparing closed_by_scale_down permits", "preparing", "closed_by_scale_down", InventoryGatePermit},
		{"preparing identity_changed permits", "preparing", "identity_changed", InventoryGatePermit},
		{"preparing unsupported permits", "preparing", "unsupported", InventoryGatePermit},
		{"preparing exclusion_failed permits", "preparing", "exclusion_failed", InventoryGatePermit},
		{"preparing busy permits", "preparing", "busy", InventoryGatePermit},
		{"hibernated carries no gate", "hibernated", "accepted", ""},
		{"hibernated without a row carries no gate", "hibernated", "-", ""},
		{"waking carries no gate", "waking", "awaiting", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := store.ListHostedWorkersForControllerRow{ID: uuid.New(), UserID: uuid.New(),
				HostedSize: pgtype.Text{String: "m", Valid: true}, TemplateDeclared: pgtype.Text{String: "base", Valid: true},
				HostedGeneration: 1, HibernationState: tc.state,
				HibernationOpID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, InventoryStatus: status(tc.status)}
			st := newFakeStore()
			st.workers = []store.ListHostedWorkersForControllerRow{row}
			response, err := newTestService(t, st).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			h := response.Workers[0].Hibernation
			if h == nil {
				t.Fatal("no hibernation object")
			}
			if h.InventoryGate != tc.want {
				t.Fatalf("gate = %q, want %q", h.InventoryGate, tc.want)
			}
		})
	}
	t.Run("an awake worker has no hibernation object", func(t *testing.T) {
		row := store.ListHostedWorkersForControllerRow{ID: uuid.New(), UserID: uuid.New(),
			HostedSize: pgtype.Text{String: "m", Valid: true}, TemplateDeclared: pgtype.Text{String: "base", Valid: true},
			HostedGeneration: 1, HibernationState: "none", InventoryStatus: status("awaiting")}
		st := newFakeStore()
		st.workers = []store.ListHostedWorkersForControllerRow{row}
		response, err := newTestService(t, st).Poll(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if response.Workers[0].Hibernation != nil {
			t.Fatalf("hibernation: %+v", response.Workers[0].Hibernation)
		}
	})
}
