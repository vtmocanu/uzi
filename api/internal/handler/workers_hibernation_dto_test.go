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
