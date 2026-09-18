package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). These pin PRD #1390 M3 — the global, pre-claim snapshot dedupe:
// ClaimRun's three exclusions (fresh-snapshot / terminal-pending, request-array, overflow
// closure), the service-level claimant guard (a flagged worker refused every claim), the
// snapshot-replace-before-overflow ordering, the two-tx pre-lock interleave, and the D2 hygiene
// clear of stale_requeue_generation. Most exclusion cases drive q.ClaimRun directly so each
// predicate is isolated with a clean mutation-watch; the service-level cases drive svc.Claim.

// ---- helpers --------------------------------------------------------------

// claimRunParams mirrors the ClaimRunParams Service.Claim builds for a plain non-docker worker,
// with the cutoffs a run seeded 3h in the past clears (so affinity/spread never decide a case) and
// an empty request-array exclusion. Individual tests override SnapshotFreshCutoff / RequestActive*
// to exercise one predicate.
func claimRunParams(wk store.Worker) store.ClaimRunParams {
	now := time.Now()
	return store.ClaimRunParams{
		WorkerID:              pgconv.UUID(wk.ID),
		UserID:                wk.UserID,
		AffinityCutoff:        pgconv.Time(now.Add(-25 * time.Minute)), // WorkerAffinityCeiling (testParams)
		HeartbeatCutoff:       pgconv.Time(now.Add(-45 * time.Second)), // WorkerHeartbeatStale
		SpreadCutoff:          pgconv.Time(now.Add(-9 * time.Second)),  // WorkerSpreadGrace
		BackgroundGraceCutoff: pgconv.Time(now.Add(-15 * time.Minute)),
		IsDockerWorker:        false,
		DockerRepoAllowlist:   []uuid.UUID{},
		WorkerCaps:            []string{},
		CapabilityAware:       true,
		WorkerProtocolCaps:    []string{},
		IsEphemeral:           false,
		ClaimantDraining:      false,
		CustodyHoldLimit:      8,
		RecoveryCapable:       false,
		WorkerIdentity:        "test-identity",
		// D3: the stale window plus one heartbeat interval (45s + 15s).
		SnapshotFreshCutoff: pgconv.Time(now.Add(-60 * time.Second)),
		RequestActiveIds:    []uuid.UUID{},
		RequestActiveGens:   []int64{},
	}
}

// seedClaimableRun seeds a queued run-lane run owned by ownerWk whose every time window (incl.
// updated_at) is pushed 3h into the past, so a sibling is past the affinity ceiling and the fleet
// spread is bypassed — the ONLY thing that can keep it unclaimed is a M3 exclusion.
func seedClaimableRun(t *testing.T, env codexTestEnv, userID, repoID, ownerWk uuid.UUID, gen int64) uuid.UUID {
	t.Helper()
	run := seedOutageRun(t, env, userID, repoID, ownerWk, "queued", "issue", gen, 0)
	env.exec(`UPDATE runs SET updated_at = now() - interval '3 hours' WHERE id = $1`, run)
	return run
}

// seedUnassignedRun seeds a queued run-lane run with worker_id NULL (claimable by any of the
// user's workers on affinity).
func seedUnassignedRun(t *testing.T, env codexTestEnv, userID, repoID uuid.UUID, gen int64) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5)`,
		runID, userID, repoID, nextOutageIID(), gen)
	return runID
}

// setReportedAt backdates a worker_active_runs row's reported_at (freshness input for exclusion 1).
func setReportedAt(t *testing.T, env codexTestEnv, workerID, runID uuid.UUID, ago string) {
	t.Helper()
	env.exec(`UPDATE worker_active_runs SET reported_at = now() - $3::interval WHERE worker_id = $1 AND run_id = $2`,
		workerID, runID, ago)
}

// claimGenOf reads a run's current claim_generation.
func claimGenOf(t *testing.T, env codexTestEnv, id uuid.UUID) int64 {
	t.Helper()
	var g int64
	if err := env.pool.QueryRow(env.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, id).Scan(&g); err != nil {
		t.Fatalf("read claim_generation: %v", err)
	}
	return g
}

// enableClaimAssembly reseals a decryptable bot PAT on the user's forge connection and adds a
// default Anthropic token, so a successful svc.Claim assembles a real payload (mirrors
// TestClaimOpensCustodyHoldLiveDB). seedCodexInfra stores a placeholder the master box cannot open.
func enableClaimAssembly(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	botPAT, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPAT, userID)
	anthropic, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), anthropic)
}

// wkr reads the current worker row (for a svc.Claim call).
func wkrRow(t *testing.T, env codexTestEnv, workerID uuid.UUID) store.Worker {
	t.Helper()
	w, err := env.q.GetWorkerByID(env.ctx, workerID)
	if err != nil {
		t.Fatalf("GetWorkerByID: %v", err)
	}
	return w
}

// ---- (1) fresh-snapshot / terminal-pending exclusion ----------------------

// TestClaimFreshSnapshotExclusionLiveDB: a queued run a FRESH snapshot lists at its current
// generation is returned to NOBODY — neither the owner nor a sibling past the affinity ceiling. The
// same run is returned once its snapshot row is stale or absent, UNLESS an unexpired terminal-pending
// lease still lists it. Dropping exclusion (1) lets a sibling double-claim the listed run.
func TestClaimFreshSnapshotExclusionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	// mustSkip / mustClaim assert the ClaimRun outcome for a given claimant.
	mustSkip := func(t *testing.T, wk store.Worker, run uuid.UUID) {
		t.Helper()
		if _, err := env.q.ClaimRun(env.ctx, claimRunParams(wk)); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun err = %v, want pgx.ErrNoRows (the run must be excluded)", err)
		}
		if got := statusOf(t, env, run); got != "queued" {
			t.Fatalf("run status = %q, want unchanged queued (excluded, no side effect)", got)
		}
	}
	mustClaim := func(t *testing.T, wk store.Worker, run uuid.UUID) {
		t.Helper()
		got, err := env.q.ClaimRun(env.ctx, claimRunParams(wk))
		if err != nil {
			t.Fatalf("ClaimRun err = %v, want the run returned", err)
		}
		if got.ID != run {
			t.Fatalf("claimed run = %s, want %s", got.ID, run)
		}
	}

	t.Run("fresh lease excludes owner and sibling", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		insertActiveLease(t, env, ownerWk, run, 1, false, "1 hour") // reported_at = now() (fresh)

		mustSkip(t, wkrRow(t, env, siblingWk), run) // sibling past the ceiling: still excluded
		mustSkip(t, wkrRow(t, env, ownerWk), run)   // the owner itself: also excluded (nobody)
	})

	t.Run("stale reported_at is returned", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		insertActiveLease(t, env, ownerWk, run, 1, false, "1 hour")
		setReportedAt(t, env, ownerWk, run, "5 minutes") // older than the 60s freshness cutoff

		mustClaim(t, wkrRow(t, env, siblingWk), run)
	})

	t.Run("no snapshot row is returned", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		// no worker_active_runs row at all
		mustClaim(t, wkrRow(t, env, siblingWk), run)
	})

	t.Run("unexpired terminal-pending lease excludes even when stale", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		insertActiveLease(t, env, ownerWk, run, 1, true, "1 hour") // terminal_pending, until now()+1h
		setReportedAt(t, env, ownerWk, run, "5 minutes")           // stale reported_at, but the lease still holds

		mustSkip(t, wkrRow(t, env, siblingWk), run)
	})

	t.Run("expired terminal-pending lease with stale reported_at is returned", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		insertActiveLease(t, env, ownerWk, run, 1, true, "-1 minute") // terminal_pending, but expired
		setReportedAt(t, env, ownerWk, run, "5 minutes")

		mustClaim(t, wkrRow(t, env, siblingWk), run)
	})

	t.Run("fresh lease at a different generation does not exclude", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 2) // run at gen 2
		insertActiveLease(t, env, ownerWk, run, 1, false, "1 hour") // fresh, but lists gen 1 ≠ 2

		mustClaim(t, wkrRow(t, env, siblingWk), run)
	})
}

// ---- (2) request-array exclusion (before any heartbeat) -------------------

// TestClaimRequestArrayExclusionLiveDB: the claimant's OWN request snapshot excludes its listed runs
// at the current generation, with NO worker_active_runs row persisted (the claim beats the first
// heartbeat, fact 7). A wrong-generation entry does not exclude, and an empty request array claims
// the run. Dropping exclusion (2) lets the claimant re-claim its own listed run.
func TestClaimRequestArrayExclusionLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	t.Run("listed at the current generation is excluded", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		run := seedClaimableRun(t, env, userID, repoID, wk, 3)

		p := claimRunParams(wkrRow(t, env, wk))
		p.RequestActiveIds = []uuid.UUID{run}
		p.RequestActiveGens = []int64{3} // == run's claim_generation
		if _, err := env.q.ClaimRun(env.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun err = %v, want pgx.ErrNoRows (own listed run excluded)", err)
		}
		if got := claimGenOf(t, env, run); got != 3 {
			t.Fatalf("claim_generation = %d, want unchanged 3 (no claim)", got)
		}
	})

	t.Run("empty request array claims the run", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		run := seedClaimableRun(t, env, userID, repoID, wk, 3)

		got, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, wk))) // empty request arrays
		if err != nil {
			t.Fatalf("ClaimRun err = %v, want the run claimed (nothing excludes it)", err)
		}
		if got.ID != run {
			t.Fatalf("claimed run = %s, want %s", got.ID, run)
		}
	})

	t.Run("listed at a different generation does not exclude", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		run := seedClaimableRun(t, env, userID, repoID, wk, 3)

		p := claimRunParams(wkrRow(t, env, wk))
		p.RequestActiveIds = []uuid.UUID{run}
		p.RequestActiveGens = []int64{2} // stale generation ≠ run's 3
		got, err := env.q.ClaimRun(env.ctx, p)
		if err != nil {
			t.Fatalf("ClaimRun err = %v, want the run claimed (generation must match to exclude)", err)
		}
		if got.ID != run {
			t.Fatalf("claimed run = %s, want %s", got.ID, run)
		}
	})
}

// TestClaimRequestArrayZipAlignmentLiveDB pins the id↔generation PAIRING of exclusion (2)'s
// two-array zip (the `ON req_gen.ord = req_id.ord` join). Every other request-array test drives
// this predicate with a SINGLE-element (or empty) request array, where a broken pairing
// (`ON true` → cross join, every id paired with every gen) is INDISTINGUISHABLE from the correct
// index-aligned zip at cardinality ≤ 1. A worker executing 2+ runs sends a multi-element request
// array, so the ordinal join is load-bearing: this test seeds TWO runs at DIFFERENT generations
// and asserts an arrangement where the aligned zip and a cross join give DIFFERENT answers — it
// passes on the current (aligned) code and would fail if the join became `ON true`.
func TestClaimRequestArrayZipAlignmentLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	// Each sub-test gets its OWN user + repo. Exclusion (2) is REQUEST-scoped (the arrays live only
	// on the ClaimRun call, never persisted), so a run it excludes stays queued and un-excluded; a
	// shared user would let one sub-test's leftover runs pollute the other's candidate pool.

	t.Run("each run listed at its OWN generation is excluded", func(t *testing.T) {
		userID, _, repoID := env.seedCodexInfra(t)
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		runA := seedClaimableRun(t, env, userID, repoID, wk, 7)
		runB := seedClaimableRun(t, env, userID, repoID, wk, 5)

		// Aligned pairs A↔7 and B↔5 — each id with ITS OWN current generation — so both are
		// excluded and the worker claims nothing. (A cross join would exclude both here too, so
		// this case is the positive baseline, not the discriminator.)
		p := claimRunParams(wkrRow(t, env, wk))
		p.RequestActiveIds = []uuid.UUID{runA, runB}
		p.RequestActiveGens = []int64{7, 5}
		if _, err := env.q.ClaimRun(env.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun err = %v, want pgx.ErrNoRows (both listed at their own generation → both excluded)", err)
		}
		if g := claimGenOf(t, env, runA); g != 7 {
			t.Fatalf("runA claim_generation = %d, want unchanged 7 (no claim)", g)
		}
		if g := claimGenOf(t, env, runB); g != 5 {
			t.Fatalf("runB claim_generation = %d, want unchanged 5 (no claim)", g)
		}
	})

	t.Run("misaligned generations exclude NEITHER run — the zip pairing is load-bearing", func(t *testing.T) {
		userID, _, repoID := env.seedCodexInfra(t)
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		runA := seedClaimableRun(t, env, userID, repoID, wk, 7)
		runB := seedClaimableRun(t, env, userID, repoID, wk, 5)

		// The SAME two ids, but the generations are SWAPPED against them: A is paired with 5
		// (A's current gen is 7) and B with 7 (B's current gen is 5). The correct index-aligned
		// zip matches NEITHER run at its own generation, so both stay claimable. A broken
		// `ON true` cross join would pair every id with every gen — the {5,7} gens array contains
		// both 7 (A's current gen) and 5 (B's current gen) — and would wrongly exclude BOTH, so the
		// first ClaimRun would return pgx.ErrNoRows (this user owns no other queued run).
		p := claimRunParams(wkrRow(t, env, wk))
		p.RequestActiveIds = []uuid.UUID{runA, runB}
		p.RequestActiveGens = []int64{5, 7}

		first, err := env.q.ClaimRun(env.ctx, p)
		if err != nil {
			t.Fatalf("first ClaimRun err = %v, want a run claimed (misaligned generations exclude neither)", err)
		}
		second, err := env.q.ClaimRun(env.ctx, p)
		if err != nil {
			t.Fatalf("second ClaimRun err = %v, want the other run claimed (both are claimable)", err)
		}
		got := map[uuid.UUID]bool{first.ID: true, second.ID: true}
		if !got[runA] || !got[runB] {
			t.Fatalf("claimed runs = {%s, %s}, want both runA=%s and runB=%s (the zip must pair each id with ITS OWN generation)",
				first.ID, second.ID, runA, runB)
		}
	})
}

// ---- (3) overflow closure — sibling half ----------------------------------

// TestClaimOverflowClosureSiblingLiveDB (D11): while a worker's pending_overflow closure is
// unexpired, every run it OWNS is closed to a sibling claimant (even one past the affinity ceiling);
// once the closure expires the sibling can claim.
func TestClaimOverflowClosureSiblingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	t.Run("unexpired overflow closes owned runs to a sibling", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		setOverflow(t, env, ownerWk, "1 hour")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)

		if _, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, siblingWk))); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun err = %v, want pgx.ErrNoRows (owner's overflow closes the run)", err)
		}
		if got := statusOf(t, env, run); got != "queued" {
			t.Fatalf("run status = %q, want unchanged queued", got)
		}
	})

	t.Run("expired overflow reopens owned runs to a sibling", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		setOverflow(t, env, ownerWk, "-1 minute") // expired
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)

		got, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, siblingWk)))
		if err != nil {
			t.Fatalf("ClaimRun err = %v, want the run claimed (closure lapsed)", err)
		}
		if got.ID != run {
			t.Fatalf("claimed run = %s, want %s", got.ID, run)
		}
	})
}

// ---- (3) overflow closure — claimant guard (service) ----------------------

// TestClaimFlaggedWorkerGuardLiveDB: the claimant guard refuses a flagged worker EVERY claim,
// including an unassigned (worker_id IS NULL) run, and an UNFLAGGED valid claim snapshot clears the
// closure in the SAME transaction so the claim proceeds.
func TestClaimFlaggedWorkerGuardLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, userID)
	svc := snapshotSvc(env, testParams())

	t.Run("a flagged claim snapshot refuses even an unassigned run", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		unassigned := seedUnassignedRun(t, env, userID, repoID, 0)

		// A flagged snapshot: the replace stamps pending_overflow_until in the future, so the guard
		// fires and the worker claims nothing — the replace still commits (valid info).
		flagged := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", PendingOverflow: true, Active: []ActiveRunEntry{}}
		payload, err := svc.Claim(env.ctx, wkrRow(t, env, wk), flagged)
		if err != nil {
			t.Fatalf("Claim(flagged): %v", err)
		}
		if payload != nil {
			t.Fatalf("Claim(flagged) returned a payload, want idle (a flagged worker is refused every claim)")
		}
		if got := statusOf(t, env, unassigned); got != "queued" {
			t.Fatalf("unassigned run status = %q, want unchanged queued", got)
		}
		// The closure must have persisted (the replace committed) so it keeps refusing.
		got := wkrRow(t, env, wk)
		if !got.PendingOverflowUntil.Valid || !got.PendingOverflowUntil.Time.After(time.Now()) {
			t.Fatalf("pending_overflow_until = %+v, want a future timestamp persisted by the replace", got.PendingOverflowUntil)
		}
	})

	t.Run("an unflagged valid snapshot clears the closure and the claim proceeds", func(t *testing.T) {
		wk := seedSnapshotWorker(t, env, userID, "nonce-A")
		setOverflow(t, env, wk, "1 hour") // worker was flagged before this claim
		run := seedClaimableRun(t, env, userID, repoID, wk, 0)

		// An unflagged valid snapshot (epoch advances past the seeded 0) clears pending_overflow_until
		// in the same tx, so the guard passes and the run is claimed and assembled.
		unflagged := &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", PendingOverflow: false, Active: []ActiveRunEntry{}}
		payload, err := svc.Claim(env.ctx, wkrRow(t, env, wk), unflagged)
		if err != nil {
			t.Fatalf("Claim(unflagged): %v", err)
		}
		if payload == nil {
			t.Fatal("Claim(unflagged) returned idle, want the run claimed (the unflagged snapshot cleared the closure)")
		}
		if payload.RunID != run.String() {
			t.Fatalf("claimed run = %s, want %s", payload.RunID, run)
		}
		if got := wkrRow(t, env, wk); got.PendingOverflowUntil.Valid {
			t.Fatalf("pending_overflow_until still set after an unflagged snapshot; the closure must clear")
		}
	})
}

// ---- invalid claim snapshot fails closed ----------------------------------

// TestClaimInvalidSnapshotFailsClosedLiveDB (D3): an invalid / stale-epoch / wrong-nonce CLAIM
// snapshot returns ErrActiveSnapshotInvalid (the handler maps it to 400) with NO claim and NO side
// effect — the worker's rows and the candidate run are untouched.
func TestClaimInvalidSnapshotFailsClosedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())

	cases := []struct {
		name string
		snap *ActiveSnapshot
	}{
		{"wrong nonce", &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-WRONG", Active: []ActiveRunEntry{}}},
		{"stale epoch", &ActiveSnapshot{SnapshotEpoch: 0, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{}}}, // 0 !> stored 0
		{"invalid phase", &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "nonce-A", Active: []ActiveRunEntry{{RunID: uuid.NewString(), ClaimGeneration: 1, Phase: "bogus"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wk := seedSnapshotWorker(t, env, userID, "nonce-A")
			run := seedClaimableRun(t, env, userID, repoID, wk, 1)

			payload, err := svc.Claim(env.ctx, wkrRow(t, env, wk), c.snap)
			if !errors.Is(err, ErrActiveSnapshotInvalid) {
				t.Fatalf("Claim err = %v, want ErrActiveSnapshotInvalid (fail closed)", err)
			}
			if payload != nil {
				t.Fatalf("Claim returned a payload on an invalid snapshot, want nil")
			}
			// No side effect: the run is untouched and no worker_active_runs row was written (rollback).
			if got := statusOf(t, env, run); got != "queued" {
				t.Fatalf("run status = %q, want unchanged queued (no claim on an invalid snapshot)", got)
			}
			var rows int
			if qerr := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM worker_active_runs WHERE worker_id = $1`, wk).Scan(&rows); qerr != nil {
				t.Fatalf("count rows: %v", qerr)
			}
			if rows != 0 {
				t.Fatalf("worker_active_runs rows = %d, want 0 (the rejected snapshot must roll back)", rows)
			}
		})
	}
}

// ---- two-tx interleave: the pre-lock makes a sibling SKIP LOCKED ----------

// TestClaimPreLockInterleaveLiveDB (D8): the claimant's request-snapshot pre-lock (LockOwnedRunsByIDs,
// FOR UPDATE) makes a sibling's concurrent claim (FOR UPDATE SKIP LOCKED) skip the pre-locked run and
// get nothing; once the pre-lock releases, the sibling can claim it.
func TestClaimPreLockInterleaveLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
	siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
	run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)

	// Claimant tx opens and pre-locks the run its request snapshot lists.
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin claimant tx: %v", err)
	}
	if _, err := store.New(tx).LockOwnedRunsByIDs(env.ctx, store.LockOwnedRunsByIDsParams{
		RunIds:   []uuid.UUID{run},
		WorkerID: pgconv.UUID(ownerWk),
	}); err != nil {
		_ = tx.Rollback(env.ctx)
		t.Fatalf("pre-lock: %v", err)
	}

	// While the pre-lock is held, the sibling's FOR UPDATE SKIP LOCKED claim skips the run.
	if _, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, siblingWk))); !errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(env.ctx)
		t.Fatalf("sibling ClaimRun err = %v, want pgx.ErrNoRows while the run is pre-locked", err)
	}

	// Release the pre-lock; now the sibling can claim it.
	if err := tx.Rollback(env.ctx); err != nil {
		t.Fatalf("rollback claimant tx: %v", err)
	}
	got, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, siblingWk)))
	if err != nil {
		t.Fatalf("sibling ClaimRun after release err = %v, want the run claimed", err)
	}
	if got.ID != run {
		t.Fatalf("claimed run = %s, want %s", got.ID, run)
	}
}

// ---- the hold CTE opens nothing for an excluded run -----------------------

// TestClaimExcludedRunOpensNoHoldLiveDB (D8): a claim excluded by a fresh snapshot opens NO custody
// hold even for a recovery-capable worker on a code-publishing run — the hold CTE reads FROM target,
// and an excluded run produces no target row. The contrast (no lease) claims and opens exactly one.
func TestClaimExcludedRunOpensNoHoldLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	holdsFor := func(run uuid.UUID) int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1 AND state = 'open'`, run).Scan(&n); err != nil {
			t.Fatalf("count holds: %v", err)
		}
		return n
	}

	t.Run("excluded run opens no hold", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		siblingWk := seedSnapshotWorker(t, env, userID, "nonce-B")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1)
		insertActiveLease(t, env, ownerWk, run, 1, false, "1 hour") // fresh → excluded

		p := claimRunParams(wkrRow(t, env, siblingWk))
		p.RecoveryCapable = true
		if _, err := env.q.ClaimRun(env.ctx, p); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ClaimRun err = %v, want pgx.ErrNoRows (excluded)", err)
		}
		if n := holdsFor(run); n != 0 {
			t.Fatalf("open holds for an excluded run = %d, want 0", n)
		}
	})

	t.Run("claimable run opens one hold", func(t *testing.T) {
		ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
		run := seedClaimableRun(t, env, userID, repoID, ownerWk, 1) // no lease → claimable

		p := claimRunParams(wkrRow(t, env, ownerWk))
		p.RecoveryCapable = true
		if _, err := env.q.ClaimRun(env.ctx, p); err != nil {
			t.Fatalf("ClaimRun err = %v, want the run claimed", err)
		}
		if n := holdsFor(run); n != 1 {
			t.Fatalf("open holds for a claimed run = %d, want 1", n)
		}
	})
}

// ---- D2 hygiene: a fresh claim clears stale_requeue_generation ------------

// TestClaimClearsStaleRequeueGenerationLiveDB (D2): ClaimRun clears stale_requeue_generation on a
// fresh claim, so a later re-adoption never refunds a requeue against a generation this claim replaced.
func TestClaimClearsStaleRequeueGenerationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)

	wk := seedSnapshotWorker(t, env, userID, "nonce-A")
	run := seedClaimableRun(t, env, userID, repoID, wk, 4)
	env.exec(`UPDATE runs SET stale_requeue_generation = 4 WHERE id = $1`, run)

	if _, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, wk))); err != nil {
		t.Fatalf("ClaimRun err = %v, want the run claimed", err)
	}
	if _, valid := staleReqGenOf(t, env, run); valid {
		t.Fatal("stale_requeue_generation still set after a fresh claim; ClaimRun must clear it (D2)")
	}
}

// ---- chat is unaffected by the run-lane predicates (D10) ------------------

// TestClaimChatUnaffectedByOverflowLiveDB (D10): a chat run owned by an overflowed worker is still
// claimable via the chat lane (ClaimChatRun carries none of the M3 run-lane predicates), while the
// run lane never returns the chat run at all.
func TestClaimChatUnaffectedByOverflowLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, _ := env.seedCodexInfra(t)

	ownerWk := seedSnapshotWorker(t, env, userID, "nonce-A")
	setOverflow(t, env, ownerWk, "1 hour")

	// A queued chat run owned by the flagged worker.
	chatRun := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
	          VALUES ($1, $2, NULL, 'chat', NULL, 't', 'd', 'queued', $3)`, chatRun, userID, ownerWk)

	// The run lane never returns a chat run (kind <> 'chat'), overflow or not — here there is also no
	// run-lane candidate at all, so it is idle.
	if _, err := env.q.ClaimRun(env.ctx, claimRunParams(wkrRow(t, env, ownerWk))); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("run-lane ClaimRun err = %v, want pgx.ErrNoRows (a chat run is never in the run lane)", err)
	}

	// The chat lane claims it despite the owner's overflow flag — chat carries no overflow closure.
	got, err := env.q.ClaimChatRun(env.ctx, store.ClaimChatRunParams{
		WorkerID:       pgconv.UUID(ownerWk),
		UserID:         userID,
		AffinityCutoff: pgconv.Time(time.Now().Add(-25 * time.Minute)),
		IsEphemeral:    false,
	})
	if err != nil {
		t.Fatalf("ClaimChatRun err = %v, want the chat run claimed (overflow does not touch the chat lane)", err)
	}
	if got.ID != chatRun {
		t.Fatalf("claimed chat run = %s, want %s", got.ID, chatRun)
	}
}
