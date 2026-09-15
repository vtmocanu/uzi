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
// (StampCredentialSwitch), and ClaimRun's fence-clear against a REAL Postgres — sqlc's type
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
// is rejected (0-row, not idempotent). Dropping the release query's `claim_released_at IS NULL`
// conjunct is caught by TestReleaseCredentialSwitchIdempotentRedeliveryLiveDB.
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
// a reclaim — both converge to IDEMPOTENT SUCCESS, NOT a stale write and NOT a double-release. It
// is the test the `claim_released_at IS NULL` conjunct in ReleaseCredentialSwitch protects:
// dropping it makes the redelivery MATCH (rows=1), re-banking the gap and re-stamping
// claim_released_at — which this test's "unchanged" assertions catch.
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
