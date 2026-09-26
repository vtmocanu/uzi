package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// custodyContinuationFixture is one fresh owner (user + forge connection + repo + worker) with
// the credentials svc.Claim needs to assemble a payload, so each subtest's custody count is
// isolated from every other subtest's holds (ClaimRun and the custody count are owner-scoped).
type custodyContinuationFixture struct {
	env      codexTestEnv
	userID   uuid.UUID
	workerID uuid.UUID
	repoID   uuid.UUID
	svc      *Service
	wkr      store.Worker
	nextIID  int64
}

func newCustodyContinuationFixture(t *testing.T, env codexTestEnv) *custodyContinuationFixture {
	t.Helper()
	userID, workerID, repoID := env.seedCodexInfra(t)
	// assembleClaim opens the bot PAT and an Anthropic token (mirrors TestClaimOpensCustodyHoldLiveDB).
	botPATSealed, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatalf("seal bot PAT: %v", err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, botPATSealed, userID)
	anthropicSealed, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatalf("seal anthropic token: %v", err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`,
		uuid.New(), userID, "anthropic-"+uuid.NewString(), anthropicSealed)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	// ListOwnersOverCustodyLimit is instance-wide, so an owner this fixture leaves at/over the
	// cap would leak into other packages' LiveDB tests sharing the database. Drop this owner's
	// holds (none carry captures) when the (sub)test ends.
	t.Cleanup(func() { dropCustodyHolds(t, env, userID) })
	return &custodyContinuationFixture{
		env: env, userID: userID, workerID: workerID, repoID: repoID, svc: svc,
		wkr: store.Worker{
			ID: workerID, UserID: userID, Name: "worker-alpha", Status: "online",
			ProtocolCapabilities: []string{capability.RecoveryArchiveV1},
		},
		nextIID: 1000,
	}
}

func dropCustodyHolds(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	if _, err := env.pool.Exec(env.ctx, `DELETE FROM recovery_custody_holds WHERE user_id = $1`, userID); err != nil {
		t.Errorf("cleanup custody holds: %v", err)
	}
}

// seedRun inserts an unpinned issue run in the given status at the given claim_generation.
func (f *custodyContinuationFixture) seedRun(status string, generation int64) uuid.UUID {
	f.nextIID++
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, claim_generation)
	            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6)`, id, f.userID, f.repoID, f.nextIID, status, generation)
	return id
}

// seedHold inserts one custody hold for (userID, runID) in the given state. Live FKs stay
// NULL (run_id is a plain column), so a hold may name a run id that does not exist or
// belongs to another owner.
func (f *custodyContinuationFixture) seedHold(userID, runID uuid.UUID, generation int64, state string) {
	f.env.exec(`INSERT INTO recovery_custody_holds
	              (user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity)
	            VALUES ($1, $2, $3, $4, $5, $6, 'fixture-identity')`,
		userID, f.repoID, runID, generation, state, f.workerID)
}

// seedUnrelatedOpenHolds opens n holds on n OTHER (nonexistent) run ids of this owner.
func (f *custodyContinuationFixture) seedUnrelatedOpenHolds(n int) {
	for range n {
		f.seedHold(f.userID, uuid.New(), 1, "open")
	}
}

func (f *custodyContinuationFixture) openHolds(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT count(*) FROM recovery_custody_holds WHERE user_id = $1 AND state = 'open'`, f.userID).Scan(&n); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	return n
}

func (f *custodyContinuationFixture) ownOpenHolds(t *testing.T, runID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT count(*) FROM recovery_custody_holds WHERE user_id = $1 AND run_id = $2 AND state = 'open'`, f.userID, runID).Scan(&n); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	return n
}

// claim runs a real svc.Claim and returns the claimed run id (uuid.Nil when idle).
func (f *custodyContinuationFixture) claim(t *testing.T) (uuid.UUID, int64) {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if p == nil {
		return uuid.Nil, 0
	}
	id, err := uuid.Parse(p.RunID)
	if err != nil {
		t.Fatalf("parse claimed run id %q: %v", p.RunID, err)
	}
	return id, p.ClaimGeneration
}

// requeue simulates an interrupt/requeue: the run goes back to queued, unpinned, and its open
// hold is left in place (custody is not released by a requeue).
func (f *custodyContinuationFixture) requeue(runID uuid.UUID) {
	f.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, updated_at = now() WHERE id = $1`, runID)
}

// TestClaimCustodyContinuationLiveDB (issue #1751 / ADR-1751) pins ClaimRun's continuation
// exemption against a real Postgres through svc.Claim: a requeued run (claim_generation >= 1)
// that still holds its OWN open custody hold re-claims even when its owner is at/above the
// custody hold cap, while a fresh run (and every non-exempt shape) stays blocked.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestClaimCustodyContinuationLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)

	// ── Positives: RED on the pre-#1751 clause (owner count alone). ──

	t.Run("requeued run with own hold claims at the cap", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		run := f.seedRun("queued", 1)
		f.seedHold(f.userID, run, 1, "open")
		f.seedUnrelatedOpenHolds(custodyHoldLimit - 1)
		if got := f.openHolds(t); got != custodyHoldLimit {
			t.Fatalf("precondition: open holds = %d, want %d", got, custodyHoldLimit)
		}
		got, gen := f.claim(t)
		if got != run {
			t.Fatalf("claimed %s, want the continuation run %s (exempt at the cap)", got, run)
		}
		if gen != 2 {
			t.Fatalf("claim generation = %d, want 2", gen)
		}
	})

	t.Run("requeued run with own hold claims above the cap", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		run := f.seedRun("queued", 1)
		f.seedHold(f.userID, run, 1, "open")
		f.seedUnrelatedOpenHolds(custodyHoldLimit + 2)
		if got, _ := f.claim(t); got != run {
			t.Fatalf("claimed %s, want the continuation run %s (exempt above the cap)", got, run)
		}
	})

	t.Run("unrelated holds alone at the cap do not block a continuation", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		run := f.seedRun("queued", 1)
		f.seedHold(f.userID, run, 1, "open")
		f.seedUnrelatedOpenHolds(custodyHoldLimit) // other runs' holds alone reach the cap
		if got, _ := f.claim(t); got != run {
			t.Fatalf("claimed %s, want the continuation run %s", got, run)
		}
	})

	t.Run("repeated interrupt/requeue/reclaim keeps claiming", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		// Below the cap on the first claim: a fresh run is admitted and opens its gen-1 hold,
		// which puts the owner exactly at the cap.
		f.seedUnrelatedOpenHolds(custodyHoldLimit - 1)
		run := f.seedRun("queued", 0)
		for wantGen := int64(1); wantGen <= 3; wantGen++ {
			got, gen := f.claim(t)
			if got != run {
				t.Fatalf("generation %d: claimed %s, want %s (owner open holds before claim = %d)",
					wantGen, got, run, f.openHolds(t))
			}
			if gen != wantGen {
				t.Fatalf("claim generation = %d, want %d", gen, wantGen)
			}
			if own := f.ownOpenHolds(t, run); own != wantGen {
				t.Fatalf("own open holds after gen %d = %d, want %d (holds accumulate)", wantGen, own, wantGen)
			}
			f.requeue(run)
		}
		if got := f.openHolds(t); got != int64(custodyHoldLimit)+2 {
			t.Fatalf("owner open holds = %d, want %d (cap overshoot by continuation generations)", got, custodyHoldLimit+2)
		}
	})

	// ── Negative controls: green both before and after #1751. ──

	t.Run("fresh run refused at the cap", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		f.seedRun("queued", 0)
		f.seedUnrelatedOpenHolds(custodyHoldLimit)
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s, want idle (fresh run blocked at the cap)", got)
		}
	})

	for _, status := range []string{"cancelled", "completed", "paused"} {
		t.Run(status+" run with own hold is not claimed", func(t *testing.T) {
			f := newCustodyContinuationFixture(t, env)
			run := f.seedRun(status, 1)
			f.seedHold(f.userID, run, 1, "open")
			f.seedUnrelatedOpenHolds(custodyHoldLimit)
			if got, _ := f.claim(t); got != uuid.Nil {
				t.Fatalf("claimed %s (status %s), want idle", got, status)
			}
		})
	}

	t.Run("hold on another run of the same owner does not exempt", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		other := f.seedRun("claimed", 1)
		f.seedHold(f.userID, other, 1, "open")
		f.seedRun("queued", 1) // requeued, but holds nothing of its own
		f.seedUnrelatedOpenHolds(custodyHoldLimit - 1)
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s, want idle (another run's hold must not exempt)", got)
		}
	})

	t.Run("hold with the same run id under another user does not exempt", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		run := f.seedRun("queued", 1)
		stranger := env.seedUser(t)
		t.Cleanup(func() { dropCustodyHolds(t, env, stranger) })
		f.seedHold(stranger, run, 1, "open") // run_id is a plain column: constructible
		f.seedUnrelatedOpenHolds(custodyHoldLimit)
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s, want idle (a foreign owner's hold must not exempt)", got)
		}
	})

	t.Run("requeued run with only released/discarded own holds is refused", func(t *testing.T) {
		f := newCustodyContinuationFixture(t, env)
		run := f.seedRun("queued", 2)
		f.seedHold(f.userID, run, 1, "released")
		f.seedHold(f.userID, run, 2, "discarded")
		f.seedUnrelatedOpenHolds(custodyHoldLimit)
		if got, _ := f.claim(t); got != uuid.Nil {
			t.Fatalf("claimed %s, want idle (no open own hold, no exemption)", got)
		}
	})
}

// TestCustodyContinuationHealthParityLiveDB (issue #1751) pins that the health pill and the
// owner aggregate agree with ClaimRun's continuation exemption: at the cap, the exempt
// requeued run gets no reasonCustodyLimit while a fresh run does, and blocked_runs counts
// only the fresh run.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestCustodyContinuationHealthParityLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)

	exempt := f.seedRun("queued", 1)
	f.seedHold(f.userID, exempt, 1, "open")
	fresh := f.seedRun("queued", 0)
	f.seedUnrelatedOpenHolds(custodyHoldLimit)

	now := time.Now()
	row := func(id uuid.UUID) store.ListActiveRunsForHealthRow {
		return store.ListActiveRunsForHealthRow{ID: id, UserID: f.userID, Status: "queued", Kind: "issue"}
	}
	if got := f.svc.queuedReason(env.ctx, now, row(exempt)); got == reasonCustodyLimit {
		t.Fatalf("exempt run reason = %q, want anything but the custody-limit reason", got)
	}
	if got := f.svc.queuedReason(env.ctx, now, row(fresh)); got != reasonCustodyLimit {
		t.Fatalf("fresh run reason = %q, want %q", got, reasonCustodyLimit)
	}

	adm, err := env.q.GetCustodyAdmissionForRun(env.ctx, store.GetCustodyAdmissionForRunParams{UserID: f.userID, RunID: exempt})
	if err != nil {
		t.Fatalf("GetCustodyAdmissionForRun(exempt): %v", err)
	}
	if !adm.ContinuationExempt || adm.OpenHolds != int64(custodyHoldLimit)+1 {
		t.Fatalf("admission(exempt) = %+v, want exempt with %d open holds", adm, custodyHoldLimit+1)
	}
	// Owner-scoped: asking about the exempt run as another user never reports it exempt.
	stranger := env.seedUser(t)
	adm, err = env.q.GetCustodyAdmissionForRun(env.ctx, store.GetCustodyAdmissionForRunParams{UserID: stranger, RunID: exempt})
	if err != nil {
		t.Fatalf("GetCustodyAdmissionForRun(stranger): %v", err)
	}
	if adm.ContinuationExempt || adm.OpenHolds != 0 {
		t.Fatalf("admission(stranger) = %+v, want not exempt with 0 open holds", adm)
	}

	agg, err := env.q.GetCustodyAggregateForOwner(env.ctx, store.GetCustodyAggregateForOwnerParams{
		UserID: f.userID, CustodyHoldLimit: custodyHoldLimit,
	})
	if err != nil {
		t.Fatalf("GetCustodyAggregateForOwner: %v", err)
	}
	if agg.BlockedRuns != 1 {
		t.Fatalf("blocked_runs = %d, want 1 (only the fresh run; the continuation is exempt)", agg.BlockedRuns)
	}

	// And the claim agrees: the continuation is admitted, the fresh run is not.
	if got, _ := f.claim(t); got != exempt {
		t.Fatalf("claimed %s, want the exempt run %s", got, exempt)
	}
	if got, _ := f.claim(t); got != uuid.Nil {
		t.Fatalf("second claim = %s, want idle (fresh run blocked at the cap)", got)
	}
}
