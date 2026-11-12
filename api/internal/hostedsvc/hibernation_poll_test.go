package hostedsvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPollMapsHibernation(t *testing.T) {
	op := uuid.New()
	mk := func(state string, withOp bool) store.ListHostedWorkersForControllerRow {
		row := store.ListHostedWorkersForControllerRow{ID: uuid.New(), UserID: uuid.New(),
			HostedSize: pgtype.Text{String: "m", Valid: true}, TemplateDeclared: pgtype.Text{String: "base", Valid: true},
			HostedGeneration: 1, HibernationState: state}
		if withOp {
			row.HibernationOpID = pgtype.UUID{Bytes: op, Valid: true}
		}
		return row
	}
	for _, tc := range []struct {
		name      string
		row       store.ListHostedWorkersForControllerRow
		wantState string // "" = no hibernation object
	}{
		{"none", mk("none", false), ""},
		{"hibernated", mk("hibernated", true), "hibernated"},
		{"preparing", mk("preparing", true), "preparing"},
		{"waking", mk("waking", true), "waking"},
		{"zero row with empty state", mk("", false), ""},
		{"non-none without op id", mk("hibernated", false), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeStore()
			st.workers = []store.ListHostedWorkersForControllerRow{tc.row}
			response, err := newTestService(t, st).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Workers) != 1 {
				t.Fatalf("workers: %d", len(response.Workers))
			}
			h := response.Workers[0].Hibernation
			if tc.wantState == "" {
				if h != nil {
					t.Fatalf("unexpected hibernation object: %+v", h)
				}
				return
			}
			if h == nil || h.State != tc.wantState || h.OpID != op.String() {
				t.Fatalf("hibernation mapping: %+v", h)
			}
		})
	}
}
