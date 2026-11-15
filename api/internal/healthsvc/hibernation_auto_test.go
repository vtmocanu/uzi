package healthsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestFleetHibernationAuto(t *testing.T) {
	stamp := func(at time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: at, Valid: true} }
	hostedRows := []store.ListAllWorkersRow{{Worker: store.Worker{Kind: "hosted", Name: "h"}}}
	on := func() *fakeSettings {
		return &fakeSettings{hibEnabled: true, hibAuto: true, hibIdleAfter: 14 * 24 * time.Hour}
	}
	idleRow := func(name, reason string, idle time.Duration, total int64) store.ListAutoHibernationBlockedIdleRow {
		return store.ListAutoHibernationBlockedIdleRow{
			ID: uuid.New(), Name: name, Anchor: stamp(fixedNow.Add(-idle)), Reason: reason,
			BlockedSince: stamp(fixedNow.Add(-idle / 2)), Total: total,
		}
	}
	eval := func(fs *fakeStore, sett *fakeSettings, workers []store.ListAllWorkersRow) apitypes.HealthCheckDTO {
		return newSvc(fs, sett).checkFleetHibernation(context.Background(), fixedNow, workers)
	}

	t.Run("na without hosted workers", func(t *testing.T) {
		c := New(Config{Settings: on(), Store: &fakeStore{}, Now: func() time.Time { return fixedNow }}).
			checkFleetHibernation(context.Background(), fixedNow, []store.ListAllWorkersRow{{Worker: store.Worker{Kind: "docker"}}})
		if c.Severity != sevNA || c.Scope != "instance" || c.Group != groupWorkers || !strings.Contains(c.Summary, "No hosted workers") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("na when hibernation is disabled", func(t *testing.T) {
		s := on()
		s.hibEnabled = false
		c := eval(&fakeStore{hibIdle: []store.ListAutoHibernationBlockedIdleRow{idleRow("a", "manual_cordon", 20*24*time.Hour, 1)}}, s, hostedRows)
		if c.Severity != sevNA || !strings.Contains(c.Summary, "Automatic hibernation is off") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("na when automatic hibernation is off", func(t *testing.T) {
		s := on()
		s.hibAuto = false
		fs := &fakeStore{}
		c := eval(fs, s, hostedRows)
		if c.Severity != sevNA || !strings.Contains(c.Summary, "Automatic hibernation is off") || len(fs.hibParams) != 0 {
			t.Fatalf("got %+v, queries %d", c, len(fs.hibParams))
		}
	})
	t.Run("unknown on a settings error", func(t *testing.T) {
		s := on()
		s.hibErr = errors.New("boom")
		if c := eval(&fakeStore{}, s, hostedRows); c.Severity != sevUnknown {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("unknown on a query error", func(t *testing.T) {
		if c := eval(&fakeStore{hibIdleErr: errors.New("boom")}, on(), hostedRows); c.Severity != sevUnknown {
			t.Fatalf("idle: got %+v", c)
		}
		if c := eval(&fakeStore{hibParkedErr: errors.New("boom")}, on(), hostedRows); c.Severity != sevUnknown {
			t.Fatalf("parked: got %+v", c)
		}
	})
	t.Run("ok when nothing is blocked", func(t *testing.T) {
		fs := &fakeStore{}
		c := eval(fs, on(), hostedRows)
		if c.Severity != sevOK || len(c.Evidence) != 0 || c.Summary != "No idle hosted worker is blocked from automatic hibernation." {
			t.Fatalf("got %+v", c)
		}
		if len(fs.hibParams) != 1 || !fs.hibParams[0].Cutoff.Time.Equal(fixedNow.Add(-14*24*time.Hour)) || fs.hibParams[0].MaxRows != 50 {
			t.Fatalf("params = %+v", fs.hibParams)
		}
	})
	t.Run("warn with both sources", func(t *testing.T) {
		runID := uuid.MustParse("abcdef12-0000-4000-8000-000000000000")
		fs := &fakeStore{
			hibIdle: []store.ListAutoHibernationBlockedIdleRow{idleRow("idle-one", "custody_hold", 20*24*time.Hour, 1)},
			hibParked: []store.ListAutoHibernationParkedBlockersRow{{
				ID: uuid.New(), Name: "parked-one", RunID: runID, Status: "paused",
				StatusSince: stamp(fixedNow.Add(-30 * 24 * time.Hour)), Total: 1,
			}},
		}
		c := eval(fs, on(), hostedRows)
		if c.Severity != sevWarn || c.Action == nil || c.Since == nil {
			t.Fatalf("got %+v", c)
		}
		want := []string{
			"2",
			"worker idle-one idle 20d, hibernation blocked: recovery custody hold",
			"worker parked-one blocked: assigned run parked (abcdef12, paused)",
		}
		if len(c.Evidence) != 3 || c.Evidence[0].Label != "Workers" {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
		for i, w := range want {
			if c.Evidence[i].Value != w {
				t.Fatalf("evidence[%d] = %q, want %q", i, c.Evidence[i].Value, w)
			}
		}
		if *c.Since != fixedNow.Add(-30*24*time.Hour).UTC().Format("2006-01-02T15:04:05Z") {
			t.Fatalf("since = %s (the oldest blocker is the parked run)", *c.Since)
		}
	})
	t.Run("idle under a day renders hours", func(t *testing.T) {
		c := eval(&fakeStore{hibIdle: []store.ListAutoHibernationBlockedIdleRow{idleRow("a", "manual_cordon", 20*time.Hour, 1)}}, on(), hostedRows)
		if c.Evidence[1].Value != "worker a idle 20h, hibernation blocked: manual cordon" {
			t.Fatalf("got %q", c.Evidence[1].Value)
		}
	})
	t.Run("workers count is the exact window total, not the row count", func(t *testing.T) {
		var rows []store.ListAutoHibernationBlockedIdleRow
		for i := 0; i < 50; i++ {
			rows = append(rows, idleRow(fmt.Sprintf("w%d", i), "maintenance", 20*24*time.Hour, 70))
		}
		c := eval(&fakeStore{hibIdle: rows}, on(), hostedRows)
		if c.Evidence[0].Label != "Workers" || c.Evidence[0].Value != "70" {
			t.Fatalf("first evidence = %+v", c.Evidence[0])
		}
		last := c.Evidence[len(c.Evidence)-1]
		if len(c.Evidence) != 1+50+1 || last.Label != "More" || last.Value != "20 more not listed" {
			t.Fatalf("evidence len %d, last %+v", len(c.Evidence), last)
		}
	})
	t.Run("totals of both sources add up", func(t *testing.T) {
		fs := &fakeStore{
			hibIdle:   []store.ListAutoHibernationBlockedIdleRow{idleRow("a", "roll", 20*24*time.Hour, 3)},
			hibParked: []store.ListAutoHibernationParkedBlockersRow{{ID: uuid.New(), Name: "p", RunID: uuid.New(), Status: "limit_wait", StatusSince: stamp(fixedNow.Add(-20 * 24 * time.Hour)), Total: 4}},
		}
		c := eval(fs, on(), hostedRows)
		if c.Evidence[0].Value != "7" || c.Evidence[len(c.Evidence)-1].Value != "5 more not listed" {
			t.Fatalf("evidence = %+v", c.Evidence)
		}
	})
	t.Run("every recorded reason has its label", func(t *testing.T) {
		labels := map[string]string{
			workersvc.BlockedCustodyHold:            "recovery custody hold",
			workersvc.BlockedManualCordon:           "manual cordon",
			workersvc.BlockedMaintenance:            "maintenance",
			workersvc.BlockedRoll:                   "image roll",
			workersvc.BlockedDraining:               "draining",
			workersvc.BlockedPendingWakeDemand:      "a queued run is waking it",
			workersvc.BlockedControllerNotAdvertise: "controller not advertising hibernation",
			workersvc.BlockedNoPodIdentity:          "no pod identity",
			workersvc.BlockedControllerRefused:      "controller refused",
			workersvc.BlockedIntakeFenced:           "intake fenced",
		}
		for _, code := range workersvc.HibernationBlockedReasons() {
			want, ok := labels[code]
			if !ok {
				t.Fatalf("reason %q has no expected label in this test", code)
			}
			c := eval(&fakeStore{hibIdle: []store.ListAutoHibernationBlockedIdleRow{idleRow("w", code, 20*24*time.Hour, 1)}}, on(), hostedRows)
			if got := c.Evidence[1].Value; got != "worker w idle 20d, hibernation blocked: "+want {
				t.Fatalf("%s: got %q", code, got)
			}
		}
		c := eval(&fakeStore{hibIdle: []store.ListAutoHibernationBlockedIdleRow{idleRow("w", "weird\x1b[31m_code", 20*24*time.Hour, 1)}}, on(), hostedRows)
		if strings.ContainsRune(c.Evidence[1].Value, 0x1b) || !strings.Contains(c.Evidence[1].Value, "weird") {
			t.Fatalf("unknown code: %q", c.Evidence[1].Value)
		}
	})
	t.Run("evidence values stay within the byte bound and keep the reason", func(t *testing.T) {
		long := strings.Repeat("w", 200)
		multi := strings.Repeat("é世", 60) // 2- and 3-byte runes
		for _, name := range []string{long, multi, "x\x1b[31m" + long} {
			fs := &fakeStore{
				hibIdle: []store.ListAutoHibernationBlockedIdleRow{
					idleRow(name, workersvc.BlockedControllerNotAdvertise, 400*24*time.Hour, 2),
					idleRow(name, strings.Repeat("世", 80), 400*24*time.Hour, 2),
				},
				hibParked: []store.ListAutoHibernationParkedBlockersRow{{ID: uuid.New(), Name: name, RunID: uuid.New(), Status: "recovery_wait", StatusSince: stamp(fixedNow.Add(-40 * 24 * time.Hour)), Total: 1}},
			}
			c := eval(fs, on(), hostedRows)
			for i, e := range c.Evidence[1:] {
				if len(e.Value) > maxEvidenceBytes || !utf8.ValidString(e.Value) || strings.ContainsRune(e.Value, 0x1b) {
					t.Fatalf("value %d invalid (%d bytes): %q", i, len(e.Value), e.Value)
				}
			}
			if !strings.HasSuffix(c.Evidence[1].Value, "hibernation blocked: controller not advertising hibernation") {
				t.Fatalf("reason cut: %q", c.Evidence[1].Value)
			}
			if !strings.HasSuffix(c.Evidence[3].Value, ", recovery_wait)") {
				t.Fatalf("parked suffix cut: %q", c.Evidence[3].Value)
			}
		}
	})
}
