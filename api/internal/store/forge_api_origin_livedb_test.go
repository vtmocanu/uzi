package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #1324 D9: direct publication/API failures feed the same advisory origin cooldown as
// clone/fetch failures. These tests execute the generated ClaimRun and the generated observation and
// success queries against a real Postgres. The fixture's repo lives at https://forge.e2e, so with a
// github connection its API origin is https://api.forge.e2e and with a gitlab connection the API
// origin equals the Git origin.
const (
	apiFxGit = "https://forge.e2e"
	apiFxAPI = "https://api.forge.e2e"
)

func newForgeAPIFixture(t *testing.T, forgeType string) *rollAffinityFixture {
	t.Helper()
	fx := newRollAffinityFixture(t)
	mustExec(fx.ctx, t, fx.pool, "UPDATE forge_connections SET forge_type=$1 WHERE user_id=$2", forgeType, fx.userID)
	return fx
}

// apiFxRow inserts one cooldown row. A due row is expired, an undue one cools for a day.
func apiFxRow(fx *rollAffinityFixture, worker uuid.UUID, origin string, due, gitUnverified, apiUnverified bool) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO forge_origin_cooldowns
		(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified)
		VALUES ($1,$2,$3,now() + CASE WHEN $4 THEN interval '-1 day' ELSE interval '1 day' END,
			'transient_publication_exhausted',now(),$5,$6)`,
		fx.userID, worker, origin, due, gitUnverified, apiUnverified)
}

type apiFxRowState struct {
	Exists                           bool
	Git, API, Reserved               bool
	RunID                            uuid.UUID
	Incarnation                      uuid.UUID
	Version, ObsVersion, ObservedVer int64
}

func apiFxRead(fx *rollAffinityFixture, worker uuid.UUID, origin string) apiFxRowState {
	fx.t.Helper()
	var st apiFxRowState
	var run, inc *uuid.UUID
	var ver, obs *int64
	err := fx.pool.QueryRow(fx.ctx, `SELECT git_unverified, api_unverified, reservation_run_id, reservation_incarnation,
		reservation_version, reservation_observation_version, observation_version
		FROM forge_origin_cooldowns WHERE user_id=$1 AND worker_id=$2 AND origin=$3`,
		fx.userID, worker, origin).Scan(&st.Git, &st.API, &run, &inc, &ver, &obs, &st.ObservedVer)
	if errors.Is(err, pgx.ErrNoRows) {
		return st
	}
	if err != nil {
		fx.t.Fatal(err)
	}
	st.Exists = true
	if run != nil {
		st.Reserved, st.RunID, st.Incarnation, st.Version, st.ObsVersion = true, *run, *inc, *ver, *obs
	}
	return st
}

func (st apiFxRowState) clearAPI(run, worker uuid.UUID) store.ClearForgeApiOriginOnSuccessParams {
	return store.ClearForgeApiOriginOnSuccessParams{RunID: run, WorkerID: worker, Generation: 1,
		Incarnation: st.Incarnation, Version: st.Version, ObservationVersion: st.ObsVersion}
}

func (st apiFxRowState) clearGit(run, worker uuid.UUID) store.ClearForgeOriginOnCloneSuccessParams {
	return store.ClearForgeOriginOnCloneSuccessParams{RunID: run, WorkerID: worker, Generation: 1,
		Incarnation: st.Incarnation, Version: st.Version, ObservationVersion: st.ObsVersion}
}

// Test 1: placement avoids a worker whose API origin is cooled while its Git origin is uncooled; a
// peer stays eligible and the same worker is claimable again once the API row is gone.
func TestForgeAPIOriginPlacementAvoidsCooledAPIOriginLiveDB(t *testing.T) {
	fx := newForgeAPIFixture(t, "github")
	cooled, peer := fx.worker("api-cooled", 0, false), fx.worker("peer", 0, false)
	first, second := fx.unclaimedRun(), fx.unclaimedRun()
	apiFxRow(fx, cooled, apiFxAPI, false, false, true)

	var eligible, peerEligible, provision bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT fn_forge_origin_eligible(w,r,now()),
		fn_forge_origin_eligible(p,r,now()), fn_forge_origin_provision_allowed(r,now())
		FROM workers w, workers p, runs r WHERE w.id=$1 AND p.id=$2 AND r.id=$3`, cooled, peer, first).
		Scan(&eligible, &peerEligible, &provision); err != nil {
		t.Fatal(err)
	}
	if eligible || !peerEligible || provision {
		t.Fatalf("eligible(cooled)=%v eligible(peer)=%v provision_allowed=%v, want false/true/false", eligible, peerEligible, provision)
	}
	if _, err := forgeAdmissionClaim(fx, fx.q, cooled); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("worker with a cooled API origin claimed: %v", err)
	}
	got, err := forgeAdmissionClaim(fx, fx.q, peer)
	if err != nil || (got.ID != first && got.ID != second) {
		t.Fatalf("peer claim: %s %v", got.ID, err)
	}
	// Positive control: the API row alone was the exclusion.
	mustExec(fx.ctx, t, fx.pool, "DELETE FROM forge_origin_cooldowns WHERE worker_id=$1", cooled)
	if got, err := forgeAdmissionClaim(fx, fx.q, cooled); err != nil || (got.ID != first && got.ID != second) {
		t.Fatalf("claim without API row: %s %v", got.ID, err)
	}
}

// Test 2: the API half-open trial reserves exactly once, records the API reservation on the claim
// snapshot, and takes the Git row in the same statement when both origins are due.
func TestForgeAPIOriginHalfOpenReservesOnceLiveDB(t *testing.T) {
	t.Run("api only", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "github")
		w := fx.worker("claimant", 0, false)
		first, second := fx.unclaimedRun(), fx.unclaimedRun()
		apiFxRow(fx, w, apiFxAPI, true, false, true)
		type result struct {
			run store.Run
			err error
		}
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			go func() {
				<-start
				run, err := forgeAdmissionClaim(fx, fx.q, w)
				results <- result{run, err}
			}()
		}
		close(start)
		both := [2]result{<-results, <-results}
		var winner uuid.UUID
		for _, got := range both {
			if got.err == nil {
				if winner != uuid.Nil {
					t.Fatalf("two due API trials: %s and %s", winner, got.run.ID)
				}
				winner = got.run.ID
			} else if !errors.Is(got.err, pgx.ErrNoRows) {
				t.Fatalf("claim: %v", got.err)
			}
		}
		if winner != first && winner != second {
			t.Fatalf("no API trial winner: %s", winner)
		}
		var exact bool
		if err := fx.pool.QueryRow(fx.ctx, `SELECT
			c.reservation_run_id=$1 AND c.reservation_generation=1
			AND c.reservation_incarnation=c.incarnation
			AND c.reservation_observation_version=c.observation_version
			AND m.api_origin=c.origin AND m.origin=$4 AND m.reservation_incarnation IS NULL
			AND m.api_reservation_incarnation=c.reservation_incarnation
			AND m.api_reservation_version=c.reservation_version
			AND m.generation=c.reservation_generation
			AND (SELECT sum(claim_generation) FROM runs WHERE id IN ($2,$3))=1
			AND (SELECT count(*) FROM recovery_custody_holds WHERE run_id IN ($2,$3))=1
			AND NOT EXISTS (SELECT 1 FROM forge_origin_cooldowns WHERE worker_id=$5 AND origin=$4)
			FROM forge_origin_cooldowns c JOIN run_forge_origins m ON m.run_id=c.reservation_run_id
			WHERE c.worker_id=$5 AND c.origin=$6`, winner, first, second, apiFxGit, w, apiFxAPI).Scan(&exact); err != nil || !exact {
			t.Fatalf("API reservation/snapshot/G/custody mismatch: %v, %v", exact, err)
		}
		// The reserved row now excludes the worker: no second trial.
		if _, err := forgeAdmissionClaim(fx, fx.q, w); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("second claim while the API trial is reserved: %v", err)
		}
	})
	t.Run("git and api both due reserve together", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "github")
		w := fx.worker("claimant", 0, false)
		id := fx.unclaimedRun()
		apiFxRow(fx, w, apiFxGit, true, true, false)
		apiFxRow(fx, w, apiFxAPI, true, false, true)
		if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
			t.Fatalf("claim: %s %v", got.ID, err)
		}
		git, api := apiFxRead(fx, w, apiFxGit), apiFxRead(fx, w, apiFxAPI)
		if !git.Reserved || git.RunID != id || !api.Reserved || api.RunID != id {
			t.Fatalf("not both reserved: git=%+v api=%+v", git, api)
		}
		var snap bool
		if err := fx.pool.QueryRow(fx.ctx, `SELECT m.reservation_incarnation=$2 AND m.reservation_version=$3
			AND m.api_reservation_incarnation=$4 AND m.api_reservation_version=$5
			FROM run_forge_origins m WHERE m.run_id=$1`, id, git.Incarnation, git.Version, api.Incarnation, api.Version).Scan(&snap); err != nil || !snap {
			t.Fatalf("claim snapshot lost a reservation: %v %v", snap, err)
		}
		// Releasing the claim releases both reservations and keeps the evidence.
		mustExec(fx.ctx, t, fx.pool, "UPDATE runs SET status='queued',claim_released_at=now() WHERE id=$1", id)
		git, api = apiFxRead(fx, w, apiFxGit), apiFxRead(fx, w, apiFxAPI)
		if git.Reserved || api.Reserved || !git.Git || !api.API {
			t.Fatalf("release: git=%+v api=%+v", git, api)
		}
	})
	t.Run("git due and api cooled admits nothing and reserves nothing", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "github")
		w := fx.worker("claimant", 0, false)
		fx.unclaimedRun()
		apiFxRow(fx, w, apiFxGit, true, true, false)
		apiFxRow(fx, w, apiFxAPI, false, false, true)
		if _, err := forgeAdmissionClaim(fx, fx.q, w); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("claim with a cooled API origin: %v", err)
		}
		if git := apiFxRead(fx, w, apiFxGit); git.Reserved {
			t.Fatalf("Git row reserved by a refused claim: %+v", git)
		}
	})
}

// A claim whose statement snapshot saw both origins due must reserve neither when one row advanced
// after the snapshot: the all-or-nothing decision runs on the locked, latest rows.
func TestForgeAPIOriginStaleSnapshotReservesNeitherLiveDB(t *testing.T) {
	fx := newForgeAPIFixture(t, "github")
	ctx, cancel := context.WithTimeout(fx.ctx, 20*time.Second)
	defer cancel()
	bounded := *fx
	bounded.ctx = ctx
	w := fx.worker("stale-api", 0, false)
	id := fx.unclaimedRun()
	apiFxRow(fx, w, apiFxGit, true, true, false)
	apiFxRow(fx, w, apiFxAPI, true, false, true)
	holder, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	waiter, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Release()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// An observation takes the worker lock first, then advances only the API row.
	if _, err := tx.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE", w); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE forge_origin_cooldowns SET observation_version=observation_version+1,
		cooled_until=now()+interval '1 day' WHERE worker_id=$1 AND origin=$2`, w, apiFxAPI); err != nil {
		t.Fatal(err)
	}
	type result struct {
		run store.Run
		err error
	}
	results := make(chan result, 1)
	go func() {
		run, err := forgeAdmissionClaim(&bounded, store.New(waiter), w)
		results <- result{run, err}
	}()
	for {
		var blocked bool
		if err := fx.pool.QueryRow(ctx, "SELECT $1::integer = ANY(pg_blocking_pids($2::integer))",
			int(holder.Conn().PgConn().PID()), int(waiter.Conn().PgConn().PID())).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case got := <-results:
			t.Fatalf("waiter returned before the worker-lock barrier: %s %v", got.run.ID, got.err)
		default:
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-results
	if !errors.Is(got.err, pgx.ErrNoRows) {
		t.Fatalf("stale snapshot admitted a run: %s %v", got.run.ID, got.err)
	}
	var untouched bool
	if err := fx.pool.QueryRow(ctx, `SELECT r.status='queued' AND r.claim_generation=0
		AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds WHERE run_id=r.id)
		AND NOT EXISTS (SELECT 1 FROM run_forge_origins WHERE run_id=r.id)
		AND NOT EXISTS (SELECT 1 FROM forge_origin_cooldowns WHERE worker_id=$2 AND reservation_run_id IS NOT NULL)
		FROM runs r WHERE r.id=$1`, id, w).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("a refused claim left a partial reservation, custody or G change: %v %v", untouched, err)
	}
}

// claimOneAPITrial claims a run against a due API row and returns the run and the row's tuple.
func claimOneAPITrial(t *testing.T, forgeType string) (*rollAffinityFixture, uuid.UUID, uuid.UUID, apiFxRowState) {
	t.Helper()
	fx := newForgeAPIFixture(t, forgeType)
	w := fx.worker("claimant", 0, false)
	id := fx.unclaimedRun()
	origin := apiFxAPI
	if forgeType != "github" {
		origin = apiFxGit
	}
	apiFxRow(fx, w, origin, true, forgeType != "github", true)
	if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
		t.Fatalf("claim: %s %v", got.ID, err)
	}
	st := apiFxRead(fx, w, origin)
	if !st.Reserved || st.RunID != id {
		t.Fatalf("no reservation after claim: %+v", st)
	}
	return fx, w, id, st
}

// Test 3 (store half): only the exact half-open API success clears an API failure.
func TestForgeAPIOriginSuccessClearsOnlyExactHalfOpenLiveDB(t *testing.T) {
	type attempt struct {
		name string
		run  func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error)
	}
	other := uuid.New()
	attempts := []attempt{
		{"git success with the API tuple", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			return fx.q.ClearForgeOriginOnCloneSuccess(fx.ctx, st.clearGit(id, w))
		}},
		{"stale generation", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			p := st.clearAPI(id, w)
			p.Generation = 2
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, p)
		}},
		{"mismatched observation version", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			p := st.clearAPI(id, w)
			p.ObservationVersion++
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, p)
		}},
		{"newer failure advanced the observation", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			mustExec(fx.ctx, fx.t, fx.pool, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", w)
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(id, w))
		}},
		{"wrong reservation version", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			p := st.clearAPI(id, w)
			p.Version++
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, p)
		}},
		{"wrong incarnation", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			p := st.clearAPI(id, w)
			p.Incarnation = other
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, p)
		}},
		{"a run that does not hold the reservation", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			peer := fx.unclaimedRun()
			mustExec(fx.ctx, fx.t, fx.pool, "UPDATE runs SET status='claimed',worker_id=$2,claim_generation=1 WHERE id=$1", peer, w)
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(peer, w))
		}},
		{"a released claim", func(fx *rollAffinityFixture, w, id uuid.UUID, st apiFxRowState) (int64, error) {
			mustExec(fx.ctx, fx.t, fx.pool, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(id, w))
		}},
	}
	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			fx, w, id, st := claimOneAPITrial(t, "github")
			// The reservation-holding run is the one under test; a released claim drops it below.
			n, err := a.run(fx, w, id, st)
			if err != nil || n != 0 {
				t.Fatalf("%s applied=%d err=%v", a.name, n, err)
			}
			if got := apiFxRead(fx, w, apiFxAPI); !got.Exists || !got.API {
				t.Fatalf("%s cleared the API failure: %+v", a.name, got)
			}
		})
	}
	t.Run("another origin's row", func(t *testing.T) {
		// The reservation is on the Git row; the API origin has no row at all.
		fx := newForgeAPIFixture(t, "github")
		w := fx.worker("claimant", 0, false)
		id := fx.unclaimedRun()
		apiFxRow(fx, w, apiFxGit, true, true, false)
		if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
			t.Fatalf("claim: %s %v", got.ID, err)
		}
		st := apiFxRead(fx, w, apiFxGit)
		if n, err := fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(id, w)); err != nil || n != 0 {
			t.Fatalf("API success cleared the Git origin's row: %d %v", n, err)
		}
		if got := apiFxRead(fx, w, apiFxGit); !got.Exists || !got.Git {
			t.Fatalf("Git row disturbed: %+v", got)
		}
	})
	t.Run("exact half-open success clears", func(t *testing.T) {
		fx, w, id, st := claimOneAPITrial(t, "github")
		if n, err := fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(id, w)); err != nil || n != 1 {
			t.Fatalf("exact API success applied=%d err=%v", n, err)
		}
		if got := apiFxRead(fx, w, apiFxAPI); got.Exists {
			t.Fatalf("API-only row survived its exact success: %+v", got)
		}
	})
}

// Test 4: a GitLab or Forgejo origin serves Git and API from one host, so one row carries both
// flags and each surface's success clears only its own.
func TestForgeAPIOriginSameHostFlagsIndependentLiveDB(t *testing.T) {
	for _, order := range []string{"api then git", "git then api"} {
		t.Run(order, func(t *testing.T) {
			fx, w, id, st := claimOneAPITrial(t, "gitlab")
			// claimOneAPITrial seeded git=true api=true on the shared row.
			var shared bool
			if err := fx.pool.QueryRow(fx.ctx, `SELECT api_origin=origin AND api_reservation_incarnation=reservation_incarnation
				AND api_reservation_version=reservation_version FROM run_forge_origins WHERE run_id=$1`, id).Scan(&shared); err != nil || !shared {
				t.Fatalf("shared-row snapshot: %v %v", shared, err)
			}
			clearAPI := func() (int64, error) { return fx.q.ClearForgeApiOriginOnSuccess(fx.ctx, st.clearAPI(id, w)) }
			clearGit := func() (int64, error) { return fx.q.ClearForgeOriginOnCloneSuccess(fx.ctx, st.clearGit(id, w)) }
			first, second := clearAPI, clearGit
			wantAfterFirst := apiFxRowState{Exists: true, Git: true, API: false}
			if order == "git then api" {
				first, second = clearGit, clearAPI
				wantAfterFirst = apiFxRowState{Exists: true, Git: false, API: true}
			}
			if n, err := first(); err != nil || n != 1 {
				t.Fatalf("first success applied=%d err=%v", n, err)
			}
			got := apiFxRead(fx, w, apiFxGit)
			if got.Exists != wantAfterFirst.Exists || got.Git != wantAfterFirst.Git || got.API != wantAfterFirst.API || !got.Reserved {
				t.Fatalf("flags after the first surface cleared: %+v want %+v", got, wantAfterFirst)
			}
			// The first surface is already verified: repeating it applies nothing.
			if n, err := first(); err != nil || n != 0 {
				t.Fatalf("repeat success applied=%d err=%v", n, err)
			}
			if n, err := second(); err != nil || n != 1 {
				t.Fatalf("second success applied=%d err=%v", n, err)
			}
			if got := apiFxRead(fx, w, apiFxGit); got.Exists {
				t.Fatalf("row survived both surfaces: %+v", got)
			}
		})
	}
}

// The park observation: the API twin writes the API origin's row with the publication reason, and a
// shared GitLab row keeps the Git flag it already had.
func TestForgeAPIOriginObservationLiveDB(t *testing.T) {
	observe := func(fx *rollAffinityFixture, w, id uuid.UUID, api bool) {
		t.Helper()
		mustExec(fx.ctx, fx.t, fx.pool, "UPDATE runs SET status='recovery_wait',recovery_wait_cause='forge_unreachable' WHERE id=$1", id)
		var err error
		if api {
			_, err = fx.q.ObserveParkedForgeApiOrigin(fx.ctx, store.ObserveParkedForgeApiOriginParams{
				RunID: id, WorkerID: w, Generation: 1, BaseMs: 1000, CapMs: 60000, Reason: "transient_publication_exhausted"})
		} else {
			_, err = fx.q.ObserveParkedForgeOrigin(fx.ctx, store.ObserveParkedForgeOriginParams{
				RunID: id, WorkerID: w, Generation: 1, BaseMs: 1000, CapMs: 60000, Reason: "transient_publication_exhausted"})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := func(fx *rollAffinityFixture) (uuid.UUID, uuid.UUID) {
		w := fx.worker("claimant", 0, false)
		id := fx.unclaimedRun()
		if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
			t.Fatalf("claim: %s %v", got.ID, err)
		}
		return w, id
	}
	t.Run("distinct origins get distinct rows", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "github")
		w, id := claim(fx)
		observe(fx, w, id, true)
		api := apiFxRead(fx, w, apiFxAPI)
		if !api.Exists || api.Git || !api.API || apiFxRead(fx, w, apiFxGit).Exists {
			t.Fatalf("API observation: api=%+v git=%+v", api, apiFxRead(fx, w, apiFxGit))
		}
		observe(fx, w, id, true)
		if again := apiFxRead(fx, w, apiFxAPI); again.ObservedVer != api.ObservedVer+1 {
			t.Fatalf("second observation did not advance the version: %+v -> %+v", api, again)
		}
		observe(fx, w, id, false)
		if git := apiFxRead(fx, w, apiFxGit); !git.Exists || !git.Git || git.API {
			t.Fatalf("Git observation: %+v", git)
		}
		var reasons int
		if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM forge_origin_cooldowns
			WHERE worker_id=$1 AND last_reason='transient_publication_exhausted'`, w).Scan(&reasons); err != nil || reasons != 2 {
			t.Fatalf("reasons=%d err=%v", reasons, err)
		}
	})
	t.Run("same host shares a row and keeps the other flag", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "gitlab")
		w, id := claim(fx)
		observe(fx, w, id, false)
		observe(fx, w, id, true)
		got := apiFxRead(fx, w, apiFxGit)
		if !got.Git || !got.API || got.ObservedVer != 2 {
			t.Fatalf("shared row: %+v", got)
		}
		var rows int
		if err := fx.pool.QueryRow(fx.ctx, "SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1", w).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("rows=%d err=%v", rows, err)
		}
	})
	t.Run("the table rejects an unknown reason and an all-verified row", func(t *testing.T) {
		fx := newForgeAPIFixture(t, "github")
		w := fx.worker("claimant", 0, false)
		if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
			VALUES ($1,$2,$3,now(),'transient_other',now())`, fx.userID, w, apiFxAPI); err == nil {
			t.Fatal("unknown last_reason accepted")
		}
		if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified)
			VALUES ($1,$2,$3,now(),'transient_publication_exhausted',now(),false,false)`, fx.userID, w, apiFxAPI); err == nil {
			t.Fatal("a row with no unverified surface accepted")
		}
	})
}

// Repo and connection lifecycle prunes cooldown keys no authorized origin names any more, and an
// API-only key at the API origin is as authorized as the Git origin.
func TestForgeAPIOriginPruneFollowsAuthorizedOriginsLiveDB(t *testing.T) {
	fx := newForgeAPIFixture(t, "github")
	w := fx.worker("prune", 0, false)
	apiFxRow(fx, w, apiFxAPI, false, false, true)
	apiFxRow(fx, w, apiFxGit, false, true, false)
	apiFxRow(fx, w, "https://api.unrelated.example", false, false, true)
	// Any repo statement runs the pruner: the unrelated key goes, both authorized keys stay.
	mustExec(fx.ctx, t, fx.pool, "UPDATE repos SET web_url=web_url WHERE id=$1", fx.repoID)
	if !apiFxRead(fx, w, apiFxAPI).Exists || !apiFxRead(fx, w, apiFxGit).Exists || apiFxRead(fx, w, "https://api.unrelated.example").Exists {
		t.Fatal("pruner kept an unauthorized key or dropped an authorized API/Git key")
	}
	// Moving the repo to another host leaves neither origin authorized.
	mustExec(fx.ctx, t, fx.pool, "UPDATE repos SET web_url='https://moved.example/g/r' WHERE id=$1", fx.repoID)
	if apiFxRead(fx, w, apiFxAPI).Exists || apiFxRead(fx, w, apiFxGit).Exists {
		t.Fatal("pruner kept keys of a repo that moved away")
	}
}

// Test 6 (SQL half): the SQL derivation equals the expected API origins shared with the agent test
// agent/test/forge-api-origin-parity.test.ts, through the authorized view the server really uses.
func TestForgeAPIOriginParityLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	raw, err := os.ReadFile("../../../fixtures/forge-api-origin/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string `json:"name"`
		ForgeType string `json:"forge_type"`
		WebURL    string `json:"web_url"`
		APIOrigin string `json:"api_origin"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 {
		t.Fatalf("fixture has %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			conn, repo := uuid.New(), uuid.New()
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
				VALUES ($1,$2,$3,$4,'bot',1,$5)`, conn, fx.userID, c.ForgeType, "https://base-"+conn.String()+".example", []byte{0x1})
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
				VALUES ($1,$2,1,'g/r',$3,'main',true)`, repo, conn, c.WebURL)
			var got *string
			if err := fx.pool.QueryRow(fx.ctx, "SELECT api_origin FROM authorized_forge_origins WHERE repo_id=$1", repo).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got == nil || *got != c.APIOrigin {
				actual := "NULL"
				if got != nil {
					actual = *got
				}
				t.Fatalf("%s %s: SQL api_origin=%s want %s", c.ForgeType, c.WebURL, actual, c.APIOrigin)
			}
		})
	}
	t.Run("unmappable url has no API origin", func(t *testing.T) {
		var got *string
		if err := fx.pool.QueryRow(fx.ctx, "SELECT canonical_forge_api_origin('github','http://github.com/o/r')").Scan(&got); err != nil || got != nil {
			t.Fatalf("api origin of a non-https url: %v %v", got, err)
		}
	})
}

// Up -> Down -> Up replays the migration's own scripts: the Down rewrites the publication reason,
// drops API-only evidence and restores the pre-D9 shape; the second Up restores the new one.
func TestForgeAPIOriginMigrationDownUpLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "forge_api_origin_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	raw, err := os.ReadFile("migrations/00390_forge_api_origin.sql")
	if err != nil {
		t.Fatal(err)
	}
	upDown := strings.SplitN(string(raw), "-- +goose Down\n", 2)
	if len(upDown) != 2 {
		t.Fatal("missing Down script")
	}
	_, up, ok := strings.Cut(upDown[0], "-- +goose Up\n")
	if !ok {
		t.Fatal("missing Up script")
	}
	down := upDown[1]

	user, worker := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@example.com")
	mustExec(ctx, t, pool, "INSERT INTO workers(id,user_id,name,token_hash) VALUES($1,$2,'w',$3)", worker, user, worker[:])
	for _, r := range []struct {
		origin, reason string
		git, api       bool
	}{
		{"https://git.example", "transient_publication_exhausted", true, false},
		{"https://shared.example", "transient_publication_exhausted", true, true},
		{"https://api.git.example", "transient_publication_exhausted", false, true},
	} {
		mustExec(ctx, t, pool, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,git_unverified,api_unverified)
			VALUES ($1,$2,$3,now(),$4,now(),$5,$6)`, user, worker, r.origin, r.reason, r.git, r.api)
	}
	mustExec(ctx, t, pool, down)
	var survivors []string
	rows, err := pool.Query(ctx, "SELECT origin||'='||last_reason FROM forge_origin_cooldowns WHERE worker_id=$1 ORDER BY origin", worker)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		survivors = append(survivors, s)
	}
	rows.Close()
	want := []string{"https://git.example=transient_clone_fetch_exhausted", "https://shared.example=transient_clone_fetch_exhausted"}
	if strings.Join(survivors, ",") != strings.Join(want, ",") {
		t.Fatalf("after Down rows=%v want %v (API-only evidence dropped, reasons rewritten)", survivors, want)
	}
	var shape bool
	if err := pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_name IN ('forge_origin_cooldowns','run_forge_origins','authorized_forge_origins')
			  AND column_name IN ('git_unverified','api_unverified','api_origin','api_reservation_incarnation','api_reservation_version'))
		AND to_regprocedure('canonical_forge_api_origin(text,text)') IS NULL`).Scan(&shape); err != nil || !shape {
		t.Fatalf("Down left the new shape behind: %v %v", shape, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
		VALUES ($1,$2,'https://x.example',now(),'transient_publication_exhausted',now())`, user, worker); err == nil {
		t.Fatal("restored CHECK accepted the publication reason")
	}
	mustExec(ctx, t, pool, up)
	var restored bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns
			WHERE table_name='forge_origin_cooldowns' AND column_name IN ('git_unverified','api_unverified'))=2
		AND (SELECT count(*) FROM information_schema.columns
			WHERE table_name='authorized_forge_origins' AND column_name='api_origin')=1
		AND (SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1 AND git_unverified AND NOT api_unverified)=2`, worker).Scan(&restored); err != nil || !restored {
		t.Fatalf("second Up did not restore the shape or default the Git flag: %v %v", restored, err)
	}
}
