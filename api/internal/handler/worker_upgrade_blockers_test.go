package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerUpgradeBlockerCapabilityOverlay(t *testing.T) {
	id := uuid.New()
	detail := "existing failure and pacing detail"
	dto := apitypes.WorkerDTO{UpgradeDetail: &detail}
	rows := []store.ListWorkerUpgradeBlockersRow{{
		WorkerID: id, SafeCount: 1, RunID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Status:                 pgtype.Text{String: "queued", Valid: true},
		HealthReason:           pgtype.Text{String: "manual hold", Valid: true},
		HoldReason:             pgtype.Text{String: "credential_disabled", Valid: true},
		MissingRepoAgentSource: true,
	}}
	appendUpgradeBlockers(&dto, workersvc.GroupUpgradeBlockers(rows), id)
	for _, want := range []string{detail, "manual hold", "credential_disabled", "repo_agent_source_v1", "assignment releases only"} {
		if !strings.Contains(*dto.UpgradeDetail, want) {
			t.Fatalf("%s missing %s", *dto.UpgradeDetail, want)
		}
	}
}

func TestWorkerUpgradeBlockerOverlayPreservesDetail(t *testing.T) {
	id := uuid.New()
	detail := "existing failure and pacing detail"
	dto := apitypes.WorkerDTO{UpgradeDetail: &detail}
	appendUpgradeBlockers(&dto, map[uuid.UUID]workersvc.UpgradeBlockers{id: {Unavailable: true}}, id)
	if !strings.HasPrefix(*dto.UpgradeDetail, detail+"; ") || !strings.Contains(*dto.UpgradeDetail, "unavailable") || strings.Contains(*dto.UpgradeDetail, "No assigned") {
		t.Fatal(*dto.UpgradeDetail)
	}
	untouched := apitypes.WorkerDTO{}
	appendUpgradeBlockers(&untouched, nil, id)
	if untouched.UpgradeDetail != nil {
		t.Fatal(untouched.UpgradeDetail)
	}
}

func TestWorkerUpgradeBlockersEmptyAndFailedRead(t *testing.T) {
	h := &Handler{}
	if got := h.upgradeBlockersByWorker(context.Background(), nil); got != nil {
		t.Fatal(got)
	}
	id := uuid.New()
	got := h.upgradeBlockersByWorker(context.Background(), []uuid.UUID{id})
	if !got[id].Unavailable || strings.Contains(got[id].Summary(), "No assigned") {
		t.Fatal(got)
	}
}
