package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerDTOHibernationFields(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w := store.Worker{
		Kind: "hosted", Status: "offline", Version: pgtype.Text{String: "0.1.0", Valid: true},
		HibernationState:  "hibernated",
		HibernationReason: pgtype.Text{String: "idle", Valid: true},
		HibernatedAt:      pgtype.Timestamptz{Time: now, Valid: true},
		WakeRequestedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		WakeError:         pgtype.Text{String: "timed out", Valid: true},
	}
	dto := workerDTOFromWorker(w, 0, false, "", "0.84.0", "0.84.0", now, now.Add(-24*time.Hour))
	if dto.HibernationState != "hibernated" || dto.HibernationReason == nil || *dto.HibernationReason != "idle" ||
		dto.HibernatedAt == nil || dto.WakeRequestedAt == nil || dto.WakeError == nil || *dto.WakeError != "timed out" {
		t.Fatalf("hibernation fields not carried: %+v", dto)
	}
	if dto.UpgradeStatus == "outdated" || dto.UpgradeStatus == "upgrading" {
		t.Fatalf("hibernated worker reported %q", dto.UpgradeStatus)
	}
	if !dto.EffectiveCordoned {
		t.Error("hibernated worker must not accept intake")
	}
	zero := workerDTOFromWorker(store.Worker{Kind: "external", Status: "online"}, 0, false, "", "0.84.0", "", now, now)
	if zero.HibernationState != "none" || zero.HibernationReason != nil || zero.HibernatedAt != nil || zero.WakeRequestedAt != nil || zero.WakeError != nil {
		t.Fatalf("zero worker: %+v", zero)
	}
}

// The owner /workers list builds DTOs from store.ListWorkersByUserRow, a separate path from
// workerDTOFromWorker; it must project the cleanup columns too.
func TestWorkerDTOFromRowHibernationCleanup(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name                   string
		row                    store.ListWorkersByUserRow
		wantNil                bool
		state, outcome, reason string
	}{
		{name: "none is null", row: store.ListWorkersByUserRow{CleanupPhase: "none"}, wantNil: true},
		{name: "started is cleaning", row: store.ListWorkersByUserRow{CleanupPhase: "started"}, state: "cleaning"},
		{name: "settled skipped unsupported", row: store.ListWorkersByUserRow{CleanupPhase: "settled", HibernationCleanup: txt("skipped"), HibernationCleanupReason: txt("unsupported")}, state: "settled", outcome: "skipped", reason: "unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.row.Kind, c.row.Status = "hosted", "offline"
			dto := workerDTOFromRow(c.row, "0.84.0", "0.84.0", now, now)
			got := dto.HibernationCleanup
			if c.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", *got)
				}
				return
			}
			if got == nil || got.State != c.state {
				t.Fatalf("state: %+v want %q", got, c.state)
			}
			str := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if str(got.Outcome) != c.outcome || str(got.CleanupReason) != c.reason {
				t.Fatalf("outcome/reason: %+v", got)
			}
		})
	}
}

func TestWorkerDTOHibernationCleanup(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name                   string
		w                      store.Worker
		wantNil                bool
		state, outcome, reason string
	}{
		{name: "none without outcome", w: store.Worker{CleanupPhase: "none"}, wantNil: true},
		{name: "empty phase without outcome", w: store.Worker{}, wantNil: true},
		{name: "requested is cleaning", w: store.Worker{CleanupPhase: "requested"}, state: "cleaning"},
		{name: "started is cleaning", w: store.Worker{CleanupPhase: "started"}, state: "cleaning"},
		{name: "unknown", w: store.Worker{CleanupPhase: "unknown"}, state: "unknown"},
		{name: "settled pruned", w: store.Worker{CleanupPhase: "settled", HibernationCleanup: txt("pruned")}, state: "settled", outcome: "pruned"},
		{name: "none with skipped outcome", w: store.Worker{CleanupPhase: "none", HibernationCleanup: txt("skipped"), HibernationCleanupReason: txt("unsupported")}, state: "none", outcome: "skipped", reason: "unsupported"},
		{name: "settled failed", w: store.Worker{CleanupPhase: "settled", HibernationCleanup: txt("failed"), HibernationCleanupReason: txt("interrupted")}, state: "settled", outcome: "failed", reason: "interrupted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.w.Kind, c.w.Status = "hosted", "offline"
			dto := workerDTOFromWorker(c.w, 0, false, "", "0.84.0", "0.84.0", now, now)
			if c.wantNil {
				if dto.HibernationCleanup != nil {
					t.Fatalf("want nil, got %+v", *dto.HibernationCleanup)
				}
				return
			}
			got := dto.HibernationCleanup
			if got == nil || got.State != c.state {
				t.Fatalf("state: %+v want %q", got, c.state)
			}
			str := func(p *string) string {
				if p == nil {
					return ""
				}
				return *p
			}
			if str(got.Outcome) != c.outcome || str(got.CleanupReason) != c.reason {
				t.Fatalf("outcome/reason: %+v", got)
			}
		})
	}
}
