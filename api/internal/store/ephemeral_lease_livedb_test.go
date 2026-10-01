package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2006: the store-layer half of the ephemeral worker lease, against a REAL Postgres. A finished
// ephemeral worker keeps its row for a bounded lease and may claim a same-owner, same-repository,
// same-branch follow-up run; the claimant clause, the spread-peer mirror, the placement mirrors, the
// reaper, the teardown delete, the eviction pair and the transitions (enter, rebind, register,
// cordon) are all SQL the fake store cannot exhibit. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (newFleetFixture skips). Reuses fleetFixture and the ephemeral helpers from the
// sibling ephemeral_*_test.go files.

const leaseTwoHours = 2 * time.Hour

func leaseInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// leaseRun describes a run row for the lease fixtures. Zero values mean: an issue run, queued,
// unowned, on the fixture's repo and user, NULL branch, NULL pipeline_ref, iid fx.nextIID().
type leaseRun struct {
	kind        string
	status      string
	workerID    *uuid.UUID
	userID      uuid.UUID
	repoID      uuid.UUID
	repoless    bool
	iid         *int64
	branch      *string
	pipelineRef *string
	snapshot    *string
	targetRunID *uuid.UUID
}

func strp(s string) *string { return &s }
func i64p(n int64) *int64   { return &n }

// insertLeaseRun inserts the run and returns its id. kind-specific required columns (the
// runs_kind_shape CHECK) are filled in: ci_fix gets a pipeline_id, mr_rework an mr_iid and a target
// run, judge a target run, task a dispatched_at and a branch.
func insertLeaseRun(fx *fleetFixture, s leaseRun) uuid.UUID {
	fx.t.Helper()
	if s.kind == "" {
		s.kind = "issue"
	}
	if s.status == "" {
		s.status = "queued"
	}
	if s.userID == uuid.Nil {
		s.userID = fx.userID
	}
	if s.repoID == uuid.Nil {
		s.repoID = fx.repoID
	}
	id := uuid.New()
	var repo any = s.repoID
	if s.repoless {
		repo = nil
	}
	var iid any
	switch {
	case s.iid != nil:
		iid = *s.iid
	case s.kind == "issue" || s.kind == "self_improve":
		iid = fx.nextIID()
	}
	var worker any
	if s.workerID != nil {
		worker = *s.workerID
	}
	var branch, pref, snap, target, mrIID, pipelineID, dispatched any
	if s.branch != nil {
		branch = *s.branch
	}
	if s.pipelineRef != nil {
		pref = *s.pipelineRef
	}
	if s.snapshot != nil {
		snap = *s.snapshot
	}
	if s.targetRunID != nil {
		target = *s.targetRunID
	}
	switch s.kind {
	case "ci_fix":
		pipelineID = fx.nextIID() + 1000
	case "mr_rework":
		mrIID = fx.nextIID() + 2000
	case "task":
		dispatched = time.Now()
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
		                   branch, pipeline_ref, failure_snapshot, target_run_id, mr_iid, pipeline_id, dispatched_at)
		 VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, $9, $10::jsonb, $11, $12, $13, $14)`,
		id, s.userID, repo, s.kind, iid, s.status, worker, branch, pref, snap, target, mrIID, pipelineID, dispatched)
	return id
}

// leasedWorker is one ephemeral worker that served `served` and entered its lease through the real
// EnterEphemeralLease query.
type leasedWorker struct {
	id     uuid.UUID
	served uuid.UUID
}

// seedLeasedWorker creates the ephemeral worker bound to a freshly inserted run `spec`
// (status forced to completed), marks it online with a fresh heartbeat and a slot cap, and enters
// the lease via EnterEphemeralLease (which must match one row). leaseAge backdates lease_since.
func seedLeasedWorker(fx *fleetFixture, spec leaseRun, leaseAge time.Duration) leasedWorker {
	fx.t.Helper()
	spec.status = "completed"
	// The bound run is inserted first without a worker (the worker does not exist yet), then pointed at it.
	served := insertLeaseRun(fx, spec)
	w := seedEphemeralWorkerBound(fx, served)
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET worker_id = $2 WHERE id = $1`, served, w)
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE workers SET status = 'online', last_heartbeat_at = now(), online_since = now(), max_concurrent_runs = 2
		  WHERE id = $1`, w)
	n, err := fx.q.EnterEphemeralLease(fx.ctx, store.EnterEphemeralLeaseParams{
		WorkerID: w, RunID: served})
	if err != nil || n != 1 {
		fx.t.Fatalf("EnterEphemeralLease = (%d, %v), want (1, nil)", n, err)
	}
	if leaseAge > 0 {
		mustExec(fx.ctx, fx.t, fx.pool,
			`UPDATE workers SET lease_since = now() - $2::interval WHERE id = $1`, w, leaseAge.String())
	}
	return leasedWorker{id: w, served: served}
}

func leaseSinceOf(fx *fleetFixture, w uuid.UUID) (pgtype.Timestamptz, pgtype.UUID, pgtype.Text) {
	fx.t.Helper()
	row, err := fx.q.GetWorkerByID(fx.ctx, w)
	if err != nil {
		fx.t.Fatalf("GetWorkerByID: %v", err)
	}
	return row.LeaseSince, row.LeaseRepoID, row.LeaseBranch
}

func hasLease(fx *fleetFixture, w uuid.UUID) bool {
	fx.t.Helper()
	since, repo, branch := leaseSinceOf(fx, w)
	if since.Valid != repo.Valid || since.Valid != branch.Valid {
		fx.t.Fatalf("lease columns set partially: %v %v %v", since.Valid, repo.Valid, branch.Valid)
	}
	return since.Valid
}

// claimLeased claims for worker w exactly as workersvc.Claim will: the claimant lease columns come
// from the worker row, the interval is the operator's lease, leaseAt the admission instant.
func claimLeased(fx *fleetFixture, w leasedWorker, interval pgtype.Interval, leaseAt time.Time, mut ...func(*store.ClaimRunParams)) (store.Run, error) {
	fx.t.Helper()
	since, repo, branch := leaseSinceOf(fx, w.id)
	p := store.ClaimRunParams{
		WorkerID:            pgtype.UUID{Bytes: w.id, Valid: true},
		UserID:              fx.userID,
		AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:      true,
		DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		IsEphemeral:         true,
		EphemeralRunID:      pgtype.UUID{Bytes: w.served, Valid: true},
		LeaseSince:          since,
		LeaseRepoID:         repo,
		LeaseBranch:         branch,
		EphemeralLease:      interval,
		LeaseAt:             pgtype.Timestamptz{Time: leaseAt, Valid: true},
	}
	for _, m := range mut {
		m(&p)
	}
	return fx.q.ClaimRun(fx.ctx, p)
}

func wantClaimed(t *testing.T, got store.Run, err error, want uuid.UUID) {
	t.Helper()
	if err != nil {
		t.Fatalf("claim: %v, want run %s claimed", err, want)
	}
	if got.ID != want {
		t.Fatalf("claimed %s, want %s", got.ID, want)
	}
}

func wantIdle(t *testing.T, got store.Run, err error) {
	t.Helper()
	if err != pgx.ErrNoRows {
		t.Fatalf("claim = (%v, %v), want pgx.ErrNoRows (the lease must not admit this run)", got.ID, err)
	}
}

// TestEphemeralLeaseClaimSameBranchLiveDB: a live leased worker claims a queued run of the same
// owner, repository and effective branch identity, for each supported kind.
func TestEphemeralLeaseClaimSameBranchLiveDB(t *testing.T) {
	t.Run("issue run, NULL branch, canonical branch from issue_iid", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(7)}, 0)
		f := insertLeaseRun(fx, leaseRun{iid: i64p(7)}) // follow-up re-run of the same issue
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, f)
	})
	t.Run("issue run served with an explicit branch, follow-up with NULL branch", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(8), branch: strp("agent/issue-8")}, 0)
		f := insertLeaseRun(fx, leaseRun{iid: i64p(8)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, f)
	})
	t.Run("mr_rework through pipeline_ref", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(9)}, 0) // lease branch agent/issue-9
		f := insertLeaseRun(fx, leaseRun{kind: "mr_rework", pipelineRef: strp("agent/issue-9"), targetRunID: &w.served})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, f)
	})
	t.Run("ci_fix through the failure snapshot ref", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(10)}, 0)
		f := insertLeaseRun(fx, leaseRun{kind: "ci_fix", pipelineRef: strp("agent/issue-10"),
			snapshot: strp(`{"ref":"agent/issue-10","pipeline_id":5}`)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, f)
	})
	t.Run("a worker served by an mr_rework leases its pipeline_ref and takes the issue re-run", func(t *testing.T) {
		fx := newFleetFixture(t)
		base := insertLeaseRun(fx, leaseRun{iid: i64p(11), status: "completed"})
		w := seedLeasedWorker(fx, leaseRun{kind: "mr_rework", pipelineRef: strp("agent/issue-11"), targetRunID: &base}, 0)
		if _, _, b := leaseSinceOf(fx, w.id); b.String != "agent/issue-11" {
			t.Fatalf("lease_branch = %q, want agent/issue-11", b.String)
		}
		f := insertLeaseRun(fx, leaseRun{iid: i64p(11)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, f)
	})
}

// TestEphemeralLeaseNeverClaimedLiveDB: every run the lease must NOT admit stays queued. Where the
// refusal is a property of the RUN, a same-branch control run inserted afterwards must be claimed by
// the same params, proving the refusal came from the run and not from the harness.
func TestEphemeralLeaseNeverClaimedLiveDB(t *testing.T) {
	type tc struct {
		name string
		// mk inserts the run the lease must refuse; served iid is 20 (lease branch agent/issue-20).
		mk func(fx *fleetFixture, w leasedWorker) uuid.UUID
	}
	otherRepo := func(fx *fleetFixture) uuid.UUID {
		fx.t.Helper()
		var connID uuid.UUID
		if err := fx.pool.QueryRow(fx.ctx, `SELECT id FROM forge_connections WHERE user_id = $1`, fx.userID).Scan(&connID); err != nil {
			fx.t.Fatalf("connection: %v", err)
		}
		id := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, 2, 'g/other', 'https://forge.e2e/g/other', 'main', true)`, id, connID)
		return id
	}
	cases := []tc{
		{"another issue (another branch)", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{iid: i64p(21)})
		}},
		{"an issue run with an explicit different branch", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{iid: i64p(120), branch: strp("agent/issue-99")})
		}},
		{"another repository, same branch", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{iid: i64p(20), repoID: otherRepo(fx)})
		}},
		{"another owner's run, same repository and branch", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			other := uuid.New()
			mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
				other, fmt.Sprintf("other-%s@e2e", other))
			return insertLeaseRun(fx, leaseRun{iid: i64p(130), branch: strp("agent/issue-20"), userID: other})
		}},
		{"an issue run with no derivable identity (issue_iid 0, NULL branch)", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{iid: i64p(0)})
		}},
		{"an mr_rework on another branch", func(fx *fleetFixture, w leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{kind: "mr_rework", pipelineRef: strp("agent/issue-77"), targetRunID: &w.served})
		}},
		{"a ci_fix whose snapshot has no ref", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{kind: "ci_fix", pipelineRef: strp("agent/other-1"), snapshot: strp(`{"pipeline_id":5}`)})
		}},
		{"a ci_fix whose ref is not a string", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{kind: "ci_fix", pipelineRef: strp("agent/other-2"), snapshot: strp(`{"ref":20}`)})
		}},
		{"an unknown kind (task) even on the leased branch", func(fx *fleetFixture, _ leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{kind: "task", branch: strp("agent/issue-20")})
		}},
		{"a repo-less run (judge)", func(fx *fleetFixture, w leasedWorker) uuid.UUID {
			return insertLeaseRun(fx, leaseRun{kind: "judge", repoless: true, targetRunID: &w.served})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
			refused := c.mk(fx, w)
			got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
			wantIdle(t, got, err)
			// Control: a same-branch run is claimable by the very same params.
			control := insertLeaseRun(fx, leaseRun{iid: i64p(20)})
			got, err = claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
			wantClaimed(t, got, err, control)
			var st string
			if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id = $1`, refused).Scan(&st); err != nil || st != "queued" {
				t.Fatalf("refused run status = %q (%v), want queued", st, err)
			}
		})
	}

	t.Run("an issue run reporting another issue's branch is not admitted by that issue's lease", func(t *testing.T) {
		// The worker's terminal report chooses runs.branch, so it must never choose a lease identity:
		// issue 20 reporting agent/issue-21 must not ride the lease of the worker that served issue 21.
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(21)}, 0)
		spoof := insertLeaseRun(fx, leaseRun{iid: i64p(20), branch: strp("agent/issue-21")})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantIdle(t, got, err)
		control := insertLeaseRun(fx, leaseRun{iid: i64p(21)})
		got, err = claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantClaimed(t, got, err, control)
		var st string
		if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id = $1`, spoof).Scan(&st); err != nil || st != "queued" {
			t.Fatalf("spoofing run status = %q (%v), want queued", st, err)
		}
	})
	t.Run("a served issue run that reported another issue's branch gets no lease and claims nothing", func(t *testing.T) {
		fx := newFleetFixture(t)
		served := insertLeaseRun(fx, leaseRun{iid: i64p(20), status: "completed", branch: strp("agent/issue-21")})
		w := seedEphemeralWorkerBound(fx, served)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, served, w)
		mustExec(fx.ctx, fx.t, fx.pool,
			`UPDATE workers SET status = 'online', last_heartbeat_at = now(), online_since = now(), max_concurrent_runs = 2 WHERE id = $1`, w)
		n, err := fx.q.EnterEphemeralLease(fx.ctx, store.EnterEphemeralLeaseParams{WorkerID: w, RunID: served})
		if err != nil || n != 0 || hasLease(fx, w) {
			t.Fatalf("EnterEphemeralLease = (%d, %v), lease=%v; a reported non-canonical branch must give no lease", n, err, hasLease(fx, w))
		}
		issue21 := insertLeaseRun(fx, leaseRun{iid: i64p(21)})
		got, err := claimLeased(fx, leasedWorker{id: w, served: served}, leaseInterval(leaseTwoHours), time.Now())
		wantIdle(t, got, err)
		_ = issue21
	})
	t.Run("a claimant holding an open custody hold claims nothing through the lease", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
		insertCustodyHold(fx, w.served, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: w.id, Valid: true}, "open")
		insertLeaseRun(fx, leaseRun{iid: i64p(20)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantIdle(t, got, err)
	})
	t.Run("an expired lease admits nothing (@lease_at past expiry)", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
		insertLeaseRun(fx, leaseRun{iid: i64p(20)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now().Add(leaseTwoHours+time.Minute))
		wantIdle(t, got, err)
	})
	t.Run("lease interval 0 or unset admits nothing, even with the lease columns set", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
		insertLeaseRun(fx, leaseRun{iid: i64p(20)})
		for _, iv := range []pgtype.Interval{{}, leaseInterval(0)} {
			got, err := claimLeased(fx, w, iv, time.Now())
			wantIdle(t, got, err)
		}
	})
	// ClaimRun refuses a draining claimant by its own clause too, so this subtest does not isolate the
	// draining check inside fn_ephemeral_lease_admits; TestEphemeralLeaseAdmitsFunctionLiveDB does.
	t.Run("a draining claimant claims nothing through the lease", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
		insertLeaseRun(fx, leaseRun{iid: i64p(20)})
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now(), func(p *store.ClaimRunParams) { p.ClaimantDraining = true })
		wantIdle(t, got, err)
	})
	t.Run("a run another ephemeral worker is already bound to is not claimed through the lease", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(20)}, 0)
		f := insertLeaseRun(fx, leaseRun{iid: i64p(20)})
		seedEphemeralWorkerBound(fx, f) // the provisioner bound its own worker to f first
		got, err := claimLeased(fx, w, leaseInterval(leaseTwoHours), time.Now())
		wantIdle(t, got, err)
	})
	t.Run("a never-leased ephemeral worker still claims only its bound run", func(t *testing.T) {
		fx := newFleetFixture(t)
		bound := queuedRunWithCaps(fx, []string{"docker"})
		other := insertLeaseRun(fx, leaseRun{iid: i64p(5)})
		w := ephemeralWorker(fx, "E", bound, true)
		got, err := claimEphemeral(fx, w, true, bound, true, []uuid.UUID{fx.repoID}, nil)
		wantClaimed(t, got, err, bound)
		if _, err := claimEphemeral(fx, w, true, bound, true, []uuid.UUID{fx.repoID}, nil); err != pgx.ErrNoRows {
			t.Fatalf("claimed a foreign run %s without a lease: %v", other, err)
		}
	})
}

// TestEphemeralLeaseConstraintAndAdmitsFunctionLiveDB pins the schema-level guarantees: the lease
// columns are all-or-nothing, only a non-isolated ephemeral worker may hold one, and
// fn_ephemeral_lease_admits refuses a profile-bound run (which no lease may serve).
func TestEphemeralLeaseConstraintAndAdmitsFunctionLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := seedLeasedWorker(fx, leaseRun{iid: i64p(30)}, 0)

	for _, set := range []string{
		`UPDATE workers SET lease_branch = NULL WHERE id = $1`,                        // partial lease
		`UPDATE workers SET lease_branch = '' WHERE id = $1`,                          // empty identity
		`UPDATE workers SET isolated_lane = true WHERE id = $1`,                       // lease on a lane worker
		`UPDATE workers SET ephemeral = false, ephemeral_run_id = NULL WHERE id = $1`, // lease on a persistent worker
	} {
		_, err := fx.pool.Exec(fx.ctx, set, w.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != "ck_workers_ephemeral_lease" {
			t.Errorf("%q: err = %v, want a violation of ck_workers_ephemeral_lease", set, err)
		}
	}

	var admits bool
	args := []any{fx.repoID, "agent/issue-30"}
	probe := func(egress any) bool {
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT fn_ephemeral_lease_admits(now(), $1::uuid, $2::text, false, interval '1 hour', now(),
			        $1::uuid, 'issue', NULL, NULL, 30, NULL, $3::uuid)`, append(args, egress)...).Scan(&admits); err != nil {
			t.Fatalf("fn_ephemeral_lease_admits: %v", err)
		}
		return admits
	}
	if !probe(nil) {
		t.Fatalf("fn_ephemeral_lease_admits(no egress profile) = false, want true")
	}
	if probe(uuid.New()) {
		t.Fatalf("fn_ephemeral_lease_admits admitted a profile-bound run")
	}
}

// TestEphemeralLeaseZeroEquivalenceLiveDB: lease 0 (an unset or zero interval) reproduces today's
// behaviour exactly, even when a worker's lease columns are set; a positive lease spares a live lease
// and reaps an expired one.
func TestEphemeralLeaseZeroEquivalenceLiveDB(t *testing.T) {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
	for name, iv := range map[string]pgtype.Interval{"unset interval": {}, "zero interval": leaseInterval(0)} {
		t.Run("DeleteEphemeralWorkerForRun deletes a leased worker at "+name, func(t *testing.T) {
			fx := newFleetFixture(t)
			w := seedLeasedWorker(fx, leaseRun{iid: i64p(40)}, 0)
			rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, store.DeleteEphemeralWorkerForRunParams{RunID: w.served, EphemeralLease: iv})
			if err != nil || rows != 1 || workerExists(fx, w.id) {
				t.Fatalf("delete = (%d, %v), exists=%v; lease 0 must delete exactly as before", rows, err, workerExists(fx, w.id))
			}
		})
		t.Run("the reaper reaps a leased worker at "+name, func(t *testing.T) {
			fx := newFleetFixture(t)
			w := seedLeasedWorker(fx, leaseRun{iid: i64p(41)}, 0)
			if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cutoff, iv); err != nil || workerExists(fx, w.id) {
				t.Fatalf("reap err=%v, exists=%v; lease 0 must reap a worker whose bound run is terminal", err, workerExists(fx, w.id))
			}
		})
	}

	t.Run("a live lease spares the worker from teardown and the reaper", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(42)}, 0)
		if rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, store.DeleteEphemeralWorkerForRunParams{RunID: w.served, EphemeralLease: leaseInterval(leaseTwoHours)}); err != nil || rows != 0 {
			t.Fatalf("delete = (%d, %v), want 0 rows for a live lease", rows, err)
		}
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cutoff, leaseInterval(leaseTwoHours)); err != nil {
			t.Fatalf("reap: %v", err)
		}
		if !workerExists(fx, w.id) {
			t.Fatal("leased worker was removed")
		}
	})
	t.Run("an expired lease is torn down and reaped", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(43)}, 3*time.Hour)
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cutoff, leaseInterval(leaseTwoHours)); err != nil || workerExists(fx, w.id) {
			t.Fatalf("reap err=%v, exists=%v; an expired lease must be reaped", err, workerExists(fx, w.id))
		}
		w2 := seedLeasedWorker(fx, leaseRun{iid: i64p(44)}, 3*time.Hour)
		if rows, err := fx.q.DeleteEphemeralWorkerForRun(fx.ctx, store.DeleteEphemeralWorkerForRunParams{RunID: w2.served, EphemeralLease: leaseInterval(leaseTwoHours)}); err != nil || rows != 1 {
			t.Fatalf("delete = (%d, %v), want 1 row for an expired lease", rows, err)
		}
	})
	t.Run("an unlinked leased worker (bound run deleted) is reaped despite a live lease", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(46)}, 0)
		// Deleting the served run nulls workers.ephemeral_run_id (FK SET NULL): no claim can ever re-bind
		// this worker, so the lease must not keep it alive.
		mustExec(fx.ctx, fx.t, fx.pool, `DELETE FROM runs WHERE id = $1`, w.served)
		row, err := fx.q.GetWorkerByID(fx.ctx, w.id)
		if err != nil || row.EphemeralRunID.Valid || !hasLease(fx, w.id) {
			t.Fatalf("harness: worker row = (%v, %v), lease=%v; want an unlinked worker still holding its lease", row.EphemeralRunID, err, hasLease(fx, w.id))
		}
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cutoff, leaseInterval(leaseTwoHours)); err != nil || workerExists(fx, w.id) {
			t.Fatalf("reap err=%v, exists=%v; an unlinked leased worker must be reaped at the next tick", err, workerExists(fx, w.id))
		}
	})
	t.Run("a custody-held leased worker is never reaped, expired or not", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(45)}, 3*time.Hour)
		insertCustodyHold(fx, w.served, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: w.id, Valid: true}, "open")
		if _, err := store.ReapEphemeralWorkers(fx.ctx, fx.pool, cutoff, leaseInterval(leaseTwoHours)); err != nil || !workerExists(fx, w.id) {
			t.Fatalf("reap err=%v exists=%v, want the custody-held worker kept", err, workerExists(fx, w.id))
		}
	})
	t.Run("the reaper's selected rows are the pre-lease ones for the same unleased fixtures", func(t *testing.T) {
		fx := newFleetFixture(t)
		runA := fx.queuedRun()
		live := seedEphemeralWorkerBound(fx, runA) // bound run still queued: kept
		runB := fx.queuedRun()
		orphan := seedEphemeralWorkerBound(fx, runB)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, runB)
		for _, iv := range []pgtype.Interval{{}, leaseInterval(0), leaseInterval(leaseTwoHours)} {
			ids, err := fx.q.LockReapableEphemeralWorkers(fx.ctx, store.LockReapableEphemeralWorkersParams{EphemeralLease: iv, DeadlineCutoff: cutoff})
			if err != nil {
				t.Fatalf("LockReapableEphemeralWorkers: %v", err)
			}
			// The database is shared with the other fixtures, so assert on this fixture's workers.
			var sawOrphan, sawLive bool
			for _, id := range ids {
				sawOrphan = sawOrphan || id == orphan
				sawLive = sawLive || id == live
			}
			if !sawOrphan || sawLive {
				t.Fatalf("selected %v: want the terminal-bound %s and not the live-bound %s", ids, orphan, live)
			}
		}
	})
}

// TestRebindLeasedEphemeralWorkerLiveDB: the in-claim rebind moves the worker to the new run and ends
// the lease only while the lease is live at the statement's own clock.
func TestRebindLeasedEphemeralWorkerLiveDB(t *testing.T) {
	rebind := func(fx *fleetFixture, w leasedWorker, next uuid.UUID, iv pgtype.Interval) int64 {
		fx.t.Helper()
		n, err := fx.q.RebindLeasedEphemeralWorker(fx.ctx, store.RebindLeasedEphemeralWorkerParams{
			NewRunID: next, WorkerID: w.id, OldRunID: w.served, EphemeralLease: iv})
		if err != nil {
			fx.t.Fatalf("RebindLeasedEphemeralWorker: %v", err)
		}
		return n
	}
	t.Run("a live lease rebinds and clears", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(50)}, 0)
		next := insertLeaseRun(fx, leaseRun{iid: i64p(50)})
		if n := rebind(fx, w, next, leaseInterval(leaseTwoHours)); n != 1 {
			t.Fatalf("rebind rows = %d, want 1", n)
		}
		row, _ := fx.q.GetWorkerByID(fx.ctx, w.id)
		if row.EphemeralRunID.Bytes != next || hasLease(fx, w.id) {
			t.Fatalf("worker not rebound/cleared: bound=%v lease=%v", row.EphemeralRunID.Bytes, hasLease(fx, w.id))
		}
		if n := rebind(fx, w, next, leaseInterval(leaseTwoHours)); n != 0 {
			t.Fatalf("a second rebind from the old run matched %d rows, want 0", n)
		}
	})
	t.Run("an expired lease does not rebind, whatever instant the claim admitted at", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(51)}, 3*time.Hour)
		next := insertLeaseRun(fx, leaseRun{iid: i64p(51)})
		if n := rebind(fx, w, next, leaseInterval(leaseTwoHours)); n != 0 {
			t.Fatalf("rebind rows = %d, want 0 for an expired lease", n)
		}
	})
	t.Run("lease 0 and a draining worker never rebind", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(52)}, 0)
		next := insertLeaseRun(fx, leaseRun{iid: i64p(52)})
		if n := rebind(fx, w, next, pgtype.Interval{}); n != 0 {
			t.Fatalf("rebind at an unset interval matched %d rows", n)
		}
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, w.id)
		if n := rebind(fx, w, next, leaseInterval(leaseTwoHours)); n != 0 {
			t.Fatalf("rebind of a draining worker matched %d rows", n)
		}
	})
	t.Run("a run another ephemeral worker holds is a unique violation, not a silent rebind", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(53)}, 0)
		next := insertLeaseRun(fx, leaseRun{iid: i64p(53)})
		seedEphemeralWorkerBound(fx, next)
		_, err := fx.q.RebindLeasedEphemeralWorker(fx.ctx, store.RebindLeasedEphemeralWorkerParams{
			NewRunID: next, WorkerID: w.id, OldRunID: w.served, EphemeralLease: leaseInterval(leaseTwoHours)})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "uq_workers_ephemeral_run" {
			t.Fatalf("rebind onto an already-bound run: err = %v, want SQLSTATE 23505 on uq_workers_ephemeral_run", err)
		}
	})
}

// TestEnterEphemeralLeaseGuardsLiveDB: lease entry matches exactly the worker that served its bound
// terminal run, and none of the guarded shapes.
func TestEnterEphemeralLeaseGuardsLiveDB(t *testing.T) {
	enter := func(fx *fleetFixture, w, run uuid.UUID) int64 {
		fx.t.Helper()
		n, err := fx.q.EnterEphemeralLease(fx.ctx, store.EnterEphemeralLeaseParams{
			WorkerID: w, RunID: run})
		if err != nil {
			fx.t.Fatalf("EnterEphemeralLease: %v", err)
		}
		return n
	}
	// fresh seeds an ephemeral worker bound to a run of the given spec, already pointed at the worker.
	fresh := func(fx *fleetFixture, spec leaseRun) (uuid.UUID, uuid.UUID) {
		fx.t.Helper()
		run := insertLeaseRun(fx, spec)
		w := seedEphemeralWorkerBound(fx, run)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, run, w)
		return w, run
	}

	t.Run("a completed repo-backed run leases with its repo and effective branch", func(t *testing.T) {
		fx := newFleetFixture(t)
		w, run := fresh(fx, leaseRun{iid: i64p(60), status: "completed"})
		if enter(fx, w, run) != 1 {
			t.Fatal("lease not entered")
		}
		since, repo, branch := leaseSinceOf(fx, w)
		if !since.Valid || repo.Bytes != fx.repoID || branch.String != "agent/issue-60" {
			t.Fatalf("lease = (%v, %v, %q), want the fixture repo and agent/issue-60", since.Valid, repo.Bytes, branch.String)
		}
		if time.Since(since.Time) > time.Minute {
			t.Fatalf("lease_since %v is not fresh", since.Time)
		}
	})
	t.Run("a failed run without a hold leases too", func(t *testing.T) {
		fx := newFleetFixture(t)
		w, run := fresh(fx, leaseRun{iid: i64p(61), status: "failed"})
		if enter(fx, w, run) != 1 {
			t.Fatal("lease not entered")
		}
	})
	t.Run("a cancelled run never leases", func(t *testing.T) {
		fx := newFleetFixture(t)
		w, run := fresh(fx, leaseRun{iid: i64p(61), status: "cancelled"})
		if enter(fx, w, run) != 0 {
			t.Fatal("a cancelled run entered a lease")
		}
	})
	guards := []struct {
		name string
		mk   func(fx *fleetFixture) (uuid.UUID, uuid.UUID)
	}{
		{"a non-terminal bound run", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			return fresh(fx, leaseRun{iid: i64p(62), status: "running"})
		}},
		{"an open custody hold", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			w, run := fresh(fx, leaseRun{iid: i64p(63), status: "completed"})
			insertCustodyHold(fx, run, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: w, Valid: true}, "open")
			return w, run
		}},
		{"another non-terminal run on the worker", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			w, run := fresh(fx, leaseRun{iid: i64p(64), status: "completed"})
			insertLeaseRun(fx, leaseRun{iid: i64p(65), status: "running", workerID: &w})
			return w, run
		}},
		{"a draining worker", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			w, run := fresh(fx, leaseRun{iid: i64p(66), status: "completed"})
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, w)
			return w, run
		}},
		{"an isolated-lane worker", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			w, run := fresh(fx, leaseRun{iid: i64p(67), status: "completed"})
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET isolated_lane = true WHERE id = $1`, w)
			return w, run
		}},
		{"a run with no derivable identity", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			return fresh(fx, leaseRun{iid: i64p(0), status: "completed"})
		}},
		{"an issue run that reported a non-canonical branch", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			return fresh(fx, leaseRun{iid: i64p(72), status: "completed", branch: strp("feature/x")})
		}},
		{"an issue run that reported another issue's branch", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			return fresh(fx, leaseRun{iid: i64p(73), status: "completed", branch: strp("agent/issue-74")})
		}},
		{"a repo-less run", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			base := insertLeaseRun(fx, leaseRun{iid: i64p(68), status: "completed"})
			return fresh(fx, leaseRun{kind: "judge", repoless: true, status: "completed", targetRunID: &base})
		}},
		{"a run the worker did not serve", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			run := insertLeaseRun(fx, leaseRun{iid: i64p(69), status: "completed"})
			w := seedEphemeralWorkerBound(fx, run) // runs.worker_id stays NULL
			return w, run
		}},
		{"a worker not bound to that run", func(fx *fleetFixture) (uuid.UUID, uuid.UUID) {
			w, _ := fresh(fx, leaseRun{iid: i64p(70), status: "completed"})
			other := insertLeaseRun(fx, leaseRun{iid: i64p(71), status: "completed", workerID: &w})
			return w, other
		}},
	}
	for _, g := range guards {
		t.Run("no lease for "+g.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			w, run := g.mk(fx)
			if n := enter(fx, w, run); n != 0 {
				t.Fatalf("EnterEphemeralLease matched %d rows, want 0", n)
			}
			if hasLease(fx, w) {
				t.Fatal("lease columns set despite 0 rows")
			}
		})
	}
}

// TestEphemeralLeaseEndedByRegisterAndCordonLiveDB: a register (a fresh pod incarnation) and a cordon
// each end the lease, and nothing else about the row is disturbed.
func TestEphemeralLeaseEndedByRegisterAndCordonLiveDB(t *testing.T) {
	t.Run("RegisterWorker clears the lease", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(80)}, 0)
		if _, err := fx.q.RegisterWorker(fx.ctx, store.RegisterWorkerParams{
			ID: w.id, Version: pgtype.Text{String: "v", Valid: true}, SnapshotRegisterNonce: pgtype.Text{String: "n", Valid: true},
		}); err != nil {
			t.Fatalf("RegisterWorker: %v", err)
		}
		if hasLease(fx, w.id) {
			t.Fatal("RegisterWorker left the lease set")
		}
	})
	t.Run("CordonHostedWorker clears the lease and drains", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(81)}, 0)
		if n, err := fx.q.CordonHostedWorker(fx.ctx, w.id); err != nil || n != 1 {
			t.Fatalf("CordonHostedWorker = (%d, %v)", n, err)
		}
		if hasLease(fx, w.id) {
			t.Fatal("CordonHostedWorker left the lease set")
		}
		row, _ := fx.q.GetWorkerByID(fx.ctx, w.id)
		if !row.DrainingSince.Valid {
			t.Fatal("worker not draining after cordon")
		}
	})
}

// TestLeaseClockNowLiveDB: the admission clock is a fresh instant, not the transaction-start now().
func TestLeaseClockNowLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	tx, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(fx.ctx) }()
	var start time.Time
	if err := tx.QueryRow(fx.ctx, `SELECT now()`).Scan(&start); err != nil {
		t.Fatalf("now: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	at, err := store.New(tx).LeaseClockNow(fx.ctx)
	if err != nil || !at.Valid {
		t.Fatalf("LeaseClockNow = (%v, %v)", at, err)
	}
	if !at.Time.After(start.Add(40 * time.Millisecond)) {
		t.Fatalf("LeaseClockNow %v is not after the transaction start %v: it must be clock_timestamp()", at.Time, start)
	}
}

// provisioningFixture opts the fixture user in and returns the ids each provisioning trigger lists.
func listedUnplaceable(fx *fleetFixture, maxPerUser int32, iv pgtype.Interval) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		EphemeralLease: iv, MaxPerUser: maxPerUser, MaxRows: 50})
	if err != nil {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func listedSaturation(fx *fleetFixture, maxPerUser int32, iv pgtype.Interval) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		SaturationDelay: leaseInterval(0), EphemeralLease: iv, MaxPerUser: maxPerUser, MaxRows: 50})
	if err != nil {
		fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func listedIsolated(fx *fleetFixture, maxPerUser int32) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListIsolatedQueuedRunsForEphemeral(fx.ctx, store.ListIsolatedQueuedRunsForEphemeralParams{
		MaxPerUser: maxPerUser, MaxRows: 50})
	if err != nil {
		fx.t.Fatalf("ListIsolatedQueuedRunsForEphemeral: %v", err)
	}
	out := map[uuid.UUID]bool{}
	for _, r := range rows {
		out[r.ID] = true
	}
	return out
}

func claimableCount(fx *fleetFixture, run uuid.UUID, iv pgtype.Interval) int64 {
	fx.t.Helper()
	n, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID:               run,
		HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		DockerRepoAllowlist: []uuid.UUID{fx.repoID},
		EphemeralLease:      iv,
	})
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
	}
	return n
}

// TestEphemeralLeasePlacementMirrorLiveDB: wherever ephemeral eligibility is computed, a run a live
// leased worker can take is treated as placed, and a run it cannot take is still provisioned for.
func TestEphemeralLeasePlacementMirrorLiveDB(t *testing.T) {
	iv := leaseInterval(leaseTwoHours)
	docker := []string{"docker"}

	t.Run("capability-gap trigger skips a run the leased worker can take", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(90)}, 0)
		same := insertLeaseRun(fx, leaseRun{iid: i64p(90)})
		other := insertLeaseRun(fx, leaseRun{iid: i64p(91)})
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET required_capabilities = $2 WHERE id = ANY($1)`, []uuid.UUID{same, other}, docker)
		got := listedUnplaceable(fx, 1000, iv)
		if got[same] || !got[other] {
			t.Fatalf("listed = %v; want the other-branch run %s only, not the same-branch run %s", got, other, same)
		}
		// Lease 0 and an expired lease: today's behaviour, the run is listed again. (The draining flag is
		// pinned on fn_ephemeral_lease_admits itself in TestEphemeralLeaseAdmitsFunctionLiveDB: this query
		// filters a draining worker out by its own predicate, so it cannot isolate that check.)
		if got := listedUnplaceable(fx, 1000, pgtype.Interval{}); !got[same] {
			t.Fatalf("at lease 0 the same-branch run must be provisioned for; listed %v", got)
		}
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET lease_since = now() - interval '3 hours' WHERE id = $1`, w.id)
		if got := listedUnplaceable(fx, 1000, iv); !got[same] {
			t.Fatalf("an expired lease must not suppress provisioning; listed %v", got)
		}
	})

	t.Run("saturation trigger skips a run the leased worker can take", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		busy := fx.worker("persistent", capOf(1), true)
		fx.holdActive(busy, 1) // the only persistent worker is saturated
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(92)}, 0)
		same := insertLeaseRun(fx, leaseRun{iid: i64p(92)})
		other := insertLeaseRun(fx, leaseRun{iid: i64p(93)})
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour', required_capabilities = $2 WHERE id = ANY($1)`,
			[]uuid.UUID{same, other}, docker)
		got := listedSaturation(fx, 1000, iv)
		if got[same] || !got[other] {
			t.Fatalf("listed = %v; want only %s (not %s)", got, other, same)
		}
		if got := listedSaturation(fx, 1000, pgtype.Interval{}); !got[same] {
			t.Fatalf("at lease 0 the run must be listed; got %v", got)
		}
		_ = w
	})

	t.Run("CountOnlineWorkersClaimableForRun counts the leased worker for its branch only", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(94)}, 0)
		same := insertLeaseRun(fx, leaseRun{iid: i64p(94)})
		other := insertLeaseRun(fx, leaseRun{iid: i64p(95)})
		if n := claimableCount(fx, same, iv); n != 1 {
			t.Fatalf("claimable(same-branch) = %d, want 1", n)
		}
		if n := claimableCount(fx, other, iv); n != 0 {
			t.Fatalf("claimable(other-branch) = %d, want 0", n)
		}
		if n := claimableCount(fx, same, pgtype.Interval{}); n != 0 {
			t.Fatalf("claimable at lease 0 = %d, want 0", n)
		}
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET lease_since = now() - interval '3 hours' WHERE id = $1`, w.id)
		if n := claimableCount(fx, same, iv); n != 0 {
			t.Fatalf("claimable with an expired lease = %d, want 0", n)
		}
	})

	t.Run("the spread clause treats a leased peer as a deferral target for its own branch", func(t *testing.T) {
		fx := newFleetFixture(t)
		peer := seedLeasedWorker(fx, leaseRun{iid: i64p(96)}, 0)
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET docker_enabled = true WHERE id = $1`, peer.id)
		// A busy persistent claimant (1 of 2 slots used) would defer a same-branch run to the idle leased
		// peer; with lease 0 no such peer exists, so it claims. Run ageing is inside the spread window.
		busy := fx.worker("busy", capOf(2), true)
		fx.holdActive(busy, 1)
		same := insertLeaseRun(fx, leaseRun{iid: i64p(96)})
		claim := func(iv pgtype.Interval) (store.Run, error) {
			return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
				WorkerID:            pgtype.UUID{Bytes: busy, Valid: true},
				UserID:              fx.userID,
				AffinityCutoff:      pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
				SpreadCutoff:        pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
				HeartbeatCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
				IsDockerWorker:      true,
				DockerRepoAllowlist: []uuid.UUID{fx.repoID},
				EphemeralLease:      iv,
			})
		}
		wantIdle(t, store.Run{}, errOf(claim(iv)))
		got, err := claim(pgtype.Interval{})
		wantClaimed(t, got, err, same)
	})

	t.Run("cap prefilter excludes releasable leased-idle workers only (capability-gap trigger)", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(97)}, 0) // one ephemeral worker: at a cap of 1
		unplaced := insertLeaseRun(fx, leaseRun{iid: i64p(98)})
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET required_capabilities = $2 WHERE id = $1`, unplaced, docker)
		assertLeasedIdleCapFilter(fx, w, unplaced, func(maxPerUser int32) map[uuid.UUID]bool {
			return listedUnplaceable(fx, maxPerUser, iv)
		})
	})

	t.Run("cap prefilter excludes releasable leased-idle workers only (saturation trigger)", func(t *testing.T) {
		fx := newFleetFixture(t)
		optInEphemeral(fx)
		busy := fx.worker("persistent", capOf(1), true)
		fx.holdActive(busy, 1) // the only persistent worker is saturated, so a queued run is slot-blocked
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(114)}, 0)
		unplaced := insertLeaseRun(fx, leaseRun{iid: i64p(115)})
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour', required_capabilities = $2 WHERE id = $1`, unplaced, docker)
		if !listedSaturation(fx, 1000, iv)[unplaced] {
			t.Fatal("harness: the saturated run must be listed when the owner is far below the cap")
		}
		assertLeasedIdleCapFilter(fx, w, unplaced, func(maxPerUser int32) map[uuid.UUID]bool {
			return listedSaturation(fx, maxPerUser, iv)
		})
	})

	t.Run("cap prefilter excludes releasable leased-idle workers only (isolated-lane trigger)", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(116)}, 0)
		profile := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
			profile, "lease-"+profile.String()[:20])
		unplaced := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, egress_profile_id)
			 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5)`, unplaced, fx.userID, fx.repoID, fx.nextIID(), profile)
		t.Cleanup(func() {
			// The profile is RESTRICTed while a run references it.
			_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = $1`, unplaced)
			_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM egress_profiles WHERE id = $1`, profile)
		})
		if !listedIsolated(fx, 1000)[unplaced] {
			t.Fatal("harness: the profile-bound run must be listed when the owner is far below the cap")
		}
		assertLeasedIdleCapFilter(fx, w, unplaced, func(maxPerUser int32) map[uuid.UUID]bool {
			return listedIsolated(fx, maxPerUser)
		})
	})
}

// assertLeasedIdleCapFilter drives the at-cap fairness prefilter of one provisioning trigger: the
// owner's only ephemeral worker is `w` (cap 1), so the run `unplaced` is listed only while `w` is a
// RELEASABLE leased-idle worker (no non-terminal run, no open custody hold), which provisionOne would
// evict. list runs the trigger at a given per-user cap.
func assertLeasedIdleCapFilter(fx *fleetFixture, w leasedWorker, unplaced uuid.UUID, list func(maxPerUser int32) map[uuid.UUID]bool) {
	fx.t.Helper()
	const cap1 = int32(1)
	if !list(cap1)[unplaced] {
		fx.t.Fatal("an owner at cap whose only worker is leased-idle must still reach provisioning")
	}
	hold := insertCustodyHold(fx, w.served, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: w.id, Valid: true}, "open")
	if list(cap1)[unplaced] {
		fx.t.Fatal("a custody-held leased worker is not releasable: the at-cap owner must stay filtered")
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE recovery_custody_holds SET state = 'released', live_worker_id = NULL, live_run_id = NULL WHERE id = $1`, hold)
	if !list(cap1)[unplaced] {
		fx.t.Fatal("after the hold is released the worker is releasable again")
	}
	busyRun := insertLeaseRun(fx, leaseRun{iid: i64p(900 + fx.nextIID()), status: "running", workerID: &w.id})
	if list(cap1)[unplaced] {
		fx.t.Fatalf("a busy leased worker (run %s) is not releasable: the at-cap owner must stay filtered", busyRun)
	}
	// Control: the same worker without a lease is never releasable, so the owner stays filtered at cap.
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, busyRun)
	if !list(cap1)[unplaced] {
		fx.t.Fatal("once the busy run is terminal the leased worker is releasable again")
	}
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET lease_since = NULL, lease_repo_id = NULL, lease_branch = NULL WHERE id = $1`, w.id)
	if list(cap1)[unplaced] {
		fx.t.Fatal("an unleased ephemeral worker counts toward the cap: the at-cap owner must stay filtered")
	}
}

func errOf(_ store.Run, err error) error { return err }

// TestEphemeralLeaseEvictionLiveDB: the eviction pair picks the oldest releasable leased worker of
// the owner, never a busy, custody-held, unleased or foreign one, and re-checks the guards at delete.
func TestEphemeralLeaseEvictionLiveDB(t *testing.T) {
	lockOldest := func(fx *fleetFixture) (uuid.UUID, bool) {
		fx.t.Helper()
		id, err := fx.q.LockOldestReleasableLeasedEphemeralWorker(fx.ctx, fx.userID)
		if err == pgx.ErrNoRows {
			return uuid.Nil, false
		}
		if err != nil {
			fx.t.Fatalf("LockOldestReleasableLeasedEphemeralWorker: %v", err)
		}
		return id, true
	}
	del := func(fx *fleetFixture, id uuid.UUID) int64 {
		fx.t.Helper()
		n, err := fx.q.DeleteReleasableLeasedEphemeralWorker(fx.ctx, id)
		if err != nil {
			fx.t.Fatalf("DeleteReleasableLeasedEphemeralWorker: %v", err)
		}
		return n
	}

	t.Run("oldest lease first, one at a time", func(t *testing.T) {
		fx := newFleetFixture(t)
		young := seedLeasedWorker(fx, leaseRun{iid: i64p(100)}, 10*time.Minute)
		oldest := seedLeasedWorker(fx, leaseRun{iid: i64p(101)}, 90*time.Minute)
		middle := seedLeasedWorker(fx, leaseRun{iid: i64p(102)}, 30*time.Minute)
		var order []uuid.UUID
		for i := 0; i < 3; i++ {
			id, ok := lockOldest(fx)
			if !ok {
				t.Fatalf("eviction %d: nothing releasable", i)
			}
			if del(fx, id) != 1 {
				t.Fatalf("eviction %d: delete matched 0 rows", i)
			}
			order = append(order, id)
		}
		want := []uuid.UUID{oldest.id, middle.id, young.id}
		for i := range want {
			if order[i] != want[i] {
				t.Fatalf("eviction order = %v, want %v", order, want)
			}
		}
		if _, ok := lockOldest(fx); ok {
			t.Fatal("something still releasable after all were evicted")
		}
	})
	t.Run("busy, custody-held, unleased and other-owner workers are not releasable", func(t *testing.T) {
		fx := newFleetFixture(t)
		busy := seedLeasedWorker(fx, leaseRun{iid: i64p(103)}, 100*time.Minute)
		insertLeaseRun(fx, leaseRun{iid: i64p(104), status: "running", workerID: &busy.id})
		held := seedLeasedWorker(fx, leaseRun{iid: i64p(105)}, 99*time.Minute)
		insertCustodyHold(fx, held.served, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: held.id, Valid: true}, "open")
		unleased := queuedRunWithCaps(fx, []string{})
		unleasedW := seedEphemeralWorkerBound(fx, unleased)
		good := seedLeasedWorker(fx, leaseRun{iid: i64p(106)}, 5*time.Minute)

		id, ok := lockOldest(fx)
		if !ok || id != good.id {
			t.Fatalf("lockOldest = (%v, %v), want only the releasable %s", id, ok, good.id)
		}
		if del(fx, busy.id) != 0 || del(fx, held.id) != 0 || del(fx, unleasedW) != 0 {
			t.Fatal("delete removed a busy, custody-held or unleased worker")
		}
		for _, w := range []uuid.UUID{busy.id, held.id, unleasedW} {
			if !workerExists(fx, w) {
				t.Fatalf("worker %s was deleted", w)
			}
		}

		// Another owner's leased worker is never the evictee for this owner.
		other := uuid.New()
		mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, other, fmt.Sprintf("o-%s@e2e", other))
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET user_id = $2 WHERE id = $1`, good.id, other)
		if _, ok := lockOldest(fx); ok {
			t.Fatal("another owner's leased worker was offered for eviction")
		}
	})
	t.Run("the delete re-checks the guards after the lock", func(t *testing.T) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(107)}, 5*time.Minute)
		id, ok := lockOldest(fx)
		if !ok || id != w.id {
			t.Fatalf("lockOldest = (%v, %v)", id, ok)
		}
		// Between the lock statement and the delete a hold opens (e.g. a recovery reservation).
		insertCustodyHold(fx, w.served, pgtype.UUID{Bytes: fx.repoID, Valid: true}, pgtype.UUID{Bytes: w.id, Valid: true}, "open")
		if n := del(fx, id); n != 0 || !workerExists(fx, w.id) {
			t.Fatalf("delete matched %d rows / exists=%v; a worker that gained a hold must be refused", n, workerExists(fx, w.id))
		}
	})
	t.Run("a worker locked by another transaction is skipped, not waited on", func(t *testing.T) {
		fx := newFleetFixture(t)
		locked := seedLeasedWorker(fx, leaseRun{iid: i64p(108)}, 100*time.Minute)
		free := seedLeasedWorker(fx, leaseRun{iid: i64p(109)}, 10*time.Minute)
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		if _, err := tx.Exec(fx.ctx, `SELECT 1 FROM workers WHERE id = $1 FOR UPDATE`, locked.id); err != nil {
			t.Fatalf("lock: %v", err)
		}
		id, ok := lockOldest(fx)
		if !ok || id != free.id {
			t.Fatalf("lockOldest = (%v, %v), want the unlocked %s", id, ok, free.id)
		}
	})
}

// TestEphemeralLeaseReapSelectionLiveDB proves the reaper never selects a live-leased worker
// alongside an expired one.
func TestEphemeralLeaseReapSelectionLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	live := seedLeasedWorker(fx, leaseRun{iid: i64p(110)}, 0)
	expired := seedLeasedWorker(fx, leaseRun{iid: i64p(111)}, 3*time.Hour)
	ids, err := fx.q.LockReapableEphemeralWorkers(fx.ctx, store.LockReapableEphemeralWorkersParams{
		EphemeralLease: leaseInterval(leaseTwoHours),
		DeadlineCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("LockReapableEphemeralWorkers: %v", err)
	}
	// The database is shared with the other fixtures, so assert on this fixture's two workers.
	var sawExpired, sawLive bool
	for _, id := range ids {
		sawExpired = sawExpired || id == expired.id
		sawLive = sawLive || id == live.id
	}
	if !sawExpired || sawLive {
		t.Fatalf("selected %v: want the expired %s and not the live %s", ids, expired.id, live.id)
	}
}

// TestEphemeralLeaseReapDeleteHalfLiveDB pins the DELETE half of the reap on its own: the lock
// statement selects the worker (its lease expired), then the lease is renewed inside the same
// transaction before DeleteLockedEphemeralWorkers runs, so only the delete statement's own lease arm
// can spare it. Removing the lease arm from DeleteLockedEphemeralWorkers alone reddens the first case.
func TestEphemeralLeaseReapDeleteHalfLiveDB(t *testing.T) {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
	iv := leaseInterval(leaseTwoHours)
	run := func(t *testing.T, renew bool) (deleted int64, exists bool) {
		fx := newFleetFixture(t)
		w := seedLeasedWorker(fx, leaseRun{iid: i64p(112)}, 3*time.Hour) // expired
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		q := store.New(tx)
		ids, err := q.LockReapableEphemeralWorkers(fx.ctx, store.LockReapableEphemeralWorkersParams{EphemeralLease: iv, DeadlineCutoff: cutoff})
		if err != nil {
			t.Fatalf("LockReapableEphemeralWorkers: %v", err)
		}
		var sawW bool
		for _, id := range ids {
			sawW = sawW || id == w.id
		}
		if !sawW {
			t.Fatalf("harness: the expired leased worker %s was not selected by the lock half (%v)", w.id, ids)
		}
		if renew {
			// The lease is renewed after the lock half selected the worker and before the delete runs.
			if _, err := tx.Exec(fx.ctx, `UPDATE workers SET lease_since = now() WHERE id = $1`, w.id); err != nil {
				t.Fatalf("renew lease: %v", err)
			}
		}
		// Only this fixture's worker is handed to the delete, so other fixtures' rows stay untouched.
		n, err := q.DeleteLockedEphemeralWorkers(fx.ctx, store.DeleteLockedEphemeralWorkersParams{
			Ids: []uuid.UUID{w.id}, EphemeralLease: iv, DeadlineCutoff: cutoff})
		if err != nil {
			t.Fatalf("DeleteLockedEphemeralWorkers: %v", err)
		}
		var cnt int
		if err := tx.QueryRow(fx.ctx, `SELECT count(*) FROM workers WHERE id = $1`, w.id).Scan(&cnt); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n, cnt > 0
	}
	t.Run("a worker whose lease is renewed after the lock is spared by the delete", func(t *testing.T) {
		if n, exists := run(t, true); n != 0 || !exists {
			t.Fatalf("delete = %d rows, exists=%v; the delete half must re-check the live lease", n, exists)
		}
	})
	t.Run("control: the same expired worker is deleted when nothing renewed it", func(t *testing.T) {
		if n, exists := run(t, false); n != 1 || exists {
			t.Fatalf("delete = %d rows, exists=%v; want the expired leased worker reaped", n, exists)
		}
	})
}

// TestEphemeralLeaseAdmitsFunctionLiveDB calls fn_ephemeral_lease_admits directly, one condition at a
// time, so each guard (notably the draining flag, which no claim or placement query isolates because
// each filters a draining worker out by its own predicate) is shown to refuse on its own.
func TestEphemeralLeaseAdmitsFunctionLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	type in struct {
		leaseSince  any // timestamptz, nil = no lease
		leaseRepo   any
		leaseBranch any
		draining    any
		lease       any // interval text, nil = unset
		at          any
		runRepo     any
		runBranch   any
		runIID      any
		egress      any
	}
	base := func() in {
		return in{
			leaseSince: time.Now().Add(-10 * time.Minute), leaseRepo: fx.repoID, leaseBranch: "agent/issue-30",
			draining: false, lease: "1 hour", at: time.Now(), runRepo: fx.repoID, runBranch: nil, runIID: int64(30), egress: nil,
		}
	}
	admits := func(i in) bool {
		var got bool
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT fn_ephemeral_lease_admits($1::timestamptz, $2::uuid, $3::text, $4::boolean, $5::interval, $6::timestamptz,
			        $7::uuid, 'issue', $8::text, NULL, $9::bigint, NULL, $10::uuid)`,
			i.leaseSince, i.leaseRepo, i.leaseBranch, i.draining, i.lease, i.at, i.runRepo, i.runBranch, i.runIID, i.egress).Scan(&got); err != nil {
			t.Fatalf("fn_ephemeral_lease_admits: %v", err)
		}
		return got
	}
	cases := []struct {
		name string
		mut  func(*in)
		want bool
	}{
		{"baseline: live lease, same repo and branch", func(*in) {}, true},
		{"a draining worker is refused", func(i *in) { i.draining = true }, false},
		{"a NULL draining flag is refused (never reads as admission)", func(i *in) { i.draining = nil }, false},
		{"an expired lease is refused", func(i *in) { i.at = time.Now().Add(2 * time.Hour) }, false},
		{"a zero interval is refused", func(i *in) { i.lease = "0 seconds" }, false},
		{"an unset interval is refused", func(i *in) { i.lease = nil }, false},
		{"no lease is refused", func(i *in) { i.leaseSince, i.leaseRepo, i.leaseBranch = nil, nil, nil }, false},
		{"another repository is refused", func(i *in) { i.runRepo = uuid.New() }, false},
		{"a repo-less run is refused", func(i *in) { i.runRepo = nil }, false},
		{"another issue's branch is refused", func(i *in) { i.runIID = int64(31) }, false},
		{"a profile-bound run is refused", func(i *in) { i.egress = uuid.New() }, false},
		{"a reported canonical branch of the same issue is admitted", func(i *in) { i.runBranch = "agent/issue-30" }, true},
		{"a reported branch of another issue is refused", func(i *in) { i.runBranch = "agent/issue-31" }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := base()
			c.mut(&i)
			if got := admits(i); got != c.want {
				t.Fatalf("fn_ephemeral_lease_admits = %v, want %v", got, c.want)
			}
		})
	}
}
