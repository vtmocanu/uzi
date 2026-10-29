package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestForgeOriginPlacementCanonicalLookupPlanLiveDB records an isolated local
// microbenchmark. It makes no claim about production latency or CI timing.
func TestForgeOriginPlacementCanonicalLookupPlanLiveDB(t *testing.T) {
	f := newForgePlacementFixture(t)
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	if _, err = tx.Exec(f.ctx, `CREATE TEMP TABLE placement_lookup_skew
		(LIKE forge_origin_cooldowns INCLUDING ALL) ON COMMIT DROP`); err != nil {
		t.Fatal(err)
	}
	// Skew the leading keys deliberately: every row has the same owner/worker,
	// so an index scan that omits origin still has thousands of rows to filter.
	tag, err := tx.Exec(f.ctx, `INSERT INTO placement_lookup_skew
		(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
		SELECT $1,$2,'https://skew-'||i::text||'.example',now()+interval '1 day',
			'transient_clone_fetch_exhausted',now() FROM generate_series(1,8192) i`, f.userID, f.owner)
	if err != nil || tag.RowsAffected() != 8192 {
		t.Fatalf("skew fixture rows=%d err=%v", tag.RowsAffected(), err)
	}
	if _, err = tx.Exec(f.ctx, "ANALYZE placement_lookup_skew"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = tx.QueryRow(f.ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
		SELECT observation_version FROM placement_lookup_skew
		WHERE user_id=$1 AND worker_id=$2 AND origin=canonical_forge_origin($3)`,
		f.userID, f.owner, "https://SKEW-4096.example/repo").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type node struct {
		Type  string            `json:"Node Type"`
		Cond  string            `json:"Index Cond"`
		Rows  float64           `json:"Actual Rows"`
		Plans []json.RawMessage `json:"Plans"`
	}
	var plan []struct {
		Plan        json.RawMessage `json:"Plan"`
		ExecutionMS float64         `json:"Execution Time"`
	}
	if err = json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
		t.Fatalf("decode plan: %v", err)
	}
	var root node
	if err = json.Unmarshal(plan[0].Plan, &root); err != nil {
		t.Fatal(err)
	}
	indexed := false
	var visit func(json.RawMessage)
	visit = func(raw json.RawMessage) {
		var n node
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(n.Type, "Index") && strings.Contains(n.Cond, "user_id") &&
			strings.Contains(n.Cond, "worker_id") && strings.Contains(n.Cond, "origin") {
			indexed = true
		}
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plan[0].Plan)
	if !indexed || root.Rows != 1 {
		t.Fatalf("canonical key lookup indexed=%v rows=%v plan=%s", indexed, root.Rows, raw)
	}
	var pgVersion string
	if err = tx.QueryRow(f.ctx, "SHOW server_version").Scan(&pgVersion); err != nil {
		t.Fatal(err)
	}
	t.Logf("environment: live PostgreSQL %s; temporary indexed table; 8192 same-owner/worker rows; execution %.3f ms; plan=%s",
		pgVersion, plan[0].ExecutionMS, raw)
}

// Run alongside the placement tests: these execute the generated consumers,
// rather than reimplementing the SQL function in a test truth table.
func TestForgeOriginPlacementConsumersLiveDB(t *testing.T) {
	for _, kind := range []string{"no_adopted_source", "forge_no_output", "publication", "owner_discard",
		"merged_predecessor_discard", "completed_publication", "held_publication", "archive"} {
		t.Run(kind, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history(kind)
			f.exec("UPDATE runs SET stale_requeue_generation=claim_generation WHERE id=$1", f.run)
			if n := f.strictEligibleCount(f.run, 10*time.Minute, 30*time.Minute); n != 1 {
				t.Fatalf("strict health count=%d want the one healthy peer", n)
			}
			rows, err := f.q.ListActiveRunsForHealth(f.ctx, []string{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range rows {
				if row.ID != f.run {
					continue
				}
				found = true
				if row.StaleRequeuePinnable || row.OwnerHoldPinnable || row.StaleRequeueWorkerName != "" {
					t.Fatalf("health retained cooled cold-source owner: stale=%v hold=%v name=%q",
						row.StaleRequeuePinnable, row.OwnerHoldPinnable, row.StaleRequeueWorkerName)
				}
			}
			if !found {
				t.Fatal("health projection omitted queued target")
			}
			allowed, err := f.q.RunAdmittableForEphemeralProvision(f.ctx, f.run)
			if err != nil || allowed {
				t.Fatalf("active cooldown must suppress replacement recheck: allowed=%v err=%v", allowed, err)
			}
			// Make a real capability gap, so suppression cannot pass merely because
			// a healthy peer already has capacity. Unrelated origins are the control.
			f.exec("UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.userID)
			f.exec("UPDATE runs SET required_capabilities=ARRAY['placement-test-gap'],stale_requeue_generation=NULL WHERE id=$1", f.run)
			params := store.ListUnplaceableQueuedRunsForEphemeralParams{
				MaxRows: 1000, MaxPerUser: 1000,
				AffinityCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true},
				BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
				CrossCheckEvaluatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
				DockerRepoAllowlist:   []uuid.UUID{}, CodexCuratedModels: []string{},
			}
			demand := func(want bool) {
				t.Helper()
				rows, err := f.q.ListUnplaceableQueuedRunsForEphemeral(f.ctx, params)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == f.run {
						found = true
					}
				}
				if found != want {
					t.Fatalf("replacement demand=%v want=%v", found, want)
				}
			}
			demand(false)
			f.exec("UPDATE forge_origin_cooldowns SET origin='https://unrelated.example' WHERE worker_id=$1", f.owner)
			allowed, err = f.q.RunAdmittableForEphemeralProvision(f.ctx, f.run)
			if err != nil || !allowed {
				t.Fatalf("unrelated origin recheck=%v err=%v", allowed, err)
			}
			demand(true)
			f.exec("UPDATE forge_origin_cooldowns SET origin='https://forge.e2e' WHERE worker_id=$1", f.owner)
			f.exec("UPDATE runs SET required_capabilities=ARRAY[]::text[] WHERE id=$1", f.run)
			f.peerClaim(true)
		})
	}
}

func TestForgeOriginPlacementSaturationDemandLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	worker := fx.worker("saturated-origin", 0, false)
	matching, unrelated := fx.unclaimedRun(), fx.unclaimedRun()
	otherRepo := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO repos
		(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
		SELECT $1,connection_id,2,'g/other','https://other.example/g/other','main',true
		FROM repos WHERE id=$2`, otherRepo, fx.repoID)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET repo_id=$1 WHERE id=$2", otherRepo, unrelated)
	mustExec(fx.ctx, t, fx.pool, "UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", fx.userID)
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET max_concurrent_runs=1 WHERE id=$1", worker)
	busy := fx.pinnedRun(worker, time.Hour)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='running',claim_generation=1 WHERE id=$1", busy)
	mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status_since=now()-interval '5 minutes',updated_at=now()-interval '1 hour' WHERE id IN ($1,$2)", matching, unrelated)
	forgeAdmissionCooldown(fx, worker, false)
	now := time.Now()
	params := store.ListSaturationQueuedRunsForEphemeralParams{
		MaxRows:                  10000,
		MaxPerUser:               1000,
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-30 * time.Minute), Valid: true},
		BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		SaturationDelay:          leaseInterval(time.Minute),
		EphemeralLease:           leaseInterval(0),
		DockerRepoAllowlist:      []uuid.UUID{},
		CodexCuratedModels:       []string{},
	}
	demand := func(wantMatching, wantUnrelated bool) {
		t.Helper()
		rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		found := map[uuid.UUID]bool{}
		for _, row := range rows {
			found[row.ID] = true
		}
		if found[matching] != wantMatching || found[unrelated] != wantUnrelated {
			t.Fatalf("saturation demand matching %s=%v want=%v unrelated %s=%v want=%v",
				matching, found[matching], wantMatching, unrelated, found[unrelated], wantUnrelated)
		}
	}
	// Both targets have no owner affinity and an empty required-capability set.
	// The unrelated target exercises both capable-worker and free-slot arms
	// even while matching-origin demand is suppressed.
	demand(false, true)
	mustExec(fx.ctx, t, fx.pool, "UPDATE forge_origin_cooldowns SET origin='https://unused.example' WHERE worker_id=$1", worker)
	demand(true, true)
	// Changing only capacity proves these were saturation candidates.
	mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET max_concurrent_runs=2 WHERE id=$1", worker)
	demand(false, false)
}

func TestForgeOriginPlacementCustodyAdmissionLiveDB(t *testing.T) {
	for _, limit := range []int32{1, 8} {
		name := "limit_one"
		if limit == 8 {
			name = "saturated"
		}
		t.Run(name, func(t *testing.T) {
			f := newForgePlacementFixture(t)
			f.history("no_adopted_source")
			// Terminal, guarded open holds count independently of the safe target's
			// released history. The claimant is not their live custodian.
			for i := int32(0); i < limit; i++ {
				run := f.pinnedRun(f.owner, 0)
				f.exec("UPDATE runs SET status='failed',claim_generation=1 WHERE id=$1", run)
				f.exec(`INSERT INTO recovery_custody_holds
					(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
					 live_worker_id,live_run_id,inventory_guarded)
					VALUES($1,$2,$3,$4,1,'open',$5,'counted-owner',$5,$4,true)`,
					uuid.New(), f.userID, f.repoID, run, f.owner)
			}
			p := manualParams(f.rollAffinityFixture, f.peer)
			p.RecoveryCapable = true
			p.WorkerIdentity = "forge-admission-test"
			p.WorkerProtocolCaps = []string{}
			p.CustodyHoldLimit = limit
			if _, err := f.q.ClaimRun(f.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cold placement bypassed custody admission at limit=%d: %v", limit, err)
			}
			// Positive control changes only the admission budget.
			p.CustodyHoldLimit = limit + 1
			got, err := f.q.ClaimRun(f.ctx, p)
			if err != nil || got.ID != f.run {
				t.Fatalf("below limit claim: %s %v", got.ID, err)
			}
		})
	}
}

func TestForgeOriginPlacementNeverStartedCleanupLiveDB(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "sweep"
		if cancel {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			w := fx.worker("due-probe", 0, false)
			run := fx.unclaimedRun()
			forgeAdmissionCooldown(fx, w, true)
			if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != run {
				t.Fatalf("due claim: %s %v", got.ID, err)
			}
			if cancel {
				n, err := fx.q.CancelRunServerSide(fx.ctx, store.CancelRunServerSideParams{ID: run, UserID: fx.userID})
				if err != nil || n != 1 {
					t.Fatalf("cancel rows=%d err=%v", n, err)
				}
			} else {
				mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET updated_at=now()-interval '1 hour',status_since=now()-interval '1 hour',claimed_at=now()-interval '1 hour' WHERE id=$1", run)
				rows, err := fx.q.SweepClaimedNeverStarted(fx.ctx, pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows {
					if row.ID == run {
						found = true
					}
				}
				if !found {
					t.Fatal("claimed-never-started sweep omitted target")
				}
			}
			var cleaned bool
			if err := fx.pool.QueryRow(fx.ctx, `SELECT reservation_run_id IS NULL
				AND reservation_generation IS NULL AND reservation_incarnation IS NULL
				AND reservation_version IS NULL AND reservation_observation_version IS NULL
				AND observation_version=1 AND last_reason='transient_clone_fetch_exhausted'
				FROM forge_origin_cooldowns WHERE worker_id=$1`, w).Scan(&cleaned); err != nil || !cleaned {
				t.Fatalf("never-started cleanup lost evidence or retained reservation: %v %v", cleaned, err)
			}
		})
	}
}
