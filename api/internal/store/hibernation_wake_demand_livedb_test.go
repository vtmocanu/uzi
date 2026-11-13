package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4205 M5: ListHibernationWakeDemand against ClaimRun / ClaimChatRun. For each case the demand
// query must list the run for the hibernated worker if and only if, after flipping that same worker
// to status online + hibernation_state none with a fresh heartbeat, the real claim query claims the
// run. The spec asks for this differential rather than a re-derivation of the predicates.
//
// Intentional divergences, deliberately NOT covered by the equivalence (the demand query omits or
// loosens them and the PRD accepts the difference):
//   - the custody-admission limit (a claim can still be refused for an owner at the limit);
//   - the per-lane spread (a claim can defer to a less-loaded peer, a wake never does);
//   - the released-incarnation nonce of ClaimChatRun (a Register rotates it before any claim);
//   - the weekly-reserve gate beyond the sweeper's reserve_hold_reason marker;
//   - the Codex subscription quarantine;
//   - the pending-overflow and slot counts: saturation never wakes, so a capable ONLINE worker that
//     is full blocks the wake even though a sleeper could have claimed (own subtest below).
//
// Skipped unless UZI_TEST_DATABASE_URL is set; run via e2e/run-store-it.sh.

type wakeWorkerOpts struct {
	caps       []string
	docker     bool
	protocol   []string
	crossSlots *int32
	maxRuns    *int32
	cordoned   bool
}

func hostedWorker(fx *fleetFixture, o wakeWorkerOpts) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	caps, protocol := o.caps, o.protocol
	if caps == nil {
		caps = []string{}
	}
	if protocol == nil {
		protocol = []string{}
	}
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared,
		                      capabilities, docker_enabled, protocol_capabilities, max_cross_check_slots, max_concurrent_runs)
		 VALUES ($1, $2, $3, $4, 'online', now(), 'hosted', 'm', 'base', $5, $6, $7, $8, $9)`,
		id, fx.userID, "h-"+id.String(), tokenHash(), caps, o.docker, protocol, o.crossSlots, o.maxRuns)
	if o.cordoned {
		mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET manual_cordoned_at = now(), manual_cordoned_by = $2, manual_cordoned_by_label = 'admin' WHERE id = $1`, id, fx.userID)
	}
	return id
}

// wakeDemandFor lists the demand rows of the fixture user.
func wakeDemandFor(fx *fleetFixture, allow []uuid.UUID) map[uuid.UUID]uuid.UUID {
	fx.t.Helper()
	return wakeDemandWith(fx, allow, 1000, pgtype.Interval{})
}

// wakeDemandWith is wakeDemandFor with an explicit scan LIMIT and ephemeral lease.
func wakeDemandWith(fx *fleetFixture, allow []uuid.UUID, maxRows int32, lease pgtype.Interval) map[uuid.UUID]uuid.UUID {
	fx.t.Helper()
	now := time.Now()
	rows, err := fx.q.ListHibernationWakeDemand(fx.ctx, store.ListHibernationWakeDemandParams{
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		DockerRepoAllowlist:      allow,
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
		EphemeralLease:           lease,
		MaxRows:                  maxRows,
	})
	if err != nil {
		fx.t.Fatalf("ListHibernationWakeDemand: %v", err)
	}
	out := map[uuid.UUID]uuid.UUID{}
	for _, r := range rows {
		if r.UserID == fx.userID {
			out[r.RunID] = r.WorkerID
		}
	}
	return out
}

// claimsAfterWake flips the worker awake and reports whether the real claim lane claims run.
func claimsAfterWake(fx *fleetFixture, worker, run uuid.UUID, allow []uuid.UUID, lane string, chat bool) bool {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE workers SET status = 'online', hibernation_state = 'none', hibernation_op_id = NULL, hibernated_at = NULL,
		        wake_requested_at = NULL, last_heartbeat_at = now() WHERE id = $1`, worker)
	var wkr store.Worker
	wkr, err := fx.q.GetWorkerByID(fx.ctx, worker)
	if err != nil {
		fx.t.Fatalf("GetWorkerByID: %v", err)
	}
	now := time.Now()
	var claimed store.Run
	if chat {
		claimed, err = fx.q.ClaimChatRun(fx.ctx, store.ClaimChatRunParams{
			WorkerID:       pgtype.UUID{Bytes: worker, Valid: true},
			UserID:         fx.userID,
			AffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		})
	} else {
		claimed, err = fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
			WorkerID:                 pgtype.UUID{Bytes: worker, Valid: true},
			Lane:                     lane,
			UserID:                   fx.userID,
			CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
			CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
			HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
			AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
			IsDockerWorker:           wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool,
			DockerRepoAllowlist:      allow,
			WorkerCaps:               wkr.Capabilities,
			CapabilityAware:          true,
			WorkerProtocolCaps:       wkr.ProtocolCapabilities,
			SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
			BackgroundGraceCutoff:    pgtype.Timestamptz{Time: now.Add(-15 * time.Minute), Valid: true},
			WorkerDockerEnabled:      wkr.DockerEnabled.Valid && wkr.DockerEnabled.Bool,
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		fx.t.Fatalf("claim: %v", err)
	}
	return claimed.ID == run
}

func queuedChat(fx *fleetFixture) uuid.UUID {
	fx.t.Helper()
	var id uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO runs (user_id, issue_title, issue_description, kind) VALUES ($1, 'c', 'd', 'chat') RETURNING id`,
		fx.userID).Scan(&id); err != nil {
		fx.t.Fatalf("insert chat run: %v", err)
	}
	return id
}

// queuedPlanChecker seeds a queued plan-stage Claude checker whose lead runs on a different worker.
func queuedPlanChecker(fx *fleetFixture) uuid.UUID {
	fx.t.Helper()
	leadWorker := fx.worker("lead-host", nil, false)
	lead, checker := fx.queuedRun(), fx.queuedRun()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status='running', worker_id=$2, claim_generation=1 WHERE id=$1`, lead, leadWorker)
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET kind='cross_check', issue_iid=NULL, target_run_id=$2, harness='claude', report_only=true,
		        budget_wall_seconds=1800, required_capabilities='{}' WHERE id=$1`, checker, lead)
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO cross_checks
		(lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class, base_commit,
		 candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled, automatic_revision_limit)
		VALUES ($1, $2, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), $3, 'claude', now() + interval '5 minutes', true, 2)`,
		lead, checker, []byte("digest"))
	return checker
}

func TestHibernationWakeDemandMatchesClaimLiveDB(t *testing.T) {
	one := int32(1)
	checkerProtocol := []string{"cross_check_v1", "cross_check_codex_lead_v1"}
	type tc struct {
		name  string
		opts  wakeWorkerOpts
		allow func(fx *fleetFixture) []uuid.UUID
		lane  string
		chat  bool
		// setup returns the run under test.
		setup func(fx *fleetFixture, h uuid.UUID) uuid.UUID
		want  bool
	}
	repoAllowed := func(fx *fleetFixture) []uuid.UUID { return []uuid.UUID{fx.repoID} }
	otherAllowed := func(*fleetFixture) []uuid.UUID { return []uuid.UUID{uuid.New()} }
	plain := func(fx *fleetFixture, _ uuid.UUID) uuid.UUID { return fx.queuedRun() }
	withCaps := func(caps ...string) func(*fleetFixture, uuid.UUID) uuid.UUID {
		return func(fx *fleetFixture, _ uuid.UUID) uuid.UUID {
			id := fx.queuedRun()
			mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET required_capabilities = $2 WHERE id = $1`, id, caps)
			return id
		}
	}
	cases := []tc{
		{name: "plain run", setup: plain, want: true},
		{name: "capability satisfied", opts: wakeWorkerOpts{caps: []string{"gpu"}}, setup: withCaps("gpu"), want: true},
		{name: "capability missing", setup: withCaps("gpu"), want: false},
		{name: "docker worker, repo outside the allowlist", opts: wakeWorkerOpts{docker: true}, allow: otherAllowed, setup: plain, want: false},
		{name: "docker worker, repo inside the allowlist", opts: wakeWorkerOpts{docker: true}, allow: repoAllowed, setup: plain, want: true},
		{name: "docker capability on an allowlisted docker worker", opts: wakeWorkerOpts{docker: true}, allow: repoAllowed, setup: withCaps("docker"), want: true},
		{name: "docker capability on a non-docker worker", setup: withCaps("docker"), want: false},
		{
			name: "judge on a docker worker with an empty allowlist", opts: wakeWorkerOpts{docker: true}, allow: func(*fleetFixture) []uuid.UUID { return []uuid.UUID{} },
			setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID {
				target := fx.queuedRun()
				mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, target)
				return fx.insertJudge(nil, target, time.Now().Add(-time.Minute))
			}, want: true,
		},
		{name: "chat run", chat: true, setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID { return queuedChat(fx) }, want: true},
		{
			name: "cross-check child, legacy run-lane worker", opts: wakeWorkerOpts{protocol: checkerProtocol},
			setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID { return queuedPlanChecker(fx) }, want: true,
		},
		{
			name: "cross-check child, lane worker", opts: wakeWorkerOpts{protocol: append([]string{"cross_check_lane_v1"}, checkerProtocol...), crossSlots: &one},
			lane: "cross_check", setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID { return queuedPlanChecker(fx) }, want: true,
		},
		{name: "cross-check child, worker without the protocol", setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID { return queuedPlanChecker(fx) }, want: false},
		{
			name: "forge-origin cooldown on the worker",
			setup: func(fx *fleetFixture, h uuid.UUID) uuid.UUID {
				run := fx.queuedRun()
				var origin string
				if err := fx.pool.QueryRow(fx.ctx, `SELECT origin FROM authorized_forge_origins WHERE repo_id = $1`, fx.repoID).Scan(&origin); err != nil {
					fx.t.Fatalf("origin: %v", err)
				}
				mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO forge_origin_cooldowns (user_id, worker_id, origin, cooled_until, last_reason, observed_at)
					VALUES ($1, $2, $3, now() + interval '1 hour', 'transient_clone_fetch_exhausted', now())`, fx.userID, h, origin)
				return run
			}, want: false,
		},
		{
			name: "run pinned to the hibernated worker",
			setup: func(fx *fleetFixture, h uuid.UUID) uuid.UUID {
				run := fx.queuedRun()
				mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, run, h)
				return run
			}, want: true,
		},
		{
			name: "run pinned to an online peer that holds the pin",
			setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID {
				run := fx.queuedRun()
				peer := fx.worker("peer", nil, false)
				mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, run, peer)
				return run
			}, want: false,
		},
		{
			name: "run pinned to a dead peer whose pin has lapsed",
			setup: func(fx *fleetFixture, _ uuid.UUID) uuid.UUID {
				run := fx.queuedRun()
				peer := fx.worker("dead-peer", nil, false)
				mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET status = 'offline', last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, peer)
				mustExec(fx.ctx, fx.t, fx.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, run, peer)
				return run
			}, want: true,
		},
		{name: "cordoned worker is not woken", opts: wakeWorkerOpts{cordoned: true}, setup: plain, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
			h := hostedWorker(fx, c.opts)
			run := c.setup(fx, h)
			var allow []uuid.UUID
			if c.allow != nil {
				allow = c.allow(fx)
			}
			setHibernated(fx, h, "hibernated")

			demand := wakeDemandFor(fx, allow)
			gotDemand := demand[run] == h
			if gotDemand != c.want {
				t.Fatalf("demand lists run %s for worker %s = %v, want %v (demand=%v)", run, h, gotDemand, c.want, demand)
			}
			if claimed := claimsAfterWake(fx, h, run, allow, c.lane, c.chat); claimed != gotDemand {
				t.Fatalf("differential: demand=%v but the awake worker's claim=%v", gotDemand, claimed)
			}
		})
	}

	t.Run("saturation does not wake: a full capable online worker blocks the demand", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		full := hostedWorker(fx, wakeWorkerOpts{maxRuns: &one})
		fx.holdActive(full, 1)
		h := hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, h, "hibernated")
		run := fx.queuedRun()
		if got := wakeDemandFor(fx, nil); got[run] != uuid.Nil {
			t.Fatalf("run %s demanded worker %s although a capable online worker exists (it is only full)", run, got[run])
		}
	})

	t.Run("oldest hibernated_at wins, NULLs last, then lowest id", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		newer, older, preparing := hostedWorker(fx, wakeWorkerOpts{}), hostedWorker(fx, wakeWorkerOpts{}), hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, newer, "hibernated")
		setHibernated(fx, older, "hibernated")
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET hibernated_at = now() - interval '2 hours' WHERE id = $1`, older)
		setHibernated(fx, preparing, "preparing")
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET hibernated_at = NULL WHERE id = $1`, preparing)
		run := fx.queuedRun()
		if got := wakeDemandFor(fx, nil)[run]; got != older {
			t.Fatalf("picked worker %s, want the oldest hibernated %s (newer=%s, preparing-with-NULL=%s)", got, older, newer, preparing)
		}
		// With the dated ones deleted, the preparing worker (NULL hibernated_at) is the only candidate.
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM workers WHERE id IN ($1, $2)`, older, newer)
		if got := wakeDemandFor(fx, nil)[run]; got != preparing {
			t.Fatalf("picked worker %s, want the preparing worker %s", got, preparing)
		}
	})

	t.Run("a run that arrives mid-wake attaches to the already-waking worker, not an older sleeper", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		sleeper, waking := hostedWorker(fx, wakeWorkerOpts{}), hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, sleeper, "hibernated")
		setHibernated(fx, waking, "waking")
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET hibernated_at = now() - interval '3 hours' WHERE id = $1`, sleeper)
		run := fx.queuedRun()
		if got := wakeDemandFor(fx, nil)[run]; got != waking {
			t.Fatalf("run %s demanded %s, want the waking worker %s (sleeper %s must not be woken)", run, got, waking, sleeper)
		}
	})

	t.Run("a recorded wake and a run bound to an ephemeral worker are not listed", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		h := hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, h, "waking") // a recorded wake names a waking worker
		woken, bound, free := fx.queuedRun(), fx.queuedRun(), fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, now())`, woken, h)
		if _, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
			UserID: fx.userID, Name: "ephemeral-" + bound.String(), TokenHash: tokenHash(),
			TemplateDeclared: pgtype.Text{String: "base", Valid: true}, HostedSize: pgtype.Text{String: "m", Valid: true},
			DockerEnabled: pgtype.Bool{Bool: false, Valid: true}, EphemeralRunID: bound, AnthropicBindMode: "default",
		}); err != nil {
			t.Fatalf("CreateEphemeralHostedWorker: %v", err)
		}
		got := wakeDemandFor(fx, nil)
		if _, ok := got[woken]; ok {
			t.Errorf("run %s already has a wake row but is listed again", woken)
		}
		if _, ok := got[bound]; ok {
			t.Errorf("run %s is bound to an ephemeral worker but is listed", bound)
		}
		if got[free] != h {
			t.Errorf("run %s not listed for %s: %v", free, h, got)
		}
	})

	t.Run("the recorded-wake and ephemeral-bound exclusions apply before LIMIT", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		h := hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, h, "waking") // a recorded wake names a waking worker
		// The two oldest runs are excluded; with MaxRows 1 the free run must still be listed.
		woken, bound, free := fx.queuedRun(), fx.queuedRun(), fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET created_at = now() - interval '3 hours' WHERE id = $1`, woken)
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET created_at = now() - interval '2 hours' WHERE id = $1`, bound)
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, now())`, woken, h)
		if _, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
			UserID: fx.userID, Name: "ephemeral-" + bound.String(), TokenHash: tokenHash(),
			TemplateDeclared: pgtype.Text{String: "base", Valid: true}, HostedSize: pgtype.Text{String: "m", Valid: true},
			DockerEnabled: pgtype.Bool{Bool: false, Valid: true}, EphemeralRunID: bound, AnthropicBindMode: "default",
		}); err != nil {
			t.Fatalf("CreateEphemeralHostedWorker: %v", err)
		}
		got := wakeDemandWith(fx, nil, 1, pgtype.Interval{})
		if len(got) != 1 || got[free] != h {
			t.Fatalf("MaxRows 1 returned %v, want only %s -> %s", got, free, h)
		}
	})

	t.Run("a row whose worker is hibernated again is stale and does not exclude its run", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		h := hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, h, "hibernated")
		run := fx.queuedRun()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO hibernation_wakes (run_id, worker_id, first_woken_at) VALUES ($1, $2, now())`, run, h)
		if got := wakeDemandFor(fx, nil); got[run] != h {
			t.Fatalf("run %s with a stale row (worker asleep again) is not listed for %s: %v", run, h, got)
		}
	})

	t.Run("a fleet with no sleeper lists nothing", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		hostedWorker(fx, wakeWorkerOpts{}) // an awake worker is never woken
		hostedWorker(fx, wakeWorkerOpts{cordoned: true})
		fx.queuedRun()
		fx.queuedRun()
		if got := wakeDemandFor(fx, nil); len(got) != 0 {
			t.Fatalf("no worker sleeps, yet demand = %v", got)
		}
	})

	t.Run("a waking worker whose wake failed is skipped for a hibernated sibling", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		failed, sibling := hostedWorker(fx, wakeWorkerOpts{}), hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, failed, "waking")
		setHibernated(fx, sibling, "hibernated")
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET wake_error = 'scale up failed' WHERE id = $1`, failed)
		run := fx.queuedRun()
		if got := wakeDemandFor(fx, nil)[run]; got != sibling {
			t.Fatalf("run %s demanded %s, want the hibernated sibling %s", run, got, sibling)
		}
		mustExec(fx.ctx, t, fx.pool, `DELETE FROM workers WHERE id = $1`, sibling)
		if got, ok := wakeDemandFor(fx, nil)[run]; ok {
			t.Fatalf("run %s demanded %s although the only candidate's wake failed", run, got)
		}
	})

	t.Run("a run a live leased ephemeral worker can take through its lease does not wake a sleeper", func(t *testing.T) {
		fx := newFleetFixture(t)
		t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
		h := hostedWorker(fx, wakeWorkerOpts{})
		setHibernated(fx, h, "hibernated")
		seedLeasedWorker(fx, leaseRun{iid: i64p(41)}, 0)
		follow := insertLeaseRun(fx, leaseRun{iid: i64p(41)}) // same repo and branch: the lease admits it
		// The leased worker is a Docker worker (as seeded), so the repo must be allowlisted for it.
		allow := []uuid.UUID{fx.repoID}
		lease := pgtype.Interval{Microseconds: (2 * time.Hour).Microseconds(), Valid: true}
		if got, ok := wakeDemandWith(fx, allow, 1000, lease)[follow]; ok {
			t.Fatalf("run %s demanded %s although the leased worker can claim it", follow, got)
		}
		// Control: with the lease off the leased worker is no claimant and the sleeper is demanded.
		if got := wakeDemandWith(fx, allow, 1000, pgtype.Interval{})[follow]; got != h {
			t.Fatalf("control: run %s demanded %s, want the sleeper %s", follow, got, h)
		}
	})
}
