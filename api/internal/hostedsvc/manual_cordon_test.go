package hostedsvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPollCarriesManualCordonMachineFields(t *testing.T) {
	at := time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC)
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-clear", true: "manual"}[manual], func(t *testing.T) {
			st := newFakeStore()
			row := store.ListHostedWorkersForControllerRow{ID: uuid.New(), UserID: uuid.New(),
				HostedSize: pgtype.Text{String: "m", Valid: true}, TemplateDeclared: pgtype.Text{String: "base", Valid: true},
				HostedGeneration: 1, EffectiveCordoned: manual}
			if manual {
				row.ManualCordonedAt = pgtype.Timestamptz{Time: at, Valid: true}
			}
			st.workers = []store.ListHostedWorkersForControllerRow{row}
			response, err := newTestService(t, st).Poll(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Workers) != 1 {
				t.Fatalf("worker omitted from desired fleet: %d", len(response.Workers))
			}
			w := response.Workers[0]
			if w.ID != row.ID.String() || w.EffectiveCordoned != manual || (w.ManualCordonedAt != nil) != manual {
				t.Fatalf("manual poll mapping: %+v", w)
			}
			if manual && !w.ManualCordonedAt.Equal(at) {
				t.Fatalf("manual timestamp changed: %s", w.ManualCordonedAt)
			}
			data, err := json.Marshal(w)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"manual_cordoned_by", "manual_cordoned_by_label", "manual_cordon_reason"} {
				if _, ok := fields[forbidden]; ok {
					t.Fatalf("owner-authored attribution crossed controller boundary: %s", forbidden)
				}
			}
			if manual {
				if _, ok := fields["manual_cordoned_at"]; !ok {
					t.Fatal("manual timestamp missing from wire")
				}
				if string(fields["effective_cordoned"]) != "true" {
					t.Fatal("manual fence missing from wire")
				}
			} else if _, ok := fields["manual_cordoned_at"]; ok {
				t.Fatal("clear timestamp must remain optional")
			}
		})
	}
}
