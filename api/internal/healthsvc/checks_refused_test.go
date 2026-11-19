package healthsvc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func refusedRow(name, kind, reason, hibernation string, observedAgo time.Duration) store.ListAllWorkersRow {
	w := hostedRow(name, workersvc.PhaseStuck, observedAgo, reason, "", "new")
	w.RollBlockingKind = pgtype.Text{String: kind, Valid: true}
	w.Worker.HibernationState = hibernation
	return w
}

// A controller refusal is reported even for a pod-less worker, which the roll check otherwise
// excludes: there is no pod to be stuck because the controller refused to create one.
func TestFleetRollReportsRefusedWorkersIncludingPodless(t *testing.T) {
	for _, hib := range []string{"none", "hibernated"} {
		t.Run("size_refused/"+hib, func(t *testing.T) {
			row := refusedRow("big", workersvc.BlockingKindSizeRefused, "size above the memory ceiling", hib, time.Second)
			c := New(Config{HostedWorkerVersion: "new"}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{row}, true)
			if c.Severity != sevDanger || !strings.Contains(c.Summary, "1 worker refused: size above ceiling") {
				t.Fatalf("summary %q severity %s", c.Summary, c.Severity)
			}
			if !strings.Contains(c.Summary, "size above the memory ceiling") {
				t.Errorf("the controller's reason is missing: %q", c.Summary)
			}
			if !hasEvidence(c.Evidence, "Refused workers", "big") {
				t.Errorf("worker not named: %+v", c.Evidence)
			}
			if c.Action == nil || !strings.Contains(*c.Action, "chart ceiling") {
				t.Errorf("action = %v", c.Action)
			}
		})
		t.Run("pvc_rejected/"+hib, func(t *testing.T) {
			row := refusedRow("claim", workersvc.BlockingKindPVCRejected, "pvc exceeds maxPVCStorage", hib, time.Second)
			c := New(Config{HostedWorkerVersion: "new"}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{row}, true)
			if c.Severity != sevDanger || !strings.Contains(c.Summary, "1 worker refused: PVC rejected") ||
				!hasEvidence(c.Evidence, "Refused workers", "claim") {
				t.Fatalf("pvc rejection not reported: %+v", c)
			}
		})
	}
}

// Podless workers WITHOUT a fresh refusal stay excluded, as does a stale refusal.
func TestFleetRollStillExcludesOtherPodlessWorkers(t *testing.T) {
	plain := hostedRow("asleep", workersvc.PhaseStuck, time.Second, "ImagePullBackOff", "agent", "new")
	plain.Worker.HibernationState = "hibernated"
	stale := refusedRow("old", workersvc.BlockingKindSizeRefused, "x", "hibernated", rollSignalTTL+time.Minute)
	other := refusedRow("other", "scheduler_capacity", "x", "hibernated", time.Second)
	healthy := hostedRow("ok", workersvc.PhaseSettled, time.Second, "", "", "new")
	c := New(Config{HostedWorkerVersion: "new"}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{plain, stale, other, healthy}, true)
	if c.Severity != sevOK || !strings.Contains(c.Summary, "3 hibernated (excluded)") {
		t.Fatalf("podless workers without a fresh refusal must stay excluded: %+v", c)
	}
}

func TestFleetRollCountsBothRefusalKinds(t *testing.T) {
	rows := []store.ListAllWorkersRow{
		refusedRow("a", workersvc.BlockingKindSizeRefused, "r1", "none", time.Second),
		refusedRow("b", workersvc.BlockingKindSizeRefused, "r1", "hibernated", time.Second),
		refusedRow("c", workersvc.BlockingKindPVCRejected, "r2", "none", time.Second),
		hostedRow("fine", workersvc.PhaseSettled, time.Second, "", "", "new"),
	}
	c := New(Config{HostedWorkerVersion: "new"}).checkFleetRoll(fixedNow, rows, true)
	for _, want := range []string{"2 workers refused: size above ceiling", "1 worker refused: PVC rejected"} {
		if !strings.Contains(c.Summary, want) {
			t.Errorf("summary %q lacks %q", c.Summary, want)
		}
	}
	if !hasEvidence(c.Evidence, "Refused workers", "a, b, c") {
		t.Errorf("evidence = %+v", c.Evidence)
	}
}

func TestFleetRollCapsRefusedWorkerNames(t *testing.T) {
	var rows []store.ListAllWorkersRow
	for i := 0; i < 12; i++ {
		rows = append(rows, refusedRow(fmt.Sprintf("w%02d", i), workersvc.BlockingKindSizeRefused, "r", "none", time.Second))
	}
	c := New(Config{HostedWorkerVersion: "new"}).checkFleetRoll(fixedNow, rows, true)
	if !hasEvidence(c.Evidence, "Refused workers", "w00, w01, w02, w03, w04 and 7 more") {
		t.Errorf("evidence = %+v", c.Evidence)
	}
}

func hasEvidence(ev []apitypes.HealthEvidenceDTO, label, value string) bool {
	for _, e := range ev {
		if e.Label == label && e.Value == value {
			return true
		}
	}
	return false
}
