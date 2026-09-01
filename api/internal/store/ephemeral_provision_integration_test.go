package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The store-query half of PRD #529 M2 against a REAL Postgres. The trigger query
// (ListUnplaceableQueuedRunsForEphemeral), the two separate quota counts, and the
// one-per-run partial UNIQUE index are all SQL the fake store cannot exhibit — the
// effective-caps fold, the NOT EXISTS subqueries, and the 23505 only exist on real rows.
//
// Reuses fleetFixture (claim_fleet_placement_integration_test.go, same package). Skipped
// unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// optInEphemeral flips the fixture user's per-user opt-in on, through the real query
// (SetUserEphemeralWorkersEnabled) rather than raw SQL — the query the CLI/handler will
// drive later, exercised here so its behaviour is covered against a real row.
func optInEphemeral(fx *fleetFixture) {
	fx.t.Helper()
	if _, err := fx.q.SetUserEphemeralWorkersEnabled(fx.ctx, store.SetUserEphemeralWorkersEnabledParams{
		ID: fx.userID, EphemeralWorkersEnabled: true,
	}); err != nil {
		fx.t.Fatalf("SetUserEphemeralWorkersEnabled: %v", err)
	}
}

// queuedRunWithCaps inserts a fresh queued, unowned run carrying required_capabilities.
func queuedRunWithCaps(fx *fleetFixture, caps []string) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, required_capabilities)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', NULL, $5)`,
		id, fx.userID, fx.repoID, fx.nextIID(), caps)
	return id
}

// unplaceableRunIDs returns the ids the trigger query surfaces for the fixture user.
// maxPerUser is the fairness cap the query filters at; a high default keeps the existing
// cases (which do not exercise the fairness filter) unaffected.
func unplaceableRunIDs(fx *fleetFixture) map[uuid.UUID]bool {
	return unplaceableRunIDsCap(fx, 1000)
}

func unplaceableRunIDsCap(fx *fleetFixture, maxPerUser int32) map[uuid.UUID]bool {
	fx.t.Helper()
	rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		MaxRows:    50,
		MaxPerUser: maxPerUser,
	})
	if err != nil {
		fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		if r.UserID == fx.userID {
			out[r.ID] = true
		}
	}
	return out
}

func TestListUnplaceableQueuedRunsForEphemeralLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	optInEphemeral(fx)

	t.Run("a docker run with a base-only fleet is unplaceable", func(t *testing.T) {
		fx.worker("base-only", capOf(1), false) // no docker, no caps
		runID := queuedRunWithCaps(fx, []string{"docker"})
		if got := unplaceableRunIDs(fx); !got[runID] {
			t.Fatalf("run %s not surfaced as unplaceable; a base-only fleet cannot satisfy docker", runID)
		}
	})

	t.Run("a run an online docker worker satisfies is placeable (not surfaced)", func(t *testing.T) {
		fx2 := newFleetFixture(t)
		optInEphemeral(fx2)
		fx2.worker("docker-capable", capOf(1), true) // docker_enabled → effective caps include docker
		runID := queuedRunWithCaps(fx2, []string{"docker"})
		if got := unplaceableRunIDs(fx2); got[runID] {
			t.Fatalf("run %s surfaced as unplaceable even though an online docker worker can claim it", runID)
		}
	})

	t.Run("a draining docker worker does not make the run placeable", func(t *testing.T) {
		fx2 := newFleetFixture(t)
		optInEphemeral(fx2)
		w := fx2.worker("draining-docker", capOf(1), true)
		mustExec(fx2.ctx, fx2.t, fx2.pool, `UPDATE workers SET draining_since = now() WHERE id = $1`, w)
		runID := queuedRunWithCaps(fx2, []string{"docker"})
		if got := unplaceableRunIDs(fx2); !got[runID] {
			t.Fatalf("run %s not surfaced; a DRAINING docker worker claims nothing and must not count", runID)
		}
	})

	t.Run("a run with no required capabilities is never surfaced", func(t *testing.T) {
		fx2 := newFleetFixture(t)
		optInEphemeral(fx2)
		runID := fx2.queuedRun() // no caps
		if got := unplaceableRunIDs(fx2); got[runID] {
			t.Fatalf("run %s surfaced with empty required_capabilities; cardinality guard failed", runID)
		}
	})

	t.Run("a run of a user who has not opted in is never surfaced", func(t *testing.T) {
		fx2 := newFleetFixture(t) // NOT opted in
		fx2.worker("base-only", capOf(1), false)
		runID := queuedRunWithCaps(fx2, []string{"docker"})
		if got := unplaceableRunIDs(fx2); got[runID] {
			t.Fatalf("run %s surfaced for a user who did not opt in", runID)
		}
	})

	t.Run("a run already bound to an ephemeral worker is not surfaced again", func(t *testing.T) {
		fx2 := newFleetFixture(t)
		optInEphemeral(fx2)
		fx2.worker("base-only", capOf(1), false)
		runID := queuedRunWithCaps(fx2, []string{"docker"})
		// Bind an ephemeral worker to it.
		if _, err := fx2.q.CreateEphemeralHostedWorker(fx2.ctx, store.CreateEphemeralHostedWorkerParams{
			UserID:            fx2.userID,
			Name:              "ephemeral-" + runID.String(),
			TokenHash:         tokenHash(),
			TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
			HostedSize:        pgtype.Text{String: "m", Valid: true},
			DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
			EphemeralRunID:    runID,
			AnthropicBindMode: "default",
		}); err != nil {
			t.Fatalf("CreateEphemeralHostedWorker: %v", err)
		}
		if got := unplaceableRunIDs(fx2); got[runID] {
			t.Fatalf("run %s surfaced again even though an ephemeral worker is already bound to it", runID)
		}
	})

	t.Run("a jvm run needing a jvm template is unplaceable against a base fleet", func(t *testing.T) {
		fx2 := newFleetFixture(t)
		optInEphemeral(fx2)
		fx2.worker("base-only", capOf(1), false)
		runID := queuedRunWithCaps(fx2, []string{"jvm"})
		if got := unplaceableRunIDs(fx2); !got[runID] {
			t.Fatalf("run %s needing jvm not surfaced against a base-only fleet", runID)
		}
	})

	// Cross-user fairness (PRD #529 M2, review follow-up): a user already AT the per-user
	// ephemeral cap has its unplaceable runs EXCLUDED from the batch, while a sub-cap user's
	// run is still INCLUDED — so an at-cap user with a large backlog cannot monopolize every
	// tick and starve everyone else. Both users share one DB, so the same query call must
	// exclude one and surface the other.
	t.Run("an at-cap user's run is excluded while a sub-cap user's run is included", func(t *testing.T) {
		// At-cap user: opted in, a base-only fleet (so a docker run is unplaceable), and one
		// existing ephemeral worker bound to a prior run — i.e. at a cap of 1.
		atCap := newFleetFixture(t)
		optInEphemeral(atCap)
		atCap.worker("base-only", capOf(1), false)
		boundRun := queuedRunWithCaps(atCap, []string{"docker"})
		if _, err := atCap.q.CreateEphemeralHostedWorker(atCap.ctx, store.CreateEphemeralHostedWorkerParams{
			UserID:            atCap.userID,
			Name:              "ephemeral-" + boundRun.String(),
			TokenHash:         tokenHash(),
			TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
			HostedSize:        pgtype.Text{String: "m", Valid: true},
			DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
			EphemeralRunID:    boundRun,
			AnthropicBindMode: "default",
		}); err != nil {
			t.Fatalf("seed at-cap ephemeral worker: %v", err)
		}
		atCapRun := queuedRunWithCaps(atCap, []string{"docker"}) // a fresh, unplaceable, unbound run

		// Sub-cap user: opted in, base-only fleet, one unplaceable run, zero ephemeral workers.
		subCap := newFleetFixture(t)
		optInEphemeral(subCap)
		subCap.worker("base-only", capOf(1), false)
		subCapRun := queuedRunWithCaps(subCap, []string{"docker"})

		// Query at max_per_user = 1: the at-cap user's fresh run is filtered out; the sub-cap
		// user's run is surfaced.
		if got := unplaceableRunIDsCap(atCap, 1); got[atCapRun] {
			t.Errorf("run %s surfaced for a user already AT the ephemeral cap — the fairness filter did not exclude it", atCapRun)
		}
		if got := unplaceableRunIDsCap(subCap, 1); !got[subCapRun] {
			t.Errorf("run %s NOT surfaced for a SUB-cap user — the fairness filter wrongly excluded a user with headroom", subCapRun)
		}

		// Sanity: with a HIGHER cap the at-cap user's run comes back, proving the exclusion is
		// the cap filter and not some other predicate.
		if got := unplaceableRunIDsCap(atCap, 2); !got[atCapRun] {
			t.Errorf("run %s still excluded at max_per_user=2 — exclusion was not the fairness cap", atCapRun)
		}
	})
}

// The two quota counts are SEPARATE: an ephemeral worker must not draw down the standing
// persistent quota, and vice versa.
func TestEphemeralAndPersistentQuotaCountsAreSeparateLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	runID := queuedRunWithCaps(fx, []string{"docker"})

	// One persistent hosted worker.
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO workers (user_id, name, token_hash, template_declared, kind, hosted_size, docker_enabled)
		 VALUES ($1, 'persistent', $2, 'base', 'hosted', 'm', false)`, fx.userID, tokenHash())
	// One ephemeral hosted worker bound to the run.
	if _, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
		UserID:            fx.userID,
		Name:              "ephemeral-" + runID.String(),
		TokenHash:         tokenHash(),
		TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
		HostedSize:        pgtype.Text{String: "m", Valid: true},
		DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
		EphemeralRunID:    runID,
		AnthropicBindMode: "default",
	}); err != nil {
		t.Fatalf("CreateEphemeralHostedWorker: %v", err)
	}

	persistent, err := fx.q.CountHostedWorkersForUser(fx.ctx, fx.userID)
	if err != nil {
		t.Fatalf("CountHostedWorkersForUser: %v", err)
	}
	if persistent != 1 {
		t.Errorf("persistent count = %d, want 1 (the ephemeral worker must NOT count toward the standing quota)", persistent)
	}
	ephemeral, err := fx.q.CountEphemeralHostedWorkersForUser(fx.ctx, fx.userID)
	if err != nil {
		t.Fatalf("CountEphemeralHostedWorkersForUser: %v", err)
	}
	if ephemeral != 1 {
		t.Errorf("ephemeral count = %d, want 1", ephemeral)
	}
}

// The partial UNIQUE index uq_workers_ephemeral_run makes a second ephemeral worker for
// one run impossible at the schema level, independent of any Go-side check.
func TestCreateEphemeralHostedWorkerOnePerRunLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	runID := queuedRunWithCaps(fx, []string{"docker"})

	mk := func(name string) error {
		_, err := fx.q.CreateEphemeralHostedWorker(fx.ctx, store.CreateEphemeralHostedWorkerParams{
			UserID:            fx.userID,
			Name:              name,
			TokenHash:         tokenHash(),
			TemplateDeclared:  pgtype.Text{String: "base", Valid: true},
			HostedSize:        pgtype.Text{String: "m", Valid: true},
			DockerEnabled:     pgtype.Bool{Bool: true, Valid: true},
			EphemeralRunID:    runID,
			AnthropicBindMode: "default",
		})
		return err
	}

	if err := mk("ephemeral-a"); err != nil {
		t.Fatalf("first ephemeral worker: %v", err)
	}
	if err := mk("ephemeral-b"); err == nil {
		t.Fatal("a SECOND ephemeral worker for the same run was created — the partial unique index uq_workers_ephemeral_run is not enforcing one-per-run")
	}
	// The row landed with the ephemeral marker and binding set.
	var eph bool
	var boundRun uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT ephemeral, ephemeral_run_id FROM workers WHERE user_id = $1 AND ephemeral`, fx.userID).Scan(&eph, &boundRun); err != nil {
		t.Fatalf("read ephemeral worker: %v", err)
	}
	if !eph || boundRun != runID {
		t.Errorf("ephemeral=%v ephemeral_run_id=%s, want true / %s", eph, boundRun, runID)
	}
}
