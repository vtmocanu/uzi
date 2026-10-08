package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type admissionFixture struct {
	*custodyEpisodeSeeder
	q                                            *store.Queries
	owner, other, repo, claimant, worker, queued uuid.UUID
	runs, holds                                  []uuid.UUID
	cutoff                                       pgtype.Timestamptz
}

func newAdmissionFixture(t *testing.T, n int) *admissionFixture {
	t.Helper()
	ctx, pool, q := openCustodyEpisodeLiveDB(t)
	s := &custodyEpisodeSeeder{ctx: ctx, t: t, pool: pool}
	f := &admissionFixture{custodyEpisodeSeeder: s, q: q, owner: s.user(), other: s.user(),
		repo: uuid.New(), claimant: uuid.New(), worker: uuid.New(),
		cutoff: pgtype.Timestamptz{Time: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), Valid: true}}
	conn := uuid.New()
	s.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, f.owner, []byte{1})
	s.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,1,'g/custody','https://forge.e2e/g/custody','main',true)`, f.repo, conn)
	for _, w := range []uuid.UUID{f.worker, f.claimant} {
		s.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'offline',$5)`, w, f.owner, w.String(), w[:], f.cutoff)
	}
	for i := 0; i < n; i++ {
		r := f.run("running", f.worker, 1)
		f.runs = append(f.runs, r)
		f.holds = append(f.holds, f.hold(r, 1))
	}
	f.queued = f.run("queued", uuid.Nil, 0)
	if _, err := q.ClaimCustodyEpisodeNotice(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Plain custody owner columns require explicit cleanup before cascading the user.
		s.exec(`DELETE FROM recovery_captures WHERE user_id=ANY($1)`, []uuid.UUID{f.owner, f.other})
		s.exec(`DELETE FROM recovery_custody_holds WHERE user_id=ANY($1)`, []uuid.UUID{f.owner, f.other})
		s.exec(`DELETE FROM users WHERE id=ANY($1)`, []uuid.UUID{f.owner, f.other})
	})
	return f
}

func (f *admissionFixture) run(status string, worker uuid.UUID, generation int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	var w any
	if worker != uuid.Nil {
		w = worker
	}
	f.n++
	f.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,worker_id,claim_generation)
 VALUES($1,$2,$3,'issue',$4,'custody','test',$5,$6,$7)`, id, f.owner, f.repo, f.n, status, w, generation)
	return id
}

func (f *admissionFixture) hold(run uuid.UUID, generation int64) uuid.UUID {
	return f.holdWithInventory(run, generation, false)
}

func (f *admissionFixture) holdWithInventory(run uuid.UUID, generation int64, guarded bool) uuid.UUID {
	id := uuid.New()
	// Deliberately different original provenance and an offline worker with fresh heartbeat.
	f.exec(`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,
 original_worker_identity,live_worker_id,live_run_id,inventory_guarded) VALUES($1,$2,$3,$4,'open',$5,'original',$6,$3,$7)`,
		id, f.owner, run, generation, uuid.New(), f.worker, guarded)
	return id
}

func (f *admissionFixture) capture(hold, run, owner uuid.UUID, state string) {
	f.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
 VALUES($1,$2,$3,$4,'original','abc',$5,$6)`, uuid.New(), hold, run, owner, uuid.New().String(), state)
}

// mirrors executes all five production queries with one caller-supplied cutoff.
// ClaimRun is rolled back so every assertion sees the same fixture.
func (f *admissionFixture) mirrors(cutoff pgtype.Timestamptz, limit int32, total, counted int64, continuation bool) {
	f.t.Helper()
	a, err := f.q.GetCustodyAggregateForOwner(f.ctx, store.GetCustodyAggregateForOwnerParams{
		UserID: f.owner, HeartbeatCutoff: cutoff, CustodyHoldLimit: limit})
	if err != nil {
		f.t.Fatal(err)
	}
	blocked := limit > 0 && counted >= int64(limit) && !continuation
	wantBlocked := int64(0)
	if blocked {
		wantBlocked = 1
	}
	if a.OpenHolds != total || a.AdmissionCountedHolds != counted || a.BlockedRuns != wantBlocked {
		f.t.Fatalf("aggregate=%+v want total=%d counted=%d blocked=%d", a, total, counted, wantBlocked)
	}
	r, err := f.q.GetCustodyAdmissionForRun(f.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.owner, RunID: f.queued, HeartbeatCutoff: cutoff, CustodyHoldLimit: limit})
	if err != nil {
		f.t.Fatal(err)
	}
	if r.OpenHolds != total || r.AdmissionCountedHolds != counted || r.ContinuationExempt != continuation {
		f.t.Fatalf("run mirror=%+v want total=%d counted=%d continuation=%v", r, total, counted, continuation)
	}
	over, err := f.q.ListOwnersOverCustodyLimit(f.ctx, store.ListOwnersOverCustodyLimitParams{HeartbeatCutoff: cutoff, CustodyHoldLimit: limit})
	if err != nil {
		f.t.Fatal(err)
	}
	cleared, err := f.q.ListOwnersWithClearedCustodyEpisode(f.ctx, store.ListOwnersWithClearedCustodyEpisodeParams{HeartbeatCutoff: cutoff, CustodyHoldLimit: limit})
	if err != nil {
		f.t.Fatal(err)
	}
	contains := func(ids []uuid.UUID) bool {
		for _, id := range ids {
			if id == f.owner {
				return true
			}
		}
		return false
	}
	// Finder semantics at disabled limits stay unchanged: compare directly, without disabling.
	if contains(over) != (total > 0 && counted >= int64(limit)) || contains(cleared) != (counted < int64(limit)) {
		f.t.Fatalf("owner finders over=%v cleared=%v count=%d limit=%d", over, cleared, counted, limit)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(f.ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			f.t.Error(err)
		}
	}()
	claimed, err := f.q.WithTx(tx).ClaimRun(f.ctx, store.ClaimRunParams{
		UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.claimant, Valid: true}, HeartbeatCutoff: cutoff,
		AffinityCutoff:        pgtype.Timestamptz{Time: time.Now(), Valid: true},
		SpreadCutoff:          pgtype.Timestamptz{Time: time.Now(), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		CapabilityAware:       true, WorkerCaps: []string{}, WorkerProtocolCaps: []string{"recovery_archive_v1"},
		CustodyHoldLimit: limit, RecoveryCapable: true, WorkerIdentity: f.claimant.String(),
	})
	if blocked {
		if !errors.Is(err, pgx.ErrNoRows) {
			f.t.Fatalf("blocked fresh claim: run=%s err=%v", claimed.ID, err)
		}
	} else if err != nil || claimed.ID != f.queued {
		f.t.Fatalf("admitted claim: run=%s err=%v want=%s", claimed.ID, err, f.queued)
	}
}

func TestCustodyAdmissionMirrorsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name         string
		n            int
		action       func(*admissionFixture)
		total, count int64
	}{
		{"eight healthy", 8, nil, 8, 0},
		{"eight decisions", 8, func(f *admissionFixture) {
			for i, h := range f.holds {
				f.capture(h, f.runs[i], f.owner, "needs_action")
			}
		}, 8, 8},
		{"duplicate qualifying one excluded", 1, func(f *admissionFixture) { f.hold(f.runs[0], 1) }, 2, 1},
		{"earlier decision sibling", 1, func(f *admissionFixture) {
			f.capture(f.holds[0], f.runs[0], f.owner, "needs_action")
			f.hold(f.runs[0], 1)
		}, 2, 1},
		{"foreign capture ignored", 1, func(f *admissionFixture) { f.capture(f.holds[0], f.runs[0], f.other, "needs_action") }, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t, tc.n)
			if tc.action != nil {
				tc.action(f)
			}
			limit := int32(1)
			if tc.n == 8 {
				limit = 8
			}
			f.mirrors(f.cutoff, limit, tc.total, tc.count, false)
		})
	}
	for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup",
		"queued", "paused", "recovery_wait", "completed", "failed", "cancelled"} {
		t.Run("status "+status, func(t *testing.T) {
			f := newAdmissionFixture(t, 1)
			f.exec(`UPDATE runs SET status=$2 WHERE id=$1`, f.runs[0], status)
			// A second queued publishing run would also appear in blocked_runs.
			if status == "queued" {
				f.exec(`UPDATE runs SET kind='chat', repo_id=NULL, issue_iid=NULL, branch=NULL WHERE id=$1`, f.runs[0])
			}
			count := int64(1)
			switch status {
			case "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup":
				count = 0
			}
			f.mirrors(f.cutoff, 1, 1, count, false)
		})
	}
	for _, tc := range []struct {
		name, sql    string
		value        any
		total, count int64
	}{
		{"null heartbeat", "UPDATE workers SET last_heartbeat_at=NULL WHERE id=$1", nil, 1, 1},
		{"before cutoff", "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", time.Date(2026, 10, 7, 11, 59, 59, 999999000, time.UTC), 1, 1},
		{"exact cutoff", "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), 1, 0},
		{"after cutoff", "UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1", time.Date(2026, 10, 7, 12, 0, 0, 1000, time.UTC), 1, 0},
		{"worker wrong owner", "UPDATE workers SET user_id=$2 WHERE id=$1", "other", 1, 1},
		{"run wrong owner", "UPDATE runs SET user_id=$2 WHERE id=$1", "other", 1, 1},
		{"missing run", "UPDATE recovery_custody_holds SET run_id=$2 WHERE id=$1", "random", 1, 1},
		{"null live run", "UPDATE recovery_custody_holds SET live_run_id=NULL WHERE id=$1", nil, 1, 1},
		{"mismatch live run", "UPDATE recovery_custody_holds SET live_run_id=$2 WHERE id=$1", "queued", 1, 1},
		{"null live worker", "UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1", nil, 1, 1},
		{"null run worker", "UPDATE runs SET worker_id=NULL WHERE id=$1", nil, 1, 1},
		{"mismatch run worker", "UPDATE runs SET worker_id=$2 WHERE id=$1", "claimant", 1, 1},
		{"old generation", "UPDATE recovery_custody_holds SET generation=$2 WHERE id=$1", int64(0), 1, 1},
		{"future generation", "UPDATE recovery_custody_holds SET generation=$2 WHERE id=$1", int64(2), 1, 1},
		{"released claim", "UPDATE runs SET claim_released_at=now() WHERE id=$1", nil, 1, 1},
		{"released hold", "UPDATE recovery_custody_holds SET state='released' WHERE id=$1", nil, 0, 0},
		{"discarded hold", "UPDATE recovery_custody_holds SET state='discarded' WHERE id=$1", nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdmissionFixture(t, 1)
			id := f.holds[0]
			if len(tc.sql) >= 14 && tc.sql[:14] == "UPDATE workers" {
				id = f.worker
			}
			if len(tc.sql) >= 11 && tc.sql[:11] == "UPDATE runs" {
				id = f.runs[0]
			}
			value := tc.value
			switch value {
			case "other":
				value = f.other
			case "random":
				value = uuid.New()
			case "queued":
				value = f.queued
			case "claimant":
				value = f.claimant
			}
			if value == nil {
				f.exec(tc.sql, id)
			} else {
				f.exec(tc.sql, id, value)
			}
			f.mirrors(f.cutoff, 1, tc.total, tc.count, false)
		})
	}
	t.Run("missing worker", func(t *testing.T) {
		f := newAdmissionFixture(t, 1)
		// FK deletion requires clearing the live pointer; provenance remains dangling.
		f.exec(`UPDATE recovery_custody_holds SET live_worker_id=NULL WHERE id=$1`, f.holds[0])
		f.exec(`DELETE FROM workers WHERE id=$1`, f.worker)
		f.mirrors(f.cutoff, 1, 1, 1, false)
	})
	t.Run("null cutoff", func(t *testing.T) { f := newAdmissionFixture(t, 1); f.mirrors(pgtype.Timestamptz{}, 1, 1, 1, false) })
	for _, limit := range []int32{0, -1} {
		t.Run(fmt.Sprintf("disabled %d", limit), func(t *testing.T) {
			f := newAdmissionFixture(t, 1)
			f.capture(f.holds[0], f.runs[0], f.owner, "needs_action")
			f.mirrors(f.cutoff, limit, 1, 1, false)
		})
	}
	t.Run("continuation uses all per run holds", func(t *testing.T) {
		f := newAdmissionFixture(t, 1)
		f.exec(`UPDATE runs SET claim_generation=1 WHERE id=$1`, f.queued)
		f.hold(f.queued, 1)
		f.mirrors(f.cutoff, 2, 2, 1, true)
		f.hold(f.queued, 1)
		f.mirrors(f.cutoff, 2, 3, 2, false)
	})
}

// Exhaustion holds remain decisions even when archives exist or captures are in flight.
func TestEightExhaustionHoldsAdmissionLiveDB(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		for _, capture := range []string{"", "available", "preparing", "uploading", "needs_action"} {
			t.Run(fmt.Sprintf("guarded=%v/capture=%s", guarded, capture), func(t *testing.T) {
				f := newAdmissionFixture(t, 0)
				for range 8 {
					run := f.run("recovery_wait", f.worker, 1)
					f.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted' WHERE id=$1`, run)
					hold := f.holdWithInventory(run, 1, guarded)
					if capture != "" {
						f.capture(hold, run, f.owner, capture)
					}
				}
				owners, err := f.q.ListCustodyHoldsForOwner(f.ctx, store.ListCustodyHoldsForOwnerParams{UserID: f.owner})
				if err != nil {
					t.Fatal(err)
				}
				workers, err := f.q.ListOpenCustodyHoldsForWorkers(f.ctx, []uuid.UUID{f.worker})
				if err != nil {
					t.Fatal(err)
				}
				if len(owners) != 8 || len(workers) != 8 {
					t.Fatalf("listings owners=%d workers=%d, want eight each", len(owners), len(workers))
				}
				want := "source_only"
				if capture == "needs_action" {
					want = "needs_action"
				}
				for _, row := range owners {
					if !row.DecisionNeeded || row.Attention != want || row.RecoveryWaitCause != "worker_requeue_exhausted" {
						t.Fatalf("owner exhaustion facts=%+v", row)
					}
				}
				for _, row := range workers {
					if !row.DecisionNeeded || row.Attention != want || row.RecoveryWaitCause != "worker_requeue_exhausted" {
						t.Fatalf("worker exhaustion facts=%+v", row)
					}
				}
				f.mirrors(f.cutoff, 8, 8, 8, false)
			})
		}
	}
}

func TestCustodyClassifierMatrixLiveDB(t *testing.T) {
	ctx, pool, _ := openCustodyEpisodeLiveDB(t)
	for _, tc := range []struct {
		state, capture, status string
		cause                  any
		available, guarded     bool
		want                   string
	}{
		{"discarded", "preparing", "running", nil, true, false, "discarded"},
		{"released", "available", "completed", nil, true, false, "released"},
		{"open", "available", "completed", nil, true, false, "archive_ready"},
		{"open", "preparing", "running", nil, false, false, "capturing"},
		{"open", "uploading", "running", nil, false, false, "capturing"},
		{"open", "needs_action", "failed", nil, false, false, "needs_action"},
		{"open", "", "running", nil, false, false, "active"},
		{"open", "", "queued", nil, false, false, "active"},
		{"open", "", "paused", nil, false, false, "active"},
		{"open", "", "completed", nil, false, false, "source_only"},
		{"open", "", "failed", nil, false, false, "source_only"},
		{"open", "", "cancelled", nil, false, false, "source_only"},
		{"open", "", "", nil, false, false, "source_only"},
		{"open", "needs_action", "running", nil, true, false, "archive_ready"},
		{"open", "available", "failed", nil, true, false, "archive_ready"},
		{"open", "preparing", "completed", nil, false, false, "capturing"},
		{"open", "needs_action", "running", nil, false, false, "needs_action"},
		{"open", "available", "running", nil, true, true, "active"},
		{"open", "available", "completed", nil, true, true, "source_only"},
		{"open", "needs_action", "running", nil, true, true, "needs_action"},
		{"open", "uploading", "failed", nil, true, true, "capturing"},
		{"open", "", "recovery_wait", "worker_requeue_exhausted", false, false, "source_only"},
		{"open", "available", "recovery_wait", "worker_requeue_exhausted", true, false, "source_only"},
		{"open", "preparing", "recovery_wait", "worker_requeue_exhausted", true, false, "source_only"},
		{"open", "uploading", "recovery_wait", "worker_requeue_exhausted", false, false, "source_only"},
		{"open", "needs_action", "recovery_wait", "worker_requeue_exhausted", true, false, "needs_action"},
		{"open", "available", "recovery_wait", "worker_requeue_exhausted", true, true, "source_only"},
		{"open", "preparing", "recovery_wait", "worker_requeue_exhausted", true, true, "source_only"},
		{"open", "uploading", "recovery_wait", "worker_requeue_exhausted", true, true, "source_only"},
		{"open", "needs_action", "recovery_wait", "worker_requeue_exhausted", true, true, "needs_action"},
		{"released", "available", "recovery_wait", "worker_requeue_exhausted", true, false, "released"},
		{"discarded", "uploading", "recovery_wait", "worker_requeue_exhausted", true, true, "discarded"},
		{"open", "", "recovery_wait", "provider_outage", false, false, "active"},
		{"open", "available", "recovery_wait", "provider_outage", true, false, "archive_ready"},
		{"open", "preparing", "recovery_wait", "provider_outage", false, false, "capturing"},
		{"open", "uploading", "recovery_wait", "provider_outage", false, true, "capturing"},
		{"open", "needs_action", "recovery_wait", "provider_outage", true, false, "archive_ready"},
		{"open", "needs_action", "recovery_wait", "provider_outage", true, true, "needs_action"},
		{"open", "", "recovery_wait", "worker_restart", false, false, "active"},
		{"open", "available", "recovery_wait", "worker_restart", true, false, "archive_ready"},
		{"open", "preparing", "recovery_wait", "worker_restart", false, false, "capturing"},
		{"open", "uploading", "recovery_wait", "worker_restart", false, true, "capturing"},
		{"open", "needs_action", "recovery_wait", "worker_restart", true, false, "archive_ready"},
		{"open", "needs_action", "recovery_wait", "worker_restart", true, true, "needs_action"},
		{"open", "", "recovery_wait", nil, false, false, "active"},
		{"open", "available", "recovery_wait", nil, true, false, "archive_ready"},
		{"open", "preparing", "recovery_wait", nil, false, false, "capturing"},
		{"open", "uploading", "recovery_wait", nil, false, true, "capturing"},
		{"open", "needs_action", "recovery_wait", nil, true, false, "archive_ready"},
		{"open", "needs_action", "recovery_wait", nil, true, true, "needs_action"},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s/%v/%v/%v", tc.state, tc.capture, tc.status, tc.cause, tc.available, tc.guarded), func(t *testing.T) {
			var attention string
			var decision bool
			err := pool.QueryRow(ctx, `SELECT a,fn_is_decision_attention(a) FROM
 (SELECT fn_custody_attention($1,$2,$3,$4,$5,$6) a) x`, tc.state, tc.available, tc.guarded, tc.capture, tc.status, tc.cause).Scan(&attention, &decision)
			if err != nil {
				t.Fatal(err)
			}
			if attention != tc.want || decision != (tc.want == "needs_action" || tc.want == "source_only") {
				t.Fatalf("got %s/%v want %s", attention, decision, tc.want)
			}
		})
	}
	for _, input := range []any{nil, "unknown", "active", "archive_ready", "capturing", "released", "discarded", "needs_action", "source_only"} {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT fn_is_decision_attention($1)`, input).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != (input == "needs_action" || input == "source_only") {
			t.Fatalf("decision(%v)=%v", input, got)
		}
	}
	var attention string
	if err := pool.QueryRow(ctx, `SELECT fn_custody_attention(NULL,NULL,NULL,NULL,NULL,NULL)`).Scan(&attention); err != nil || attention != "source_only" {
		t.Fatalf("null inputs: %s %v", attention, err)
	}
}

func TestCustodyFactsListingsLiveDB(t *testing.T) {
	// Inventory guarding is immutable; seed each case rather than trying to toggle it.
	for _, guarded := range []bool{false, true} {
		f := newAdmissionFixture(t, 0)
		run := f.run("running", f.worker, 1)
		hold := f.holdWithInventory(run, 1, guarded)
		f.capture(hold, run, f.owner, "available")
		f.capture(hold, run, f.owner, "needs_action")
		owner, err := f.q.ListCustodyHoldsForOwner(f.ctx, store.ListCustodyHoldsForOwnerParams{UserID: f.owner})
		if err != nil {
			t.Fatal(err)
		}
		batch, err := f.q.ListOpenCustodyHoldsForWorkers(f.ctx, []uuid.UUID{f.worker})
		if err != nil {
			t.Fatal(err)
		}
		want := "archive_ready"
		if guarded {
			want = "needs_action"
		}
		if len(owner) != 1 || len(batch) != 1 {
			t.Fatalf("listing lengths owner=%d batch=%d", len(owner), len(batch))
		}
		if owner[0].Attention != want || batch[0].Attention != want || owner[0].DecisionNeeded != guarded || batch[0].DecisionNeeded != guarded {
			t.Fatalf("owner=%+v batch=%+v", owner[0], batch[0])
		}
		f.mirrors(f.cutoff, 1, 1, map[bool]int64{false: 0, true: 1}[guarded], false)
	}
}

func TestCustodyAdmissionStatementSnapshotLiveDB(t *testing.T) {
	ctx, pool, _ := openCustodyEpisodeLiveDB(t)
	for name, want := range map[string]string{"fn_custody_attention": "i", "fn_is_decision_attention": "i", "fn_custody_admission_count": "s"} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT provolatile::text FROM pg_proc WHERE oid=$1::regproc`, name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s volatility=%s want=%s", name, got, want)
		}
	}
	for _, change := range []string{"heartbeat", "capture", "generation"} {
		t.Run(change, func(t *testing.T) {
			f := newAdmissionFixture(t, 1)
			snapshotCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			blocker, err := pool.Acquire(snapshotCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Release()
			reader, err := pool.Acquire(snapshotCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Release()
			key := int64(reader.Conn().PgConn().PID())
			if _, err = blocker.Exec(snapshotCtx, `SELECT pg_advisory_lock($1)`, key); err != nil {
				t.Fatal(err)
			}
			locked := true
			defer func() {
				if locked {
					_, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
				}
			}()
			type result struct {
				before, after, total int64
				err                  error
			}
			done := make(chan result, 1)
			go func() {
				var r result
				// Test-only barrier: before is materialized before blocking; after depends on barrier.
				r.err = reader.QueryRow(snapshotCtx, `WITH before AS MATERIALIZED (
 SELECT fn_custody_admission_count($1,$2) n),
 barrier AS MATERIALIZED (SELECT pg_advisory_xact_lock($3) FROM before)
 SELECT before.n, fn_custody_admission_count($1,$2),
 (SELECT count(*) FROM recovery_custody_holds WHERE user_id=$1 AND state='open')
 FROM before CROSS JOIN barrier`, f.owner, f.cutoff, key).Scan(&r.before, &r.after, &r.total)
				done <- r
			}()
			// Bound synchronization by context; inspect the reader's exact backend, without sleeps.
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waiting bool
				err = pool.QueryRow(snapshotCtx, `SELECT COALESCE(wait_event='advisory',false) FROM pg_stat_activity WHERE pid=$1`, reader.Conn().PgConn().PID()).Scan(&waiting)
				if err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-snapshotCtx.Done():
					t.Fatal(snapshotCtx.Err())
				case <-ticker.C:
				}
			}
			switch change {
			case "heartbeat":
				f.exec(`UPDATE workers SET last_heartbeat_at=NULL WHERE id=$1`, f.worker)
			case "capture":
				f.capture(f.holds[0], f.runs[0], f.owner, "needs_action")
			case "generation":
				f.exec(`UPDATE runs SET claim_generation=2 WHERE id=$1`, f.runs[0])
			}
			if _, err = blocker.Exec(snapshotCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
				t.Fatal(err)
			}
			locked = false
			r := <-done
			if r.err != nil || r.before != 0 || r.after != 0 || r.total != 1 {
				t.Fatalf("statement snapshot: %+v", r)
			}
			f.mirrors(f.cutoff, 1, 1, 1, false)
		})
	}
}
