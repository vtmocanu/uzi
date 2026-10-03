package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunUsageTailSchemaLiveDB pins the ADR-2014 D8 schema and queries against a real Postgres:
// the monotone message upsert (GREATEST tokens, OR output_final, COALESCE identity, conflict on a
// differing leg/ordinal), the ordinal-conflict flag, the cascades from runs and from a leg, the
// inline CHECKs, the NULL-safe tail predicate, and that LockRunUsage really holds the per-run
// advisory key. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh).
func TestRunUsageTailSchemaLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	userID, connID, repoID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("usageschema-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/r', 'main', true)`, repoID, connID, "g/"+repoID.String())
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
	      VALUES ($1, $2, $3, 1, 't', 'd', 'running', 'issue')`, runID, userID, repoID)

	leg := uuid.New()
	if err := q.UpsertRunUsageLegInit(ctx, store.UpsertRunUsageLegInitParams{RunID: runID, LegID: leg, InitSeq: 4, SdkSessionID: "S"}); err != nil {
		t.Fatalf("leg init: %v", err)
	}
	msg := func(id string, ordinal int32, in, out int64, final bool) store.UpsertRunUsageMessageParams {
		return store.UpsertRunUsageMessageParams{
			RunID: runID, MessageID: id, LegID: leg, Ordinal: ordinal, Model: "claude-sonnet-5-5",
			InputTokens: in, OutputTokens: out, OutputFinal: final,
		}
	}
	for _, p := range []store.UpsertRunUsageMessageParams{msg("m1", 1, 10, 5, false), msg("m1", 1, 7, 9, true), msg("m1", 1, 3, 1, false)} {
		if err := q.UpsertRunUsageMessage(ctx, p); err != nil {
			t.Fatalf("upsert message: %v", err)
		}
	}
	var in, out int64
	var final, conflict bool
	read := func(id string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT input_tokens, output_tokens, output_final, conflict FROM run_usage_messages WHERE run_id=$1 AND message_id=$2`, runID, id).Scan(&in, &out, &final, &conflict); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
	read("m1")
	if in != 10 || out != 9 || !final || conflict {
		t.Fatalf("m1 = in %d out %d final %v conflict %v, want GREATEST 10/9, OR true, no conflict", in, out, final, conflict)
	}
	// A differing ordinal for the same id flags a conflict and keeps the stored ordinal.
	if err := q.UpsertRunUsageMessage(ctx, msg("m1", 2, 1, 1, true)); err != nil {
		t.Fatal(err)
	}
	read("m1")
	var ord int32
	if err := pool.QueryRow(ctx, `SELECT ordinal FROM run_usage_messages WHERE run_id=$1 AND message_id='m1'`, runID).Scan(&ord); err != nil || !conflict || ord != 1 {
		t.Fatalf("m1 after a moved re-post: conflict %v ordinal %d (%v), want conflict true, ordinal kept 1", conflict, ord, err)
	}
	// Two ids on one ordinal are flagged by FlagRunUsageOrdinalConflicts, and never un-flagged.
	if err := q.UpsertRunUsageMessage(ctx, msg("a", 3, 1, 1, true)); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertRunUsageMessage(ctx, msg("b", 3, 1, 1, true)); err != nil {
		t.Fatal(err)
	}
	if err := q.FlagRunUsageOrdinalConflicts(ctx, store.FlagRunUsageOrdinalConflictsParams{RunID: runID, LegIds: []uuid.UUID{leg}}); err != nil {
		t.Fatal(err)
	}
	read("a")
	if !conflict {
		t.Fatal("two ids on ordinal 3: a not flagged")
	}

	// The tail predicate is NULL-safe: a leg with no init_seq contributes no tail messages.
	bare := uuid.New()
	if err := q.UpsertRunUsageLegMarker(ctx, store.UpsertRunUsageLegMarkerParams{RunID: runID, LegID: bare}); err != nil {
		t.Fatal(err)
	}
	bareMsg := msg("bare1", 1, 99, 0, true)
	bareMsg.LegID = bare
	if err := q.UpsertRunUsageMessage(ctx, bareMsg); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertRunUsageMessage(ctx, msg("ok1", 5, 1, 1, true)); err != nil {
		t.Fatal(err)
	}
	tail, err := q.ListRunUsageTailMessages(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].MessageID != "ok1" {
		t.Fatalf("tail = %+v, want only ok1 (conflicting rows and a leg with no init_seq are excluded)", tail)
	}

	// Inline CHECKs reject a malformed row at the database.
	var pgErr *pgconn.PgError
	_, err = pool.Exec(ctx, `INSERT INTO run_usage_messages (run_id, message_id, leg_id, ordinal, model) VALUES ($1, 'z', $2, 0, 'm')`, runID, leg)
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("ordinal 0 insert: err = %v, want check_violation 23514", err)
	}
	// A message must name an existing leg.
	_, err = pool.Exec(ctx, `INSERT INTO run_usage_messages (run_id, message_id, leg_id, ordinal, model) VALUES ($1, 'z', $2, 1, 'm')`, runID, uuid.New())
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("orphan leg insert: err = %v, want foreign_key_violation 23503", err)
	}

	// LockRunUsage holds the run's advisory key until its transaction ends.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.New(tx).LockRunUsage(ctx, runID); err != nil {
		t.Fatalf("LockRunUsage: %v", err)
	}
	// pg_try_advisory_lock on a pooled connection is session-scoped: release it when taken.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2::text))`, store.RunUsageLockClass, runID.String()).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1, hashtext($2::text))`, store.RunUsageLockClass, runID.String())
		t.Fatal("another session acquired the run usage lock while LockRunUsage's transaction held it")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext($2::text))`, store.RunUsageLockClass, runID.String()).Scan(&got); err != nil || !got {
		t.Fatalf("after rollback the lock must be free: got %v (%v)", got, err)
	}
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1, hashtext($2::text))`, store.RunUsageLockClass, runID.String())
	conn.Release()

	// Cascades: deleting a leg removes its messages; deleting the run removes everything.
	if _, err := pool.Exec(ctx, `DELETE FROM run_usage_legs WHERE run_id=$1 AND leg_id=$2`, runID, bare); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_usage_messages WHERE run_id=$1 AND leg_id=$2`, runID, bare).Scan(&n); err != nil || n != 0 {
		t.Fatalf("messages of a deleted leg = %d (%v), want 0 (ON DELETE CASCADE)", n, err)
	}
	if err := q.UpsertRunUsageTailState(ctx, store.UpsertRunUsageTailStateParams{RunID: runID, CappedRecords: 2, CappedLegs: 1}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertRunUsageTailState(ctx, store.UpsertRunUsageTailStateParams{RunID: runID, CappedRecords: 3}); err != nil {
		t.Fatal(err)
	}
	st, err := q.GetRunUsageTailState(ctx, runID)
	if err != nil || !st.RecordCapReached || st.CappedRecords != 5 || st.CappedLegs != 1 {
		t.Fatalf("tail state = %+v (%v), want cap reached, 5 records, 1 leg", st, err)
	}
	exec(`DELETE FROM runs WHERE id = $1`, runID)
	for _, tbl := range []string{"run_usage_legs", "run_usage_messages", "run_usage_tail_state"} {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl+` WHERE run_id=$1`, runID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s after the run was deleted = %d (%v), want 0 (ON DELETE CASCADE)", tbl, n, err)
		}
	}
}
