package handler

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCapacityDTOParityHistoryAndExpiry(t *testing.T) {
	now := time.Now()
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	user := store.ListWorkersByUserRow{
		Kind: "hosted", Status: "offline", RollPhase: txt("stuck"),
		RollObservedAt:   pgtype.Timestamptz{Time: now, Valid: true},
		RollBlockingKind: txt("scheduler_capacity"), RollPodPhase: txt("Pending"),
		RollBlockingReason: txt("scheduler capacity: insufficient cpu"),
	}
	admin := store.ListAllWorkersRow{
		Worker:    store.Worker{Kind: "hosted", Status: "offline"},
		RollPhase: user.RollPhase, RollObservedAt: user.RollObservedAt,
		RollBlockingKind: user.RollBlockingKind, RollPodPhase: user.RollPodPhase,
		RollBlockingReason: user.RollBlockingReason,
	}
	assert := func(want string, cause bool) {
		t.Helper()
		u := workerDTOFromRow(user, "0.11.7", "", now, now.Add(-24*time.Hour))
		a := workerDTOFromAdminRow(admin, "0.11.7", "", now, now.Add(-24*time.Hour))
		for _, dto := range []apitypes.WorkerDTO{u, a} {
			if dto.UpgradeStatus != want || (dto.UpgradeBlockingKind != nil) != cause {
				t.Fatalf("status=%s cause=%v want=%s/%v", dto.UpgradeStatus, dto.UpgradeBlockingKind, want, cause)
			}
			if workersvc.IsUpgradeFailure(want) && dto.UpgradeBlockingReason == nil {
				t.Fatal("failed status missing diagnostics")
			}
			if !workersvc.IsUpgradeFailure(want) && (dto.UpgradeBlockingReason != nil || dto.UpgradeBlockingContainer != nil || dto.UpgradeLastExitCode != nil) {
				t.Fatal("non-failed status leaked diagnostics")
			}
		}
		if !reflect.DeepEqual(u.UpgradeBlockingKind, a.UpgradeBlockingKind) ||
			!reflect.DeepEqual(u.UpgradeBlockingReason, a.UpgradeBlockingReason) ||
			!reflect.DeepEqual(u.UpgradeDetail, a.UpgradeDetail) || u.UpgradeTarget != a.UpgradeTarget {
			t.Fatal("owner/admin diagnostic parity")
		}
	}
	assert("capacity_blocked", true)
	user.RollPodPhase, admin.RollPodPhase = txt("Running"), txt("Running")
	assert("provisioning_failed", false) // retained cause from an older API
	user.RollBlockingKind, admin.RollBlockingKind = pgtype.Text{}, pgtype.Text{}
	for mask := 0; mask < 32; mask++ {
		user.LastHeartbeatAt.Valid, admin.Worker.LastHeartbeatAt.Valid = mask&1 != 0, mask&1 != 0
		user.Version.Valid, admin.Worker.Version.Valid = mask&2 != 0, mask&2 != 0
		user.SnapshotRegisterNonce.Valid, admin.Worker.SnapshotRegisterNonce.Valid = mask&4 != 0, mask&4 != 0
		user.RegisteredPodUid.Valid, admin.Worker.RegisteredPodUid.Valid = mask&8 != 0, mask&8 != 0
		user.RollHeartbeatAt.Valid, admin.Worker.RollHeartbeatAt.Valid = mask&16 != 0, mask&16 != 0
		want := "upgrade_failed"
		if mask == 0 {
			want = "provisioning_failed"
		}
		assert(want, false)
		user.RollPhase, admin.RollPhase = txt("rolling"), txt("rolling")
		want = "upgrading"
		if mask == 0 {
			want = "provisioning"
		}
		assert(want, false)
		user.RollPhase, admin.RollPhase = txt("stuck"), txt("stuck")
	}
	user.Version, admin.Worker.Version = txt("0.11.0"), txt("0.11.0")
	user.RollBlockingKind, admin.RollBlockingKind = txt("scheduler_capacity"), txt("scheduler_capacity")
	user.RollPodPhase, admin.RollPodPhase = txt("Pending"), txt("Pending")
	user.RollObservedAt.Time, admin.RollObservedAt.Time = now.Add(-61*time.Second), now.Add(-61*time.Second)
	assert("outdated", false)
	user.RollObservedAt.Time, admin.RollObservedAt.Time = now, now
	user.Kind, admin.Worker.Kind = "external", "external"
	assert("outdated", false)
}
