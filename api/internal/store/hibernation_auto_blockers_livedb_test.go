package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestListAutoHibernationBlockersExactTotalLiveDB (issue #4207): the merged blockers query
// reports the exact number of matching workers and the true oldest since under a row limit, and
// returns the idle set before the parked set. Three matching workers are seeded: two idle with a
// recorded reason (one whose first sighting predates its anchor) and one whose only run has been
// paused for 30 days. The database is shared, so the total is asserted relative to a baseline read
// before seeding.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestListAutoHibernationBlockersExactTotalLiveDB(t *testing.T) {
	ctx, pool, q := openRepoActivityLiveDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := pgtype.Timestamptz{Time: now.Add(-14 * 24 * time.Hour), Valid: true}

	list := func(max int32) []ListAutoHibernationBlockersRow {
		t.Helper()
		rows, err := q.ListAutoHibernationBlockers(ctx, ListAutoHibernationBlockersParams{Cutoff: cutoff, MaxRows: max})
		if err != nil {
			t.Fatalf("ListAutoHibernationBlockers: %v", err)
		}
		return rows
	}
	var baseline int64
	if rows := list(1); len(rows) > 0 {
		baseline = rows[0].Total
	}

	user, _ := raSeedUserConn(ctx, t, pool, true)
	var conn uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM forge_connections WHERE user_id = $1`, user).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	repo := uuid.New()
	raProjectSeq++
	raExec(ctx, t, pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,enabled) VALUES ($1,$2,$3,$4,'https://hib.example/g/hib',false)`,
		repo, conn, raProjectSeq, "g/hib-"+repo.String()[:8])
	iid := int32(time.Now().UnixNano()%1_000_000_000) + 1
	newWorker := func(name string, createdAgo time.Duration) uuid.UUID {
		id, pod := uuid.New(), uuid.New()
		raExec(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, registered_pod_uid, created_at, last_heartbeat_at)
		                      VALUES ($1,$2,$3,$4,'online','hosted','m','base',$5,$6,$7)`, id, user, name, id[:], pod, now.Add(-createdAgo), now)
		t.Cleanup(func() { raExec(ctx, t, pool, `DELETE FROM workers WHERE id = $1`, id) })
		return id
	}
	newRun := func(worker uuid.UUID, status string, since time.Time, finished any) {
		iid++
		raExec(ctx, t, pool, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, status_since, finished_at)
		                      VALUES ($1,$2,$3,'issue',$4,'t','d',$5,$6,1,$7,$8)`, uuid.New(), user, repo, iid, status, worker, since, finished)
	}
	suffix := uuid.NewString()[:8]
	nameA, nameB, nameC := "blk-a-"+suffix, "blk-b-"+suffix, "blk-c-"+suffix

	// A: idle since creation (60d), reason first seen 50d ago.
	a := newWorker(nameA, 60*24*time.Hour)
	raExec(ctx, t, pool, `UPDATE workers SET hibernation_blocked_reason='controller_refused', hibernation_reason='roll_reservation', hibernation_blocked_at=$2, hibernation_blocked_since=$3 WHERE id=$1`, a, now, now.Add(-50*24*time.Hour))
	// B: a run finished 20d ago (the anchor); reason first seen 40d ago, before the anchor.
	b := newWorker(nameB, 60*24*time.Hour)
	fin := now.Add(-20 * 24 * time.Hour)
	newRun(b, "completed", fin, fin)
	raExec(ctx, t, pool, `UPDATE workers SET hibernation_blocked_reason='maintenance', hibernation_blocked_at=$2, hibernation_blocked_since=$3 WHERE id=$1`, b, now, now.Add(-40*24*time.Hour))
	// C: only run paused for 30d.
	c := newWorker(nameC, 60*24*time.Hour)
	newRun(c, "paused", now.Add(-30*24*time.Hour), nil)

	one := list(1)
	if len(one) != 1 {
		t.Fatalf("max_rows=1 returned %d rows, want 1", len(one))
	}
	if one[0].Total != baseline+3 {
		t.Fatalf("total under LIMIT 1 = %d, want baseline %d + 3", one[0].Total, baseline)
	}

	full := list(10000)
	if int64(len(full)) != baseline+3 || full[0].Total != baseline+3 {
		t.Fatalf("unlimited list has %d rows, total %d; want %d", len(full), full[0].Total, baseline+3)
	}
	for _, r := range full {
		if r.OldestSince != full[0].OldestSince {
			t.Fatalf("oldest_since differs between rows: %v vs %v", r.OldestSince, full[0].OldestSince)
		}
	}
	// The window-wide oldest is the same whether or not the row limit cuts the listing.
	if one[0].OldestSince != full[0].OldestSince {
		t.Fatalf("oldest_since under LIMIT 1 = %v, unlimited %v", one[0].OldestSince, full[0].OldestSince)
	}
	if !full[0].OldestSince.Valid || full[0].OldestSince.Time.After(now.Add(-50*24*time.Hour)) {
		t.Fatalf("oldest_since %v is later than the seeded 50d blocker", full[0].OldestSince)
	}

	got := map[string]ListAutoHibernationBlockersRow{}
	lastIdle, firstParked := -1, len(full)
	for i, r := range full {
		got[r.Name] = r
		if r.Kind == "idle" {
			lastIdle = i
		} else if i < firstParked {
			firstParked = i
		}
	}
	if lastIdle > firstParked {
		t.Fatalf("an idle row follows a parked row (idle ends at %d, parked starts at %d)", lastIdle, firstParked)
	}
	if r, ok := got[nameA]; !ok || r.Kind != "idle" || r.Reason != "controller_refused" || r.ControllerReason != "roll_reservation" || !r.Since.Time.Equal(now.Add(-50*24*time.Hour)) {
		t.Fatalf("A = %+v (listed %v)", r, ok)
	}
	// controller_reason is the empty string, never NULL, when hibernation_reason is unset or the row is parked.
	if r := got[nameB]; r.ControllerReason != "" {
		t.Fatalf("B controller_reason = %q, want empty", r.ControllerReason)
	}
	if r := got[nameC]; r.ControllerReason != "" {
		t.Fatalf("C controller_reason = %q, want empty", r.ControllerReason)
	}
	// B's reason predates its anchor: since is the anchor (GREATEST), not the stale first sighting.
	if r, ok := got[nameB]; !ok || r.Kind != "idle" || !r.Since.Time.Equal(fin) || !r.Anchor.Time.Equal(fin) {
		t.Fatalf("B = %+v (listed %v), want since == anchor == %v", r, ok, fin)
	}
	if r, ok := got[nameC]; !ok || r.Kind != "parked" || r.Status.String != "paused" || !r.RunID.Valid || !r.Since.Time.Equal(now.Add(-30*24*time.Hour)) {
		t.Fatalf("C = %+v (listed %v)", r, ok)
	}
}
