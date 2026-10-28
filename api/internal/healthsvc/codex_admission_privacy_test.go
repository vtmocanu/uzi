package healthsvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexAdmissionHealthEvidencePrivacy(t *testing.T) {
	eligible := fixedNow.Add(7 * time.Hour)
	ordinary := "waiting: maintenance"
	// Exercise waiting evidence and blocker enrichment independently: either can leak.
	for _, source := range []string{"waiting", "blocker"} {
		t.Run(source, func(t *testing.T) {
			f := &fakeStore{controller: pgtype.Timestamptz{Time: fixedNow, Valid: true}}
			for _, class := range []string{"rate_limit", "overload", "ordinary"} {
				owner, worker, run, blocker := uuid.New(), uuid.New(), uuid.New(), uuid.New()
				reason := ordinary
				if class != "ordinary" {
					reason = apitypes.CodexAdmissionReason(class, eligible)
				}
				waitReason, blockerReason := ordinary, ordinary
				if source == "waiting" {
					waitReason = reason
				} else {
					blockerReason = reason
				}
				since := pgtype.Timestamptz{Time: fixedNow.Add(-time.Hour), Valid: true}
				f.capacityRows = append(f.capacityRows, store.ListOwnersWaitingNoCapacityRow{
					RunID: run, UserID: owner, HealthSince: since, HealthReason: pgtype.Text{String: waitReason, Valid: true},
				})
				f.waitingRows = append(f.waitingRows, store.ListWaitingWorkerRunsRow{
					RunID: run, UserID: owner, HealthSince: since, HealthReason: pgtype.Text{String: waitReason, Valid: true},
				})
				f.workers = append(f.workers, store.ListAllWorkersRow{Worker: store.Worker{
					ID: worker, UserID: owner, Kind: "hosted", DrainingSince: since,
				}})
				f.blockerRows = append(f.blockerRows, store.ListWorkerUpgradeBlockersRow{
					WorkerID: worker, WorkerOwner: owner, SafeCount: 1,
					RunID:        pgtype.UUID{Bytes: blocker, Valid: true},
					Status:       pgtype.Text{String: "queued", Valid: true},
					Kind:         pgtype.Text{String: "chat", Valid: true},
					HealthReason: pgtype.Text{String: blockerReason, Valid: true},
				})
			}
			doc, err := newSvc(f, &fakeSettings{healthEnabled: true}).Evaluate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"provider reported", "rate limit", "overload", eligible.Format(time.RFC3339)} {
				if strings.Contains(string(data), private) {
					t.Errorf("whole health document leaked %q", private)
				}
			}
			found := 0
			for _, c := range doc.Checks {
				if c.ID != "queue.waiting" && c.ID != "fleet.capacity" {
					continue
				}
				found++
				if c.Severity != sevDanger {
					t.Errorf("%s severity = %s", c.ID, c.Severity)
				}
				var waiting []string
				for _, ev := range c.Evidence {
					if ev.Label == "Waiting run" {
						waiting = append(waiting, ev.Value)
					}
					if c.ID == "fleet.capacity" && ev.Label == "Owners affected" && ev.Value != "3" {
						t.Errorf("affected owner count = %s", ev.Value)
					}
				}
				if len(waiting) != 3 {
					t.Fatalf("%s waiting evidence = %v", c.ID, waiting)
				}
				for i, value := range waiting {
					row := f.capacityRows[i]
					for _, want := range []string{row.RunID.String(), row.UserID.String(), "waited 1h"} {
						if !strings.Contains(value, want) {
							t.Errorf("%s missing %q: %s", c.ID, want, value)
						}
					}
					if i == 2 && !strings.Contains(value, ordinary) {
						t.Errorf("ordinary reason lost: %s", value)
					}
					if i < 2 && (source == "waiting" || c.ID == "fleet.capacity") && !strings.Contains(value, "waiting: admission held") {
						t.Errorf("%s missing generic admission reason: %s", c.ID, value)
					}
					if c.ID == "fleet.capacity" {
						for _, want := range []string{uuid.UUID(f.blockerRows[i].RunID.Bytes).String(), "0 other safe blocker(s) omitted", "release requires terminal or unassignment"} {
							if !strings.Contains(value, want) {
								t.Errorf("blocker enrichment missing %q: %s", want, value)
							}
						}
					}
				}
			}
			if found != 2 || len(f.blockerQueries) != 1 {
				t.Fatalf("checks = %d; blocker queries = %v", found, f.blockerQueries)
			}
		})
	}
}
