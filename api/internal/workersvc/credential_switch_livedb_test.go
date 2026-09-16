package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1247 M5a-1 (the protocol core): it EXECUTES the
// generation fence (service.go's FOR UPDATE wrapper + the fenced InsertRunMessage), the
// credential_switch RELEASE transition (ReleaseCredentialSwitch), the held-state stamp
// (StampHeldCredentialSwitch), and ClaimRun's fence-clear against a REAL Postgres — sqlc's type
// deduction is not Postgres's, so these guarded statements can pass `sqlc generate` yet fail at
// prepare/execute. Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// seedHeldRun inserts one run for owner o in the given HELD/running-family status, at an explicit
// claim_generation, owned by o's worker. status_since is 5 minutes back and started_at 20 minutes
// back so a release banks a MEASURABLE gap and its wall is visibly PRESERVED. When withStamp, the
// switch stamp (credential_switch_requested_at + _generation=gen) is pre-set as the verb would
// leave it, so a release can assert the stamp is KEPT. released marks the claim already released.
func seedHeldRun(t *testing.T, env codexTestEnv, o reevalOwner, issueIID int64, status string, gen int64, withStamp, released bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	stampAt := pgtype.Timestamptz{}
	stampGen := pgtype.Int8{}
	if withStamp {
		stampAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
		stampGen = pgtype.Int8{Int64: gen, Valid: true}
	}
	releasedAt := pgtype.Timestamptz{}
	if released {
		releasedAt = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
	             status, status_since, worker_id, anthropic_secret_id, started_at, claim_generation,
	             credential_switch_requested_at, credential_switch_generation, claim_released_at, budget_paused_seconds)
	          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, now() - interval '5 minutes', $6, $7,
	                  now() - interval '20 minutes', $8, $9, $10, $11, 0)`,
		id, o.userID, o.repoID, issueIID, status, o.workerID, o.altTok, gen, stampAt, stampGen, releasedAt)
	return id
}

// fenceSvc builds a Service wired with the pool as its tx beginner — REQUIRED for the FOR UPDATE
// generation fence, which opens a transaction when a report carries a claim generation.
func fenceSvc(env codexTestEnv) *Service {
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	return svc
}

func runningReport(gen *int64) StateRequest {
	return StateRequest{State: "running", ClaimGeneration: gen}
}

// TestSetStateGenerationFenceLiveDB is the fence's core (PRD #1247 M5, D3): a `running` report at
// generation G is ACCEPTED before a release, REJECTED (ErrStaleClaim) once the claim is released
// at G, and REJECTED once a reclaim has bumped the generation past G — and a LEGACY report (no
// generation) is honoured UNFENCED (back-compat). Dropping `locked.ClaimReleasedAt.Valid` from
// SetState's fence check reddens the released sub-test; dropping the generation compare reddens
// the reclaim sub-test.
func TestSetStateGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(7)

	t.Run("accepted before release", func(t *testing.T) {
		id := seedHeldRun(t, env, o, 5001, "running", g, false, false)
		run, applied, err := svc.SetState(env.ctx, wkr, id, runningReport(&g))
		if err != nil {
			t.Fatalf("running report at current generation: err = %v, want nil", err)
		}
		if !applied || run.Status != "running" {
			t.Fatalf("applied=%v status=%q, want applied running", applied, run.Status)
		}
	})

	t.Run("rejected after release at G", func(t *testing.T) {
		id := seedHeldRun(t, env, o, 5002, "running", g, true, false)
		// Release at G: status->queued, claim_released_at set (as the worker's credential_switch
		// report would leave it).
		gg := g
		if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &gg}); err != nil || !applied {
			t.Fatalf("release setup: applied=%v err=%v", applied, err)
		}
		_, applied, err := svc.SetState(env.ctx, wkr, id, runningReport(&g))
		if !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("report on released claim: err = %v, want ErrStaleClaim", err)
		}
		if applied {
			t.Fatal("a stale report must not be applied")
		}
		if got := statusOf(t, env, id); got != "queued" {
			t.Fatalf("released run status = %q, want it to STAY queued (the stale report must not flip it to running)", got)
		}
	})

	t.Run("rejected after reclaim past G", func(t *testing.T) {
		id := seedHeldRun(t, env, o, 5003, "running", g, false, false)
		// Simulate a reclaim: generation advances, claim_released_at cleared, still owned by W.
		env.exec(`UPDATE runs SET claim_generation = claim_generation + 1, claim_released_at = NULL WHERE id = $1`, id)
		_, applied, err := svc.SetState(env.ctx, wkr, id, runningReport(&g))
		if !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("report at superseded generation: err = %v, want ErrStaleClaim", err)
		}
		if applied {
			t.Fatal("a superseded report must not be applied")
		}
	})

	t.Run("legacy report honoured unfenced", func(t *testing.T) {
		// A run whose claim is RELEASED at G, but the report carries NO generation (a legacy,
		// non-capability worker). The fence never engages, so the report is applied exactly as
		// before the feature — proving nil generation is unfenced back-compat.
		id := seedHeldRun(t, env, o, 5004, "running", g, false, true)
		run, applied, err := svc.SetState(env.ctx, wkr, id, runningReport(nil))
		if err != nil {
			t.Fatalf("legacy report: err = %v, want nil (unfenced)", err)
		}
		if !applied || run.Status != "running" {
			t.Fatalf("legacy report applied=%v status=%q, want applied running", applied, run.Status)
		}
	})
}

// TestSetStateFenceConcurrentInterleaveLiveDB proves the FOR UPDATE serializes the fence against a
// concurrent claim (PRD #1247 M5, D3): a competing transaction holds the run's row lock and bumps
// its generation before committing, so the fenced report — which BLOCKS on that lock at its
// GetRunOwnedByWorkerForUpdate — observes the NEW generation and is rejected as stale. A Go-side
// check on a stale read then an unfenced UPDATE (the rejected TOCTOU design) would instead read
// the OLD generation and apply.
func TestSetStateFenceConcurrentInterleaveLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(11)
	id := seedHeldRun(t, env, o, 5100, "running", g, false, false)

	// T1 locks the row FIRST and holds it, so the fenced report blocks behind it.
	tx, err := env.pool.Begin(env.ctx)
	if err != nil {
		t.Fatalf("begin T1: %v", err)
	}
	defer func() { _ = tx.Rollback(env.ctx) }()
	if _, err := tx.Exec(env.ctx, `SELECT 1 FROM runs WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatalf("T1 lock: %v", err)
	}

	type result struct {
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, applied, rerr := svc.SetState(env.ctx, wkr, id, runningReport(&g))
		done <- result{applied, rerr}
	}()

	// Give the fenced report time to reach (and block on) its FOR UPDATE, then bump the
	// generation under T1 and commit — releasing the lock so the report proceeds and sees G+1.
	time.Sleep(200 * time.Millisecond)
	if _, err := tx.Exec(env.ctx, `UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, id); err != nil {
		t.Fatalf("T1 bump: %v", err)
	}
	if err := tx.Commit(env.ctx); err != nil {
		t.Fatalf("T1 commit: %v", err)
	}

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrStaleClaim) {
			t.Fatalf("interleaved report: err = %v, want ErrStaleClaim", r.err)
		}
		if r.applied {
			t.Fatal("the interleaved-stale report must not be applied")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fenced report did not return; the FOR UPDATE likely deadlocked")
	}
}

// TestReleaseCredentialSwitchLiveDB is the credential_switch RELEASE transition (PRD #1247 M5,
// D4/D14): at the right generation it requeues the held run, sets claim_released_at, BANKS the
// held gap into budget_paused_seconds (the banked-gap column), PRESERVES started_at, and KEEPS the
// switch stamp (credential_switch_requested_at + credential_switch_generation). A wrong generation
// is rejected (0-row, not idempotent). The release query's `claim_released_at IS NULL` conjunct
// is DEFENSE IN DEPTH here (the wrong-generation case is excluded by the generation compare, not
// the conjunct); it is isolated and pinned by TestReleaseCredentialSwitchReleasedConjunctLiveDB.
func TestReleaseCredentialSwitchLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	t.Run("right generation requeues and keeps the stamp", func(t *testing.T) {
		g := int64(3)
		id := seedHeldRun(t, env, o, 5200, "running", g, true, false)
		before := mustRun(t, env, id)
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g})
		if err != nil || !applied {
			t.Fatalf("release: applied=%v err=%v", applied, err)
		}
		if run.Status != "queued" {
			t.Fatalf("status = %q, want queued", run.Status)
		}
		if !run.ClaimReleasedAt.Valid {
			t.Fatal("claim_released_at must be set by the release (the fence is armed)")
		}
		// The wall is PRESERVED (not reset) and the held gap is BANKED (Open Question 3).
		if !run.StartedAt.Valid || !run.StartedAt.Time.Equal(before.StartedAt.Time) {
			t.Fatalf("started_at = %v, want it PRESERVED at %v", run.StartedAt, before.StartedAt)
		}
		if run.BudgetPausedSeconds <= 0 {
			t.Fatalf("budget_paused_seconds = %d, want the ~5-minute held gap BANKED (>0)", run.BudgetPausedSeconds)
		}
		// The stamp is KEPT — visible as "released, awaiting reclaim" (D14).
		if !run.CredentialSwitchRequestedAt.Valid || !run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 != g {
			t.Fatalf("switch stamp = (%v, %v), want it KEPT at generation %d", run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration, g)
		}
	})

	t.Run("wrong generation is rejected", func(t *testing.T) {
		g := int64(4)
		id := seedHeldRun(t, env, o, 5201, "running", g, true, false)
		wrong := g + 1
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &wrong})
		if err != nil {
			t.Fatalf("wrong-generation release: err = %v, want nil (a 0-row no-op ack)", err)
		}
		if applied {
			t.Fatal("a wrong-generation release must NOT apply")
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want the run UNCHANGED at running", got)
		}
	})
}

// TestReleaseCredentialSwitchIdempotentRedeliveryLiveDB is the retained-retry (PRD #1247 M5, D14):
// a redelivered same-generation release after the release already applied — and a redelivery after
// a reclaim — both converge to IDEMPOTENT SUCCESS, NOT a stale write and NOT a double-release.
//
// It does NOT pin the release query's `claim_released_at IS NULL` conjunct (a prior comment
// over-claimed that it did). Both redelivery cases here are excluded INDEPENDENTLY of that
// conjunct: after a release the run is 'queued' (excluded by the query's status IN (...) clause),
// and after a reclaim the generation has advanced past g (excluded by claim_generation = @generation).
// Dropping the conjunct leaves both sub-tests green. The conjunct is isolated and pinned by
// TestReleaseCredentialSwitchReleasedConjunctLiveDB instead.
func TestReleaseCredentialSwitchIdempotentRedeliveryLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	t.Run("redelivery after release is idempotent success", func(t *testing.T) {
		g := int64(6)
		id := seedHeldRun(t, env, o, 5300, "running", g, true, false)
		if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g}); err != nil || !applied {
			t.Fatalf("first release: applied=%v err=%v", applied, err)
		}
		afterFirst := mustRun(t, env, id)
		// Redeliver the SAME release.
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g})
		if err != nil || !applied {
			t.Fatalf("redelivery: applied=%v err=%v, want idempotent success", applied, err)
		}
		if run.Status != "queued" {
			t.Fatalf("redelivery status = %q, want queued", run.Status)
		}
		// No double-release: the banked gap and the release timestamp must NOT change.
		if run.BudgetPausedSeconds != afterFirst.BudgetPausedSeconds {
			t.Fatalf("budget_paused_seconds re-banked (%d -> %d): the release must not apply twice", afterFirst.BudgetPausedSeconds, run.BudgetPausedSeconds)
		}
		if !run.ClaimReleasedAt.Time.Equal(afterFirst.ClaimReleasedAt.Time) {
			t.Fatalf("claim_released_at moved (%v -> %v): the release must not apply twice", afterFirst.ClaimReleasedAt.Time, run.ClaimReleasedAt.Time)
		}
	})

	t.Run("redelivery after reclaim is idempotent success", func(t *testing.T) {
		g := int64(6)
		id := seedHeldRun(t, env, o, 5301, "running", g, true, false)
		if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g}); err != nil || !applied {
			t.Fatalf("first release: applied=%v err=%v", applied, err)
		}
		// Reclaim (same worker keeps affinity): generation advances, fence cleared.
		env.exec(`UPDATE runs SET claim_generation = claim_generation + 1, claim_released_at = NULL, status = 'running' WHERE id = $1`, id)
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g})
		if err != nil || !applied {
			t.Fatalf("redelivery after reclaim: applied=%v err=%v, want idempotent success", applied, err)
		}
		// The reclaimed run is untouched by the stale redelivery (still running at the new gen).
		if run.Status != "running" {
			t.Fatalf("status = %q, want the reclaimed run UNCHANGED at running", run.Status)
		}
	})
}

// TestClaimRunClearsReleaseFenceLiveDB proves ClaimRun clears claim_released_at as it increments
// the generation (PRD #1247 M5, D3): a claim after a release closes the fence window so the NEW
// flight's reports are not immediately rejected. Dropping `claim_released_at = NULL` from ClaimRun
// reddens this test.
func TestClaimRunClearsReleaseFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	// A queued run left RELEASED at generation g (as ReleaseCredentialSwitch leaves it), claimable
	// by o's worker (affinity via worker_id).
	g := int64(2)
	id := seedHeldRun(t, env, o, 5400, "queued", g, true, true)
	before := mustRun(t, env, id)
	if !before.ClaimReleasedAt.Valid {
		t.Fatal("precondition: the seeded run must carry claim_released_at")
	}
	claimed, err := env.q.ClaimRun(env.ctx, env.codexClaimParams(o.userID, o.workerID, nil))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed.ID != id {
		t.Fatalf("claimed %s, want the released run %s", claimed.ID, id)
	}
	if claimed.ClaimReleasedAt.Valid {
		t.Fatal("ClaimRun must CLEAR claim_released_at (close the fence window on reclaim)")
	}
	if claimed.ClaimGeneration != g+1 {
		t.Fatalf("claim_generation = %d, want %d (incremented on reclaim)", claimed.ClaimGeneration, g+1)
	}
}

// TestSetRunCredentialHeldStateStampLiveDB is the held-state stamp arm of `uzi run set-token` (PRD
// #1247 M5, D4): a switch on a run held by a worker that ADVERTISES credential_switch writes the
// override AND the stamp (200, visible as "requested"); a switch held by a worker that LACKS it is
// refused with CredentialSwitchWorkerUnsupportedError and writes NOTHING.
func TestSetRunCredentialHeldStateStampLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	g := int64(9)

	t.Run("capable worker stamps the switch", func(t *testing.T) {
		env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, o.workerID, []string{capability.CredentialSwitchV1})
		id := seedHeldRun(t, env, o, 5500, "running", g, false, false)
		res, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
		if err != nil {
			t.Fatalf("held switch (capable worker): %v", err)
		}
		// Status is UNCHANGED (the switch is only REQUESTED until the worker releases).
		if res.Run.Status != "running" {
			t.Fatalf("status = %q, want it UNCHANGED at running", res.Run.Status)
		}
		if res.Run.CredentialOverrideMode.String != CredentialOverrideModePinned ||
			!res.Run.CredentialOverrideSecretID.Valid || uuid.UUID(res.Run.CredentialOverrideSecretID.Bytes) != o.altTok {
			t.Fatalf("override not written: mode=%q id=%+v", res.Run.CredentialOverrideMode.String, res.Run.CredentialOverrideSecretID)
		}
		if !res.Run.CredentialSwitchRequestedAt.Valid || !res.Run.CredentialSwitchGeneration.Valid || res.Run.CredentialSwitchGeneration.Int64 != g {
			t.Fatalf("switch stamp not set: (%v, %v), want requested at generation %d", res.Run.CredentialSwitchRequestedAt, res.Run.CredentialSwitchGeneration, g)
		}
	})

	t.Run("incapable worker is refused and writes nothing", func(t *testing.T) {
		env.exec(`UPDATE workers SET protocol_capabilities = '{}' WHERE id = $1`, o.workerID)
		id := seedHeldRun(t, env, o, 5501, "running", g, false, false)
		_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
		var unsupported *CredentialSwitchWorkerUnsupportedError
		if !errors.As(err, &unsupported) {
			t.Fatalf("held switch (incapable worker): err = %v, want CredentialSwitchWorkerUnsupportedError", err)
		}
		if unsupported.WorkerID != o.workerID {
			t.Fatalf("refusal names worker %s, want the holding worker %s", unsupported.WorkerID, o.workerID)
		}
		run := mustRun(t, env, id)
		if run.CredentialOverrideMode.Valid || run.CredentialSwitchRequestedAt.Valid {
			t.Fatalf("a refused held switch wrote something: override=%v stamp=%v", run.CredentialOverrideMode, run.CredentialSwitchRequestedAt)
		}
	})
}

// TestSetRunCredentialHeldStateStampRacedReleaseLiveDB is the BLOCKING-1 regression: the held-state
// switch is ONE atomic fenced write, so a claim RELEASED (or reclaimed) between the verb's read and
// its write is a raced conflict — the fence (worker_id + claim_generation + claim_released_at IS
// NULL + held status) matches 0 rows → ErrCredentialSwitchRaced with NOTHING written. The prior
// two-write path (SetRunCredentialOverride then a user_id-only StampCredentialSwitch) had no
// released/generation fence, so it wrote the override AND the stamp onto an already-released claim
// while still returning 200 — the exact inconsistency this rework closes. Reverting run_credential
// to the two-write path reddens this test (err would be nil and the columns would be set).
func TestSetRunCredentialHeldStateStampRacedReleaseLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, o.workerID, []string{capability.CredentialSwitchV1})
	g := int64(7)
	// A held 'running' run whose claim was RELEASED (claim_released_at set) but still reads as
	// 'running' — the raced window a real release→requeue passes through before ClaimRun clears it.
	id := seedHeldRun(t, env, o, 5502, "running", g, false, true)

	_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
	if !errors.Is(err, ErrCredentialSwitchRaced) {
		t.Fatalf("raced held switch: err = %v, want ErrCredentialSwitchRaced", err)
	}
	run := mustRun(t, env, id)
	if run.CredentialOverrideMode.Valid || run.CredentialOverrideSecretID.Valid ||
		run.CredentialSwitchRequestedAt.Valid || run.CredentialSwitchGeneration.Valid {
		t.Fatalf("a raced held switch wrote something: override=(%v,%v) stamp=(%v,%v)",
			run.CredentialOverrideMode, run.CredentialOverrideSecretID,
			run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration)
	}
}

// TestInsertRunMessageFenceLiveDB proves the message-append fence (PRD #1247 M5, D3): a batch
// carrying the current generation lands; the SAME batch after the claim is released lands NOTHING;
// a legacy batch (no generation) lands unconditionally.
func TestInsertRunMessageFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(5)
	id := seedHeldRun(t, env, o, 5600, "running", g, false, false)

	msg := func(seq int32) IncomingMessage {
		return IncomingMessage{Seq: seq, Kind: "text", Payload: []byte(`{"t":"x"}`)}
	}
	countMsgs := func() int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		return n
	}

	// (1) At the current generation: lands.
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, id, []IncomingMessage{msg(1)}, &g); err != nil {
		t.Fatalf("append at current generation: %v", err)
	}
	if countMsgs() != 1 {
		t.Fatalf("message count = %d, want 1 (the current-generation append lands)", countMsgs())
	}

	// (2) Release the claim, then a same-generation batch must land NOTHING (fenced).
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
	if err := svc.AppendMessagesForClaim(env.ctx, wkr, id, []IncomingMessage{msg(2)}, &g); err != nil {
		t.Fatalf("append on released claim: %v", err)
	}
	if countMsgs() != 1 {
		t.Fatalf("message count = %d, want 1 (a released-claim append is fenced out)", countMsgs())
	}

	// (3) A legacy batch (nil generation) lands unconditionally, even on the released run.
	if err := svc.AppendMessages(env.ctx, wkr, id, []IncomingMessage{msg(3)}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	if countMsgs() != 2 {
		t.Fatalf("message count = %d, want 2 (a legacy append is unfenced)", countMsgs())
	}
}

// TestSetStateFailsClosedForCapabilityWorkerLiveDB is the M5a-1 rework fail-closed guard (auditor
// fail-open finding): a worker that ADVERTISES credential_switch_v1 MUST stamp claim_generation on
// a mutating report, so omitting it is REFUSED (ErrMissingClaimGeneration) rather than running
// unfenced — closing the "downgrade by omission" bypass. A LEGACY worker (no capability) that omits
// the field is still honoured unfenced (back-compat). Dropping the fail-closed check in SetState
// reddens the first sub-test (the capability worker's omission would apply).
func TestSetStateFailsClosedForCapabilityWorkerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	capWkr := store.Worker{ID: o.workerID, UserID: o.userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	legacyWkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(7)

	t.Run("capability worker omitting generation is rejected", func(t *testing.T) {
		id := seedHeldRun(t, env, o, 5700, "running", g, false, false)
		_, applied, err := svc.SetState(env.ctx, capWkr, id, runningReport(nil))
		if !errors.Is(err, ErrMissingClaimGeneration) {
			t.Fatalf("capability worker without generation: err = %v, want ErrMissingClaimGeneration", err)
		}
		if applied {
			t.Fatal("a fail-closed report must not be applied")
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want running UNCHANGED (the refused report wrote nothing)", got)
		}
	})

	t.Run("legacy worker omitting generation is honoured unfenced", func(t *testing.T) {
		id := seedHeldRun(t, env, o, 5701, "running", g, false, false)
		run, applied, err := svc.SetState(env.ctx, legacyWkr, id, runningReport(nil))
		if err != nil {
			t.Fatalf("legacy worker without generation: err = %v, want nil (unfenced back-compat)", err)
		}
		if !applied || run.Status != "running" {
			t.Fatalf("legacy report applied=%v status=%q, want applied running", applied, run.Status)
		}
	})
}

// TestAppendMessagesFailsClosedForCapabilityWorkerLiveDB is the message-append half of the same
// fail-closed guard: a capability worker that omits claim_generation on a batch is refused
// (ErrMissingClaimGeneration) and persists NOTHING, while a legacy worker's unstamped batch persists
// unfenced. Dropping the fail-closed check in appendMessages reddens the first assertion (the
// capability batch would land unfenced).
func TestAppendMessagesFailsClosedForCapabilityWorkerLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	capWkr := store.Worker{ID: o.workerID, UserID: o.userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	legacyWkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(5)
	id := seedHeldRun(t, env, o, 5710, "running", g, false, false)

	msg := func(seq int32) IncomingMessage {
		return IncomingMessage{Seq: seq, Kind: "text", Payload: []byte(`{"t":"x"}`)}
	}
	countMsgs := func() int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		return n
	}

	if err := svc.AppendMessagesForClaim(env.ctx, capWkr, id, []IncomingMessage{msg(1)}, nil); !errors.Is(err, ErrMissingClaimGeneration) {
		t.Fatalf("capability worker append without generation: err = %v, want ErrMissingClaimGeneration", err)
	}
	if countMsgs() != 0 {
		t.Fatalf("a fail-closed append must persist nothing; got %d", countMsgs())
	}
	if err := svc.AppendMessages(env.ctx, legacyWkr, id, []IncomingMessage{msg(2)}); err != nil {
		t.Fatalf("legacy append: %v", err)
	}
	if countMsgs() != 1 {
		t.Fatalf("a legacy append must persist unfenced; got %d", countMsgs())
	}
}

// TestSetLimitWaitGenerationFenceLiveDB proves the M5a-1-rework per-query fence on SetRunLimitWait
// (reviewer NB1): an OLD-generation limit_wait report against a run already RECLAIMED to G+1 under
// same-worker affinity (status stays 'running', worker unchanged — so the status guard alone does
// NOT exclude it) is fenced out (0 rows, not applied) and cannot clobber the reclaiming flight's
// run. A CURRENT-generation report on the same run DOES park, proving the FENCE — not the park
// decision — rejected the stale one. Dropping the fence conjunct from SetRunLimitWait reddens the
// old-generation assertion (the stale park would apply and flip the run to limit_wait).
func TestSetLimitWaitGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	// A svc whose limit-park budget actually parks (testParams leaves MaxWaits/MaxPark at 0, which
	// opts every report OUT of the park path), so the report exercises SetRunLimitWait, not the
	// opt-out SetRunFailed.
	p := testParams()
	p.RunLimitMaxWaits = 5
	p.RunLimitMaxPark = 2 * time.Hour
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(4)

	id := seedHeldRun(t, env, o, 5800, "running", g, false, false)
	env.exec(`UPDATE runs SET wait_on_limit = true, claim_generation = claim_generation + 1 WHERE id = $1`, id)

	// OLD generation (g) against the run now at g+1: fenced out.
	_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "limit_wait", ClaimGeneration: &g})
	if err != nil {
		t.Fatalf("old-generation limit_wait: err = %v, want nil (0-row no-op ack)", err)
	}
	if applied {
		t.Fatal("an old-generation park must NOT apply (fenced out)")
	}
	if got := statusOf(t, env, id); got != "running" {
		t.Fatalf("status = %q, want the reclaimed run UNCHANGED at running (a stale park must not clobber it)", got)
	}

	// CURRENT generation (g+1) parks — the fence, not the park decision, rejected the old one.
	cur := g + 1
	_, applied2, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "limit_wait", ClaimGeneration: &cur})
	if err != nil {
		t.Fatalf("current-generation limit_wait: %v", err)
	}
	if !applied2 {
		t.Fatal("a current-generation park must apply")
	}
	if got := statusOf(t, env, id); got != "limit_wait" {
		t.Fatalf("status = %q, want limit_wait after the current-generation park", got)
	}
}

// TestSetRecoveryWaitGenerationFenceLiveDB is the recovery_wait analog of the above (reviewer NB1):
// an OLD-generation recovery_wait report against a run reclaimed to G+1 is fenced out (0 rows, not
// applied); a current-generation report parks. recovery_wait always parks (no opt-out), so it needs
// no special budget. Dropping the fence conjunct from SetRunRecoveryWait reddens the old-generation
// assertion.
func TestSetRecoveryWaitGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(4)

	id := seedHeldRun(t, env, o, 5810, "running", g, false, false)
	env.exec(`UPDATE runs SET claim_generation = claim_generation + 1 WHERE id = $1`, id)

	_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "recovery_wait", ClaimGeneration: &g})
	if err != nil {
		t.Fatalf("old-generation recovery_wait: err = %v, want nil (0-row no-op ack)", err)
	}
	if applied {
		t.Fatal("an old-generation recovery park must NOT apply (fenced out)")
	}
	if got := statusOf(t, env, id); got != "running" {
		t.Fatalf("status = %q, want the reclaimed run UNCHANGED at running", got)
	}

	cur := g + 1
	_, applied2, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "recovery_wait", ClaimGeneration: &cur})
	if err != nil {
		t.Fatalf("current-generation recovery_wait: %v", err)
	}
	if !applied2 {
		t.Fatal("a current-generation recovery park must apply")
	}
	if got := statusOf(t, env, id); got != "recovery_wait" {
		t.Fatalf("status = %q, want recovery_wait after the current-generation park", got)
	}
}

// TestReleaseCredentialSwitchReleasedConjunctLiveDB isolates the release query's
// `claim_released_at IS NULL` conjunct (M5a-1 rework, tester finding) so it is genuinely
// load-bearing rather than only defense-in-depth. It forces the ARTIFICIAL state the normal flow
// never produces — a claim already RELEASED (claim_released_at set) while the run is STILL in a
// held status ('running') at the SAME generation — directly via SQL. In that state ONLY the
// conjunct excludes the release: the status IN (...) clause admits 'running' and the generation
// compare admits g. So a same-generation release must be REFUSED (0 rows, the run untouched);
// dropping the conjunct makes it MATCH and requeue the run to 'queued', which the status assertion
// reddens.
func TestReleaseCredentialSwitchReleasedConjunctLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(8)

	id := seedHeldRun(t, env, o, 5900, "running", g, true, true) // withStamp + released, but held
	before := mustRun(t, env, id)
	if !before.ClaimReleasedAt.Valid || before.Status != "running" {
		t.Fatalf("precondition: want a released run still at running; got released=%v status=%q", before.ClaimReleasedAt.Valid, before.Status)
	}

	run, _, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch", ClaimGeneration: &g})
	if err != nil {
		t.Fatalf("release against a released-but-held run: err = %v, want nil (0-row no-op)", err)
	}
	if run.Status != "running" {
		t.Fatalf("status = %q, want the run UNCHANGED at running (dropping `claim_released_at IS NULL` would requeue it to 'queued')", run.Status)
	}
	if !run.ClaimReleasedAt.Time.Equal(before.ClaimReleasedAt.Time) {
		t.Fatalf("claim_released_at moved (%v -> %v): the conjunct must prevent a re-release", before.ClaimReleasedAt.Time, run.ClaimReleasedAt.Time)
	}
	if run.BudgetPausedSeconds != before.BudgetPausedSeconds {
		t.Fatalf("budget_paused_seconds re-banked (%d -> %d): the conjunct must prevent a re-release", before.BudgetPausedSeconds, run.BudgetPausedSeconds)
	}
}

// TestConsumeInputsConsumeNothingWhilePendingLiveDB is the transport half's consume-nothing rule
// (PRD #1247 M5, step 2): while a credential switch is pending for the run's CURRENT claim,
// ConsumeInputs returns the worker-facing switch signal and DRAINS NOTHING — ConsumeRunInputs marks
// rows consumed on read, so a buffered follow_up that would otherwise race the release stays
// UNCONSUMED for the reclaim. Once the claim is released the signal clears but the drain stays
// FENCED until the reclaim bumps the generation; only then does the buffered input drain (exactly
// once). The three branches below walk pending -> released -> reclaimed.
func TestConsumeInputsConsumeNothingWhilePendingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	g := int64(12)

	// A running run held by o's worker with a switch pending for the current claim (stamp at g),
	// plus a buffered follow_up the pending switch must NOT drain.
	id := seedHeldRun(t, env, o, 6000, "running", g, true, false)
	env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'follow_up', 'an answer that races the switch')`, id)

	unconsumed := func() int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND consumed_at IS NULL`, id).Scan(&n); err != nil {
			t.Fatalf("count unconsumed: %v", err)
		}
		return n
	}

	// (a) Pending switch → signal returned, nothing drained, the buffered row survives.
	res, err := svc.ConsumeInputs(env.ctx, wkr, id)
	if err != nil {
		t.Fatalf("ConsumeInputs (pending switch): %v", err)
	}
	if res.CredentialSwitch == nil || res.CredentialSwitch.Generation != g {
		t.Fatalf("credential_switch = %+v, want generation %d", res.CredentialSwitch, g)
	}
	if len(res.Inputs) != 0 {
		t.Fatalf("inputs = %v, want none drained while a switch is pending", res.Inputs)
	}
	if unconsumed() != 1 {
		t.Fatalf("unconsumed rows = %d, want the buffered follow_up STILL present (consume-nothing)", unconsumed())
	}

	// (b) RELEASED but NOT yet reclaimed: the signal clears, but the drain is STILL fenced — the
	// old worker (worker_id unchanged) must not orphan the row the reclaim is owed. The buffered
	// follow_up survives.
	env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
	res2, err := svc.ConsumeInputs(env.ctx, wkr, id)
	if err != nil {
		t.Fatalf("ConsumeInputs (after release): %v", err)
	}
	if res2.CredentialSwitch != nil {
		t.Fatalf("credential_switch = %+v, want nil after release", res2.CredentialSwitch)
	}
	if len(res2.Inputs) != 0 {
		t.Fatalf("inputs = %+v, want NONE drained in the released-but-not-reclaimed window", res2.Inputs)
	}
	if unconsumed() != 1 {
		t.Fatalf("unconsumed rows = %d, want the buffered follow_up STILL present until reclaim", unconsumed())
	}

	// (c) RECLAIMED (ClaimRun clears claim_released_at and bumps the generation): the normal drain
	// resumes and the buffered follow_up is delivered exactly once to the reclaim.
	env.exec(`UPDATE runs SET claim_released_at = NULL, claim_generation = claim_generation + 1 WHERE id = $1`, id)
	res3, err := svc.ConsumeInputs(env.ctx, wkr, id)
	if err != nil {
		t.Fatalf("ConsumeInputs (after reclaim): %v", err)
	}
	if res3.CredentialSwitch != nil {
		t.Fatalf("credential_switch = %+v, want nil after reclaim (stamp generation now stale)", res3.CredentialSwitch)
	}
	if len(res3.Inputs) != 1 || res3.Inputs[0].Kind != "follow_up" {
		t.Fatalf("inputs = %+v, want the buffered follow_up drained on reclaim", res3.Inputs)
	}
	if unconsumed() != 0 {
		t.Fatalf("unconsumed rows = %d, want 0 after the reclaim drain", unconsumed())
	}
}

// TestFailCredentialSwitchLiveDB is the bounded capture-failure GIVE-UP (PRD #1247 M5, D3 step 3 /
// D14): a credential_switch_failed report from the HOLDING worker at the CURRENT generation, with a
// stamp pending, CLEARS the stamp (credential_switch_requested_at/_generation → NULL) WITHOUT
// changing status — the run keeps running on its current token (applied=true). A stale generation,
// a run with no pending stamp, and an idempotent redelivery after a successful clear all clear
// NOTHING (0-row, applied=false, no error). A report missing claim_generation is ErrInvalidState.
// Dropping the credential_switch_requested_at IS NOT NULL conjunct from ClearCredentialSwitchByWorker
// reddens the idempotent-redelivery assertion (the second clear would report applied on 0 change).
func TestFailCredentialSwitchLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}

	t.Run("holding worker at current generation clears the stamp, status unchanged", func(t *testing.T) {
		g := int64(3)
		id := seedHeldRun(t, env, o, 6100, "running", g, true, false)
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &g})
		if err != nil || !applied {
			t.Fatalf("give-up: applied=%v err=%v, want applied", applied, err)
		}
		// Status is UNCHANGED — the run keeps running on its current token.
		if run.Status != "running" {
			t.Fatalf("status = %q, want it UNCHANGED at running (the run continues on its old token)", run.Status)
		}
		// The stamp is CLEARED so the "switch requested" chip drops and the run is no longer
		// "switch requested forever".
		if run.CredentialSwitchRequestedAt.Valid || run.CredentialSwitchGeneration.Valid {
			t.Fatalf("switch stamp = (%v, %v), want both CLEARED to NULL", run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration)
		}
	})

	t.Run("stale generation clears nothing", func(t *testing.T) {
		g := int64(4)
		id := seedHeldRun(t, env, o, 6101, "running", g, true, false)
		stale := g - 1
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &stale})
		if err != nil {
			t.Fatalf("stale-generation give-up: err = %v, want nil (0-row no-op ack)", err)
		}
		if applied {
			t.Fatal("a stale-generation give-up must NOT apply")
		}
		// The stamp is UNTOUCHED — a stale/superseded report clears nothing.
		if !run.CredentialSwitchRequestedAt.Valid || !run.CredentialSwitchGeneration.Valid || run.CredentialSwitchGeneration.Int64 != g {
			t.Fatalf("switch stamp = (%v, %v), want it KEPT at generation %d (a stale report clears nothing)", run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration, g)
		}
	})

	t.Run("no pending stamp clears nothing", func(t *testing.T) {
		g := int64(5)
		id := seedHeldRun(t, env, o, 6102, "running", g, false, false) // withStamp=false
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &g})
		if err != nil {
			t.Fatalf("give-up with no pending stamp: err = %v, want nil (0-row no-op ack)", err)
		}
		if applied {
			t.Fatal("a give-up with no pending stamp must NOT apply (nothing to clear)")
		}
		if got := statusOf(t, env, id); got != "running" {
			t.Fatalf("status = %q, want running UNCHANGED", got)
		}
	})

	t.Run("idempotent redelivery after a successful clear", func(t *testing.T) {
		g := int64(6)
		id := seedHeldRun(t, env, o, 6103, "running", g, true, false)
		if _, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &g}); err != nil || !applied {
			t.Fatalf("first give-up: applied=%v err=%v, want applied", applied, err)
		}
		// Redeliver the SAME report: the stamp is already cleared, so it converges to a benign
		// no-op (applied=false, no error) instead of erroring or re-clearing.
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &g})
		if err != nil {
			t.Fatalf("redelivery: err = %v, want nil (benign no-op)", err)
		}
		if applied {
			t.Fatal("an idempotent redelivery after the clear must NOT apply")
		}
		if run.Status != "running" {
			t.Fatalf("status = %q, want running UNCHANGED on redelivery", run.Status)
		}
	})

	t.Run("missing claim_generation is ErrInvalidState", func(t *testing.T) {
		g := int64(7)
		id := seedHeldRun(t, env, o, 6104, "running", g, true, false)
		_, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: nil})
		if !errors.Is(err, ErrInvalidState) {
			t.Fatalf("give-up without claim_generation: err = %v, want ErrInvalidState", err)
		}
		if applied {
			t.Fatal("an invalid give-up must not be applied")
		}
		// Nothing was cleared — the stamp survives the invalid report.
		if got := mustRun(t, env, id); !got.CredentialSwitchRequestedAt.Valid {
			t.Fatal("an invalid give-up (no generation) must clear nothing; the stamp was dropped")
		}
	})

	t.Run("a RELEASED claim clears nothing (pins the claim_released_at IS NULL conjunct)", func(t *testing.T) {
		// A run whose claim was already RELEASED by a successful credential_switch keeps its stamp
		// (D14: the stamp stays "released, awaiting reclaim" until the reclaim's epoch write clears
		// it, M9). A give-up at that same generation must therefore clear NOTHING — the release, not
		// a capture failure, owns that stamp now. This pins the ClearCredentialSwitchByWorker
		// `claim_released_at IS NULL` conjunct: dropping it lets the give-up clear a released stamp
		// (applied=true), reddening this test — the mutation check the sibling
		// ReleaseCredentialSwitch conjunct already has.
		g := int64(8)
		id := seedHeldRun(t, env, o, 6105, "running", g, true, true) // withStamp=true, released=true
		run, applied, err := svc.SetState(env.ctx, wkr, id, StateRequest{State: "credential_switch_failed", ClaimGeneration: &g})
		if err != nil {
			t.Fatalf("give-up on a released claim: err = %v, want nil (0-row no-op ack)", err)
		}
		if applied {
			t.Fatal("a give-up on a RELEASED claim must NOT apply (the release owns the stamp until reclaim)")
		}
		if !run.CredentialSwitchRequestedAt.Valid || !run.CredentialSwitchGeneration.Valid {
			t.Fatalf("switch stamp = (%v, %v), want it KEPT on the released claim", run.CredentialSwitchRequestedAt, run.CredentialSwitchGeneration)
		}
	})
}
