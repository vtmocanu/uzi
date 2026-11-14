package healthsvc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestFleetHibernationCleanup(t *testing.T) {
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	hosted := func(name, phase string, unknownAgo time.Duration) store.ListAllWorkersRow {
		w := store.Worker{Kind: "hosted", Name: name, CleanupPhase: phase}
		if phase == "unknown" {
			w.CleanupUnknownAt = stamp(fixedNow.Add(-unknownAgo))
		}
		return store.ListAllWorkersRow{Worker: w}
	}
	svc := newSvc(&fakeStore{}, &fakeSettings{})

	t.Run("na without hosted workers", func(t *testing.T) {
		c := New(Config{}).checkFleetHibernationCleanup([]store.ListAllWorkersRow{{Worker: store.Worker{Kind: "docker", CleanupPhase: "unknown"}}})
		if c.Severity != sevNA || c.Scope != "instance" || c.Group != groupWorkers {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("ok while attempts are in progress or settled", func(t *testing.T) {
		c := svc.checkFleetHibernationCleanup([]store.ListAllWorkersRow{
			hosted("a", "requested", 0), hosted("b", "started", 0), hosted("c", "settled", 0), hosted("d", "none", 0),
		})
		if c.Severity != sevOK || len(c.Evidence) != 0 {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("danger for an unknown settlement, since is the oldest", func(t *testing.T) {
		c := svc.checkFleetHibernationCleanup([]store.ListAllWorkersRow{
			hosted("new", "unknown", time.Minute), hosted("old", "unknown", time.Hour), hosted("ok", "none", 0),
		})
		if c.Severity != sevDanger || len(c.Evidence) != 3 || c.Evidence[0].Value != "2" || c.Since == nil {
			t.Fatalf("got %+v", c)
		}
		if *c.Since != fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339) {
			t.Fatalf("since = %v", c.Since)
		}
		if c.Action == nil || !strings.Contains(*c.Action, "no acknowledge route") {
			t.Fatalf("action = %v", c.Action)
		}
	})
	t.Run("evidence is bounded and names are sanitized", func(t *testing.T) {
		var rows []store.ListAllWorkersRow
		for i := 0; i < 15; i++ {
			rows = append(rows, hosted(fmt.Sprintf("w%d\x1b[31m", i), "unknown", time.Hour))
		}
		c := svc.checkFleetHibernationCleanup(rows)
		if len(c.Evidence) != 1+maxQuarantineEvidence || c.Evidence[0].Value != "15" {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
		for _, e := range c.Evidence {
			if strings.ContainsRune(e.Value, 0x1b) {
				t.Fatalf("control byte in %q", e.Value)
			}
		}
	})
}
