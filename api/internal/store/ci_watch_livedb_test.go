package store_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func ciWatchFixture(t *testing.T) (context.Context, *store.Queries, *pgxpool.Pool, store.RunSchedule) {
	t.Helper()
	ctx := context.Background()
	q, owner, repo := schedFixture(ctx, t)
	pool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID: owner, RepoID: repo, Target: "ci_watch",
		Prompt: pgtype.Text{String: "repair", Valid: true}, Timing: "recurring",
		CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mustExec(ctx, t, pool, "DELETE FROM users WHERE id = $1", owner) })
	return ctx, q, pool, s
}

func ciWatchLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s store.RunSchedule) (pgx.Tx, *store.Queries) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	q := store.New(pool).WithTx(tx)
	if _, err := q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: s.ID, UserID: s.UserID}); err != nil {
		t.Fatal(err)
	}
	return tx, q
}

func TestCIWatchMigrationLiveDB(t *testing.T) {
	ctx, _, pool, s := ciWatchFixture(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// All migration probes are rolled back; the test never downgrades the shared DB.
	if _, err := tx.Exec(ctx, "DELETE FROM run_schedules WHERE id = $1", s.ID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("migrations/00334_ci_watch.sql")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(raw), "-- +goose Down")
	up := strings.Split(parts[0], "-- +goose Up")[1]
	down := parts[1]
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	constraints := func() map[string]string {
		t.Helper()
		rows, err := tx.Query(ctx, "SELECT conname, pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'run_schedules'::regclass AND conname IN ('run_schedules_target_check','run_schedules_target_shape')")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// Establish the exact 00157 Up definitions as the downgrade oracle.
	old, err := os.ReadFile("migrations/00157_run_schedules_self_improve_target.sql")
	if err != nil {
		t.Fatal(err)
	}
	oldUp := strings.Split(strings.Split(string(old), "-- +goose Up")[1], "-- +goose Down")[0]
	if _, err := tx.Exec(ctx, oldUp); err != nil {
		t.Fatal(err)
	}
	want := constraints()
	for _, target := range []string{"issue", "sweep", "prompt", "self_improve"} {
		var iid, prompt any
		if target == "issue" {
			iid = int64(42)
		}
		if target == "prompt" {
			prompt = "old prompt"
		}
		if _, err := tx.Exec(ctx, "INSERT INTO run_schedules (user_id,repo_id,target,issue_iid,prompt,timing,cron_expr,timezone) VALUES ($1,$2,$3,$4,$5,'recurring','0 * * * *','UTC')", s.UserID, s.RepoID, target, iid, prompt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, "INSERT INTO run_schedules (user_id,repo_id,target,origin,catalog_slug,timing,cron_expr,timezone) VALUES ($1,$2,'prompt','default','old-default','recurring','0 * * * *','UTC')", s.UserID, s.RepoID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("Up over every old arm: %v", err)
	}
	for _, origin := range []string{"user", "default"} {
		var prompt any
		if origin == "user" {
			prompt = "repair"
		}
		if _, err := tx.Exec(ctx, "INSERT INTO run_schedules (user_id,repo_id,target,prompt,origin,catalog_slug,timing,cron_expr,timezone) VALUES ($1,$2,'ci_watch',$3,$4,'ci-repair','recurring','0 * * * *','UTC')", s.UserID, s.RepoID, prompt, origin); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM run_schedules WHERE user_id = $1 AND target = 'ci_watch'", s.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	if got := constraints(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Down constraints = %v, want 00157 Up %v", got, want)
	}
}

func TestCIWatchBoundsAndSelectorsLiveDB(t *testing.T) {
	ctx, q, pool, s := ciWatchFixture(t)
	for _, selectors := range [][]byte{nil, []byte("[]"), []byte("[\"nightly\",\"123\"]")} {
		row, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{ID: s.ID, UserID: s.UserID, RepoID: s.RepoID, Target: "ci_watch", Prompt: s.Prompt, Timing: s.Timing, CronExpr: s.CronExpr, Timezone: s.Timezone, CiWorkflows: selectors})
		if err != nil {
			t.Fatal(err)
		}
		if len(selectors) == 0 {
			if len(row.CiWorkflows) != 0 {
				t.Fatal("NULL selectors lost")
			}
		} else if string(row.CiWorkflows) != string(selectors) && selectors != nil {
			var equal bool
			if err := pool.QueryRow(ctx, "SELECT $1::jsonb = $2::jsonb", row.CiWorkflows, selectors).Scan(&equal); err != nil || !equal {
				t.Fatalf("selectors lost: %s %v", row.CiWorkflows, err)
			}
		}
	}
	def, err := q.CreateDefaultSchedule(ctx, store.CreateDefaultScheduleParams{UserID: s.UserID, RepoID: s.RepoID, Target: "ci_watch", CatalogSlug: pgtype.Text{String: "ci-repair", Valid: true}, CronExpr: s.CronExpr, Timezone: "UTC", NextFireAt: tsPast(), CiWorkflows: []byte("[\"nightly\"]")})
	if err != nil || len(def.CiWorkflows) == 0 {
		t.Fatalf("default selectors: %v", err)
	}
	def, err = q.ResetDefaultSchedule(ctx, store.ResetDefaultScheduleParams{ID: def.ID, UserID: s.UserID, CronExpr: s.CronExpr, Timezone: s.Timezone, NextFireAt: tsPast()})
	if err != nil || len(def.CiWorkflows) != 0 {
		t.Fatalf("reset selectors: %+v %v", def, err)
	}
	for _, tc := range []struct {
		key   string
		valid bool
	}{{"", false}, {strings.Repeat("a", 200), true}, {strings.Repeat("a", 201), false}, {strings.Repeat("é", 100), true}, {strings.Repeat("é", 101), false}} {
		tx, tq := ciWatchLock(t, ctx, pool, s)
		_, err := tq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: tc.key})
		if tc.valid && err != nil {
			t.Fatal(err)
		}
		if !tc.valid {
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" {
				t.Fatalf("key bound: %v", err)
			}
		}
		_ = tx.Rollback(ctx)
	}
	tx, tq := ciWatchLock(t, ctx, pool, s)
	if _, err := tq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE ci_watch_episodes SET attempts = -1 WHERE schedule_id = $1",
		"UPDATE ci_watch_episodes SET halted_reason = 'other' WHERE schedule_id = $1",
		"UPDATE ci_watch_episodes SET observed_created_at = now() WHERE schedule_id = $1",
	} {
		_, err := pool.Exec(ctx, statement, s.ID)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatalf("episode constraint: %v", err)
		}
	}
	for _, tc := range []struct {
		iid    pgtype.Int8
		labels []byte
		prompt pgtype.Text
	}{{pgtype.Int8{Int64: 1, Valid: true}, nil, s.Prompt}, {pgtype.Int8{}, []byte("[]"), s.Prompt}, {pgtype.Int8{}, nil, pgtype.Text{}}} {
		_, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{UserID: s.UserID, RepoID: s.RepoID, Target: "ci_watch", IssueIid: tc.iid, Labels: tc.labels, Prompt: tc.prompt, Timing: s.Timing, CronExpr: s.CronExpr, Timezone: s.Timezone})
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.ConstraintName != "run_schedules_target_shape" {
			t.Fatalf("ci shape: %v", err)
		}
	}
}

func TestCIWatchConcurrentAdmissionAndPagingLiveDB(t *testing.T) {
	ctx, q, pool, s := ciWatchFixture(t)
	mustExec(ctx, t, pool, "INSERT INTO ci_watch_episodes (schedule_id,workflow_key) SELECT $1, 'key-' || lpad(n::text,4,'0') FROM generate_series(1,1023) n", s.ID)
	tx, tq := ciWatchLock(t, ctx, pool, s)
	if _, err := tq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "winner"}); err != nil {
		t.Fatal(err)
	}
	// The competitor cannot take the parent lock until the winner commits.
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(ctx) }()
	mustTxExec := func(sql string) {
		t.Helper()
		if _, err := other.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	mustTxExec("SET LOCAL lock_timeout = '100ms'")
	_, err = store.New(pool).WithTx(other).LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: s.ID, UserID: s.UserID})
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("parent lock failed to serialize: %v", err)
	}
	_ = other.Rollback(ctx)
	result := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		contender, err := pool.Begin(ctx)
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = contender.Rollback(ctx) }()
		cq := store.New(pool).WithTx(contender)
		close(ready)
		if _, err = cq.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: s.ID, UserID: s.UserID}); err != nil {
			result <- err
			return
		}
		_, err = cq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "loser"})
		result <- err
	}()
	<-ready
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("1024 admission: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("contender timed out")
	}
	var beforeVersion, afterVersion string
	var beforeEpisode store.CiWatchEpisode
	beforeEpisode, err = q.GetCIWatchEpisode(ctx, store.GetCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "winner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT xmin::text FROM ci_watch_episodes WHERE schedule_id=$1 AND workflow_key='winner'", s.ID).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	tx, tq = ciWatchLock(t, ctx, pool, s)
	existing, err := tq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "winner"})
	if err != nil {
		t.Fatalf("existing key at capacity: %v", err)
	}
	if !reflect.DeepEqual(store.CiWatchEpisode(existing), beforeEpisode) {
		t.Fatalf("admission changed episode history: %+v", existing)
	}
	if err := tx.QueryRow(ctx, "SELECT xmin::text FROM ci_watch_episodes WHERE schedule_id=$1 AND workflow_key='winner'", s.ID).Scan(&afterVersion); err != nil {
		t.Fatal(err)
	}
	if afterVersion != beforeVersion {
		t.Fatalf("admission rewrote existing row: xmin %s -> %s", beforeVersion, afterVersion)
	}
	if _, err := tq.LockCIWatchEpisode(ctx, store.LockCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "winner"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, pageSize := range []pgtype.Int4{{}, {Int32: 999, Valid: true}} {
		page, err := q.ListCIWatchEpisodes(ctx, store.ListCIWatchEpisodesParams{ScheduleID: s.ID, UserID: s.UserID, PageSize: pageSize})
		if err != nil || len(page) != 100 || page[0].WorkflowKey != "key-0001" || page[99].WorkflowKey != "key-0100" {
			t.Fatalf("first page: %v %v", len(page), err)
		}
	}
	cursor := pgtype.Text{}
	var keys []string
	for page := 0; page < 11; page++ {
		rows, err := q.ListCIWatchEpisodes(ctx, store.ListCIWatchEpisodesParams{ScheduleID: s.ID, UserID: s.UserID, AfterKey: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			keys = append(keys, row.WorkflowKey)
		}
		if len(rows) == 0 {
			break
		}
		cursor = pgtype.Text{String: rows[len(rows)-1].WorkflowKey, Valid: true}
	}
	if len(keys) != 1024 || keys[1022] != "key-1023" || keys[1023] != "winner" {
		t.Fatalf("keyset lost rows: %v", len(keys))
	}
	foreign := uuid.New()
	rows, err := q.ListCIWatchEpisodes(ctx, store.ListCIWatchEpisodesParams{ScheduleID: s.ID, UserID: foreign})
	if err != nil || len(rows) != 0 {
		t.Fatalf("foreign page: %v", err)
	}
	if _, err := q.LockRunScheduleForUser(ctx, store.LockRunScheduleForUserParams{ID: s.ID, UserID: foreign}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign parent: %v", err)
	}
	if _, err := q.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: foreign, WorkflowKey: "winner"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign admit: %v", err)
	}
	if has, err := q.HasCIWatchEpisodes(ctx, store.HasCIWatchEpisodesParams{ScheduleID: s.ID, UserID: s.UserID}); err != nil || !has {
		t.Fatalf("history missing: %v", err)
	}
	if has, err := q.HasCIWatchEpisodes(ctx, store.HasCIWatchEpisodesParams{ScheduleID: s.ID, UserID: foreign}); err != nil || has {
		t.Fatalf("foreign history: %v", err)
	}
	if _, err := q.DeleteRunSchedule(ctx, store.DeleteRunScheduleParams{ID: s.ID, UserID: s.UserID}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM ci_watch_episodes WHERE schedule_id=$1", s.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cascade: %d %v", remaining, err)
	}
}

func TestCIWatchTransitionsAndRunRollbackLiveDB(t *testing.T) {
	ctx, q, pool, s := ciWatchFixture(t)
	tx, tq := ciWatchLock(t, ctx, pool, s)
	if _, err := tq.EnsureCIWatchEpisode(ctx, store.EnsureCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	insert := func(tq *store.Queries, guard pgtype.Bool) store.Run {
		t.Helper()
		run, err := tq.CreatePromptRun(ctx, store.CreatePromptRunParams{UserID: s.UserID, RepoID: s.RepoID, ScheduleID: s.ID, IssueTitle: "repair", Harness: "claude", CiConfigGuard: guard, CiConfigPaths: []string{".github/workflows/**"}})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	tx, tq = ciWatchLock(t, ctx, pool, s)
	if _, err := tq.LockCIWatchEpisode(ctx, store.LockCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly"}); err != nil {
		t.Fatal(err)
	}
	run := insert(tq, pgtype.Bool{Bool: true, Valid: true})
	if !run.CiConfigGuard || !reflect.DeepEqual(run.CiConfigPaths, []string{".github/workflows/**"}) {
		t.Fatal("guard snapshots lost")
	}
	stamp := pgtype.Timestamptz{Time: time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC), Valid: true}
	row, err := tq.StartCIWatchEpisode(ctx, store.StartCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly", ObservedRunID: 101, ObservedCreatedAt: stamp, ObservedUpdatedAt: stamp, LastSignature: pgtype.Text{String: "signature", Valid: true}})
	if err != nil || row.Attempts != 1 || row.ObservedRunID.Int64 != 101 || !row.ObservedCreatedAt.Valid || !row.ObservedCreatedAt.Time.Equal(stamp.Time) || !row.ObservedUpdatedAt.Valid || !row.ObservedUpdatedAt.Time.Equal(stamp.Time) {
		t.Fatalf("attempt coordinates: %+v %v", row, err)
	}
	// Inject a downstream transaction failure; both writes must disappear.
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("injection did not fail")
	}
	_ = tx.Rollback(ctx)
	if _, err := q.GetRunByID(ctx, run.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("run survived rollback: %v", err)
	}
	row, err = q.GetCIWatchEpisode(ctx, store.GetCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly"})
	if err != nil || row.Attempts != 0 || row.ObservedRunID.Valid {
		t.Fatalf("episode survived rollback: %+v %v", row, err)
	}
	tx, tq = ciWatchLock(t, ctx, pool, s)
	ordinary := insert(tq, pgtype.Bool{})
	if ordinary.CiConfigGuard {
		t.Fatal("ordinary prompt defaults guarded")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx, tq = ciWatchLock(t, ctx, pool, s)
	row, err = tq.UpdateCIWatchEpisode(ctx, store.UpdateCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly", Attempts: 2, LastHandledRunID: pgtype.Int8{Int64: 102, Valid: true}, ObservedRunID: pgtype.Int8{Int64: 102, Valid: true}, ObservedCreatedAt: stamp, ObservedUpdatedAt: stamp, LastSignature: pgtype.Text{String: "signature", Valid: true}, HaltedReason: pgtype.Text{String: "no_progress", Valid: true}})
	if err != nil || row.HaltedReason.String != "no_progress" || row.Attempts != 2 {
		t.Fatalf("halt: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Converting the target/config must retain loop history.
	if _, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{ID: s.ID, UserID: s.UserID, RepoID: s.RepoID, Target: "prompt", Prompt: s.Prompt, Timing: s.Timing, CronExpr: s.CronExpr, Timezone: s.Timezone}); err != nil {
		t.Fatal(err)
	}
	row, err = q.GetCIWatchEpisode(ctx, store.GetCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly"})
	if err != nil || row.Attempts != 2 || row.HaltedReason.String != "no_progress" {
		t.Fatalf("config cleared history: %+v %v", row, err)
	}
	tx, tq = ciWatchLock(t, ctx, pool, s)
	row, err = tq.ResetCIWatchEpisode(ctx, store.ResetCIWatchEpisodeParams{ScheduleID: s.ID, UserID: s.UserID, WorkflowKey: "nightly", ObservedRunID: 103, ObservedCreatedAt: stamp, ObservedUpdatedAt: stamp})
	if err != nil || row.Attempts != 0 || row.HaltedReason.Valid || row.LastSignature.Valid || row.ObservedRunID.Int64 != 103 {
		t.Fatalf("reset: %+v %v", row, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
