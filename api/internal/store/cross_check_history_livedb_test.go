package store_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCrossCheckHistorySnapshotLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh")
	}
	ctx := t.Context()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	defer mustExec(ctx, t, f.pool, "DELETE FROM users WHERE id=$1", f.userID)
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=1 WHERE id=$1", f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
 size_class,base_commit,candidate_digest,verdict,deadline_at)
 VALUES($1,'plan',1,1,'private candidate','[]','s',repeat('a',40),$2,'pending',now()-interval '1 hour')`, f.runID, []byte("digest"))
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := f.q.WithTx(tx)
	var isolation, readOnly string
	if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
		t.Fatal(err)
	}
	if isolation != "repeatable read" || readOnly != "on" {
		t.Fatalf("snapshot mode: %s/%s", isolation, readOnly)
	}
	groups, err := q.CrossCheckHistoryGroups(ctx, store.CrossCheckHistoryGroupsParams{
		OwnerID: f.userID, ParentIds: []uuid.UUID{}, Lifecycle: "active", StageFilter: "all", OutcomeFilter: "any", GroupLimit: 26,
		BeforeCreated: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil || len(groups) != 1 || !groups[0].Active {
		t.Fatalf("group read: %+v %v", groups, err)
	}
	// Commit settlement on another connection between the two actual query calls.
	mustExec(ctx, t, f.pool, "UPDATE cross_checks SET verdict='approve',reason_class='approve',decided_at=now() WHERE lead_run_id=$1", f.runID)
	args := store.CrossCheckHistoryAttemptsParams{OwnerID: f.userID, StageFilter: "all", OutcomeFilter: "any", ParentIds: []uuid.UUID{f.runID}, BeforeKeys: []int64{2 * 2147483648}, AttemptLimit: 11}
	attempts, err := q.CrossCheckHistoryAttempts(ctx, args)
	if err != nil || len(attempts) != 1 || attempts[0].Result != "pending" || !attempts[0].Active {
		t.Fatalf("repeatable attempts: %+v %v", attempts, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	settled, err := f.q.CrossCheckHistoryAttempts(ctx, args)
	if err != nil || len(settled) != 1 || settled[0].Result != "approve" || settled[0].Active {
		t.Fatalf("fresh read: %+v %v", settled, err)
	}
}

func TestCrossCheckHistoryBoundedMetadataLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh")
	}
	ctx := t.Context()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()
	defer mustExec(ctx, t, f.pool, "DELETE FROM users WHERE id=$1", f.userID)
	mustExec(ctx, t, f.pool, "UPDATE runs SET claim_generation=1 WHERE id=$1", f.runID)
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
 size_class,base_commit,candidate_digest,verdict,reason_class,checker_model,checker_effort,findings,deadline_at)
 VALUES($1,'plan',1,1,'private candidate','[]','s',repeat('a',40),$2,'approve','approve',$3::text,$3::text,
 jsonb_build_object('summary',$3::text,'items',jsonb_build_array(jsonb_build_object('summary',$3::text))),now())`, f.runID, []byte("digest"), strings.Repeat("oversized", 8192))
	mustExec(ctx, t, f.pool, `INSERT INTO cross_checks(lead_run_id,stage,round,lead_claim_generation,base_commit,head_commit,candidate_digest,
 verdict,outcome,code_context,guidance_snapshot,guidance_text,guidance_digest,repo_instructions_text,repo_instructions_digest,findings,deadline_at)
 VALUES($1,'code',1,1,repeat('a',40),repeat('b',40),$2,'failed','completed','{}','','',
 sha256(convert_to('','UTF8')),'',sha256(convert_to('','UTF8')),'{"not":"an array"}',now())`, f.runID, []byte("digest"))
	args := store.CrossCheckHistoryAttemptsParams{OwnerID: f.userID, StageFilter: "all", OutcomeFilter: "any", ParentIds: []uuid.UUID{f.runID}, BeforeKeys: []int64{2 * 2147483648}, AttemptLimit: 21}
	rows, err := f.q.CrossCheckHistoryAttempts(ctx, args)
	if err != nil || len(rows) != 2 {
		t.Fatalf("guarded rows: %+v %v", rows, err)
	}
	if rows[0].Stage != "code" || rows[0].FindingsCount != -1 || rows[1].Model != "" || rows[1].Effort != "" || rows[1].FindingsCount != -1 {
		t.Fatal("metadata guards transferred oversized or invalid values")
	}
	for _, tc := range []struct {
		json  string
		count int32
	}{
		{"[]", 0}, {`[{"id":"one"},{"id":"two"}]`, 2}, {"null", -1}, {`"scalar"`, -1},
	} {
		mustExec(ctx, t, f.pool, "UPDATE cross_checks SET findings=$2::jsonb WHERE lead_run_id=$1 AND stage='code'", f.runID, tc.json)
		got, err := f.q.CrossCheckHistoryAttempts(ctx, args)
		if err != nil || len(got) != 2 || got[0].FindingsCount != tc.count {
			t.Fatalf("typed Code count: %+v %v", got, err)
		}
	}
	available, err := f.q.CrossCheckHistoryAvailability(ctx, f.userID)
	if err != nil || !available {
		t.Fatal("retained attempts unavailable")
	}
	available, err = f.q.CrossCheckHistoryAvailability(ctx, uuid.New())
	if err != nil || available {
		t.Fatal("foreign availability")
	}
}
