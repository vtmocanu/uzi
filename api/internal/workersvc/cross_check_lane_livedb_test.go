package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func laneFixture(t *testing.T) crossCheckContentionFixture {
	t.Helper()
	f := newCrossCheckContentionFixture(t)
	f.env.exec(`UPDATE workers SET max_concurrent_runs=1,max_cross_check_slots=1,
        protocol_capabilities=array_append(protocol_capabilities,'cross_check_lane_v1'),
        last_heartbeat_at=now() WHERE id=$1`, f.workerID)
	f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.runID, f.workerID)
	return f
}

func laneClaim(t *testing.T, f crossCheckContentionFixture, lane string, snap *ActiveSnapshot) *ClaimPayload {
	t.Helper()
	w := wkrRow(t, f.env, f.workerID)
	var p *ClaimPayload
	var err error
	if lane == "cross_check" {
		p, err = f.svc.ClaimCrossCheck(f.env.ctx, w, snap)
	} else {
		p, err = f.svc.Claim(f.env.ctx, w, snap)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCrossCheckLaneOwnLeadLiveDB(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		t.Run(fmt.Sprint(snapshot), func(t *testing.T) {
			f := laneFixture(t)
			f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
			if p := laneClaim(t, f, "run", nil); p != nil {
				t.Fatal("run lane selected a lane child")
			}
			var snap *ActiveSnapshot
			if snapshot {
				snap = &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2", Active: []ActiveRunEntry{}}
			}
			p := laneClaim(t, f, "cross_check", snap)
			f.claimed(t, p)
			if !mustRun(t, f.env, f.runID).CrossCheckLane {
				t.Fatal("claim did not stamp lane")
			}
			rows, err := f.env.q.ListWorkersByUser(f.env.ctx, f.userID)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.ID == f.workerID && (!row.Busy || row.ActiveRuns != 1) {
					t.Fatal("lane child inflated lead's displayed run load")
				}
			}
			if _, err := f.svc.DecidePlanCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.runID, p.ClaimGeneration+1,
				"approve", "approve", []byte(`{"summary":"ok","items":[]}`)); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("stale generation verdict: %v", err)
			}
		})
	}
}

func TestCrossCheckLaneNegotiationLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, sql, lane string
		want            bool
	}{
		{"legacy", `UPDATE workers SET max_cross_check_slots=NULL,protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, "run", true},
		{"legacy-full", `UPDATE workers SET max_cross_check_slots=NULL,protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, "run", false},
		{"zero-no-cap-run", `UPDATE workers SET max_cross_check_slots=0,protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, "run", false},
		{"zero-cap-lane", `UPDATE workers SET max_cross_check_slots=0 WHERE id=$1`, "cross_check", false},
		{"null-cap-run", `UPDATE workers SET max_cross_check_slots=NULL WHERE id=$1`, "run", false},
		{"null-cap-lane", `UPDATE workers SET max_cross_check_slots=NULL WHERE id=$1`, "cross_check", false},
		{"slots-no-cap-run", `UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, "run", false},
		{"slots-no-cap-lane", `UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, "cross_check", false},
		{"missing-check-protocol", `UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_v1') WHERE id=$1`, "cross_check", false},
		{"missing-runtime", `UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'codex_runtime_v2') WHERE id=$1`, "cross_check", false},
		{"missing-custom-model", `UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'codex_custom_model_v1') WHERE id=$1`, "cross_check", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := laneFixture(t)
			f.env.exec(tc.sql, f.workerID)
			if tc.name == "legacy-full" {
				f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
			}
			p := laneClaim(t, f, tc.lane, nil)
			if (p != nil) != tc.want {
				t.Fatalf("payload present=%v want=%v", p != nil, tc.want)
			}
			if p != nil {
				f.claimed(t, p)
				if mustRun(t, f.env, f.runID).CrossCheckLane {
					t.Fatal("legacy fallback stamped lane")
				}
				rows, err := f.env.q.ListWorkersByUser(f.env.ctx, f.userID)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.ID == f.workerID && row.ActiveRuns != 1 {
						t.Fatal("legacy child did not consume run load")
					}
				}
			} else if mustRun(t, f.env, f.runID).ClaimGeneration != 0 {
				t.Fatal("refused child was claimed")
			}
		})
	}
}

func cloneLaneChild(t *testing.T, f crossCheckContentionFixture) uuid.UUID {
	t.Helper()
	lead, child, check := uuid.New(), uuid.New(), uuid.New()
	f.env.exec(`INSERT INTO runs SELECT (jsonb_populate_record(NULL::runs,
        to_jsonb(r) || jsonb_build_object('id',$2::uuid,'issue_iid',(SELECT max(issue_iid)+1 FROM runs WHERE repo_id=r.repo_id)))).* FROM runs r WHERE id=$1`, f.lead, lead)
	f.env.exec(`INSERT INTO runs SELECT (jsonb_populate_record(NULL::runs,
        to_jsonb(r) || jsonb_build_object('id',$2::uuid,'target_run_id',$3::uuid))).* FROM runs r WHERE id=$1`, f.runID, child, lead)
	f.env.exec(`INSERT INTO cross_checks SELECT (jsonb_populate_record(NULL::cross_checks,
        to_jsonb(cc) || jsonb_build_object('id',$2::uuid,'lead_run_id',$3::uuid,'checker_run_id',$4::uuid))).*
        FROM cross_checks cc WHERE lead_run_id=$1`, f.lead, check, lead, child)
	return child
}

func TestCrossCheckLaneCompetingBodylessClaimsLiveDB(t *testing.T) {
	f := laneFixture(t)
	child2 := cloneLaneChild(t, f)
	ctx, cancel := context.WithTimeout(f.env.ctx, 10*time.Second)
	defer cancel()
	held, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback(ctx)
	if _, err := store.New(held).GetWorkerForUpdate(ctx, f.workerID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		p   *ClaimPayload
		err error
	}
	results := make(chan result, 2)
	w := wkrRow(t, f.env, f.workerID)
	for i := 0; i < 2; i++ {
		go func() { p, err := f.svc.ClaimCrossCheck(ctx, w, nil); results <- result{p, err} }()
	}
	// The held worker lock forces both requests to wait before their occupancy read.
	// Bounded polling confirms they actually reached PostgreSQL; one failure ends the test.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
            WHERE datname=current_database() AND wait_event_type='Lock'
              AND query LIKE '%GetWorkerForUpdate%'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claims did not reach separate worker lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := held.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	claimed := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.p != nil {
			claimed++
			if r.p.RunID != f.runID.String() && r.p.RunID != child2.String() {
				t.Fatal("wrong child")
			}
		}
	}
	if claimed != 1 {
		t.Fatalf("cap1 delivered %d children", claimed)
	}
	a, b := mustRun(t, f.env, f.runID), mustRun(t, f.env, child2)
	if a.ClaimGeneration+b.ClaimGeneration != 1 {
		t.Fatal("losing claim incremented a generation")
	}
}

func TestCrossCheckLaneEligibilityLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      bool
	}{
		{"own-draining", `UPDATE workers SET draining_since=now() WHERE id=$1`, true},
		{"maintenance-fenced", `UPDATE workers SET maintenance_fenced=true WHERE id=$1`, false},
		{"foreign-draining", `UPDATE workers SET draining_since=now() WHERE id=$1; UPDATE runs SET worker_id=NULL WHERE id=$2`, false},
		{"fresh-other-affinity", `UPDATE runs SET worker_id=(SELECT worker_id FROM runs WHERE id=(SELECT target_run_id FROM runs WHERE id=$2)) WHERE id=$2`, false},
		{"expired-other-affinity", `UPDATE runs SET worker_id=(SELECT worker_id FROM runs WHERE id=(SELECT target_run_id FROM runs WHERE id=$2)),updated_at=now()-interval '3 minutes' WHERE id=$2`, true},
		{"ephemeral-bound-lead", `UPDATE workers SET ephemeral=true,ephemeral_run_id=(SELECT target_run_id FROM runs WHERE id=$2) WHERE id=$1`, true},
		{"ephemeral-bound-child", `UPDATE workers SET ephemeral=true,ephemeral_run_id=$2 WHERE id=$1`, true},
		{"ephemeral-foreign", `UPDATE workers SET ephemeral=true,ephemeral_run_id=(SELECT target_run_id FROM runs WHERE id<>$2 AND kind='cross_check' AND user_id=(SELECT user_id FROM workers WHERE id=$1) LIMIT 1) WHERE id=$1`, false},
		{"expired-parent", `UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE checker_run_id=$2`, false},
		{"stale-parent-generation", `UPDATE runs SET claim_generation=2 WHERE id=(SELECT target_run_id FROM runs WHERE id=$2)`, false},
		{"docker-denied", `UPDATE workers SET docker_enabled=true WHERE id=$1`, false},
		{"overflow", `UPDATE workers SET pending_overflow_until=now()+interval '5 minutes' WHERE id=$1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := laneFixture(t)
			if tc.name == "ephemeral-foreign" {
				foreignChild := cloneLaneChild(t, f)
				f.env.exec(`UPDATE runs SET status='failed' WHERE id=$1`, foreignChild)
			}
			// Literal fixture statements are split for parameter-count correctness.
			for _, sql := range strings.Split(tc.sql, ";") {
				args := []any{pgx.QueryExecModeSimpleProtocol}
				if strings.Contains(sql, "$2") && !strings.Contains(sql, "$1") {
					sql = strings.ReplaceAll(sql, "$2", "$1")
					args = append(args, f.runID)
				} else if strings.Contains(sql, "$2") {
					args = append(args, f.workerID, f.runID)
				} else {
					args = append(args, f.workerID)
				}
				if _, err := f.env.pool.Exec(f.env.ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			p := laneClaim(t, f, "cross_check", nil)
			if (p != nil) != tc.want {
				t.Fatalf("payload present=%v want=%v", p != nil, tc.want)
			}
		})
	}
}

func TestCrossCheckLaneAffinityGraceLiveDB(t *testing.T) {
	f := laneFixture(t)
	evaluatedAt := time.Now().UTC()
	f.svc.now = func() time.Time { return evaluatedAt }
	f.svc.p.WorkerAffinityGrace = 10 * time.Minute
	f.env.exec(`UPDATE runs SET worker_id=(SELECT worker_id FROM runs WHERE id=$2),updated_at=$3 WHERE id=$1`,
		f.runID, f.lead, evaluatedAt.Add(-3*time.Minute))
	n, err := f.svc.WorkerEligibilityForHealth(f.env.ctx, evaluatedAt, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Claimable != 0 {
		t.Fatalf("fresh affinity counted %d claimants", n.Claimable)
	}
	if p := laneClaim(t, f, "cross_check", nil); p != nil {
		t.Fatal("nondefault ten-minute grace was ignored")
	}
	// The explicit evaluation time crosses the grace without sleeping or changing the stage deadline.
	evaluatedAt = evaluatedAt.Add(8 * time.Minute)
	n, err = f.svc.WorkerEligibilityForHealth(f.env.ctx, evaluatedAt, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	// Advance the heartbeat with the evaluation clock so worker liveness stays eligible.
	f.env.exec(`UPDATE workers SET last_heartbeat_at=$2 WHERE id=$1`, f.workerID, evaluatedAt)
	n, err = f.svc.WorkerEligibilityForHealth(f.env.ctx, evaluatedAt, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if n.Claimable != 1 {
		t.Fatalf("expired affinity counted %d claimants", n.Claimable)
	}
	f.claimed(t, laneClaim(t, f, "cross_check", nil))
}

func TestCrossCheckLaneRequeueMarkerLiveDB(t *testing.T) {
	f := laneFixture(t)
	f.claimed(t, laneClaim(t, f, "cross_check", nil))
	// This old-API-shaped update knows only status; the database clears the lane marker.
	f.env.exec(`UPDATE runs SET status='queued' WHERE id=$1`, f.runID)
	if mustRun(t, f.env, f.runID).CrossCheckLane {
		t.Fatal("requeue retained lane marker")
	}
	f.env.exec(`UPDATE workers SET max_cross_check_slots=NULL,protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, f.workerID)
	p := laneClaim(t, f, "run", nil)
	if p == nil || p.RunID != f.runID.String() || p.ClaimGeneration != 2 {
		t.Fatal("legacy reclaim did not advance generation")
	}
	if mustRun(t, f.env, f.runID).CrossCheckLane {
		t.Fatal("legacy reclaim retained lane marker")
	}
	inserted := uuid.New()
	f.env.exec(`INSERT INTO runs SELECT (jsonb_populate_record(NULL::runs,
        to_jsonb(r) || jsonb_build_object('id',$2::uuid,'status','queued','cross_check_lane',true))).*
        FROM runs r WHERE id=$1`, f.runID, inserted)
	if mustRun(t, f.env, inserted).CrossCheckLane {
		t.Fatal("queued insert retained lane marker")
	}
}

func TestCrossCheckLaneSnapshotBudgetLiveDB(t *testing.T) {
	f := laneFixture(t)
	f.env.exec(`UPDATE workers SET max_cross_check_slots=3 WHERE id=$1`, f.workerID)
	f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.lead, f.workerID)
	ids := []uuid.UUID{f.runID, cloneLaneChild(t, f), cloneLaneChild(t, f)}
	snap := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-U2",
		Active: []ActiveRunEntry{entry(f.lead, 1, "running", false)}}
	for _, id := range ids {
		f.env.exec(`UPDATE runs SET status='running',worker_id=$2,claim_generation=1,cross_check_lane=true WHERE id=$1`, id, f.workerID)
		snap.Active = append(snap.Active, entry(id, 1, "running", false))
	}
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.env.ctx)
	qtx := store.New(tx)
	w := wkrRow(t, f.env, f.workerID)
	applied, err := f.svc.ReplaceWorkerActiveRuns(f.env.ctx, qtx, w, snap, snapshotModeClaim)
	if err != nil || !applied {
		t.Fatalf("cap1+cross3 snapshot applied=%v err=%v", applied, err)
	}
	f.svc.p.ActiveSnapshotMaxEntries = 3
	snap.SnapshotEpoch++
	if _, err := f.svc.ReplaceWorkerActiveRuns(f.env.ctx, qtx, w, snap, snapshotModeClaim); !errors.Is(err, ErrActiveSnapshotInvalid) {
		t.Fatalf("absolute ceiling not enforced: %v", err)
	}
}
