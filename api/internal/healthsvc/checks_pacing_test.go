package healthsvc

import (
	"encoding/json"
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

func TestFleetHeldPacingMigrationMultiplicity(t *testing.T) {
	const prefix = "fleet: multiple migration-held drains; intake risk; operator decision; "
	const ownCause = "ImagePullBackOff"
	const firstID = "22222222-2222-4222-8222-222222222222"
	const secondID = "33333333-3333-4333-8333-333333333333"
	for _, tc := range []struct {
		name     string
		multiple bool
		allStuck bool
		unknown  bool
	}{
		{name: "single"},
		{name: "multiple", multiple: true},
		{name: "single all stuck", allStuck: true},
		{name: "multiple all stuck", multiple: true, allStuck: true},
		{name: "unknown multiple", multiple: true, unknown: true},
		{name: "unknown multiple all stuck", multiple: true, allStuck: true, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := func(id, target, reason string) store.ListAllWorkersRow {
				t.Helper()
				row := hostedRow("held", workersvc.PhaseStuck, 0, "ImagePullBackOff", "agent", "bad")
				row.Worker.ID = uuid.MustParse(id)
				raw, err := json.Marshal(map[string]string{
					"state": "held", "target_image": target, "blocking_reason": reason,
				})
				if err != nil {
					t.Fatal(err)
				}
				row.RollPacing = raw
				row.RollPacingObservedAt = pgtype.Timestamptz{Time: fixedNow, Valid: true}
				return row
			}
			localCause := "layout_migration_custody: retained drain without coverage"
			selectedCause := ownCause
			if tc.multiple {
				localCause = prefix + localCause + "; held drains=2; sample=" + firstID
				selectedCause = prefix + ownCause
			}
			target := "repo:affected"
			if tc.unknown {
				target = ""
				localCause = prefix + "layout_migration_custody: retained drain; held drains=2; sample=" + firstID
			}
			workers := []store.ListAllWorkersRow{report(firstID, target, localCause)}
			if tc.multiple {
				workers = append(workers, report(secondID, target, localCause))
			}
			// checkFleetRoll must select the lowest ID at equal held rank,
			// even though the unrelated report arrives last.
			unrelated := report("11111111-1111-4111-8111-111111111111", "repo:unrelated", selectedCause)
			if !tc.allStuck {
				unrelated.RollPhase.String = workersvc.PhaseSettled
			}
			workers = append(workers, unrelated)
			check := New(Config{HostedWorkerVersion: "bad"}).checkFleetRoll(fixedNow, workers, true)
			wantSeverity := sevWarn
			if tc.allStuck {
				wantSeverity = sevDanger
			}
			if check.Severity != wantSeverity || !strings.Contains(check.Summary, "repo:unrelated: "+selectedCause+".") {
				t.Fatalf("selected report lost cause or severity: %+v", check)
			}
			if strings.Contains(check.Summary, "multiple migration-held drains") != tc.multiple {
				t.Fatalf("incorrect fleet multiplicity notice: %q", check.Summary)
			}
			displayed, err := json.Marshal(check)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{firstID, secondID, "layout_migration_custody", "held drains=", "sample=", "repo:affected"} {
				if strings.Contains(string(displayed), forbidden) {
					t.Fatalf("affected detail %q escaped into selected summary: %q", forbidden, check.Summary)
				}
			}
			if len(check.Evidence) < 2 || check.Evidence[0].Value != "repo:unrelated" || check.Evidence[1].Value != selectedCause {
				t.Fatalf("unrelated selected report did not retain its own diagnostics: %+v", check.Evidence)
			}
			if tc.allStuck && (!strings.Contains(check.Summary, "All ") || !strings.Contains(check.Summary, "ImagePullBackOff")) {
				t.Fatalf("all-stuck diagnostics suppressed: %+v", check)
			}
		})
	}
}

// These are representative controller JSON/SQL rows at the API health seam.
// They do not execute migration selection or Loop's synthetic status fallback.
func TestFleetHeldPacingUnknownTarget(t *testing.T) {
	const firstID = "22222222-2222-4222-8222-222222222222"
	const secondID = "33333333-3333-4333-8333-333333333333"
	const reason = "fleet: multiple migration-held drains; intake risk; operator decision; layout_migration_custody: retained drain; held drains=2; sample=" + firstID
	// Health evidence has its own 96-byte display cap; persistence retains the
	// complete 192-byte-bounded controller diagnostic (handler LiveDB test).
	const visibleReason = "fleet: multiple migration-held drains; intake risk; operator decision; layout_migration_custody:"
	const raw = `{"state":"held","target_image":"","target_version":"","blocking_reason":"fleet: multiple migration-held drains; intake risk; operator decision; layout_migration_custody: retained drain; held drains=2; sample=22222222-2222-4222-8222-222222222222"}`
	if len(reason) > 192 {
		t.Fatal("fixture exceeds controller reason budget")
	}
	report := func(id string) store.ListAllWorkersRow {
		row := hostedRow("unresolvable", workersvc.PhaseRolling, 0, "", "", "2.0.0")
		row.Worker.ID = uuid.MustParse(id)
		row.RollPacing = []byte(raw)
		row.RollPacingObservedAt = pgtype.Timestamptz{Time: fixedNow, Valid: true}
		return row
	}
	first, second := report(firstID), report(secondID)
	unrelated := hostedRow("unrelated", workersvc.PhaseSettled, 0, "", "", "2.0.0")
	unrelated.Worker.ID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	unrelated.RollPacing = []byte(`{"state":"active","target_image":"repo:observed","target_version":"2.0.0","retained_image":"repo:foreign","retained_version":"1.0.0"}`)
	unrelated.RollPacingObservedAt = first.RollPacingObservedAt
	for _, rows := range [][]store.ListAllWorkersRow{
		{unrelated, second, first},
		{first, second, unrelated},
	} {
		check := New(Config{HostedWorkerVersion: "2.0.0"}).checkFleetRoll(fixedNow, rows, true)
		if check.Severity != sevWarn || check.Summary != "Worker roll held at : "+visibleReason+"." {
			t.Fatalf("unknown-target hold lost selection or exact reason: %+v", check)
		}
		if len(check.Evidence) != 3 ||
			check.Evidence[0].Value != "" ||
			check.Evidence[1].Value != visibleReason ||
			check.Evidence[2].Value != "none confirmed" {
			t.Fatalf("unknown target inferred observed image or replacement coverage: %+v", check.Evidence)
		}
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
