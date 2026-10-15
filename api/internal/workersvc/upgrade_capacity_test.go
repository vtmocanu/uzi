package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func capacitySignal(now time.Time) RollSignal {
	return RollSignal{Phase: PhaseStuck, ObservedAt: now, PodPhase: "Pending",
		BlockingKind: BlockingKindSchedulerCapacity, BlockingReason: "scheduler capacity: insufficient cpu"}
}

func TestCapacityClassificationAndHistory(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	old := now.Add(-24 * time.Hour)
	for _, reason := range []string{"scheduler capacity: insufficient cpu", "scheduler capacity: insufficient memory", "scheduler capacity: insufficient cpu and memory"} {
		for _, never := range []bool{false, true} {
			sig := capacitySignal(now)
			sig.BlockingReason, sig.UpgradingSince = reason, &old
			in := fleetInput(now, &sig)
			in.NeverRegistered = never
			if got, _ := ClassifyUpgrade(in, ceilingParams()); got != UpgradeStatusCapacityBlocked {
				t.Fatalf("capacity %q never=%v beyond ceiling = %s", reason, never, got)
			}
		}
	}
	for _, tc := range []struct {
		phase string
		never bool
		want  string
	}{
		{PhaseStuck, true, UpgradeStatusProvisioningFailed},
		{PhaseStuck, false, UpgradeStatusUpgradeFailed},
		{PhaseRolling, true, UpgradeStatusProvisioning},
		{PhaseRolling, false, UpgradeStatusUpgrading},
	} {
		sig := capacitySignal(now)
		sig.BlockingKind, sig.Phase = "", tc.phase
		in := fleetInput(now, &sig)
		in.NeverRegistered = tc.never
		if got, _ := ClassifyUpgrade(in, ceilingParams()); got != tc.want {
			t.Errorf("phase=%s never=%v got=%s want=%s", tc.phase, tc.never, got, tc.want)
		}
	}
	sig := capacitySignal(now)
	sig.Phase, sig.BlockingKind, sig.UpgradingSince = PhaseRolling, "", &old
	in := fleetInput(now, &sig)
	in.NeverRegistered = true
	if got, _ := ClassifyUpgrade(in, ceilingParams()); got != UpgradeStatusOutdated {
		t.Fatalf("provisioning bypassed ceiling: %s", got)
	}
	for _, kind := range []string{"hosted", "external"} {
		sig := capacitySignal(now)
		in := fleetInput(now, &sig)
		in.Kind = kind
		if kind == "hosted" {
			sig.ObservedAt = now.Add(-testTTL - time.Nanosecond)
		}
		if got, _ := ClassifyUpgrade(in, ceilingParams()); got != UpgradeStatusOutdated {
			t.Errorf("stale/external %s = %s", kind, got)
		}
	}
}

func TestCapacityStoredSignalGuardAndDirectService(t *testing.T) {
	now := time.Now()
	zero := int32(0)
	changes := []struct {
		name   string
		change func(*RollSignal)
	}{
		{"kind", func(s *RollSignal) { s.BlockingKind = "unknown" }},
		{"phase", func(s *RollSignal) { s.Phase = PhaseRolling }},
		{"pod", func(s *RollSignal) { s.PodPhase = "Running" }},
		{"reason", func(s *RollSignal) { s.BlockingReason = "CrashLoopBackOff" }},
		{"raw reason", func(s *RollSignal) { s.BlockingReason += "\u202e" }},
		{"container", func(s *RollSignal) { s.BlockingContainer = "worker" }},
		{"restarts", func(s *RollSignal) { s.RestartCount = 1 }},
		{"exit zero", func(s *RollSignal) { s.LastExitCode = &zero }},
	}
	st := &pacingStore{}
	svc := &Service{q: st}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			sig := capacitySignal(now)
			tc.change(&sig)
			if EffectiveBlockingKind(&sig) != "" {
				t.Fatal("contradictory legacy-retained cause survived")
			}
			if got, _ := ClassifyUpgrade(fleetInput(now, &sig), ceilingParams()); got == UpgradeStatusCapacityBlocked {
				t.Fatal("contradictory cause classified as capacity")
			}
			rep := RollHealthReport{BlockingKind: sig.BlockingKind, Phase: sig.Phase,
				PodPhase: sig.PodPhase, BlockingReason: &sig.BlockingReason,
				BlockingContainer: &sig.BlockingContainer, RestartCount: sig.RestartCount,
				LastExitCode: sig.LastExitCode, ObservedAt: now}
			if _, err := svc.RecordRollHealth(context.Background(), rep); err != nil {
				t.Fatal(err)
			}
			if st.calls[len(st.calls)-1].BlockingKind.Valid {
				t.Fatal("invalid direct cause persisted")
			}
		})
	}
	sig := capacitySignal(now)
	rep := RollHealthReport{BlockingKind: sig.BlockingKind, Phase: sig.Phase, PodPhase: sig.PodPhase, BlockingReason: &sig.BlockingReason, ObservedAt: now}
	if _, err := svc.RecordRollHealth(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	if got := st.calls[len(st.calls)-1].BlockingKind; !got.Valid || got.String != BlockingKindSchedulerCapacity {
		t.Fatalf("valid direct cause lost: %+v", got)
	}
	rep.BlockingKind = ""
	if _, err := svc.RecordRollHealth(context.Background(), rep); err != nil {
		t.Fatal(err)
	}
	if st.calls[len(st.calls)-1].BlockingKind.Valid {
		t.Fatal("ordinary report must forward null to clear cause")
	}
}

func TestNeverRegisteredPositiveEvidence(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		for _, status := range []string{"offline", "online", ""} {
			got := NeverRegistered(status, mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0, mask&16 != 0)
			if got != (status == "offline" && mask == 0) {
				t.Fatalf("status=%s history=%d got=%v", status, mask, got)
			}
		}
	}
}

func TestCapacitySummaryAttentionAndMutes(t *testing.T) {
	now := time.Now()
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	base := store.GetWorkerUpgradeSummaryForUserRow{
		Kind: "hosted", Status: "offline", Phase: text(PhaseStuck),
		ObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		BlockingKind: text(BlockingKindSchedulerCapacity), PodPhase: text("Pending"),
		BlockingReason: text("scheduler capacity: insufficient cpu"),
	}
	for _, tc := range []struct {
		name   string
		change func(*store.GetWorkerUpgradeSummaryForUserRow)
		want   int
	}{
		{"capacity", func(_ *store.GetWorkerUpgradeSummaryForUserRow) {}, 1},
		{"muted capacity", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.Muted = true }, 0},
		{"provision failed", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.BlockingKind = pgtype.Text{} }, 1},
		{"provisioning", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.Phase = text(PhaseRolling) }, 0},
		{"stale unregistered", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.ObservedAt.Time = now.Add(-61 * time.Second) }, 0},
		{"external", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.Kind = "external" }, 0},
		{"legacy retained invalid", func(r *store.GetWorkerUpgradeSummaryForUserRow) { r.PodPhase = text("Running") }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base
			tc.change(&row)
			st := &capacitySummaryStore{rows: []store.GetWorkerUpgradeSummaryForUserRow{row}}
			svc := &Service{q: st, now: func() time.Time { return now }}
			got, err := svc.UpgradeSummaryForUser(context.Background(), uuid.New(), "0.11.7", "", now.Add(-24*time.Hour), UpgradeParams{})
			if err != nil || got.Attention != tc.want {
				t.Fatalf("summary=%+v err=%v want=%d", got, err, tc.want)
			}
		})
	}
	for _, status := range []string{UpgradeStatusCapacityBlocked, UpgradeStatusProvisioningFailed, UpgradeStatusUpgradeFailed, UpgradeStatusOutdated} {
		if !InUpgradeAttentionSet(status) {
			t.Fatalf("missing attention: %s", status)
		}
	}
	if InUpgradeAttentionSet(UpgradeStatusProvisioning) {
		t.Fatal("provisioning needs no attention")
	}
}

type capacitySummaryStore struct {
	Store
	rows []store.GetWorkerUpgradeSummaryForUserRow
}

func (s *capacitySummaryStore) GetWorkerUpgradeSummaryForUser(_ context.Context, _ uuid.UUID) ([]store.GetWorkerUpgradeSummaryForUserRow, error) {
	return s.rows, nil
}
