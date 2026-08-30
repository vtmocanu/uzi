package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Capability-aware ClaimRun (PRD #84 M2) against a REAL Postgres — the routing
// invariant the fake-store unit tests cannot cover because they never run the SQL.
// The extended fn_worker_can_claim (migration 00142) ANDs a new clause onto the
// shipped docker→allowlist clause: a run's required_capabilities must be a subset of
// the claiming worker's EFFECTIVE caps (@worker_caps ∪ {docker when is_docker}), gated
// by @capability_aware. These tests prove, against real queue state:
//   - flag ON: a base worker (no caps, not docker) does NOT claim a run requiring
//     {docker} or {jvm}; a run requiring {} claims anywhere; a worker whose caps include
//     the requirement DOES claim (self-reported {docker}/{jvm}, or folded is_docker);
//   - flag OFF: the same base worker DOES claim the {docker} run (degraded mode);
//   - the shipped docker→allowlist clause is NEVER gated by the flag (a docker worker
//     with an empty allowlist is fenced in BOTH flag states);
//   - the #216 spread is unchanged, and its peer-eligibility now shares the SAME
//     capability predicate (a docker run defers only to a docker-capable peer).
//
// Reuses fleetFixture from claim_fleet_placement_integration_test.go (same package).
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store e2e
// runner, e2e/run-store-it.sh, provides one).

// claimCaps claims as workerID, threading the two PRD #84 params (worker_caps,
// capability_aware) the base fleetFixture.claim omits.
func claimCaps(fx *fleetFixture, workerID uuid.UUID, isDocker bool, allow []uuid.UUID, caps []string, capAware bool) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      isDocker,
		DockerRepoAllowlist: allow,
		WorkerCaps:          caps,
		CapabilityAware:     capAware,
	})
}

// setRunCaps overwrites a run's required_capabilities (the claim predicate's input).
func setRunCaps(fx *fleetFixture, runID uuid.UUID, caps []string) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET required_capabilities = $2 WHERE id = $1`, runID, caps)
}

// requeueRun resets a claimed run to fresh-queued so a later claim can re-target it.
func requeueRun(fx *fleetFixture, runID uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET status = 'queued', worker_id = NULL, claimed_at = NULL, updated_at = now() WHERE id = $1`, runID)
}

// workerWithCaps inserts an online worker carrying an explicit capabilities set (the
// peer-eligibility path reads p.capabilities). fleetFixture.worker seeds none.
func workerWithCaps(fx *fleetFixture, name string, cap *int32, caps []string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, capabilities)
		 VALUES ($1, $2, $3, $4, 'online', now(), $5, $6)`,
		id, fx.userID, name, tokenHash(), cap, caps)
	return id
}

// TestClaimRunCapabilityMatchLiveDB is the flag-ON matcher: a base worker skips runs it
// cannot satisfy, and only a worker whose caps cover the requirement claims them.
func TestClaimRunCapabilityMatchLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false) // single worker: no peers, so no spread deferral

	plain := fx.queuedRun() // required {} (default), oldest
	dockerRun := fx.queuedRun()
	setRunCaps(fx, dockerRun, []string{"docker"})
	jvmRun := fx.queuedRun()
	setRunCaps(fx, jvmRun, []string{"jvm"})

	// Base worker (no caps, not docker), flag ON: only the {}-required run is claimable,
	// even though it is the NEWEST — the older docker/jvm runs are fenced out.
	c, err := claimCaps(fx, w, false, nil, nil, true)
	if err != nil {
		t.Fatalf("base worker must claim the unrequired run: %v", err)
	}
	if c.ID != plain {
		t.Fatalf("base worker claimed %s, want the unrequired run %s", c.ID, plain)
	}
	// With only the docker and jvm runs left, a base worker is IDLE — it claims NEITHER.
	if _, err := claimCaps(fx, w, false, nil, nil, true); err != pgx.ErrNoRows {
		t.Fatalf("base worker must NOT claim a {docker}/{jvm}-required run; got %v, want pgx.ErrNoRows", err)
	}

	// Self-reported {docker} caps ⇒ claims the docker run (jvm still fenced).
	c, err = claimCaps(fx, w, false, nil, []string{"docker"}, true)
	if err != nil {
		t.Fatalf("a {docker}-capable worker must claim the docker run: %v", err)
	}
	if c.ID != dockerRun {
		t.Fatalf("claimed %s, want the docker run %s", c.ID, dockerRun)
	}
	// {jvm} caps ⇒ claims the jvm run (the jvm match is exercised even though its full
	// retrofit is M5).
	c, err = claimCaps(fx, w, false, nil, []string{"jvm"}, true)
	if err != nil {
		t.Fatalf("a {jvm}-capable worker must claim the jvm run: %v", err)
	}
	if c.ID != jvmRun {
		t.Fatalf("claimed %s, want the jvm run %s", c.ID, jvmRun)
	}
}

// TestClaimRunCapabilityFlagOffDegradesLiveDB is the kill-switch: with capability_aware
// false the same base worker claims a {docker}-required run best-effort (degraded mode).
func TestClaimRunCapabilityFlagOffDegradesLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	dockerRun := fx.queuedRun()
	setRunCaps(fx, dockerRun, []string{"docker"})

	// Flag ON first: the base worker is fenced (control).
	if _, err := claimCaps(fx, w, false, nil, nil, true); err != pgx.ErrNoRows {
		t.Fatalf("flag ON: base worker must be fenced from the docker run; got %v, want pgx.ErrNoRows", err)
	}
	// Flag OFF: the capability clause is trivially true, so the base worker claims it.
	c, err := claimCaps(fx, w, false, nil, nil, false)
	if err != nil {
		t.Fatalf("flag OFF: base worker must claim the docker run (degraded mode); got %v", err)
	}
	if c.ID != dockerRun {
		t.Fatalf("claimed %s, want the docker run %s", c.ID, dockerRun)
	}
}

// TestClaimRunCapabilityUnrequiredClaimsInBothStatesLiveDB pins the empty-required
// regression: a run requiring {} claims regardless of the flag.
func TestClaimRunCapabilityUnrequiredClaimsInBothStatesLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	run := fx.queuedRun() // required {}

	c, err := claimCaps(fx, w, false, nil, nil, true)
	if err != nil || c.ID != run {
		t.Fatalf("flag ON: an unrequired run must claim on any worker; got id=%s err=%v, want %s", c.ID, err, run)
	}
	requeueRun(fx, run)
	c, err = claimCaps(fx, w, false, nil, nil, false)
	if err != nil || c.ID != run {
		t.Fatalf("flag OFF: an unrequired run must claim on any worker; got id=%s err=%v, want %s", c.ID, err, run)
	}
}

// TestClaimRunDockerFoldAndAllowlistUngatedLiveDB proves two things at once:
//   - is_docker folds `docker` into the effective caps, so a hosted docker worker
//     (is_docker true) satisfies a {docker} requirement WITHOUT a self-report; and
//   - the shipped docker→allowlist clause is NEVER gated by capability_aware — a docker
//     worker with an EMPTY allowlist is fenced in BOTH flag states, so the flag reverts
//     ONLY #84's added capability clause (Decision 13).
func TestClaimRunDockerFoldAndAllowlistUngatedLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("W", capOf(4), false)
	dockerRun := fx.queuedRun()
	setRunCaps(fx, dockerRun, []string{"docker"})

	// is_docker=true + repo on the allowlist: folded `docker` satisfies the requirement
	// and the allowlist clause permits the repo, so the run claims (flag ON).
	c, err := claimCaps(fx, w, true, []uuid.UUID{fx.repoID}, nil, true)
	if err != nil {
		t.Fatalf("a hosted docker worker (allowlisted repo) must claim the docker run: %v", err)
	}
	if c.ID != dockerRun {
		t.Fatalf("claimed %s, want the docker run %s", c.ID, dockerRun)
	}

	// Empty allowlist, flag ON: the docker→allowlist clause fences a repo-bearing run
	// regardless of the capability match.
	requeueRun(fx, dockerRun)
	if _, err := claimCaps(fx, w, true, []uuid.UUID{}, nil, true); err != pgx.ErrNoRows {
		t.Fatalf("flag ON: a docker worker with an empty allowlist must be fenced; got %v, want pgx.ErrNoRows", err)
	}
	// Empty allowlist, flag OFF: STILL fenced — the flag does not touch the allowlist
	// clause, proving "off" reverts only the capability addition.
	if _, err := claimCaps(fx, w, true, []uuid.UUID{}, nil, false); err != pgx.ErrNoRows {
		t.Fatalf("flag OFF: the docker allowlist clause must still fence (never gated by the flag); got %v, want pgx.ErrNoRows", err)
	}
}

// TestClaimRunSpreadUnchangedUnderCapabilityFlagLiveDB re-runs the #216 steady-state
// spread with capability_aware ON and unrequired runs: two idle cap-2 workers, two fresh
// runs — A claims one then DEFERS the second to idle B — proving the added claimant AND
// peer capability clauses do not disturb the spread for unrequired runs.
func TestClaimRunSpreadUnchangedUnderCapabilityFlagLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false)
	b := fx.worker("B", capOf(2), false)
	r1, r2 := fx.queuedRun(), fx.queuedRun()

	c1, err := claimCaps(fx, a, false, nil, nil, true)
	if err != nil {
		t.Fatalf("A's first claim must succeed: %v", err)
	}
	if _, err := claimCaps(fx, a, false, nil, nil, true); err != pgx.ErrNoRows {
		t.Fatalf("A holding 1 must defer to idle peer B under the capability flag; got %v, want pgx.ErrNoRows", err)
	}
	c2, err := claimCaps(fx, b, false, nil, nil, true)
	if err != nil {
		t.Fatalf("B must claim the deferred run: %v", err)
	}
	if c1.ID == c2.ID {
		t.Fatalf("A and B claimed the same run %s", c1.ID)
	}
	got := map[uuid.UUID]bool{c1.ID: true, c2.ID: true}
	if !got[r1] || !got[r2] {
		t.Fatalf("claims %s,%s do not cover both runs %s,%s", c1.ID, c2.ID, r1, r2)
	}
}

// TestClaimRunCapabilityPeerEligibilityLiveDB exercises the peer-site edit (p.capabilities):
// a docker-required run defers ONLY to a docker-CAPABLE peer. A docker-capable claimant
// that is busy defers to a docker-capable idle peer; when the only idle peer is a BASE
// worker (not a valid target for the requirement), the claimant takes the run itself.
func TestClaimRunCapabilityPeerEligibilityLiveDB(t *testing.T) {
	// Case 1: docker-capable idle peer IS a valid deferral target.
	fx := newFleetFixture(t)
	a := fx.worker("A", capOf(2), false) // claimant, docker via param caps below
	bDocker := workerWithCaps(fx, "B", capOf(2), []string{"docker"})
	fx.holdActive(a, 1) // A busy: would defer to a less-loaded eligible peer
	run := fx.queuedRun()
	setRunCaps(fx, run, []string{"docker"})

	if _, err := claimCaps(fx, a, false, nil, []string{"docker"}, true); err != pgx.ErrNoRows {
		t.Fatalf("A must defer the docker run to the docker-capable peer; got %v, want pgx.ErrNoRows", err)
	}
	cB, err := claimCaps(fx, bDocker, false, nil, []string{"docker"}, true)
	if err != nil {
		t.Fatalf("docker-capable peer B must claim the deferred docker run: %v", err)
	}
	if cB.ID != run {
		t.Fatalf("peer B claimed %s, want %s", cB.ID, run)
	}

	// Case 2: the only idle peer is a BASE worker — not eligible for a docker run — so the
	// docker-capable claimant does NOT defer and takes the run itself.
	fx2 := newFleetFixture(t)
	a2 := fx2.worker("A", capOf(2), false)
	workerWithCaps(fx2, "B", capOf(2), []string{}) // base peer, idle
	fx2.holdActive(a2, 1)
	run2 := fx2.queuedRun()
	setRunCaps(fx2, run2, []string{"docker"})

	c, err := claimCaps(fx2, a2, false, nil, []string{"docker"}, true)
	if err != nil {
		t.Fatalf("A must claim itself when no peer can satisfy the docker run; got %v", err)
	}
	if c.ID != run2 {
		t.Fatalf("claimed %s, want %s", c.ID, run2)
	}
}

// TestCreateRunCopiesRepoCapabilityHintLiveDB pins step 5: createRun/CreateRun copies the
// repo's required_capabilities hint onto the new run via the INSERT subquery, while a
// repo with no hint yields the empty default.
func TestCreateRunCopiesRepoCapabilityHintLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	// Give the fixture's repo a static hint, then create a run against it.
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE repos SET required_capabilities = $2 WHERE id = $1`,
		fx.repoID, []string{"docker"})
	run, err := fx.q.CreateRun(fx.ctx, store.CreateRunParams{
		UserID: fx.userID, RepoID: fx.repoID,
		IssueIid: pgtype.Int8{Int64: fx.nextIID(), Valid: true}, IssueTitle: "hinted", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if len(run.RequiredCapabilities) != 1 || run.RequiredCapabilities[0] != "docker" {
		t.Fatalf("run.RequiredCapabilities = %v, want [docker] copied from the repo hint", run.RequiredCapabilities)
	}

	// A second repo with NO hint yields the empty default on its run.
	plainRepo := uuid.New()
	insertPlainRepo(fx, plainRepo)
	run2, err := fx.q.CreateRun(fx.ctx, store.CreateRunParams{
		UserID: fx.userID, RepoID: plainRepo,
		IssueIid: pgtype.Int8{Int64: fx.nextIID(), Valid: true}, IssueTitle: "plain", IssueDescription: "d", PlanSource: "agent", TriggerSource: "manual",
	})
	if err != nil {
		t.Fatalf("CreateRun(plain): %v", err)
	}
	if len(run2.RequiredCapabilities) != 0 {
		t.Fatalf("run2.RequiredCapabilities = %v, want empty (repo has no hint)", run2.RequiredCapabilities)
	}
}

// insertPlainRepo adds a second enabled repo under the fixture's connection.
func insertPlainRepo(fx *fleetFixture, repoID uuid.UUID) {
	fx.t.Helper()
	var connID uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT connection_id FROM repos WHERE id = $1`, fx.repoID).Scan(&connID); err != nil {
		fx.t.Fatalf("resolve connection: %v", err)
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 2, 'g/plain', 'https://forge.e2e/g/plain', 'main', true)`, repoID, connID)
}
