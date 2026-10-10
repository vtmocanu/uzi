package store_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// #2705: a run the stale-worker sweeper requeued stays pinned to its previous worker for
// WORKER_STALE_REQUEUE_GRACE (ClaimRun's @stale_requeue_cutoff arm), so a returning worker
// resumes it instead of a peer stealing it cold. Against a REAL Postgres; skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway one (e2e/run-store-it.sh provides it).
//
// Modeled clocks: grace 10m, ceiling (@affinity_cutoff) 30m, @heartbeat_cutoff 45s. Time never
// passes for real: status_since / updated_at are back-dated with plain UPDATEs and the cutoffs
// are computed from time.Now() minus those windows, so every case has minutes of margin.
//
// The requeue itself is driven through the real RequeueRunsOfStaleWorkers, so
// stale_requeue_generation is stamped by production SQL. Cases (e) and the Codex cycle (f)
// set columns directly where noted.

const (
	staleGrace   = 10 * time.Minute
	staleCeiling = 30 * time.Minute
)

// staleClaim runs ClaimRun as workerID with the grace cutoff now-grace (NULL when grace <= 0,
// the disabled pin) and the ceiling cutoff now-ceiling.
func (fx *rollAffinityFixture) staleClaim(workerID uuid.UUID, grace, ceiling time.Duration) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:           pgtype.UUID{Bytes: workerID, Valid: true},
		UserID:             fx.userID,
		AffinityCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-ceiling), Valid: true},
		StaleRequeueCutoff: staleCutoff(grace),
		SpreadCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		IsDockerWorker:     false,
		// The Codex protocol caps are harmless for a Claude run and let the Codex cycle test
		// reach the pin instead of being refused by the protocol clause.
		WorkerProtocolCaps: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1},
	})
}

func staleCutoff(grace time.Duration) pgtype.Timestamptz {
	if grace <= 0 {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: time.Now().Add(-grace), Valid: true}
}

// staleRequeuedRun inserts a claimed run owned by the (heartbeat-stale) owner, then runs the
// real RequeueRunsOfStaleWorkers and asserts it came back queued with its worker_id kept and
// stale_requeue_generation == claim_generation.
func (fx *rollAffinityFixture) staleRequeuedRun(owner uuid.UUID) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, claim_generation)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'claimed', $5, 1)`,
		id, fx.userID, fx.repoID, fx.nextIID(), owner)
	rows, err := fx.q.RequeueRunsOfStaleWorkers(fx.ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
	})
	if err != nil {
		fx.t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	found := false
	for _, r := range rows {
		found = found || r.ID == id
	}
	if !found {
		fx.t.Fatalf("the stale sweep did not requeue run %s", id)
	}
	run, err := fx.q.GetRunByID(fx.ctx, id)
	if err != nil {
		fx.t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "queued" || !run.WorkerID.Valid || !run.StaleRequeueGeneration.Valid ||
		run.StaleRequeueGeneration.Int64 != run.ClaimGeneration {
		fx.t.Fatalf("after the stale requeue: status=%s worker=%v stale_gen=%v claim_gen=%d, want queued/kept/equal",
			run.Status, run.WorkerID, run.StaleRequeueGeneration, run.ClaimGeneration)
	}
	return id
}

// age back-dates a run's status_since and updated_at (the clocks the pin and the ceiling read).
func (fx *rollAffinityFixture) age(run uuid.UUID, statusSince, updated time.Duration) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool,
		`UPDATE runs SET status_since = now() - make_interval(secs => $2), updated_at = now() - make_interval(secs => $3) WHERE id = $1`,
		run, statusSince.Seconds(), updated.Seconds())
}

func (fx *rollAffinityFixture) mustBlock(workerID uuid.UUID, grace, ceiling time.Duration, why string) {
	fx.t.Helper()
	if _, err := fx.staleClaim(workerID, grace, ceiling); err != pgx.ErrNoRows {
		fx.t.Fatalf("%s: peer claim got %v, want pgx.ErrNoRows (pinned)", why, err)
	}
}

func (fx *rollAffinityFixture) mustClaim(workerID, run uuid.UUID, grace, ceiling time.Duration, why string) {
	fx.t.Helper()
	c, err := fx.staleClaim(workerID, grace, ceiling)
	if err != nil {
		fx.t.Fatalf("%s: claim failed: %v", why, err)
	}
	if c.ID != run {
		fx.t.Fatalf("%s: claimed %s, want %s", why, c.ID, run)
	}
}

// strictEligibleCount mirrors the health projection for one run with the same cutoffs ClaimRun gets.
func (fx *rollAffinityFixture) strictEligibleCount(run uuid.UUID, grace, ceiling time.Duration) int64 {
	fx.t.Helper()
	row, err := fx.q.CountOnlineWorkersClaimableForRun(fx.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID:                    run,
		HeartbeatCutoff:          pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: time.Now().Add(-ceiling), Valid: true},
		StaleRequeueCutoff:       staleCutoff(grace),
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		DockerRepoAllowlist:      []uuid.UUID{},
		CodexCuratedModels:       []string{},
	})
	if err != nil {
		fx.t.Fatalf("CountOnlineWorkersClaimableForRun: %v", err)
	}
	return row.StrictEligible
}

// (a) Inside the grace a live peer cannot claim a stale-requeued run; the owner can.
func TestClaimRunStaleRequeueGraceHoldsPinLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false) // heartbeat-stale, not draining: death/hang
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)

	fx.mustBlock(p, staleGrace, staleCeiling, "inside the stale-requeue grace")
	fx.mustClaim(w, run, staleGrace, staleCeiling, "the returning owner")
}

// (b) After the grace expires a peer can claim (status_since older than the grace, run still
// inside the ceiling so only the grace arm decides). Control: just inside the grace blocks.
func TestClaimRunStaleRequeueGraceExpiryReleasesLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)

	fx.age(run, 9*time.Minute, 9*time.Minute)
	fx.mustBlock(p, staleGrace, staleCeiling, "9m into a 10m grace")
	fx.age(run, 11*time.Minute, 11*time.Minute)
	fx.mustClaim(p, run, staleGrace, staleCeiling, "11m into a 10m grace")
}

// (c) Owner row deleted (teardown): the peer claims at once, inside the grace.
func TestClaimRunStaleRequeueOwnerDeletedFallsOpenLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)

	fx.mustBlock(p, staleGrace, staleCeiling, "control before the owner row is deleted")
	fx.deleteWorker(w)
	fx.mustClaim(p, run, staleGrace, staleCeiling, "owner row deleted")
}

// (d) A NULL cutoff (grace 0, the pin disabled) reproduces today's fall-open.
func TestClaimRunStaleRequeueGraceDisabledLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)

	fx.mustClaim(p, run, 0, staleCeiling, "grace 0 (NULL cutoff)")
}

// (e) A run whose stale_requeue_generation is NULL, or does not match claim_generation, is
// not pinned. These set the column directly (the requeue is what stamps it; every fresh claim
// clears it and a later claim bumps claim_generation).
func TestClaimRunStaleRequeueGenerationGuardLiveDB(t *testing.T) {
	for _, tc := range []struct{ name, set string }{
		{"null", `UPDATE runs SET stale_requeue_generation = NULL WHERE id = $1`},
		{"mismatched", `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRollAffinityFixture(t)
			w := fx.worker("W", 2*time.Minute, false)
			p := fx.worker("P", 0, false)
			run := fx.staleRequeuedRun(w)
			fx.mustBlock(p, staleGrace, staleCeiling, "control: generation matches")
			mustExec(fx.ctx, t, fx.pool, tc.set, run)
			fx.mustClaim(p, run, staleGrace, staleCeiling, "stale_requeue_generation "+tc.name)
		})
	}
}

// (g) The ceiling shorter than the grace still wins: once updated_at is past the ceiling a
// peer CAN claim even though status_since is inside the grace.
func TestClaimRunStaleRequeueCeilingShorterThanGraceLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)
	const ceiling = 5 * time.Minute

	fx.age(run, time.Minute, time.Minute)
	fx.mustBlock(p, staleGrace, ceiling, "inside both the grace and the 5m ceiling")
	fx.age(run, time.Minute, 6*time.Minute)
	if n := fx.strictEligibleCount(run, staleGrace, ceiling); n != 1 {
		t.Fatalf("health count past the ceiling inside the grace = %d, want 1 (agrees with the claim)", n)
	}
	fx.mustClaim(p, run, staleGrace, ceiling, "past the 5m ceiling, inside the 10m grace")
}

// The health mirror (CountOnlineWorkersClaimableForRun's affinity column, read via strict_eligible) agrees with ClaimRun:
// pinned inside the grace (the stale owner is not live, so nobody counts), released after it.
func TestCountClaimableStaleRequeueGraceAgreesWithClaimLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	run := fx.staleRequeuedRun(w)

	if n := fx.strictEligibleCount(run, staleGrace, staleCeiling); n != 0 {
		t.Fatalf("strict_eligible inside the grace = %d, want 0 (the claim blocks the live peer)", n)
	}
	fx.mustBlock(p, staleGrace, staleCeiling, "claim agrees inside the grace")
	if n := fx.strictEligibleCount(run, 0, staleCeiling); n != 1 {
		t.Fatalf("strict_eligible with the pin disabled = %d, want 1", n)
	}
	fx.age(run, 11*time.Minute, 11*time.Minute)
	if n := fx.strictEligibleCount(run, staleGrace, staleCeiling); n != 1 {
		t.Fatalf("strict_eligible after the grace = %d, want 1", n)
	}
	fx.mustClaim(p, run, staleGrace, staleCeiling, "claim agrees after the grace")
}

// stalePredecessor returns id-1 in the 128-bit order Postgres compares uuids in, so a keyset
// page after it starts exactly at id (the park page then examines only this run).
func stalePredecessor(id uuid.UUID) uuid.UUID {
	out := id
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] > 0 {
			out[i]--
			return out
		}
		out[i] = 0xff
	}
	return out
}

// (f) A stale-requeued Codex run cycled through the REAL ParkQueuedCodexAccountUnavailablePage
// and PromoteCodexAccountWaitRun. Each promotion resets status_since AND updated_at, so every
// promotion opens a fresh grace AND restarts the ceiling: the pin is per-episode, and there is
// NO global bound across park/promote cycles (documented in ClaimRun's comment). The test pins
// exactly that, plus: stale_requeue_generation survives each cycle (the #1390 readoption and
// refund provenance), and an expiry inside an episode releases the run.
//
// The account gate is toggled with ccs.material_revision (1 = ahead of the run's frozen 0 on a
// 'staging' alias: gated and parked; 0 = open), the minimal state the shared gate predicate
// reads. The clock is advanced by back-dating status_since/updated_at, never by waiting.
func TestClaimRunStaleRequeueCodexParkPromoteCyclesLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	w := fx.worker("W", 2*time.Minute, false)
	p := fx.worker("P", 0, false)
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = ANY($1)`,
		[]uuid.UUID{w, p}, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1})

	secret, err := insertSecret(fx.ctx, fx.pool, fx.userID, store.KindCodexAuth, "codex-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("insert secret: %v", err)
	}
	if _, err := fx.q.InsertCodexCredentialState(fx.ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secret, UserID: fx.userID, Status: "staging",
	}); err != nil {
		t.Fatalf("insert credential state: %v", err)
	}
	run := fx.staleRequeuedRun(w)
	if _, err := fx.q.FreezeRunCodexBinding(fx.ctx, store.FreezeRunCodexBindingParams{
		SecretID: secret, AuthMode: "subscription", SecretLabel: "codex", MaterialRevision: 0,
		ID: run, UserID: fx.userID,
	}); err != nil {
		t.Fatalf("freeze binding: %v", err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET codex_account_key = '["p","w"]' WHERE id = $1`, run)
	setGate := func(closed bool) {
		rev := 0
		if closed {
			rev = 1
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE codex_credential_state SET material_revision = $2 WHERE user_secret_id = $1`, secret, rev)
	}
	readRun := func() store.Run {
		r, err := fx.q.GetRunByID(fx.ctx, run)
		if err != nil {
			t.Fatalf("GetRunByID: %v", err)
		}
		return r
	}
	firstGen := readRun().StaleRequeueGeneration

	// cycle runs one park -> promote episode. The clock first advances 3h (past both the 10m
	// grace and the 30m ceiling, measured from the previous requeue/promotion), so only a
	// promotion that restarts the clocks can explain a block afterwards.
	cycle := func(n int) {
		t.Helper()
		fx.age(run, 3*time.Hour, 3*time.Hour)
		setGate(true)
		row, err := fx.q.ParkQueuedCodexAccountUnavailablePage(fx.ctx, store.ParkQueuedCodexAccountUnavailablePageParams{
			AfterID: stalePredecessor(run), PageCap: 1,
		})
		if err != nil {
			t.Fatalf("cycle %d park page: %v", n, err)
		}
		if len(row.ParkedIds) != 1 || row.ParkedIds[0] != run {
			t.Fatalf("cycle %d: park page parked %v, want exactly the run", n, row.ParkedIds)
		}
		if got := readRun().Status; got != "recovery_wait" {
			t.Fatalf("cycle %d: status after park = %s, want recovery_wait", n, got)
		}
		setGate(false) // the account recovers
		if rows, err := fx.q.PromoteCodexAccountWaitRun(fx.ctx, run); err != nil || rows != 1 {
			t.Fatalf("cycle %d promote = (%d, %v), want (1, nil)", n, rows, err)
		}
		r := readRun()
		if r.Status != "queued" || r.StaleRequeueGeneration != firstGen {
			t.Fatalf("cycle %d: status=%s stale_gen=%v, want queued and the preserved %v", n, r.Status, r.StaleRequeueGeneration, firstGen)
		}
		fx.mustBlock(p, staleGrace, staleCeiling, "cycle "+strconv.Itoa(n)+": a fresh grace after the promotion")
	}
	cycle(1)
	cycle(2)

	// An expiry inside an episode releases the run (still inside the 30m ceiling, past grace).
	fx.age(run, 11*time.Minute, 11*time.Minute)
	fx.mustClaim(p, run, staleGrace, staleCeiling, "grace expired inside the second episode")
}
