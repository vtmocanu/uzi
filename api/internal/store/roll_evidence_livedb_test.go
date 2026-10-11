package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRollEvidenceDrainWritersLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	user := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@roll.test")
	w, err := q.CreateHostedWorker(ctx, store.CreateHostedWorkerParams{UserID: user, Name: "label", TokenHash: []byte(uuid.NewString()), TemplateDeclared: pgconv.TextOrNull("base"), HostedSize: pgconv.TextOrNull("m"), AnthropicBindMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	maintenance := func(phase string) store.Worker {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		qt := store.New(tx)
		if _, err := qt.GetWorkerForUpdate(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		updated, err := qt.SetDindMaintenance(ctx, store.SetDindMaintenanceParams{ID: w.ID, MaintenancePhase: phase, MaintenanceFenced: phase == "requested"})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return updated
	}
	read := func() store.Worker {
		t.Helper()
		row, err := q.GetWorkerByID(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if _, err := q.CordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if got := maintenance("requested"); got.DrainingReason.String != "image_roll" || got.MaintenanceOwnsDrain {
		t.Fatal("maintenance stole prior image drain")
	}
	if _, err := q.UncordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if !read().DrainingSince.Valid {
		t.Fatal("active maintenance fence lost")
	}
	maintenance("complete")
	for _, column := range []string{"stats_disk_pressure_streak", "dind_pressure_streak"} {
		// Column names are fixed test constants, never request data.
		mustExec(ctx, t, pool, "UPDATE workers SET "+column+"=2 WHERE id=$1", w.ID)
		if _, err := q.UncordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		if !read().DrainingSince.Valid || read().DrainingReason.String != "image_roll" {
			t.Fatal("pressure fence lost")
		}
		mustExec(ctx, t, pool, "UPDATE workers SET "+column+"=0 WHERE id=$1", w.ID)
	}
	if _, err := q.UncordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if read().DrainingSince.Valid || read().DrainingReason.Valid {
		t.Fatal("image drain not released after fences ended")
	}
	maintenance("requested")
	if got := maintenance("complete"); got.DrainingSince.Valid || got.DrainingReason.Valid || got.MaintenanceOwnsDrain {
		t.Fatal("maintenance-owned cleanup left stale provenance")
	}
	if _, err := q.CordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	maintenance("requested")
	updated, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: w.ID, SnapshotRegisterNonce: pgconv.TextOrNull("new"), Capabilities: []string{}, ProtocolCapabilities: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.DrainingSince.Valid || updated.DrainingReason.Valid || !updated.MaintenanceOwnsDrain {
		t.Fatal("register failed maintenance handoff")
	}
	maintenance("complete")
	if _, err := q.CordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.CordonHostedWorker(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if read().DrainingReason.Valid {
		t.Fatal("legacy cordon did not remove roll ownership")
	}
	if _, err := q.UncordonHostedWorkerImageRoll(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if !read().DrainingSince.Valid {
		t.Fatal("roll release erased legacy cordon")
	}
	if _, err := q.UncordonHostedWorker(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if read().DrainingSince.Valid || read().DrainingReason.Valid {
		t.Fatal("legacy release left stale drain provenance")
	}
}
