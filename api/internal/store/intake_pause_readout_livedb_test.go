package store_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestMaintenanceReadoutClear(t *testing.T) {
	if !(store.MaintenanceReadout{}).Clear() {
		t.Fatal("empty readout must be clear")
	}
	for name, m := range map[string]store.MaintenanceReadout{
		"active":  {Active: store.MaintenanceSection{Count: 1}},
		"held":    {Held: store.MaintenanceSection{Count: 1}},
		"custody": {Custody: store.MaintenanceSection{Count: 1}},
	} {
		if m.Clear() {
			t.Fatalf("%s > 0 must not be clear", name)
		}
	}
	info := store.MaintenanceReadout{Queued: store.MaintenanceSection{Count: 9}, IdleBoundWorkers: store.MaintenanceWorkers{Count: 3}}
	if !info.Clear() {
		t.Fatal("queued work and idle bound workers are informational and must not block clear")
	}
}

// readoutFixture seeds runs under a uniquely named owner and a far-past status_since so the
// seeded rows sort first (lists are oldest first) regardless of rows other tests left behind.
type readoutFixture struct {
	*fleetFixture
	label  string
	worker uuid.UUID
	n      int
}

func newReadoutFixture(t *testing.T) *readoutFixture {
	fx := newFleetFixture(t)
	t.Cleanup(func() {
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM recovery_captures WHERE user_id=$1`, fx.userID)
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM recovery_custody_holds WHERE user_id=$1`, fx.userID)
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM workers WHERE user_id=$1`, fx.userID)
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id=$1`, fx.userID)
	})
	label := "readout-" + fx.userID.String()
	mustExec(fx.ctx, t, fx.pool, `UPDATE users SET display_name=$2 WHERE id=$1`, fx.userID, label)
	return &readoutFixture{fleetFixture: fx, label: label, worker: fx.worker("w", capOf(4), false)}
}

type seedRun struct {
	status, cause, kind string
	gen                 int
	autoApprove         bool
	target              uuid.UUID // lead run of a cross_check child
}

func (f *readoutFixture) run(s seedRun) uuid.UUID {
	f.t.Helper()
	f.n++
	insertKind := s.kind
	if insertKind == "" || insertKind == "cross_check" || insertKind == "chat" {
		insertKind = "issue" // reshaped below: the kind-shape CHECK needs per-kind columns
	}
	var cause any
	if s.cause != "" {
		cause = s.cause
	}
	var worker any
	if s.status == "claimed" || s.status == "running" {
		worker = f.worker
	}
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO runs
		(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation,
		 auto_approve,recovery_wait_cause,status_since)
		VALUES ($1,$2,$3,$4,$5,'t','d',$6,$7,$8,$9,$10, now() - interval '1 year' + $11 * interval '1 second')`,
		id, f.userID, f.repoID, insertKind, f.nextIID(), s.status, worker, s.gen, s.autoApprove, cause, f.n)
	switch s.kind {
	case "cross_check":
		mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET kind='cross_check',issue_iid=NULL,target_run_id=$2,harness='claude',
			report_only=true,budget_wall_seconds=1800 WHERE id=$1`, id, s.target)
	case "chat":
		mustExec(f.ctx, f.t, f.pool, `UPDATE runs SET kind='chat',repo_id=NULL,issue_iid=NULL WHERE id=$1`, id)
	}
	return id
}

func (f *readoutFixture) hold(run uuid.UUID, gen int, guarded bool, ageSeconds int) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded,created_at)
		VALUES ($1,$2,$3,$4,$5,'open',$6,'original',$6,$4,$7, now() - interval '1 year' + $8 * interval '1 second')`,
		id, f.userID, f.repoID, run, gen, f.worker, guarded, ageSeconds)
	return id
}

func (f *readoutFixture) capture(hold, run uuid.UUID, state string) {
	f.t.Helper()
	mustExec(f.ctx, f.t, f.pool, `INSERT INTO recovery_captures
		(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
		VALUES ($1,$2,$3,$4,'original','abc',$5,$6)`, uuid.New(), hold, run, f.userID, uuid.New().String(), state)
}

// mine returns this fixture's items keyed by run id.
func (f *readoutFixture) mine(items []store.MaintenanceItem) map[uuid.UUID]store.MaintenanceItem {
	out := map[uuid.UUID]store.MaintenanceItem{}
	for _, it := range items {
		if it.OwnerLabel == f.label {
			out[it.RunID] = it
		}
	}
	return out
}

func tagOf(it store.MaintenanceItem) string {
	if it.Tag == nil {
		return "<nil>"
	}
	return *it.Tag
}

// Every non-terminal status lands in exactly one of active/held/queued; custody is its own
// deduplicated view of open holds (terminal runs included) tagged by decision_needed only.
func TestMaintenanceReadoutPartitionLiveDB(t *testing.T) {
	f := newReadoutFixture(t)
	base, err := store.ReadMaintenanceReadout(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}

	type want struct {
		section, tag string
	}
	expect := map[uuid.UUID]want{}
	add := func(s seedRun, section, tag string) uuid.UUID {
		id := f.run(s)
		expect[id] = want{section, tag}
		return id
	}
	add(seedRun{status: "claimed", gen: 1}, "active", "")
	running := add(seedRun{status: "running", gen: 1}, "active", "")
	add(seedRun{status: "queued"}, "queued", "")
	add(seedRun{status: "queued", gen: 1}, "held", "progressing")
	add(seedRun{status: "queued", kind: "cross_check", target: running}, "held", "progressing")
	chat := add(seedRun{status: "queued", kind: "chat"}, "queued", "")
	add(seedRun{status: "awaiting_approval"}, "held", "needs_owner")
	add(seedRun{status: "awaiting_approval", autoApprove: true}, "held", "progressing")
	add(seedRun{status: "awaiting_input"}, "held", "needs_owner")
	add(seedRun{status: "awaiting_followup"}, "held", "progressing")
	add(seedRun{status: "paused"}, "held", "needs_owner")
	add(seedRun{status: "limit_wait"}, "held", "progressing")
	add(seedRun{status: "pool_wait"}, "held", "progressing")
	add(seedRun{status: "recovery_wait", cause: "worker_requeue_exhausted"}, "held", "needs_owner")
	add(seedRun{status: "recovery_wait", cause: "worker_memory_pressure"}, "held", "needs_owner")
	add(seedRun{status: "recovery_wait", cause: "forge_unreachable"}, "held", "progressing")
	add(seedRun{status: "recovery_wait"}, "held", "progressing")
	for _, terminal := range []string{"completed", "failed", "cancelled"} {
		f.run(seedRun{status: terminal, gen: 1}) // in no section
	}

	// Custody: a terminal run whose only open hold needs a decision (source_only), a running
	// run (attention active, no decision), and a run with two open holds, the older one
	// archive_ready and the newer one needs_action, which must dedup to one item from the
	// older hold tagged needs_owner (bool_or across the run's open holds).
	termNeeds := f.run(seedRun{status: "completed", gen: 1})
	termHold := f.hold(termNeeds, 1, false, 10)
	runningHold := f.hold(running, 1, false, 20)
	dup := f.run(seedRun{status: "failed", gen: 2})
	dupOld := f.hold(dup, 1, false, 30)
	f.capture(dupOld, dup, "available")
	dupNew := f.hold(dup, 2, false, 40)
	f.capture(dupNew, dup, "needs_action")
	// A released hold is not custody.
	released := f.run(seedRun{status: "completed", gen: 1})
	releasedHold := f.hold(released, 1, false, 50)
	mustExec(f.ctx, t, f.pool, `UPDATE recovery_custody_holds SET state='released', released_at=now(), live_worker_id=NULL, live_run_id=NULL WHERE id=$1`, releasedHold)

	// Ephemeral workers bound to a not-admitted queued run are idle-bound; one bound to an
	// admitted queued run is not.
	notAdmitted := f.run(seedRun{status: "queued"})
	expect[notAdmitted] = want{"queued", ""}
	admitted := f.run(seedRun{status: "queued", gen: 1})
	expect[admitted] = want{"held", "progressing"}
	idleWorker := seedEphemeralWorkerBound(f.fleetFixture, notAdmitted)
	seedEphemeralWorkerBound(f.fleetFixture, admitted)

	got, err := store.ReadMaintenanceReadout(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if got.AsOf.IsZero() {
		t.Fatal("as_of unset")
	}

	// Partition: each seeded non-terminal run appears in exactly its section, nowhere else.
	sections := map[string][]store.MaintenanceItem{"active": got.Active.Items, "held": got.Held.Items, "queued": got.Queued.Items}
	seen := map[uuid.UUID]string{}
	for name, items := range sections {
		for id, it := range f.mine(items) {
			if prev, dup := seen[id]; dup {
				t.Fatalf("run %s (%s) in both %s and %s", id, it.Status, prev, name)
			}
			seen[id] = name
			w, ok := expect[id]
			if !ok {
				t.Fatalf("unexpected run %s (%s) in %s", id, it.Status, name)
			}
			wantTag := w.tag
			if wantTag == "" {
				wantTag = "<nil>"
			}
			if w.section != name || tagOf(it) != wantTag {
				t.Fatalf("run %s (%s) in %s tag=%s, want %s tag=%q", id, it.Status, name, tagOf(it), w.section, w.tag)
			}
			if (id == chat) != (it.Repo == nil) || (it.Repo != nil && *it.Repo != "g/r") || it.Since == nil || it.HoldID != nil {
				t.Fatalf("item shape: %+v", it)
			}
		}
	}
	for id, w := range expect {
		if seen[id] != w.section {
			t.Fatalf("run %s expected in %s, found in %q", id, w.section, seen[id])
		}
	}
	count := func(section string) (n int64) {
		for _, w := range expect {
			if w.section == section {
				n++
			}
		}
		return n
	}
	if got.Active.Count != base.Active.Count+count("active") || got.Held.Count != base.Held.Count+count("held") ||
		got.Queued.Count != base.Queued.Count+count("queued") {
		t.Fatalf("counts active/held/queued = %d/%d/%d, base %d/%d/%d, seeded %d/%d/%d",
			got.Active.Count, got.Held.Count, got.Queued.Count, base.Active.Count, base.Held.Count, base.Queued.Count,
			count("active"), count("held"), count("queued"))
	}

	// Custody: three runs (the released hold's run and non-custody runs are absent).
	cust := f.mine(got.Custody.Items)
	if len(cust) != 3 || got.Custody.Count != base.Custody.Count+3 {
		t.Fatalf("custody items=%d count=%d base=%d: %+v", len(cust), got.Custody.Count, base.Custody.Count, cust)
	}
	for _, c := range []struct {
		run         uuid.UUID
		hold        uuid.UUID
		tag, status string
	}{
		{termNeeds, termHold, "needs_owner", "completed"},
		{running, runningHold, "progressing", "running"},
		{dup, dupOld, "needs_owner", "failed"},
	} {
		it, ok := cust[c.run]
		if !ok || tagOf(it) != c.tag || it.Status != c.status || it.HoldID == nil || *it.HoldID != c.hold {
			t.Fatalf("custody run %s: %+v, want tag %s status %s hold %s", c.run, it, c.tag, c.status, c.hold)
		}
	}
	if _, ok := cust[released]; ok {
		t.Fatal("released hold must not be custody")
	}
	var order []uuid.UUID
	for _, it := range got.Custody.Items {
		if it.OwnerLabel == f.label {
			order = append(order, it.RunID)
		}
	}
	if fmt.Sprint(order) != fmt.Sprint([]uuid.UUID{termNeeds, running, dup}) {
		t.Fatalf("custody must be oldest hold first: %v", order)
	}

	// Idle bound workers: only the one bound to the not-admitted queued run.
	var idle []store.MaintenanceWorker
	for _, w := range got.IdleBoundWorkers.Items {
		if w.OwnerLabel == f.label {
			idle = append(idle, w)
		}
	}
	if len(idle) != 1 || idle[0].WorkerID != idleWorker || idle[0].RunID != notAdmitted || idle[0].Since == nil ||
		got.IdleBoundWorkers.Count != base.IdleBoundWorkers.Count+1 {
		t.Fatalf("idle bound workers: %+v count=%d base=%d", idle, got.IdleBoundWorkers.Count, base.IdleBoundWorkers.Count)
	}
	if got.Clear() {
		t.Fatal("readout with active, held and custody work must not be clear")
	}
}

// Counts are exact beyond the 50-row list cap, and lists are oldest first.
func TestMaintenanceReadoutCapLiveDB(t *testing.T) {
	f := newReadoutFixture(t)
	base, err := store.ReadMaintenanceReadout(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 55; i++ {
		f.run(seedRun{status: "queued"})
	}
	got, err := store.ReadMaintenanceReadout(f.ctx, f.pool)
	if err != nil {
		t.Fatal(err)
	}
	if got.Queued.Count != base.Queued.Count+55 || len(got.Queued.Items) != 50 {
		t.Fatalf("queued count=%d (base %d) items=%d, want exact +55 and 50 items", got.Queued.Count, base.Queued.Count, len(got.Queued.Items))
	}
	if !sort.SliceIsSorted(got.Queued.Items, func(i, j int) bool { return got.Queued.Items[i].Since.Before(*got.Queued.Items[j].Since) }) {
		t.Fatal("queued list not oldest first")
	}
	if got.Active.Items == nil || got.Held.Items == nil || got.Custody.Items == nil || got.IdleBoundWorkers.Items == nil {
		t.Fatal("empty sections must be non-nil slices")
	}
}

// The readout's active query must be able to use idx_runs_nonterminal_since: its status IN
// list implies the index's NOT IN predicate. With seqscan, bitmap scans and explicit sorts
// all disabled, the only cheap plan for the status_since ordering is the ordered scan of that
// index (idx_runs_claimable cannot supply the order), so this proves the index can serve the
// ordered read. It does not prove the default planner picks it: on a populated database with
// few claimed/running rows the planner may legitimately prefer idx_runs_claimable plus a sort.
// The query text is a copy of the ListMaintenanceActiveRuns filter and order (the sqlc
// constant is unexported from this external test package).
func TestMaintenanceActiveQueryUsesNonterminalIndexLiveDB(t *testing.T) {
	f := newReadoutFixture(t)
	f.run(seedRun{status: "running"})
	mustExec(f.ctx, t, f.pool, `ANALYZE runs`)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	for _, s := range []string{
		`SET LOCAL enable_seqscan = off`,
		`SET LOCAL enable_bitmapscan = off`,
		`SET LOCAL enable_sort = off`,
	} {
		if _, err := tx.Exec(f.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.Query(f.ctx, `EXPLAIN SELECT r.id FROM runs r WHERE r.status IN ('claimed', 'running') ORDER BY r.status_since, r.id LIMIT 50`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	if !strings.Contains(plan.String(), "idx_runs_nonterminal_since") {
		t.Fatalf("plan did not use idx_runs_nonterminal_since:\n%s", plan.String())
	}
}
