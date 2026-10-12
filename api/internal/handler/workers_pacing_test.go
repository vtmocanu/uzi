package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerPacingProjections(t *testing.T) {
	now := time.Now()
	data := []byte(`{"state":"held","target_image":"repo:bad","blocking_reason":"ImagePullBackOff"}`)
	receipt := pgtype.Timestamptz{Time: now, Valid: true}
	userRow := store.ListWorkersByUserRow{RollPacing: data, RollPacingObservedAt: receipt}
	adminRow := store.ListAllWorkersRow{RollPacing: data, RollPacingObservedAt: receipt}
	user := workerDTOFromRow(userRow, "dev", "", now, now)
	admin := workerDTOFromAdminRow(adminRow, "dev", "", now, now)
	if string(user.RollPacing) != string(admin.RollPacing) || len(user.RollPacing) == 0 {
		t.Fatalf("user %s admin %s", user.RollPacing, admin.RollPacing)
	}
	receipt.Time = now.Add(-61 * time.Second)
	userRow.RollPacingObservedAt, adminRow.RollPacingObservedAt = receipt, receipt
	// Ordinary readiness remains fresh while pacing expires.
	userRow.RollObservedAt, adminRow.RollObservedAt = pgtype.Timestamptz{Time: now, Valid: true}, pgtype.Timestamptz{Time: now, Valid: true}
	user = workerDTOFromRow(userRow, "dev", "", now, now)
	admin = workerDTOFromAdminRow(adminRow, "dev", "", now, now)
	if len(user.RollPacing) != 0 || len(admin.RollPacing) != 0 {
		t.Fatal("readiness renewed pacing")
	}
	out, err := json.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["roll_pacing"]; exists {
		t.Fatal("stale optional pacing serialized")
	}
}
