package healthsvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestFleetHeldPacingCoverage(t *testing.T) {
	now := fixedNow
	one := hostedRow("one", workersvc.PhaseStuck, 0, "ImagePullBackOff", "agent", "bad")
	one.Worker.ID = uuid.New()
	one.RollPacing = []byte(`{"state":"held","target_image":"repo:bad","blocking_reason":"original image failure","retained_image":"repo:old","retained_version":"1.2.3"}`)
	one.RollPacingObservedAt = pgtype.Timestamptz{Time: now, Valid: true}
	two := one
	two.Worker.ID = uuid.New()
	two.RollPhase.String = workersvc.PhaseSettled
	two.RollPacing = []byte(`{"state":"held","target_image":"repo:bad","blocking_reason":"original image failure","retained_image":"repo:older","retained_version":"1.2.2"}`)
	s := New(Config{HostedWorkerVersion: "bad"})
	check := s.checkFleetRoll(now, []store.ListAllWorkersRow{one, two}, true)
	if check.Severity != sevWarn || !strings.Contains(check.Summary, "repo:bad: original image failure") {
		t.Fatalf("%+v", check)
	}
	foreign := two
	foreign.Worker.ID = uuid.New()
	foreign.RollPacing = []byte(`{"state":"active","target_image":"repo:unrelated","retained_image":"foreign-image","retained_version":"foreign-version"}`)
	check = s.checkFleetRoll(now, []store.ListAllWorkersRow{one, two, foreign}, true)
	coverage := check.Evidence[2].Value
	if strings.Contains(coverage, "foreign") {
		t.Fatalf("mixed target coverage %q", coverage)
	}
	if !strings.Contains(coverage, "1.2.3") || !strings.Contains(coverage, "1.2.2") || strings.Contains(coverage, "workers") {
		t.Fatalf("coverage %q", coverage)
	}
	one.RollPacingObservedAt.Time = now.Add(-time.Minute - time.Second)
	two.RollPacingObservedAt = one.RollPacingObservedAt
	check = s.checkFleetRoll(now, []store.ListAllWorkersRow{one, two}, true)
	if !strings.Contains(check.Summary, "stuck rolling") || strings.Contains(check.Summary, "original image failure") {
		t.Fatalf("stale %+v", check)
	}
	one.RollPacingObservedAt.Time = now
	one.RollPhase.String = workersvc.PhaseSettled
	one.RollPacing = []byte(`{"state":"override","target_image":"repo:bad"}`)
	check = s.checkFleetRoll(now, []store.ListAllWorkersRow{one}, true)
	if check.Severity != sevWarn || !strings.Contains(check.Summary, "at risk") {
		t.Fatalf("override %+v", check)
	}
}

func TestFleetPacingPreservesAllStuckDanger(t *testing.T) {
	for _, state := range []string{"held", "override"} {
		for _, size := range []int{1, 3} {
			workers := make([]store.ListAllWorkersRow, 0, size)
			for i := 0; i < size; i++ {
				row := hostedRow("failed", workersvc.PhaseStuck, 0, "ImagePullBackOff", "agent", "bad")
				row.Worker.ID = uuid.New()
				row.RollPacing = []byte(`{"state":"` + state + `","target_image":"repo:bad"}`)
				row.RollPacingObservedAt = pgtype.Timestamptz{Time: fixedNow, Valid: true}
				workers = append(workers, row)
			}
			check := New(Config{HostedWorkerVersion: "bad"}).checkFleetRoll(fixedNow, workers, true)
			if check.Severity != sevDanger || !strings.Contains(check.Summary, "ImagePullBackOff") {
				t.Fatalf("state=%s size=%d suppressed fleet failure: %+v", state, size, check)
			}
		}
	}
}
