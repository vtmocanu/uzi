package healthsvc

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// A hibernating hosted worker (PRD #4205) has no pod to roll, so a stale stuck signal on
// its row must not count toward fleet.roll.
func TestFleetRollExcludesHibernatingWorkers(t *testing.T) {
	fresh := 10 * time.Second
	hib := func(name, state string) store.ListAllWorkersRow {
		r := hostedRow(name, workersvc.PhaseStuck, fresh, "ImagePullBackOff", "agent", "0.84.0")
		r.Worker.HibernationState = state
		return r
	}
	for _, state := range []string{"preparing", "hibernated", "waking"} {
		t.Run(state+" only", func(t *testing.T) {
			workers := []store.ListAllWorkersRow{hib("w1", state)}
			svc := New(Config{
				Store: &fakeStore{workers: workers}, Settings: &fakeSettings{healthEnabled: true},
				Now: func() time.Time { return fixedNow }, HostedWorkerVersion: "0.84.0",
			})
			c := svc.checkFleetRoll(fixedNow, workers, true)
			if c.Severity != sevOK || strings.Contains(c.Summary, "stuck") {
				t.Fatalf("severity %q summary %q: hibernating worker counted as stuck", c.Severity, c.Summary)
			}
		})
	}
	t.Run("awake stuck worker still counted", func(t *testing.T) {
		workers := []store.ListAllWorkersRow{
			hib("w1", "hibernated"),
			hostedRow("w2", workersvc.PhaseStuck, fresh, "ImagePullBackOff", "agent", "0.84.0"),
			hostedRow("w3", workersvc.PhaseSettled, fresh, "", "", "0.84.0"),
		}
		svc := New(Config{
			Store: &fakeStore{workers: workers}, Settings: &fakeSettings{healthEnabled: true},
			Now: func() time.Time { return fixedNow }, HostedWorkerVersion: "0.84.0",
		})
		c := svc.checkFleetRoll(fixedNow, workers, true)
		if c.Severity != sevWarn || !strings.Contains(c.Summary, "1 of 2 hosted workers stuck") {
			t.Fatalf("severity %q summary %q", c.Severity, c.Summary)
		}
	})
}
