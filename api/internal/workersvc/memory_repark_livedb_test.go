package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// #3638: after an owner Resume (episode incremented, memory_policy NULL, zero
// interventions) and a fresh claim under a new registration nonce and claim
// generation, the agent re-parks an unproven memory continuation with no
// reservation and no episode increment. The server must accept that park once,
// bind it to the claim's current episode, and refuse an exact replay.
func TestMemoryReparkFromFreshClaimAfterResumeLiveDB(t *testing.T) {
	prepare := func(t *testing.T, checker bool) (memoryFixture, uuid.UUID, store.Worker, string, store.Run) {
		var f memoryFixture
		var lead uuid.UUID
		if checker {
			f, lead = memoryCheckerFixture(t)
		} else {
			f = newMemoryFixture(t)
		}
		f.reserve(t, f.b)
		f.hold(t)
		if _, err := f.e.q.ResumeMemoryEpisode(f.e.ctx, store.ResumeMemoryEpisodeParams{
			ID: f.b.RunID, UserID: f.w.UserID, GlobalTimeoutSeconds: 86400}); err != nil {
			t.Fatal(err)
		}
		w, nonce, err := f.s.Register(f.e.ctx, f.w, "fixture", "", nil, nil,
			[]string{capability.WorkerMemoryPressureV1}, nil)
		if err != nil || nonce == "" || nonce == f.b.RegisterNonce {
			t.Fatalf("registration nonce=%q err=%v", nonce, err)
		}
		fresh, err := f.e.q.ClaimRun(f.e.ctx, claimRunParams(w))
		if checker && err == nil && fresh.ID == lead {
			// Register's orphan recovery requeued the live parent; it claims first.
			// Re-bind the attempt snapshot to the parent's new generation so the
			// parent stays eligible, then claim the checker itself.
			// The parent's new claim superseded the attempt (verdict failed); re-arm
			// it as a pending attempt bound to the parent's current generation.
			f.e.exec(`UPDATE cross_checks SET lead_claim_generation=$2,verdict='pending',reason_class=NULL,
				decided_at=NULL,interrupted_at=NULL WHERE lead_run_id=$1`, lead, fresh.ClaimGeneration)
			// ClaimRun's cross-check lane needs a full child-eligibility setup that
			// is not under test; write the claim columns the real claim writes.
			f.e.exec(`UPDATE runs SET status='running',worker_id=$2,claim_generation=3,claim_released_at=NULL,
				released_worker_id=NULL,released_worker_nonce=NULL WHERE id=$1`, f.b.RunID, f.w.ID)
			fresh = f.run(t)
		}
		if err != nil || fresh.ID != f.b.RunID || fresh.ClaimGeneration != 3 ||
			fresh.MemoryEpisode != 1 || fresh.MemoryPolicy != nil || fresh.MemoryInterventionCount != 0 {
			t.Fatalf("fresh claim=%+v err=%v", fresh, err)
		}
		return f, lead, w, nonce, fresh
	}
	req := func(nonce string, gen, ep int64) StateRequest {
		cause := recoveryCauseWorkerMemoryPressure
		return StateRequest{State: "recovery_wait", RecoveryCause: &cause,
			ClaimGeneration: &gen, MemoryEpisode: &ep, RegisterNonce: nonce}
	}

	t.Run("issue run", func(t *testing.T) {
		f, _, w, nonce, fresh := prepare(t, false)
		held, applied, err := f.s.SetState(f.e.ctx, w, f.b.RunID, req(nonce, fresh.ClaimGeneration, fresh.MemoryEpisode))
		if err != nil || !applied {
			t.Fatalf("re-park applied=%v err=%v", applied, err)
		}
		if held.Status != "recovery_wait" || held.RecoveryWaitCause.String != recoveryCauseWorkerMemoryPressure ||
			!held.ClaimReleasedAt.Valid || held.ReleasedWorkerNonce.String != nonce ||
			held.MemoryEpisode != 1 || held.MemoryInterventionCount != 0 {
			t.Fatalf("re-park row=%+v", held)
		}
		// Exact replay is refused and changes nothing.
		_, applied, err = f.s.SetState(f.e.ctx, w, f.b.RunID, req(nonce, fresh.ClaimGeneration, fresh.MemoryEpisode))
		if err != nil || applied {
			t.Fatalf("exact replay must be a no-op, not a refusal: applied=%v err=%v", applied, err)
		}
		if got := f.run(t); got.MemoryEpisode != 1 || got.MemoryInterventionCount != 0 || got.Status != "recovery_wait" {
			t.Fatalf("replay changed row=%+v", got)
		}
	})

	t.Run("cross-check eligible parent", func(t *testing.T) {
		f, _, w, nonce, fresh := prepare(t, true)
		held, applied, err := f.s.SetState(f.e.ctx, w, f.b.RunID, req(nonce, fresh.ClaimGeneration, fresh.MemoryEpisode))
		if err != nil || !applied || held.Status != "recovery_wait" || held.MemoryEpisode != 1 {
			t.Fatalf("checker re-park applied=%v err=%v row=%+v", applied, err, held)
		}
	})

	t.Run("cross-check parent fence", func(t *testing.T) {
		f, lead, w, nonce, fresh := prepare(t, true)
		f.e.exec("UPDATE runs SET claim_released_at=now() WHERE id=$1", lead)
		_, applied, err := f.s.SetState(f.e.ctx, w, f.b.RunID, req(nonce, fresh.ClaimGeneration, fresh.MemoryEpisode))
		if applied || !errors.Is(err, ErrStaleClaim) {
			t.Fatalf("ineligible parent re-park applied=%v err=%v", applied, err)
		}
		if got := f.run(t); got.Status == "recovery_wait" || got.MemoryInterventionCount != 0 {
			t.Fatalf("fenced park changed row=%+v", got)
		}
	})
}
