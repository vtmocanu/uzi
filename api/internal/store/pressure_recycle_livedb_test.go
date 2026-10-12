package store_test

import (
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"os"
	"testing"
)

func TestPressureRecycleDrainWritersLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	user := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@pressure.test")
	w, err := q.CreateHostedWorker(ctx, store.CreateHostedWorkerParams{UserID: user, Name: "pressure", TokenHash: []byte(uuid.NewString()), TemplateDeclared: pgconv.TextOrNull("base"), HostedSize: pgconv.TextOrNull("m"), AnthropicBindMode: "default"})
	if err != nil {
		t.Fatal(err)
	}
	read := func() store.Worker {
		t.Helper()
		w, err := q.GetWorkerByID(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	cordon := func() {
		t.Helper()
		n, err := q.CordonHostedWorkerPressureRecycle(ctx, w.ID)
		if err != nil || n != 1 {
			t.Fatalf("cordon rows=%d err=%v", n, err)
		}
	}
	release := func() {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qt := q.WithTx(tx)
		if _, err := qt.GetWorkerForUpdate(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		n, err := qt.UncordonHostedWorkerPressureRecycle(ctx, w.ID)
		if err != nil || n != 1 {
			t.Fatalf("release rows=%d err=%v", n, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	maintenance := func(phase string, fenced bool) store.Worker {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qt := q.WithTx(tx)
		if _, err := qt.GetWorkerForUpdate(ctx, w.ID); err != nil {
			t.Fatal(err)
		}
		got, err := qt.SetDindMaintenance(ctx, store.SetDindMaintenanceParams{ID: w.ID, MaintenancePhase: phase, MaintenanceFenced: fenced})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return got
	}
	cordon()
	at := read().DrainingSince.Time
	cordon()
	if got := read(); got.DrainingReason.String != "pressure_recycle" || !got.DrainingSince.Time.Equal(at) {
		t.Fatal("repeat changed ownership/timestamp")
	}
	for _, fence := range []string{"stats_disk_pressure_streak=2", "dind_pressure_streak=2", "maintenance_fenced=true", "maintenance_owns_drain=true", "maintenance_phase='requested'", "maintenance_phase='ready'", "maintenance_phase='stopping'", "maintenance_phase='recycling'"} {
		mustExec(ctx, t, pool, "UPDATE workers SET "+fence+" WHERE id=$1", w.ID)
		release()
		if !read().DrainingSince.Valid || read().DrainingReason.String != "pressure_recycle" {
			t.Fatalf("lost fence %s", fence)
		}
		mustExec(ctx, t, pool, "UPDATE workers SET stats_disk_pressure_streak=0,dind_pressure_streak=0,maintenance_fenced=false,maintenance_owns_drain=false,maintenance_phase='' WHERE id=$1", w.ID)
	}
	for _, phase := range []string{"complete", "cancelled"} {
		maintenance("requested", true)
		got := maintenance(phase, false)
		if !got.DrainingSince.Valid || got.DrainingReason.String != "pressure_recycle" || got.MaintenanceOwnsDrain {
			t.Fatalf("%s lost pressure owner", phase)
		}
	}
	release()
	if read().DrainingSince.Valid || read().DrainingReason.Valid {
		t.Fatal("pressure did not clear")
	}
	for _, owner := range []string{"NULL", "'image_roll'"} {
		mustExec(ctx, t, pool, "UPDATE workers SET draining_since=now(),draining_reason="+owner+" WHERE id=$1", w.ID)
		before := read()
		cordon()
		release()
		after := read()
		if before.DrainingReason != after.DrainingReason || !before.DrainingSince.Time.Equal(after.DrainingSince.Time) {
			t.Fatal("independent drain changed")
		}
	}
	for _, guard := range []string{"maintenance_fenced=true", "maintenance_owns_drain=true", "maintenance_phase='requested'"} {
		mustExec(ctx, t, pool, "UPDATE workers SET draining_since=NULL,draining_reason=NULL,"+guard+" WHERE id=$1", w.ID)
		cordon()
		if read().DrainingReason.Valid {
			t.Fatal("took maintenance ownership")
		}
		mustExec(ctx, t, pool, "UPDATE workers SET maintenance_fenced=false,maintenance_owns_drain=false,maintenance_phase='' WHERE id=$1", w.ID)
	}
}

// Constraint replacement needs ACCESS EXCLUSIVE even when its data change is additive.
func TestPressureRecycleMigrationLockLiveDB(t *testing.T) {
	ctx, pool, _ := openHealthLiveDB(t)
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if _, err := reader.Exec(ctx, "SELECT 1 FROM workers"); err != nil {
		t.Fatal(err)
	}
	migration, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = migration.Rollback(ctx) }()
	if _, err := migration.Exec(ctx, "SET LOCAL lock_timeout = '100ms'"); err != nil {
		t.Fatal(err)
	}
	// This is 00332's first statement; its failure leaves the constraint intact.
	_, err = migration.Exec(ctx, "ALTER TABLE workers DROP CONSTRAINT workers_draining_reason_check")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("expected migration lock timeout behind a reader, got %v", err)
	}
	t.Log("00332 constraint replacement blocks behind a worker-table reader; deployment needs ACCESS EXCLUSIVE")
}

func TestPressureRecycleMigrationRoundtripLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	user := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@migration.test")
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash,draining_since,draining_reason) VALUES($1,$2,'migration',$3,now(),$4)", id, user, []byte(uuid.NewString()), []any{"pressure_recycle", "image_roll", nil}[i])
	}
	before := make([]store.Worker, 3)
	for i, id := range ids {
		var err error
		before[i], err = q.GetWorkerByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Error(err)
		}
	}()
	if err := store.MigrateDownTo(ctx, dsn, 331); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		got, err := q.GetWorkerByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.DrainingSince.Time.Equal(before[i].DrainingSince.Time) {
			t.Fatal("Down removed drain")
		}
		if i == 0 {
			if got.DrainingReason.Valid {
				t.Fatal("pressure owner retained")
			}
		} else if got.DrainingReason != before[i].DrainingReason {
			t.Fatal("Down changed another owner")
		}
	}
	if _, err := pool.Exec(ctx, "UPDATE workers SET draining_reason='pressure_recycle' WHERE id=$1", ids[0]); err == nil {
		t.Fatal("old constraint accepted pressure")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	mustExec(ctx, t, pool, "UPDATE workers SET draining_reason='pressure_recycle' WHERE id=$1", ids[0])
}
