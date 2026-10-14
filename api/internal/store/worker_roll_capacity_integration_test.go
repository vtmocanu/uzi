package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerRollBlockingKindPersistenceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	user := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, fmt.Sprintf("%s@capacity.test", user))
	seed := func() uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status,kind,version,hosted_size,template_declared)
			VALUES($1,$2,'capacity',$3,'offline','hosted','1.0.0','m','base')`, id, user, []byte(id.String()))
		return id
	}
	t0 := time.Now().UTC().Truncate(time.Second)
	cause := pgtype.Text{String: "storage_fault", Valid: true}
	write := func(p store.UpsertWorkerRollHealthParams) {
		t.Helper()
		n, err := q.UpsertWorkerRollHealth(ctx, p)
		if err != nil || n != 1 {
			t.Fatalf("upsert rows=%d err=%v", n, err)
		}
	}
	readCause := func(id uuid.UUID) pgtype.Text {
		t.Helper()
		var got pgtype.Text
		if err := pool.QueryRow(ctx, "SELECT blocking_kind FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	for _, phase := range []string{"stuck", "rolling", "settled"} {
		t.Run("ordinary_"+phase+"_clears", func(t *testing.T) {
			id := seed()
			p := rollReportTag(id, "stuck", t0, "2.0.0", true)
			p.BlockingKind = cause
			p.BlockingReason = pgtype.Text{String: "FailedMount", Valid: true}
			p.RestartCount = 5
			write(p)
			if got := readCause(id); got != cause {
				t.Fatalf("insert cause=%v", got)
			}
			// Exercise conflict writes as well as the insert.
			p.BlockingKind = pgtype.Text{String: "capacity", Valid: true}
			write(p)
			if got := readCause(id); got != p.BlockingKind {
				t.Fatalf("conflict cause=%v", got)
			}
			write(rollReportTag(id, phase, t0.Add(time.Minute), "2.0.0", true))
			if got := readCause(id); got.Valid {
				t.Fatalf("ordinary %s retained cause=%v", phase, got)
			}
			var anchor pgtype.Timestamptz
			var reason pgtype.Text
			var restarts int32
			if err := pool.QueryRow(ctx, "SELECT upgrading_since,blocking_reason,restart_count FROM worker_upgrade_reports WHERE worker_id=$1", id).Scan(&anchor, &reason, &restarts); err != nil {
				t.Fatal(err)
			}
			if !anchor.Valid || !anchor.Time.Equal(t0) {
				t.Fatalf("anchor changed: %v", anchor)
			}
			if phase == "settled" {
				if !reason.Valid || reason.String != "FailedMount" || restarts != 5 {
					t.Fatalf("settled diagnostics changed: %v %d", reason, restarts)
				}
			} else if reason.Valid || restarts != 0 {
				t.Fatalf("ordinary diagnostics not cleared: %v %d", reason, restarts)
			}
		})
	}

	t.Run("legacy_statement_retains_new_column", func(t *testing.T) {
		id := seed()
		p := rollReportTag(id, "stuck", t0, "2.0.0", true)
		p.BlockingKind = cause
		write(p)
		legacy, err := os.ReadFile("testdata/worker_roll_health_legacy.sql")
		if err != nil {
			t.Fatal(err)
		}
		p = rollReportTag(id, "stuck", t0.Add(time.Minute), "2.0.0", true)
		p.BlockingReason = pgtype.Text{String: "FailedMount", Valid: true}
		result, err := pool.Exec(ctx, string(legacy),
			p.Phase, p.PhaseSince, p.TargetImage, p.PodPhase,
			p.BlockingContainer, p.BlockingReason, p.RestartCount, p.LastExitCode,
			p.ControllerReportedAt, p.ObservedAt, p.TagValid, p.WorkerImageTag,
			p.PollIntervalSeconds, p.UpdatePacing, p.Pacing, p.WorkerID)
		if err != nil || result.RowsAffected() != 1 {
			t.Fatalf("legacy rows=%d err=%v", result.RowsAffected(), err)
		}
		if got := readCause(id); got != cause {
			t.Fatalf("legacy statement changed new column: %v", got)
		}
	})

	t.Run("projections", func(t *testing.T) {
		id := seed()
		p := rollReportTag(id, "stuck", t0, "2.0.0", true)
		p.BlockingKind = cause
		write(p)
		podUID := uuid.New()
		mustExec(ctx, t, pool, `UPDATE workers SET status='online',last_heartbeat_at=$2,
			snapshot_register_nonce='capacity-nonce',registered_pod_uid=$3,roll_heartbeat_at=$2 WHERE id=$1`, id, t0, podUID)
		rows, err := q.ListWorkersByUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range rows {
			if r.ID == id {
				found = true
				if r.RollBlockingKind != cause {
					t.Fatalf("user cause=%v", r.RollBlockingKind)
				}
			}
		}
		if !found {
			t.Fatal("worker absent from user list")
		}
		all, err := q.ListAllWorkers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, r := range all {
			if r.Worker.ID == id {
				found = true
				if r.RollBlockingKind != cause {
					t.Fatalf("admin cause=%v", r.RollBlockingKind)
				}
			}
		}
		if !found {
			t.Fatal("worker absent from admin list")
		}
		summary, err := q.GetWorkerUpgradeSummaryForUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		found = false
		for _, r := range summary {
			if r.WorkerID == id {
				found = true
				if r.BlockingKind != cause || r.Status != "online" || !r.Version.Valid || r.Version.String != "1.0.0" ||
					!r.SnapshotRegisterNonce.Valid || r.SnapshotRegisterNonce.String != "capacity-nonce" || !r.RegisteredPodUid.Valid ||
					uuid.UUID(r.RegisteredPodUid.Bytes) != podUID || !r.LastHeartbeatAt.Valid ||
					!r.LastHeartbeatAt.Time.Equal(t0) || !r.RollHeartbeatAt.Valid || !r.RollHeartbeatAt.Time.Equal(t0) {
					t.Fatalf("summary lost cause or identity: %+v", r)
				}
			}
		}
		if !found {
			t.Fatal("worker absent from summary")
		}
	})

	for _, isolated := range []bool{false, true} {
		t.Run(fmt.Sprintf("registration_isolated_%v", isolated), func(t *testing.T) {
			id := seed()
			p := rollReportTag(id, "stuck", t0, "2.0.0", true)
			p.BlockingKind = cause
			write(p)
			if isolated {
				mustExec(ctx, t, pool, "UPDATE worker_upgrade_reports SET upgrading_since=NULL WHERE worker_id=$1", id)
			}
			for _, version := range []string{"1.0.0", "1.0.0+build"} {
				if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: id, Version: pgtype.Text{String: version, Valid: true}}); err != nil {
					t.Fatal(err)
				}
				if got := readCause(id); got != cause {
					t.Fatalf("same release %s cleared cause=%v", version, got)
				}
			}
			if _, err := q.RegisterWorker(ctx, store.RegisterWorkerParams{ID: id, Version: pgtype.Text{String: "2.0.0", Valid: true}}); err != nil {
				t.Fatal(err)
			}
			if got := readCause(id); got.Valid {
				t.Fatalf("release move retained cause=%v", got)
			}
		})
	}
}
