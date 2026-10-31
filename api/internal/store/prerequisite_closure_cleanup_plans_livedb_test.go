package store_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPrerequisiteCleanupRawPagesAndPlansLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_cleanup_plans_")
	const owners, perOwner, pageSize, expiredTail = 16, 256, 10, 16
	const total = owners * perOwner

	// Each owner stays within the 256-active-capture admission bound. Copy
	// manifest facts from prerequisiteNew; leave every production guard enabled.
	for owner := 0; owner < owners; owner++ {
		f := prerequisiteNew(t, ctx, pool)
		mustExec(ctx, t, pool, `INSERT INTO recovery_captures
			SELECT (jsonb_populate_record(NULL::recovery_captures,to_jsonb(c) ||
				jsonb_build_object('id',gen_random_uuid(),'idempotency_key',gen_random_uuid()::text))).*
			FROM recovery_captures c CROSS JOIN generate_series(1,$2::int) WHERE c.id=$1`,
			f.capture, perOwner-1)
		mustExec(ctx, t, pool, `INSERT INTO recovery_prerequisite_closures
			(capture_id,hold_id,run_id,user_id,repo_id,checksum,prerequisite_shas,pin_ref,next_attempt_at)
			SELECT c.id,c.hold_id,c.run_id,c.user_id,$2,c.checksum,c.prerequisite_shas,
				'refs/uzi-prereq/'||c.id::text,now()
			FROM recovery_captures c WHERE c.hold_id=$1 AND c.id<>$3`, f.hold, f.repo, f.capture)
		// Walk pending -> reserved -> invoked -> pinned with the lease,
		// normalized forge binding and live run required by prerequisite_closure_guard.
		mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
			SET attempts=1,attempt_deadline=now()+interval '1 minute',next_attempt_at=now()+interval '2 minutes'
			WHERE hold_id=$1`, f.hold)
		mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
			SET state='invoked',live_run_id=run_id,invoked_at=now(),invoked_deadline=now()+interval '30 seconds',
				pin_tip=$2,connection_id=(SELECT connection_id FROM repos WHERE id=$3),
				forge_kind='github',normalized_base_url='https://example.com',project_id=1,default_branch='main'
			WHERE hold_id=$1`, f.hold, heldTip, f.repo)
		mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures SET state='pinned' WHERE hold_id=$1`, f.hold)
	}
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
		SET next_attempt_at=now(),audit_next_at=now()+interval '24 hours'`)

	// All due timestamps are equal, so PostgreSQL UUID order determines both
	// the expected raw pages and the ONLY captures explicitly expired below.
	want := make([]uuid.UUID, 0, total)
	captures := make([]uuid.UUID, 0, total)
	raw, err := pool.Query(ctx, "SELECT id,capture_id FROM recovery_prerequisite_closures ORDER BY next_attempt_at,id")
	if err != nil {
		t.Fatal(err)
	}
	for raw.Next() {
		var id, capture uuid.UUID
		if err := raw.Scan(&id, &capture); err != nil {
			raw.Close()
			t.Fatal(err)
		}
		want = append(want, id)
		captures = append(captures, capture)
	}
	raw.Close()
	if err := raw.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != total {
		t.Fatalf("fixture closures=%d want=%d", len(want), total)
	}
	tag, err := pool.Exec(ctx, "UPDATE recovery_captures SET state='expired' WHERE id=ANY($1::uuid[])", captures[total-expiredTail:])
	prerequisiteRows(t, expiredTail, tag.RowsAffected(), err)

	// A crashed delete retry remains eligible alongside initial cleanup.
	qSeed := store.New(pool)
	if _, err := qSeed.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{
		ID: want[total-1], PassID: prerequisiteUUID(uuid.New()), LeaseSeconds: 8,
	}); err != nil {
		t.Fatal(err)
	}
	prerequisiteAdvance(t, ctx, pool, 38*time.Second)
	mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now()")
	// Check the entire skew and its correspondence, with bounded diagnostics.
	var available, expired, shaped int
	err = pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE c.state='available'),
		count(*) FILTER (WHERE c.state='expired'),
		count(*) FILTER (WHERE p.state='pinned' AND p.next_attempt_at=now()
			AND p.audit_next_at>now())
		FROM recovery_prerequisite_closures p JOIN recovery_captures c ON c.id=p.capture_id`).Scan(&available, &expired, &shaped)
	if err != nil || available != total-expiredTail || expired != expiredTail || shaped != total {
		t.Fatalf("fixture available=%d expired=%d pinned/due/audit=%d err=%v", available, expired, shaped, err)
	}
	for _, table := range []string{"recovery_prerequisite_closures", "recovery_captures", "runs", "repos", "forge_connections"} {
		mustExec(ctx, t, pool, "ANALYZE "+table)
	}

	want = want[total-expiredTail:]
	captures = captures[total-expiredTail:]
	planDB := &prerequisitePlanDB{DBTX: pool}
	q := store.New(planDB)
	args := store.ListPrerequisiteCleanupDueParams{
		DueAt:      pgtype.Timestamptz{Time: prerequisiteNow(t, ctx, pool), Valid: true},
		AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
		Lim:        pageSize,
	}
	first, err := q.ListPrerequisiteCleanupDue(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != pageSize {
		t.Fatalf("first raw cleanup page count=%d want=%d", len(first), pageSize)
	}
	for i, c := range first {
		if c.ID != want[i] {
			t.Fatalf("first raw cleanup page mismatch at %d: id=%s want=%s", i, c.ID, want[i])
		}
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM recovery_captures WHERE id=$1", c.CaptureID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if c.CaptureID != captures[i] || state != "expired" {
			t.Fatalf("first raw cleanup page capture at %d: id=%s want=%s state=%s", i, c.CaptureID, captures[i], state)
		}
	}
	t.Run("initial_cursor", func(t *testing.T) {
		prerequisiteExplain(t, ctx, pool, planDB.sql, planDB.args, "prerequisite_closure_cleanup_due", true, "recovery_prerequisite_closures_capture_id_key")
	})

	seen := make(map[uuid.UUID]bool, total)
	offset, reachedExpired, nonemptyPages := 0, 0, 0
	ended, lateExplained := false, false
	// At most ceil(total/pageSize) nonempty pages plus one empty end page.
	// Any failed query or invariant stops this test; there are no retries.
	for page := 0; page < (total+pageSize-1)/pageSize+1; page++ {
		rows, err := q.ListPrerequisiteCleanupDue(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > pageSize {
			t.Fatalf("raw page %d size=%d exceeds %d", page, len(rows), pageSize)
		}
		if len(rows) == 0 {
			ended = true
			break
		}
		nonemptyPages++
		// The second page starts within the eligible tail after the available prefix.
		if offset == pageSize {
			lateExplained = t.Run("late_cursor", func(t *testing.T) {
				prerequisiteExplain(t, ctx, pool, planDB.sql, planDB.args, "prerequisite_closure_cleanup_due", true, "recovery_prerequisite_closures_capture_id_key")
			})
		}
		for _, c := range rows {
			if c.ID == want[len(want)-1] && c.CleanupAttempts != 1 {
				t.Fatal("cleanup retry was not selected")
			}
			if offset >= len(want) {
				t.Fatalf("raw cursor returned extra row at %d: %s", offset, c.ID)
			}
			if c.ID != want[offset] || c.CaptureID != captures[offset] {
				t.Fatalf("raw cursor first mismatch at %d: closure=%s want=%s capture=%s want=%s",
					offset, c.ID, want[offset], c.CaptureID, captures[offset])
			}
			if seen[c.ID] || (offset > 0 && bytes.Compare(want[offset-1][:], c.ID[:]) >= 0) {
				t.Fatalf("raw cursor duplicate or nonmonotonic identity at %d: %s", offset, c.ID)
			}
			if !c.NextAttemptAt.Time.Equal(args.DueAt.Time) {
				t.Fatalf("raw cursor due timestamp mismatch at %d: %s", offset, c.NextAttemptAt.Time)
			}
			seen[c.ID] = true
			reachedExpired++
			offset++
			args.AfterDueAt = c.NextAttemptAt
			args.AfterID = c.ID
		}
	}
	if !ended || offset != expiredTail || len(seen) != expiredTail || reachedExpired != expiredTail {
		t.Fatalf("raw cursor ended=%t rows=%d distinct=%d expired tail=%d want=%d/%d",
			ended, offset, len(seen), reachedExpired, expiredTail, expiredTail)
	}
	if !lateExplained {
		t.Fatal("late cursor plan contract was not verified")
	}
	if t.Failed() {
		return
	}
	t.Logf("raw cleanup: available prefix=%d expired tail=%d rows=%d pages=%d limit=%d; initial/late custom+generic plans use a cleanup-due or capture-ID index with top Limit actual rows <=10",
		available, reachedExpired, offset, nonemptyPages, pageSize)
}
