package workersvc

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4162 (ADR-2445 amendment): custody admission counts UNITS, not raw holds. One run's open
// holds on one original worker are one unit, and a run whose OWN open holds reach the limit is
// refused by a hard per-run bound whatever the owner count says. These tests drive the real SQL
// seam (fn_custody_admission_count / _members, ClaimRun, GetCustodyAdmissionForRun, the owner
// aggregate, the queued park) and the service-level health reason, against a REAL Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// unitsFixture is one owner (plus a second, foreign owner for the mismatch cases).
type unitsFixture struct {
	*custodyContinuationFixture
	otherUser, otherWorker, otherRepo uuid.UUID
	base                              time.Time
}

func newUnitsFixture(t *testing.T, env codexTestEnv) *unitsFixture {
	t.Helper()
	f := &unitsFixture{custodyContinuationFixture: newCustodyContinuationFixture(t, env), base: time.Now().Add(-time.Hour)}
	f.otherUser, f.otherWorker, f.otherRepo = env.seedCodexInfra(t)
	return f
}

// seedWorkerFor inserts an offline worker of owner with a healthy (now) or long-stale heartbeat.
func (f *unitsFixture) seedWorkerFor(owner uuid.UUID, healthy bool) uuid.UUID {
	id := uuid.New()
	hb := time.Now().Add(-24 * time.Hour)
	if healthy {
		hb = time.Now()
	}
	f.env.exec(`INSERT INTO workers (id,user_id,name,token_hash,status,last_heartbeat_at) VALUES ($1,$2,'units',$3,'offline',$4)`,
		id, owner, id[:], hb)
	return id
}

func (f *unitsFixture) seedWorker(healthy bool) uuid.UUID { return f.seedWorkerFor(f.userID, healthy) }

// seedRunIn inserts a run in the given status/generation, bound to worker when it is non-nil.
func (f *unitsFixture) seedRunIn(status string, gen int64, worker uuid.UUID) uuid.UUID {
	run := f.seedRun(status, gen)
	if worker != uuid.Nil {
		f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, run, worker)
	}
	switch status {
	case "completed", "failed", "cancelled":
		f.env.exec(`UPDATE runs SET finished_at=now() WHERE id=$1`, run)
	case "recovery_wait":
		f.env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1`, run)
	}
	return run
}

// hold inserts one OPEN hold of f.userID. live == uuid.Nil leaves the live pointers NULL;
// otherwise live_worker_id = live and live_run_id = run. repo nil stores NULL. order is the
// created_at offset in seconds from a fixed base, so ordering is explicit and deterministic.
func (f *unitsFixture) hold(run uuid.UUID, gen int64, orig, live uuid.UUID, repo any, order int) uuid.UUID {
	id := uuid.New()
	var liveW, liveR any
	if live != uuid.Nil {
		liveW, liveR = live, run
	}
	f.env.exec(`INSERT INTO recovery_custody_holds
	  (id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,created_at)
	  VALUES ($1,$2,$3,$4,$5,'open',$6,'fixture',$7,$8,$9)`,
		id, f.userID, repo, run, gen, orig, liveW, liveR, f.base.Add(time.Duration(order)*time.Second))
	return id
}

// groupHold is a group-eligible hold: live pointers equal the run and its own original worker.
func (f *unitsFixture) groupHold(run uuid.UUID, gen int64, w uuid.UUID) uuid.UUID {
	return f.hold(run, gen, w, w, f.repoID, int(gen))
}

func (f *unitsFixture) cutoff(now time.Time) time.Time { return now.Add(-f.svc.p.WorkerHeartbeatStale) }

func (f *unitsFixture) admission(t *testing.T, run uuid.UUID, limit int32, now time.Time) store.GetCustodyAdmissionForRunRow {
	t.Helper()
	adm, err := f.env.q.GetCustodyAdmissionForRun(f.env.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.userID, RunID: run, CustodyHoldLimit: limit, HeartbeatCutoff: pgconv.Time(f.cutoff(now)),
	})
	if err != nil {
		t.Fatalf("GetCustodyAdmissionForRun: %v", err)
	}
	return adm
}

func (f *unitsFixture) aggregate(t *testing.T, limit int32, now time.Time) store.GetCustodyAggregateForOwnerRow {
	t.Helper()
	agg, err := f.env.q.GetCustodyAggregateForOwner(f.env.ctx, store.GetCustodyAggregateForOwnerParams{
		UserID: f.userID, CustodyHoldLimit: limit, HeartbeatCutoff: pgconv.Time(f.cutoff(now)),
	})
	if err != nil {
		t.Fatalf("GetCustodyAggregateForOwner: %v", err)
	}
	return agg
}

func (f *unitsFixture) healthReason(run uuid.UUID, limit int32, now time.Time) string {
	row := store.ListActiveRunsForHealthRow{ID: run, UserID: f.userID, Status: "queued", Kind: "issue"}
	return f.svc.queuedCustodyReason(f.env.ctx, now, row, thWithLimit(limit))
}

func (f *unitsFixture) setLimit(n int32) { f.svc.p.CustodyHoldLimit = &fakeCustodyLimit{n: n} }

type memberRow struct {
	counted bool
	reason  string
}

func (f *unitsFixture) members(t *testing.T, now time.Time) (map[uuid.UUID]memberRow, int64) {
	t.Helper()
	rows, err := f.env.pool.Query(f.env.ctx, `SELECT hold_id, counted, reason FROM fn_custody_admission_members($1,$2)`, f.userID, f.cutoff(now))
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]memberRow{}
	var sum int64
	for rows.Next() {
		var id uuid.UUID
		var m memberRow
		if err := rows.Scan(&id, &m.counted, &m.reason); err != nil {
			t.Fatal(err)
		}
		out[id] = m
		if m.counted {
			sum++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out, sum
}

func (f *unitsFixture) count(t *testing.T, now time.Time) int64 {
	t.Helper()
	var n int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT fn_custody_admission_count($1,$2)`, f.userID, f.cutoff(now)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requireUnits asserts ONE identical unit value from every reader the claim, the pill and the
// board alert use, plus per-hold reasons where the case names them, and that a claim agrees.
func (f *unitsFixture) requireUnits(t *testing.T, want int64, holds []uuid.UUID, reasons map[int]string, singletons bool) {
	t.Helper()
	probe := f.seedRun("queued", 0)
	now := time.Now()
	if got := f.count(t, now); got != want {
		t.Fatalf("fn_custody_admission_count = %d, want %d", got, want)
	}
	members, sum := f.members(t, now)
	if sum != want {
		t.Fatalf("sum(counted) over fn_custody_admission_members = %d, want %d (members=%v)", sum, want, members)
	}
	for i, id := range holds {
		m, ok := members[id]
		if !ok {
			t.Fatalf("hold %d (%s) missing from members", i, id)
		}
		if wantReason, named := reasons[i]; named && m.reason != wantReason {
			t.Fatalf("hold %d reason = %q, want %q", i, m.reason, wantReason)
		}
		if m.counted == (m.reason == "live_claim" || m.reason == "same_run_group") {
			t.Fatalf("hold %d counted=%v with reason %q: only live_claim and same_run_group are uncounted", i, m.counted, m.reason)
		}
		if singletons && m.reason == "same_run_group" {
			t.Fatalf("hold %d reported same_run_group, want every hold a singleton", i)
		}
	}
	adm := f.admission(t, probe, int32(want), now)
	if adm.AdmissionCountedHolds != want || adm.OpenHolds != int64(len(holds)) || adm.RunHoldBoundReached || adm.OwnOpenHolds != 0 {
		t.Fatalf("admission for the probe = %+v, want counted %d / open %d / no per-run bound", adm, want, len(holds))
	}
	for _, limit := range []int32{int32(want), int32(want) + 1} {
		agg := f.aggregate(t, limit, now)
		blocked := int64(0)
		wantReason := ""
		if int64(limit) <= want {
			blocked, wantReason = 1, reasonCustodyLimit
		}
		if agg.AdmissionCountedHolds != want || agg.OpenHolds != int64(len(holds)) || agg.BlockedRuns != blocked {
			t.Fatalf("limit %d: aggregate = %+v, want counted %d / open %d / blocked %d", limit, agg, want, len(holds), blocked)
		}
		if got := f.healthReason(probe, limit, now); got != wantReason {
			t.Fatalf("limit %d: health reason = %q, want %q", limit, got, wantReason)
		}
	}
	before := f.openHolds(t)
	f.setLimit(int32(want))
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("claimed %s at limit %d = units, want refused", got, want)
	}
	if n := f.openHolds(t); n != before {
		t.Fatalf("refused claim changed open holds %d -> %d", before, n)
	}
	f.setLimit(int32(want) + 1)
	if got, _ := f.claim(t); got != probe {
		t.Fatalf("claimed %s at limit %d, want the probe %s admitted one above the units", got, want+1, probe)
	}
}

// TestCustodyAdmissionUnitsParityLiveDB: every reader reports the same unit value for each shape.
func TestCustodyAdmissionUnitsParityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	live, grp, same := "live_claim", "same_run_group", "earlier_generation"
	cases := []struct {
		name       string
		want       int64
		singletons bool
		reasons    map[int]string
		build      func(f *unitsFixture) []uuid.UUID
	}{
		{name: "same worker g1..g5 healthy live g5", want: 1,
			reasons: map[int]string{0: same, 1: grp, 2: grp, 3: grp, 4: live},
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(true)
				run := f.seedRunIn("running", 5, w)
				var h []uuid.UUID
				for g := int64(1); g <= 5; g++ {
					h = append(h, f.groupHold(run, g, w))
				}
				return h
			}},
		{name: "same worker g1..g5 no healthy live claim", want: 1,
			reasons: map[int]string{0: "decision_needed", 1: grp, 2: grp, 3: grp, 4: grp},
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("failed", 5, w)
				var h []uuid.UUID
				for g := int64(1); g <= 5; g++ {
					h = append(h, f.groupHold(run, g, w))
				}
				return h
			}},
		{name: "cross worker split", want: 2,
			reasons: map[int]string{0: "decision_needed", 1: grp, 2: "decision_needed", 3: grp},
			build: func(f *unitsFixture) []uuid.UUID {
				w1, w2 := f.seedWorker(false), f.seedWorker(false)
				run := f.seedRunIn("failed", 4, w2)
				return []uuid.UUID{f.groupHold(run, 1, w1), f.groupHold(run, 2, w1), f.groupHold(run, 3, w2), f.groupHold(run, 4, w2)}
			}},
		{name: "live worker differs from original worker", want: 3, singletons: true,
			build: func(f *unitsFixture) []uuid.UUID {
				w1, w2 := f.seedWorker(false), f.seedWorker(false)
				run := f.seedRunIn("failed", 3, w2)
				var h []uuid.UUID
				for g := int64(1); g <= 3; g++ {
					h = append(h, f.hold(run, g, w1, w2, f.repoID, int(g)))
				}
				return h
			}},
		{name: "run owned by another user", want: 2, singletons: true,
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := uuid.New()
				f.env.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation,finished_at)
				  VALUES ($1,$2,$3,'issue',7,'t','d','failed',2,now())`, run, f.otherUser, f.otherRepo)
				return []uuid.UUID{f.groupHold(run, 1, w), f.groupHold(run, 2, w)}
			}},
		{name: "original worker owned by another user", want: 2, singletons: true,
			build: func(f *unitsFixture) []uuid.UUID {
				run := f.seedRunIn("failed", 2, uuid.Nil)
				return []uuid.UUID{f.groupHold(run, 1, f.otherWorker), f.groupHold(run, 2, f.otherWorker)}
			}},
		{name: "repo differs from run repo", want: 2, singletons: true,
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("failed", 2, w)
				return []uuid.UUID{f.hold(run, 1, w, w, f.otherRepo, 1), f.hold(run, 2, w, w, f.otherRepo, 2)}
			}},
		{name: "repo null", want: 2, singletons: true,
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("failed", 2, w)
				return []uuid.UUID{f.hold(run, 1, w, w, nil, 1), f.hold(run, 2, w, w, nil, 2)}
			}},
		{name: "future generations stay singletons", want: 3,
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("failed", 2, w)
				var h []uuid.UUID
				for g := int64(1); g <= 4; g++ {
					h = append(h, f.groupHold(run, g, w))
				}
				return h
			}},
		{name: "decision needed inside a group", want: 1,
			reasons: map[int]string{0: "decision_needed", 1: grp, 2: grp},
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("recovery_wait", 3, w)
				var h []uuid.UUID
				for g := int64(1); g <= 3; g++ {
					h = append(h, f.groupHold(run, g, w))
				}
				return h
			}},
		{name: "terminal run group", want: 1,
			reasons: map[int]string{0: "decision_needed", 1: grp, 2: grp},
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(false)
				run := f.seedRunIn("completed", 3, w)
				var h []uuid.UUID
				for g := int64(1); g <= 3; g++ {
					h = append(h, f.groupHold(run, g, w))
				}
				return h
			}},
		{name: "live claim hold is the oldest of its group", want: 1,
			reasons: map[int]string{0: same, 1: grp, 2: live},
			build: func(f *unitsFixture) []uuid.UUID {
				w := f.seedWorker(true)
				run := f.seedRunIn("running", 3, w)
				// g3 (the live claim) is created first; g1 then g2 follow, so the group's
				// representative is g1, the oldest of the remaining members.
				g3 := f.hold(run, 3, w, w, f.repoID, 0)
				g1 := f.hold(run, 1, w, w, f.repoID, 1)
				g2 := f.hold(run, 2, w, w, f.repoID, 2)
				return []uuid.UUID{g1, g2, g3}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUnitsFixture(t, env)
			holds := tc.build(f)
			f.requireUnits(t, tc.want, holds, tc.reasons, tc.singletons)
		})
	}
}

// TestCustodyUnitsAdmitUnrelatedRunLiveDB is the regression for the bug: one run's five
// same-worker generations plus three other holds are raw 8 holds (the limit), but only 4 units,
// so an unrelated queued run is admitted. On the per-hold count it was refused.
func TestCustodyUnitsAdmitUnrelatedRunLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newUnitsFixture(t, env)
	w := f.seedWorker(false)
	run := f.seedRunIn("failed", 5, w)
	for g := int64(1); g <= 5; g++ {
		f.groupHold(run, g, w)
	}
	f.seedUnrelatedOpenHolds(3)
	if got := f.openHolds(t); got != testCustodyLimit {
		t.Fatalf("precondition: raw open holds = %d, want %d (the limit)", got, testCustodyLimit)
	}
	if got := f.count(t, time.Now()); got != 4 {
		t.Fatalf("units = %d, want 4", got)
	}
	unrelated := f.seedRun("queued", 0)
	f.setLimit(testCustodyLimit)
	got, gen := f.claim(t)
	if got != unrelated || gen != 1 {
		t.Fatalf("claimed %s gen %d, want the unrelated run %s at gen 1", got, gen, unrelated)
	}
	if n := f.ownOpenHolds(t, unrelated); n != 1 {
		t.Fatalf("claim opened %d holds for the unrelated run, want 1", n)
	}
}

// TestCustodyRunHoldBoundLiveDB: a run whose own open holds equal the limit is refused by the hard
// per-run bound while the owner is below the limit in units; nothing it does bypasses it.
func TestCustodyRunHoldBoundLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	const limit = 4
	f := newUnitsFixture(t, env)
	f.setLimit(limit)
	w := f.seedWorker(false)
	bound := f.seedRunIn("recovery_wait", limit, w)
	for g := int64(1); g <= limit; g++ {
		f.groupHold(bound, g, w)
	}
	now := time.Now()
	if units := f.count(t, now); units != 1 {
		t.Fatalf("precondition: owner units = %d, want 1 (< limit %d)", units, limit)
	}
	// Owner Resume (the existing exhaustion release) requeues the run but cannot waive the bound.
	if _, err := env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: bound, UserID: f.userID, GlobalTimeoutSeconds: 86400}); err != nil {
		t.Fatalf("owner Resume: %v", err)
	}
	if s := runStatus(t, f.custodyContinuationFixture, bound); s != "queued" {
		t.Fatalf("status after Resume = %s, want queued", s)
	}
	holdsBefore := f.openHolds(t)
	adm := f.admission(t, bound, limit, now)
	if !adm.RunHoldBoundReached || adm.ContinuationExempt || adm.OwnOpenHolds != limit || adm.AdmissionCountedHolds != 1 {
		t.Fatalf("admission = %+v, want run bound reached, not exempt, own %d, units 1", adm, limit)
	}
	if agg := f.aggregate(t, limit, now); agg.BlockedRuns != 1 || agg.AdmissionCountedHolds != 1 {
		t.Fatalf("aggregate = %+v, want blocked_runs 1 with units 1 below the limit", agg)
	}
	if got := f.healthReason(bound, limit, now); got != apitypes.ReasonCustodyRunHoldBound {
		t.Fatalf("health reason = %q, want %q", got, apitypes.ReasonCustodyRunHoldBound)
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("claimed %s, want the bound run refused after Resume", got)
	}
	if n := f.openHolds(t); n != holdsBefore {
		t.Fatalf("refused claim changed open holds %d -> %d", holdsBefore, n)
	}
	if s := runStatus(t, f.custodyContinuationFixture, bound); s != "queued" {
		t.Fatalf("refused run status = %s, want queued", s)
	}
	// An unrelated queued run is still admitted (owner units are below the limit).
	unrelated := f.seedRun("queued", 0)
	if got, _ := f.claim(t); got != unrelated {
		t.Fatalf("claimed %s, want the unrelated run %s", got, unrelated)
	}
	if n := f.ownOpenHolds(t, bound); n != limit {
		t.Fatalf("bound run own open holds = %d, want %d unchanged", n, limit)
	}
}

// holdRowSnapshot is every column of the named run's holds, so any write to a hold row shows.
func (f *unitsFixture) holdRowSnapshot(t *testing.T, run uuid.UUID) string {
	t.Helper()
	var s string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(h) ORDER BY h.id),'[]'::jsonb)::text
	  FROM recovery_custody_holds h WHERE h.run_id=$1`, run).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCustodyUnitsSafeguardsLiveDB: grouping changes admission only. A worker whose only open
// holds are grouped holds stays retained, undeletable, decision-visible, custody-held and
// maintenance-held; reads and a claim leave the hold rows untouched; discard settles exactly the
// named hold. The variant discards the group's representative, leaving only former non-members.
func TestCustodyUnitsSafeguardsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, discardRep := range []bool{false, true} {
		t.Run(fmt.Sprintf("discard_representative_%v", discardRep), func(t *testing.T) {
			f := newUnitsFixture(t, env)
			f.setLimit(testCustodyLimit)
			w := f.seedWorker(false)
			env.exec(`UPDATE workers SET kind='hosted', hosted_size='s', template_declared='base' WHERE id=$1`, w)
			// A failed, unbound run: terminal custody with no capture is decision-needed, and the
			// worker holds no non-terminal run, so only the custody guard can refuse its deletion.
			run := f.seedRunIn("failed", 3, uuid.Nil)
			holds := []uuid.UUID{f.groupHold(run, 1, w), f.groupHold(run, 2, w), f.groupHold(run, 3, w)}
			rs := recovery.New(env.q, env.pool, env.box, recovery.Limits{}, time.Now)
			discard := func(id uuid.UUID) int64 {
				n, err := env.q.DiscardCustodyHoldForOwner(env.ctx, store.DiscardCustodyHoldForOwnerParams{
					ReleaseEvidence: pgtype.Text{String: "owner_discard", Valid: true}, HoldID: id, RunID: run, UserID: f.userID})
				if err != nil {
					t.Fatalf("DiscardCustodyHoldForOwner: %v", err)
				}
				return n
			}
			open := int64(len(holds))
			if discardRep {
				// The representative (oldest) goes: only former non-representatives remain.
				if n := discard(holds[0]); n != 1 {
					t.Fatalf("discard representative settled %d rows, want 1", n)
				}
				open = 2
				if s := unitsHoldState(t, env, holds[0]); s != "discarded" {
					t.Fatalf("representative state = %s, want discarded", s)
				}
				for _, id := range holds[1:] {
					if s := unitsHoldState(t, env, id); s != "open" {
						t.Fatalf("sibling %s state = %s, want open", id, s)
					}
				}
				if units := f.count(t, time.Now()); units != 1 {
					t.Fatalf("units after discarding the representative = %d, want 1 (next oldest represents)", units)
				}
			}
			// Release decisions: reads and a claim never touch the hold rows.
			snap := f.holdRowSnapshot(t, run)
			wkr, err := env.q.GetWorkerByID(env.ctx, w)
			if err != nil {
				t.Fatalf("GetWorkerByID: %v", err)
			}
			retained, err := f.svc.RetainingUnpublishedWork(env.ctx, wkr)
			if err != nil || !retained {
				t.Fatalf("RetainingUnpublishedWork = %v, %v, want true", retained, err)
			}
			err = f.svc.DeleteWorker(env.ctx, f.userID, w)
			var held *WorkerHasCustodyError
			if !asCustodyErr(err, &held) || held.Holds != open {
				t.Fatalf("DeleteWorker err = %v, want WorkerHasCustodyError with %d holds", err, open)
			}
			if _, gone := workerGone(t, env, w); gone {
				t.Fatalf("DeleteWorker removed a custody-holding worker")
			}
			decisions, err := rs.CustodyDecisionsByWorker(env.ctx, []uuid.UUID{w})
			if err != nil || decisions[w] != int(open) {
				t.Fatalf("CustodyDecisionsByWorker = %v, %v, want %d decisions for the worker", decisions, err, open)
			}
			desired, err := env.q.ListHostedWorkersForController(env.ctx, store.ListHostedWorkersForControllerParams{HeartbeatCutoff: pgconv.Time(time.Now().Add(-45 * time.Second))})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range desired {
				if d.ID == w {
					found = true
					if !d.CustodyHeld {
						t.Fatalf("ListHostedWorkersForController custody_held = false for a worker with %d grouped open holds", open)
					}
				}
			}
			if !found {
				t.Fatal("hosted worker missing from the controller listing")
			}
			if heldMaint, err := env.q.DindMaintenanceCustodyHeld(env.ctx, pgconv.UUID(w)); err != nil || !heldMaint {
				t.Fatalf("DindMaintenanceCustodyHeld = %v, %v, want true", heldMaint, err)
			}
			now := time.Now()
			f.admission(t, run, testCustodyLimit, now)
			f.aggregate(t, testCustodyLimit, now)
			f.members(t, now)
			_ = f.healthReason(run, testCustodyLimit, now)
			unrelated := f.seedRun("queued", 0)
			if got, _ := f.claim(t); got != unrelated {
				t.Fatalf("claimed %s, want the unrelated run %s", got, unrelated)
			}
			if after := f.holdRowSnapshot(t, run); after != snap {
				t.Fatalf("hold rows changed by admission reads and a claim:\nbefore=%s\nafter=%s", snap, after)
			}
			// Discard settles exactly the one named hold; a repeat settles none.
			target := holds[len(holds)-1]
			if n := discard(target); n != 1 {
				t.Fatalf("discard settled %d rows, want exactly 1", n)
			}
			if n := discard(target); n != 0 {
				t.Fatalf("repeat discard settled %d rows, want 0", n)
			}
			var openAfter int64
			if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open'`, run).Scan(&openAfter); err != nil {
				t.Fatal(err)
			}
			if openAfter != open-1 {
				t.Fatalf("open holds after one discard = %d, want %d", openAfter, open-1)
			}
		})
	}
}

func workerGone(t *testing.T, env codexTestEnv, id uuid.UUID) (int, bool) {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM workers WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n, n == 0
}

func asCustodyErr(err error, target **WorkerHasCustodyError) bool { return errors.As(err, target) }

func unitsHoldState(t *testing.T, env codexTestEnv, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := env.pool.QueryRow(env.ctx, `SELECT state FROM recovery_custody_holds WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestCustodyUnitsQueuedParkBelowOwnerLimitLiveDB: the queued custody park no longer needs the
// owner count at the limit. A grouped, exhausted run whose in-range own holds reach the limit
// parks while the owner is below the limit in units; every control stays queued and holdless.
func TestCustodyUnitsQueuedParkBelowOwnerLimitLiveDB(t *testing.T) {
	const limit = 3
	env := setupCodexLiveDB(t)
	// seed builds an exhausted queued run (requeue budget 3 spent) owned by the claimant worker
	// with three grouped holds on that worker, so owner units are 1, below the limit.
	seed := func(t *testing.T, gen int64, hold func(c *claimantExhaustion, run uuid.UUID)) (*claimantExhaustion, uuid.UUID) {
		c := newClaimantExhaustion(t, env, true, 3)
		queuedCustodyCache(t, env, c.svc, limit)
		run := c.seedRun("queued", gen)
		env.exec(`UPDATE runs SET worker_id=$2, claimed_at=now(), requeue_count=3 WHERE id=$1`, run, c.workerID)
		hold(c, run)
		return c, run
	}
	grouped := func(c *claimantExhaustion, run uuid.UUID) {
		f := &unitsFixture{custodyContinuationFixture: c.custodyContinuationFixture, base: time.Now().Add(-time.Hour)}
		for g := int64(1); g <= 3; g++ {
			f.groupHold(run, g, c.workerID)
		}
	}
	requireBelowOwnerLimit := func(t *testing.T, c *claimantExhaustion) {
		t.Helper()
		var n int64
		if err := env.pool.QueryRow(env.ctx, `SELECT fn_custody_admission_count($1,$2)`, c.userID, time.Now().Add(-45*time.Second)).Scan(&n); err != nil || n >= limit {
			t.Fatalf("precondition: owner units = %d (%v), want below the limit %d", n, err, limit)
		}
	}
	runParkCase := func(t *testing.T, c *claimantExhaustion, run uuid.UUID, wantParked int, wantStatus string) {
		t.Helper()
		before := observeQueuedCustody(t, env, run)
		if got := runQueuedCustodyFrozen(t, c); got != wantParked {
			t.Fatalf("parked %d, want %d", got, wantParked)
		}
		after := observeQueuedCustody(t, env, run)
		if string(after.run["status"]) != fmt.Sprintf("%q", wantStatus) {
			t.Fatalf("status = %s, want %s", after.run["status"], wantStatus)
		}
		requireQueuedCustodyPreserved(t, before, after, wantParked == 1)
		if wantParked == 0 && c.openHolds(t) != 3 {
			t.Fatalf("control opened or released a hold: open=%d", c.openHolds(t))
		}
	}

	t.Run("exhausted grouped run parks below the owner limit", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		requireBelowOwnerLimit(t, c)
		runParkCase(t, c, run, 1, "recovery_wait")
		var cause string
		if err := env.pool.QueryRow(env.ctx, `SELECT recovery_wait_cause FROM runs WHERE id=$1`, run).Scan(&cause); err != nil || cause != "worker_requeue_exhausted" {
			t.Fatalf("cause = %q (%v), want worker_requeue_exhausted", cause, err)
		}
		// The owner Resume renews the episode: nothing re-parks it until the budget is spent again.
		if _, err := env.q.ResumeWorkerRecoveryEpisode(env.ctx, store.ResumeWorkerRecoveryEpisodeParams{ID: run, UserID: c.userID, GlobalTimeoutSeconds: 86400}); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		runParkCase(t, c, run, 0, "queued")
	})
	t.Run("parks through the sweep path", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		requireBelowOwnerLimit(t, c)
		queuedCustodyTrigger(t, c, "sweep")
		var status string
		if err := env.pool.QueryRow(env.ctx, `SELECT status FROM runs WHERE id=$1`, run).Scan(&status); err != nil || status != "recovery_wait" {
			t.Fatalf("status after sweep = %q (%v), want recovery_wait", status, err)
		}
	})
	t.Run("below requeue budget stays queued", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		env.exec(`UPDATE runs SET requeue_count=2 WHERE id=$1`, run)
		runParkCase(t, c, run, 0, "queued")
	})
	t.Run("unassigned run stays queued", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		env.exec(`UPDATE runs SET worker_id=NULL WHERE id=$1`, run)
		runParkCase(t, c, run, 0, "queued")
	})
	t.Run("pending pause stays queued", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		env.exec(`UPDATE runs SET pause_requested_at=now(), pause_mode='now' WHERE id=$1`, run)
		runParkCase(t, c, run, 0, "queued")
	})
	t.Run("fresh terminal-pending outcome stays queued", func(t *testing.T) {
		c, run := seed(t, 3, grouped)
		snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: exhaustionNonce, Active: []ActiveRunEntry{entry(run, 3, "running", true)}}
		if ok, err := applySnapshot(t, c.svc, c.env, c.workerID, snap, snapshotModeHeartbeat); err != nil || !ok {
			t.Fatalf("snapshot = %v %v", ok, err)
		}
		runParkCase(t, c, run, 0, "queued")
	})
	t.Run("run bound only by out-of-range generations is not parked", func(t *testing.T) {
		// claim_generation 1 with g1 in range and g2, g3 beyond it: own open holds = limit (the
		// ClaimRun per-run bound refuses it) but only one is in range, so the park refuses.
		c, run := seed(t, 1, func(c *claimantExhaustion, run uuid.UUID) {
			f := &unitsFixture{custodyContinuationFixture: c.custodyContinuationFixture, base: time.Now().Add(-time.Hour)}
			for g := int64(1); g <= 3; g++ {
				f.groupHold(run, g, c.workerID)
			}
		})
		runParkCase(t, c, run, 0, "queued")
		f := &unitsFixture{custodyContinuationFixture: c.custodyContinuationFixture}
		now := time.Now()
		if got := f.healthReason(run, limit, now); got != apitypes.ReasonCustodyRunHoldBound {
			t.Fatalf("health reason = %q, want %q", got, apitypes.ReasonCustodyRunHoldBound)
		}
		if got, _ := c.claim(t); got == run {
			t.Fatalf("claimed the per-run bound run %s", run)
		}
	})
}
