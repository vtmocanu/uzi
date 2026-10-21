package store_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These are compatibility guards for predicates already present in runtime.sql,
// not historical-red evidence. Each query arm has its own fixture and assertion:
// a different placement query cannot supply its oracle or mask its failure.
// "Old" means missing ONLY provider_policy_hold_v1, not missing Codex support.
func policyPlacementProtocols(policy bool) []string {
	caps := []string{
		"codex_harness_v1", "codex_runtime_v2", "codex_custom_model_v1",
		"completion_interlock_v1", "codex_completion_interlock_v1",
	}
	if policy {
		caps = append(caps, capability.ProviderPolicyHoldV1)
	}
	return caps
}

func newPolicyPlacementFixture(t *testing.T) *fleetFixture {
	t.Helper()
	fx := newFleetFixture(t)
	// Only our own runs are removed; other LiveDB fixtures may share the database.
	t.Cleanup(func() {
		mustExec(fx.ctx, t, fx.pool, "DELETE FROM runs WHERE user_id = $1", fx.userID)
	})
	return fx
}

func policyPlacementWorker(fx *fleetFixture, name string, capacity int32, policy bool) uuid.UUID {
	fx.t.Helper()
	w := fx.worker(name, capOf(capacity), true)
	mustExec(fx.ctx, fx.t, fx.pool,
		"UPDATE workers SET protocol_capabilities = $2 WHERE id = $1", w, policyPlacementProtocols(policy))
	return w
}

// Seed eligible noninteractive Codex roots, without review/target children,
// ownership pins, isolated profiles, leases or tool requirements. A positive
// refusal count alone is durable: hold_reason remains NULL after resumption and
// ordinary required_capabilities are empty (the owner-override shape).
func policyPlacementRun(fx *fleetFixture, kind string, marked bool) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	var issue any
	if kind == "issue" {
		issue = fx.nextIID()
	}
	refusals := 0
	if marked {
		refusals = 1
	}
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO runs
		(id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		 branch, dispatched_at, status, status_since, harness, interactive, completion_contract_version,
		 claim_generation, policy_refusal_count, required_capabilities)
		VALUES ($1, $2, $3, $4, $5, 'Policy placement control', 'Continue the approved work',
		        'uzi/task/' || $1::uuid::text, CASE WHEN $4 = 'task' THEN now() END,
		        'queued', now() - interval '1 hour', 'codex', false, 1, 1, $6, '{}')`,
		id, fx.userID, fx.repoID, kind, issue, refusals)
	return id
}

// Unlike claimCaps, pass protocol support and the actual custody setting. The
// Docker allowlist and stored worker protocol set agree with the claimant params.
// spreadCutoff is captured before inserting the demand, keeping it inside the
// fresh-run spread window without sleeps or a wall-clock race.
func policyPlacementClaim(fx *fleetFixture, worker uuid.UUID, policy, aware bool, spreadCutoff time.Time) (store.Run, error) {
	fx.t.Helper()
	limit, err := settings.New(fx.q, time.Minute).CustodyHoldLimit(fx.ctx)
	if err != nil {
		fx.t.Fatalf("CustodyHoldLimit: %v", err)
	}
	now := time.Now()
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		Lane: "run", WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, UserID: fx.userID,
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		SpreadCutoff:             pgtype.Timestamptz{Time: spreadCutoff, Valid: true},
		BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		IsDockerWorker:           true, DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		WorkerCaps: []string{}, WorkerProtocolCaps: policyPlacementProtocols(policy),
		CapabilityAware: aware, CustodyHoldLimit: limit, WorkerIdentity: worker.String(),
	})
}

func TestPolicyHoldPlacementPeerDeferralLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt"} {
		for _, aware := range []bool{false, true} {
			for _, tc := range []struct {
				name               string
				marked, peerPolicy bool
				deferToPeer        bool
			}{
				{"marked_old_peer", true, false, false},
				{"marked_policy_peer", true, true, true},
				{"ordinary_old_peer", false, false, true},
			} {
				t.Run(fmt.Sprintf("%s/aware=%t/%s", kind, aware, tc.name), func(t *testing.T) {
					fx := newPolicyPlacementFixture(t)
					busy := policyPlacementWorker(fx, "busy eligible claimant", 2, true)
					peer := policyPlacementWorker(fx, "idle peer", 2, tc.peerPolicy)
					fx.holdActive(busy, 1)
					cutoff := time.Now().Add(-time.Second)
					run := policyPlacementRun(fx, kind, tc.marked)

					got, err := policyPlacementClaim(fx, busy, true, aware, cutoff)
					if !tc.deferToPeer {
						if err != nil || got.ID != run {
							t.Fatalf("ClaimRun peer predicate: busy claimant got %s, %v; want run %s", got.ID, err, run)
						}
						return
					}
					if err != pgx.ErrNoRows {
						t.Fatalf("ClaimRun peer predicate: got %s, %v; want deferral for run %s", got.ID, err, run)
					}
					got, err = policyPlacementClaim(fx, peer, tc.peerPolicy, aware, cutoff)
					if err != nil || got.ID != run {
						t.Fatalf("ClaimRun deferred peer: got %s, %v; want run %s", got.ID, err, run)
					}
				})
			}
		}
	}
}

// The provisioning queries expose no CapabilityAware parameter. They execute
// independently below, with the same zero-required-capability marked demand used
// by both flag states above. No readiness overlay or alternate SQL is involved.
func requirePolicyPlacementGap(fx *fleetFixture, run uuid.UUID, want bool) {
	fx.t.Helper()
	rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		AffinityCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
		EphemeralLease:        leaseInterval(leaseTwoHours),
		WorkerDockerEnabled:   true, DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		MaxPerUser: 1000, MaxRows: math.MaxInt32,
	})
	if err != nil {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == run {
			found = true
			if row.UserID != fx.userID || !row.RepoID.Valid || row.RepoID.Bytes != fx.repoID {
				fx.t.Fatalf("unplaceable run %s has wrong owner/repo: %+v", run, row)
			}
		}
	}
	if found != want {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral run %s listed=%t, want %t", run, found, want)
	}
}

func requirePolicyPlacementSaturation(fx *fleetFixture, run uuid.UUID, want bool) {
	fx.t.Helper()
	rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		AffinityCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		BackgroundGraceCutoff: pgtype.Timestamptz{Time: time.Now().Add(-15 * time.Minute), Valid: true},
		SaturationDelay:       leaseInterval(0), EphemeralLease: leaseInterval(leaseTwoHours),
		WorkerDockerEnabled: true, DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		MaxPerUser: 1000, MaxRows: math.MaxInt32,
	})
	if err != nil {
		fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == run {
			found = true
			if row.UserID != fx.userID || !row.RepoID.Valid || row.RepoID.Bytes != fx.repoID {
				fx.t.Fatalf("saturation run %s has wrong owner/repo: %+v", run, row)
			}
		}
	}
	if found != want {
		fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral run %s listed=%t, want %t", run, found, want)
	}
}

func TestPolicyHoldPlacementUnplaceableLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt"} {
		for _, tc := range []struct {
			name                         string
			marked, workerPolicy, listed bool
		}{
			{"marked_old_worker", true, false, true},
			{"marked_policy_worker", true, true, false},
			{"ordinary_old_worker", false, false, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				fx := newPolicyPlacementFixture(t)
				optInEphemeral(fx)
				policyPlacementWorker(fx, "idle placement", 1, tc.workerPolicy)
				run := policyPlacementRun(fx, kind, tc.marked)
				if !tc.marked {
					// Keep ordinary demand inside the gap query's entry guard;
					// otherwise its absence would not exercise worker placement.
					setRunCaps(fx, run, []string{"docker"})
				}
				requirePolicyPlacementGap(fx, run, tc.listed)
			})
		}
	}
}

func TestPolicyHoldPlacementSaturationCapableExistsLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt"} {
		for _, tc := range []struct {
			name                         string
			marked, workerPolicy, listed bool
		}{
			{"marked_old_busy_worker", true, false, false},
			{"marked_policy_busy_worker", true, true, true},
			{"ordinary_old_busy_worker", false, false, true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				fx := newPolicyPlacementFixture(t)
				optInEphemeral(fx)
				busy := policyPlacementWorker(fx, "only busy placement", 1, tc.workerPolicy)
				fx.holdActive(busy, 1)
				run := policyPlacementRun(fx, kind, tc.marked)
				// Every worker is full even if its policy fence is removed.
				// Thus only the capable-exists arm can change this oracle.
				requirePolicyPlacementSaturation(fx, run, tc.listed)
			})
		}
	}
}

func TestPolicyHoldPlacementSaturationFreeSlotLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "task", "prompt"} {
		for _, tc := range []struct {
			name                       string
			marked, idlePolicy, listed bool
		}{
			{"marked_old_idle_worker", true, false, true},
			{"marked_policy_idle_worker", true, true, false},
			{"ordinary_old_idle_worker", false, false, false},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				fx := newPolicyPlacementFixture(t)
				optInEphemeral(fx)
				busy := policyPlacementWorker(fx, "busy policy placement", 1, true)
				fx.holdActive(busy, 1)
				policyPlacementWorker(fx, "idle placement", 1, tc.idlePolicy)
				run := policyPlacementRun(fx, kind, tc.marked)
				// The busy policy worker always satisfies capable-exists;
				// only the idle worker can suppress saturation.
				requirePolicyPlacementSaturation(fx, run, tc.listed)
			})
		}
	}
}
