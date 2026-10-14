package healthsvc

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestFleetEvidenceUTF8Bound(t *testing.T) {
	reason := strings.Repeat("A", maxEvidenceBytes-1) + "😀"
	storage := hostedRow("storage", workersvc.PhaseStuck, 0, reason, "data", "new")
	capacity := capacityHealthRow("scheduler capacity: insufficient memory")
	svc := New(Config{HostedWorkerVersion: "new"})
	check := svc.checkFleetRoll(fixedNow, []store.ListAllWorkersRow{storage, capacity}, true)
	for _, evidence := range check.Evidence {
		if len(evidence.Value) > maxEvidenceBytes || !utf8.ValidString(evidence.Value) {
			t.Fatalf("unbounded or invalid UTF-8 evidence %q: %q", evidence.Label, evidence.Value)
		}
	}
	if !strings.Contains(check.Summary, "scheduler capacity: insufficient memory") || !strings.Contains(check.Summary, strings.Repeat("A", 20)) || check.Severity != sevDanger {
		t.Fatalf("mixed faults concealed: %+v", check)
	}
}

func capacityHealthRow(reason string) store.ListAllWorkersRow {
	w := hostedRow("capacity", workersvc.PhaseStuck, 0, reason, "", "new")
	w.Worker.ID = uuid.New()
	w.RollBlockingKind = pgtype.Text{String: workersvc.BlockingKindSchedulerCapacity, Valid: true}
	w.RollPodPhase = pgtype.Text{String: "Pending", Valid: true}
	w.RollPhaseSince.Time = fixedNow.Add(-24 * time.Hour)
	return w
}

func TestFleetCapacityCauseHealth(t *testing.T) {
	for _, reason := range []string{
		"scheduler capacity: insufficient cpu",
		"scheduler capacity: insufficient memory",
		"scheduler capacity: insufficient cpu and memory",
	} {
		t.Run(reason, func(t *testing.T) {
			w := capacityHealthRow(reason)
			c := New(Config{}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{w}, true)
			if c.Severity != sevDanger || !strings.Contains(c.Summary, "1 capacity blocked") || !strings.Contains(c.Summary, reason) ||
				c.Action == nil || !strings.Contains(*c.Action, "Wait for available capacity") ||
				!strings.Contains(*c.Action, "smaller worker") || !strings.Contains(*c.Action, "operator") {
				t.Fatalf("capacity beyond ceiling: %+v", c)
			}
			for _, forbidden := range []string{"Publish the image", "WFFC", "storage health", "rollback"} {
				if strings.Contains(c.Summary+*c.Action, forbidden) {
					t.Fatalf("capacity has unrelated remedy %q: %+v", forbidden, c)
				}
			}
		})
	}
}

func TestFleetCapacityMixedAndPacingHealth(t *testing.T) {
	for _, pacing := range []string{"", "held", "override"} {
		for _, all := range []bool{false, true} {
			t.Run(fmt.Sprint(pacing, all), func(t *testing.T) {
				capacity := capacityHealthRow("scheduler capacity: insufficient cpu")
				storage := hostedRow("storage", workersvc.PhaseStuck, 0, "storage unavailable", "", "new")
				rows := []store.ListAllWorkersRow{capacity, storage}
				wantSev := sevDanger
				if !all {
					rows = append(rows, hostedRow("healthy", workersvc.PhaseSettled, 0, "", "", "new"))
					wantSev = sevWarn
				}
				if pacing != "" {
					rows[0].RollPacing = []byte(`{"state":"` + pacing + `","target_image":"repo:new"}`)
					rows[0].RollPacingObservedAt = pgtype.Timestamptz{Time: fixedNow, Valid: true}
				}
				c := New(Config{}).checkFleetRoll(fixedNow, rows, true)
				if c.Severity != wantSev || !strings.Contains(c.Summary, "1 capacity blocked") ||
					!strings.Contains(c.Summary, "storage unavailable") || !strings.Contains(c.Summary, "2 of") ||
					c.Action == nil || !strings.Contains(*c.Action, "available capacity") || !strings.Contains(*c.Action, "blocking diagnostics") {
					t.Fatalf("mixed failures concealed: %+v", c)
				}
				if c.Since == nil || len(c.Evidence) < 4 {
					t.Fatalf("missing failure evidence: %+v", c)
				}
			})
		}
	}
}

func TestFleetCapacityHistoryAndInvalidCauseHealth(t *testing.T) {
	w := capacityHealthRow("storage unavailable")
	w.Worker.Status = "offline"
	for _, registered := range []bool{false, true} {
		w.Worker.RegisteredPodUid = pgtype.UUID{Bytes: [16]byte{1}, Valid: registered}
		c := New(Config{}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{w}, true)
		if c.Severity != sevDanger || c.Action == nil || strings.Contains(*c.Action, "available capacity") ||
			strings.Contains(c.Summary, "capacity blocked") || strings.Contains(c.Summary, "first-provision") == registered {
			t.Fatalf("invalid retained cause/history: %+v", c)
		}
	}
	for _, history := range []func(*store.Worker){
		func(w *store.Worker) { w.Status = "online" },
		func(w *store.Worker) { w.LastHeartbeatAt = pgtype.Timestamptz{Time: fixedNow, Valid: true} },
		func(w *store.Worker) { w.Version = pgtype.Text{String: "old", Valid: true} },
		func(w *store.Worker) { w.SnapshotRegisterNonce = pgtype.Text{String: "nonce", Valid: true} },
		func(w *store.Worker) { w.RegisteredPodUid = pgtype.UUID{Bytes: [16]byte{1}, Valid: true} },
		func(w *store.Worker) { w.RollHeartbeatAt = pgtype.Timestamptz{Time: fixedNow, Valid: true} },
	} {
		row := hostedRow("registered", workersvc.PhaseStuck, 0, "ImagePullBackOff", "agent", "new")
		row.Worker.Status = "offline"
		history(&row.Worker)
		c := New(Config{}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{row}, true)
		if c.Severity != sevDanger || strings.Contains(c.Summary, "first-provision") {
			t.Fatalf("registration history concealed ordinary failure: %+v", c)
		}
	}
	for _, mutate := range []func(*store.ListAllWorkersRow){
		func(w *store.ListAllWorkersRow) { w.RollBlockingContainer = pgtype.Text{String: "agent", Valid: true} },
		func(w *store.ListAllWorkersRow) { w.RollRestartCount = pgtype.Int4{Int32: 1, Valid: true} },
		func(w *store.ListAllWorkersRow) { w.RollLastExitCode = pgtype.Int4{Int32: 0, Valid: true} },
		func(w *store.ListAllWorkersRow) { w.RollPodPhase.String = "Running" },
	} {
		row := capacityHealthRow("scheduler capacity: insufficient cpu")
		mutate(&row)
		c := New(Config{}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{row}, true)
		if c.Action == nil || strings.Contains(*c.Action, "available capacity") {
			t.Fatalf("incomplete diagnostic revalidation: %+v", c)
		}
	}
}

func TestFleetCapacityFreshnessAndExternalHealth(t *testing.T) {
	stale := capacityHealthRow("scheduler capacity: insufficient cpu")
	stale.RollObservedAt.Time = fixedNow.Add(-rollSignalTTL - time.Second)
	external := capacityHealthRow("scheduler capacity: insufficient memory")
	external.Worker.Kind = "external"
	for _, crOK := range []bool{false, true} {
		c := New(Config{}).checkFleetRoll(fixedNow, []store.ListAllWorkersRow{stale, external}, crOK)
		want := sevUnknown
		if crOK {
			want = sevOK
		}
		if c.Severity != want || strings.Contains(c.Summary, "capacity blocked") {
			t.Fatalf("stale/external: %+v", c)
		}
	}
}
