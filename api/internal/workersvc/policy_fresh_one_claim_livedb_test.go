package workersvc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// freshHeld seeds a Codex run held by a same_conversation policy refusal at generation 1 and resumes
// it with the owner's one-claim Fresh through the real service: queued, pending, no active session.
func (p policyLive) freshHeld(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	id := p.running(t, 1)
	req := p.mustHold(t, id, holdSpec{claim: 1, head: hexHead("a"), published: true, latest: true})
	if _, err := p.resume(id, PolicyResumeFreshOneClaim, req.OutcomeID, "start over narrower"); err != nil {
		t.Fatal(err)
	}
	return id, req.OutcomeID
}

// freshStamps renders the one-claim Fresh facts of a run, keeping failure output readable.
func freshStamps(r store.Run) string {
	g := func(v pgtype.Int8) string {
		if !v.Valid {
			return "null"
		}
		return fmt.Sprint(v.Int64)
	}
	return fmt.Sprintf("pending=%v consumed=%s confirmed=%s preserve=%v session=%q gen=%d status=%s",
		r.PolicyPendingFresh, g(r.PolicyFreshConsumedGeneration), g(r.PolicyFreshConfirmedGeneration),
		r.PolicyPreserveRequired, r.SessionID.String, r.ClaimGeneration, r.Status)
}

func (p policyLive) freshAuditCount(t *testing.T, id uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := p.e.pool.QueryRow(p.e.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume' AND body LIKE $2`,
		id, `{"policy_fresh":"`+kind+`",%`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPolicyFreshResumeArmsPendingLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	id, episode := p.freshHeld(t)
	got := p.run(t, id)
	if got.Status != "queued" || got.HoldReason.Valid || got.SessionID.Valid || !got.PolicyPendingFresh || !got.PolicyPreserveRequired ||
		got.PolicyFreshConsumedGeneration.Valid || got.PolicyFreshConfirmedGeneration.Valid || !got.PolicyResolvedAt.Valid ||
		got.PolicyRequiredHead.String != hexHead("a") {
		t.Fatalf("fresh_one_claim resume: %s R=%s", freshStamps(got), got.PolicyRequiredHead.String)
	}
	in := p.inputs(t, id)
	wantAudit := `resume:{"policy_mode":"fresh_one_claim","episode_id":"` + episode + `","guidance_sha256":"` + guidanceHash("start over narrower") + `","prior_session_id":"original-session"}`
	if len(in) != 2 || in[0] != "follow_up:start over narrower" || in[1] != wantAudit {
		t.Fatalf("inputs %q, want guidance then %s", in, wantAudit)
	}
	// A lost response retried is a replay that changes nothing.
	p.untouched(t, id, "replay", func() {
		if res, err := p.resume(id, PolicyResumeFreshOneClaim, episode, "start over narrower"); err != nil || !res.Replay {
			t.Fatalf("replay: %+v %v", res, err)
		}
	})
}

// Competing owner mutations: one resume of an episode wins, every other mode or guidance is refused
// with zero mutation, whether sequential or concurrent.
func TestPolicyFreshCompetingOwnerMutationsLiveDB(t *testing.T) {
	t.Run("sequential", func(t *testing.T) {
		p := newPolicyLive(t)
		id, episode := p.freshHeld(t)
		p.untouched(t, id, "preserve after fresh_one_claim", func() {
			if _, err := p.resume(id, PolicyResumePreserve, episode, "other"); !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("preserve: %v", err)
			}
		})
		p.untouched(t, id, "fresh_from_saved_work after fresh_one_claim", func() {
			if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, episode, ""); !errors.Is(err, ErrPolicyResumeConflict) {
				t.Fatalf("fresh_from_saved_work: %v", err)
			}
		})
		p.untouched(t, id, "different guidance", func() {
			if _, err := p.resume(id, PolicyResumeFreshOneClaim, episode, "different"); !errors.Is(err, ErrPolicyResumeGuidanceMismatch) {
				t.Fatalf("different guidance: %v", err)
			}
		})
	})
	t.Run("concurrent", func(t *testing.T) {
		p := newPolicyLive(t)
		id := p.running(t, 1)
		episode := p.mustHold(t, id, holdSpec{claim: 1, head: hexHead("a"), published: true, latest: true}).OutcomeID
		modes := []string{PolicyResumeFreshOneClaim, PolicyResumePreserve, PolicyResumeFreshOneClaim, PolicyResumePreserve, PolicyResumeFreshOneClaim, PolicyResumeFreshFromSavedWork}
		var wg sync.WaitGroup
		results := make([]error, len(modes))
		for i, mode := range modes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, results[i] = p.resume(id, mode, episode, "g-"+mode)
			}()
		}
		wg.Wait()
		wins := 0
		for _, err := range results {
			if err == nil {
				wins++
			}
		}
		audits := 0
		for _, s := range p.inputs(t, id) {
			if strings.HasPrefix(s, "resume:") {
				audits++
			}
		}
		got := p.run(t, id)
		if audits != 1 || wins < 1 || got.Status != "queued" || got.PolicyPendingFresh != (got.SessionID.String == "") {
			t.Fatalf("concurrent resumes: %d audit rows, %d successes, %s, errors=%v", audits, wins, freshStamps(got), results)
		}
	})
}

// ClaimRun spends the authorization exactly once, with the audit committed in the same statement.
func TestPolicyFreshConsumedOnceLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	id, _ := p.freshHeld(t)
	claimed := p.reclaim(t, id)
	if claimed.ClaimGeneration != 2 || claimed.PolicyPendingFresh || !claimed.PolicyFreshConsumedGeneration.Valid ||
		claimed.PolicyFreshConsumedGeneration.Int64 != 2 || claimed.PolicyFreshConfirmedGeneration.Valid || claimed.SessionID.Valid {
		t.Fatalf("claim must consume the authorization at its own generation: %s", freshStamps(claimed))
	}
	if !policyFreshSession(claimed) || policyFreshUnconfirmed(claimed) {
		t.Fatal("the consuming claim must carry policy_fresh_session and not policy_fresh_unconfirmed")
	}
	if n := p.freshAuditCount(t, id, "consumed"); n != 1 {
		t.Fatalf("consumption audit rows = %d, want 1", n)
	}
	var body string
	var sameTx bool
	if err := p.e.pool.QueryRow(p.e.ctx, `SELECT i.body, i.xmin::text = r.xmin::text FROM run_user_inputs i JOIN runs r ON r.id=i.run_id
		WHERE i.run_id=$1 AND i.body LIKE '{"policy_fresh":"consumed",%'`, id).Scan(&body, &sameTx); err != nil {
		t.Fatal(err)
	}
	if body != `{"policy_fresh":"consumed","generation":2}` || !sameTx {
		t.Fatalf("consumption audit %q committed in the claim's own transaction = %v", body, sameTx)
	}
	// A lost Claim response (no report) and a reclaim at G+1: no fresh grant, an unconfirmed flag.
	again := p.releaseAndReclaim(t, id)
	if again.ClaimGeneration != 3 || again.PolicyPendingFresh || again.PolicyFreshConsumedGeneration.Int64 != 2 || policyFreshSession(again) || !policyFreshUnconfirmed(again) {
		t.Fatalf("reclaim at G+1: %s session=%v unconfirmed=%v", freshStamps(again), policyFreshSession(again), policyFreshUnconfirmed(again))
	}
	if n := p.freshAuditCount(t, id, "consumed"); n != 1 {
		t.Fatalf("reclaim re-audited consumption: %d rows", n)
	}
	// And again: a second reclaim still shows the same unconfirmed history, never a new grant.
	third := p.releaseAndReclaim(t, id)
	if policyFreshSession(third) || !policyFreshUnconfirmed(third) || third.PolicyFreshConsumedGeneration.Int64 != 2 {
		t.Fatalf("second reclaim: %s", freshStamps(third))
	}
}

// Confirmation needs the exact current generation, the consuming generation and a session id.
func TestPolicyFreshConfirmationLiveDB(t *testing.T) {
	confirmWith := func(t *testing.T, p policyLive, id uuid.UUID, gen *int64, session *string) {
		t.Helper()
		if _, err := p.e.q.SetRunRunning(p.e.ctx, store.SetRunRunningParams{
			ID: id, WorkerID: pgtype.UUID{Bytes: p.wid, Valid: true},
			SessionID: pgtype.Text{String: deref(session), Valid: session != nil}, FreshConfirmGeneration: int8Ptr(gen),
		}); err != nil {
			t.Fatalf("SetRunRunning: %v", err)
		}
	}
	seed := func(t *testing.T) (policyLive, uuid.UUID) {
		p := newPolicyLive(t)
		id, _ := p.freshHeld(t)
		p.reclaim(t, id) // generation 2 consumes the authorization
		return p, id
	}
	g1, g2, g3 := int64(1), int64(2), int64(3)
	sid := "new-conversation"
	t.Run("a stale-generation report cannot confirm", func(t *testing.T) {
		p, id := seed(t)
		confirmWith(t, p, id, &g1, &sid)
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("generation 1 confirmed generation 2's consumption: %s", freshStamps(got))
		}
	})
	t.Run("a future-generation report cannot confirm", func(t *testing.T) {
		p, id := seed(t)
		confirmWith(t, p, id, &g3, &sid)
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("generation 3 confirmed: %s", freshStamps(got))
		}
	})
	t.Run("a report at the consuming generation cannot confirm once the claim moved on", func(t *testing.T) {
		p, id := seed(t)
		if moved := p.releaseAndReclaim(t, id); moved.ClaimGeneration != 3 {
			t.Fatalf("setup: want generation 3, got %s", freshStamps(moved))
		}
		confirmWith(t, p, id, &g2, &sid) // consumed == 2 == report, but claim_generation == 3
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("generation 2 confirmed on a generation 3 claim: %s", freshStamps(got))
		}
	})
	t.Run("a legacy report without a generation cannot confirm", func(t *testing.T) {
		p, id := seed(t)
		confirmWith(t, p, id, nil, &sid)
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("a report without a generation confirmed: %s", freshStamps(got))
		}
	})
	t.Run("a current report without a session id cannot confirm", func(t *testing.T) {
		p, id := seed(t)
		confirmWith(t, p, id, &g2, nil)
		empty := ""
		confirmWith(t, p, id, &g2, &empty)
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("a report without a session confirmed: %s", freshStamps(got))
		}
	})
	t.Run("the current-generation session report confirms and keeps the constraint", func(t *testing.T) {
		p, id := seed(t)
		confirmWith(t, p, id, &g2, &sid)
		got := p.run(t, id)
		if !got.PolicyFreshConfirmedGeneration.Valid || got.PolicyFreshConfirmedGeneration.Int64 != 2 || got.SessionID.String != sid ||
			!got.PolicyPreserveRequired || got.PolicyRequiredHead.String != hexHead("a") {
			t.Fatalf("confirmation: %s R=%s", freshStamps(got), got.PolicyRequiredHead.String)
		}
		// A confirmed history is not an unconfirmed one at the next claim; the new session is ordinary.
		next := p.releaseAndReclaim(t, id)
		if policyFreshSession(next) || policyFreshUnconfirmed(next) || next.SessionID.String != sid || !next.PolicyPreserveRequired {
			t.Fatalf("reclaim after confirmation: %s session=%v unconfirmed=%v", freshStamps(next), policyFreshSession(next), policyFreshUnconfirmed(next))
		}
	})
	t.Run("through the service state report", func(t *testing.T) {
		p, id := seed(t)
		if _, _, err := p.svc.SetState(p.e.ctx, p.worker, id, StateRequest{State: "running", ClaimGeneration: &g1, SessionID: &sid}); err == nil {
			t.Fatal("a stale-generation state report must be refused")
		}
		if got := p.run(t, id); got.PolicyFreshConfirmedGeneration.Valid {
			t.Fatalf("stale service report confirmed: %s", freshStamps(got))
		}
		if _, applied, err := p.svc.SetState(p.e.ctx, p.worker, id, StateRequest{State: "running", ClaimGeneration: &g2, SessionID: &sid}); err != nil || !applied {
			t.Fatalf("current report: applied=%v %v", applied, err)
		}
		if got := p.run(t, id); !got.PolicyFreshConfirmedGeneration.Valid || got.PolicyFreshConfirmedGeneration.Int64 != 2 {
			t.Fatalf("current service report did not confirm: %s", freshStamps(got))
		}
	})
}

// Crash windows: before consumption the grant stays pending; after consumption (before or after the
// thread starts) with no confirming report, the next claim is unconfirmed, never a new grant.
func TestPolicyFreshCrashWindowsLiveDB(t *testing.T) {
	t.Run("before consumption", func(t *testing.T) {
		p := newPolicyLive(t)
		id, _ := p.freshHeld(t)
		// The worker never claimed: nothing changed, and a worker restart does not matter.
		if got := p.run(t, id); !got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Valid {
			t.Fatalf("pending grant lost: %s", freshStamps(got))
		}
		claimed := p.reclaim(t, id)
		if !policyFreshSession(claimed) {
			t.Fatalf("first claim after the crash must carry the grant: %s", freshStamps(claimed))
		}
	})
	for name, report := range map[string]func(t *testing.T, p policyLive, id uuid.UUID){
		"after consumption before start": func(t *testing.T, p policyLive, id uuid.UUID) {},
		"after start before the session report": func(t *testing.T, p policyLive, id uuid.UUID) {
			// The running report that carries no session id (the thread exists only at the agent).
			gen := int64(2)
			if _, _, err := p.svc.SetState(p.e.ctx, p.worker, id, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPolicyLive(t)
			id, _ := p.freshHeld(t)
			claimed := p.reclaim(t, id)
			if !policyFreshSession(claimed) {
				t.Fatalf("consuming claim: %s", freshStamps(claimed))
			}
			report(t, p, id)
			next := p.releaseAndReclaim(t, id)
			if policyFreshSession(next) || !policyFreshUnconfirmed(next) || next.SessionID.Valid {
				t.Fatalf("%s: reclaim %s session=%v unconfirmed=%v", name, freshStamps(next), policyFreshSession(next), policyFreshUnconfirmed(next))
			}
		})
	}
}

// The restore writers: only the exact claim's server-known non-delivery returns the authorization.
func TestPolicyFreshRestoreLiveDB(t *testing.T) {
	// The restore is exercised through the real finishRunClaim path, so the claim must not need a
	// Codex binding: the policy facts are harness-independent claim inputs.
	seed := func(t *testing.T) (policyLive, uuid.UUID, store.Run) {
		p := newPolicyLive(t)
		id, _ := p.freshHeld(t)
		p.e.exec(t, "UPDATE runs SET harness='claude' WHERE id=$1", id)
		return p, id, p.reclaim(t, id)
	}
	ident := func(p policyLive) claimRecoveryIdentity { return claimRecoveryIdentity{workerID: p.wid} }
	for name, cause := range map[string]error{"transient assembly requeue": errVaultLocked, "credential disabled park": errCredentialDisabled} {
		t.Run(name+" restores the exact claim", func(t *testing.T) {
			p, id, claimed := seed(t)
			if !policyFreshSession(claimed) {
				t.Fatalf("setup: %s", freshStamps(claimed))
			}
			if out, err := p.svc.finishRunClaim(p.e.ctx, claimed, nil, cause, ident(p)); err != nil || out != nil {
				t.Fatalf("finishRunClaim: %v %v", out, err)
			}
			got := p.run(t, id)
			if !got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Valid || got.PolicyFreshConfirmedGeneration.Valid || !got.PolicyPreserveRequired {
				t.Fatalf("non-delivery did not restore the grant: %s", freshStamps(got))
			}
			if p.freshAuditCount(t, id, "consumed") != 1 || p.freshAuditCount(t, id, "restored") != 1 {
				t.Fatalf("audit rows consumed=%d restored=%d, want 1 and 1", p.freshAuditCount(t, id, "consumed"), p.freshAuditCount(t, id, "restored"))
			}
			var body string
			if err := p.e.pool.QueryRow(p.e.ctx, `SELECT body FROM run_user_inputs WHERE run_id=$1 AND body LIKE '{"policy_fresh":"restored",%'`, id).Scan(&body); err != nil || body != `{"policy_fresh":"restored","generation":2}` {
				t.Fatalf("restore audit %q %v", body, err)
			}
			if name == "credential disabled park" {
				// Re-enable: the owner path returns the run to queued; the next claim holds the grant again.
				p.e.exec(t, "UPDATE runs SET status='queued',status_since=now(),hold_reason=NULL WHERE id=$1", id)
			}
			again := p.reclaim(t, id)
			if again.ClaimGeneration != 3 || !policyFreshSession(again) || policyFreshUnconfirmed(again) {
				t.Fatalf("claim after restore: %s session=%v unconfirmed=%v", freshStamps(again), policyFreshSession(again), policyFreshUnconfirmed(again))
			}
		})
	}
	t.Run("a stale generation never restores", func(t *testing.T) {
		p, id, _ := seed(t)
		// Generation 2 consumed and was lost; generation 3 is a later claim with an unconfirmed history.
		later := p.releaseAndReclaim(t, id)
		if later.ClaimGeneration != 3 || !policyFreshUnconfirmed(later) {
			t.Fatalf("setup: %s", freshStamps(later))
		}
		before := p.freshAuditCount(t, id, "restored")
		if out, err := p.svc.finishRunClaim(p.e.ctx, later, nil, errVaultLocked, ident(p)); err != nil || out != nil {
			t.Fatalf("finishRunClaim: %v %v", out, err)
		}
		got := p.run(t, id)
		if got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Int64 != 2 || p.freshAuditCount(t, id, "restored") != before {
			t.Fatalf("generation 3's requeue restored generation 2's consumption: %s", freshStamps(got))
		}
		// The writer itself, fenced at the wrong generation, changes nothing at all.
		p.e.exec(t, "UPDATE runs SET status='claimed' WHERE id=$1", id)
		p.untouched(t, id, "stale-generation requeue", func() {
			n, err := p.e.q.RequeueClaimAssemblyExact(p.e.ctx, store.RequeueClaimAssemblyExactParams{
				ID: id, WorkerID: pgtype.UUID{Bytes: p.wid, Valid: true}, ClaimGeneration: 2})
			if err != nil || n != 0 {
				t.Fatalf("stale requeue: rows=%d %v", n, err)
			}
		})
	})
	t.Run("a confirmed claim never restores", func(t *testing.T) {
		p, id, claimed := seed(t)
		p.e.exec(t, "UPDATE runs SET policy_fresh_confirmed_generation=2 WHERE id=$1", id)
		if out, err := p.svc.finishRunClaim(p.e.ctx, claimed, nil, errVaultLocked, ident(p)); err != nil || out != nil {
			t.Fatalf("finishRunClaim: %v %v", out, err)
		}
		got := p.run(t, id)
		if got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Int64 != 2 || p.freshAuditCount(t, id, "restored") != 0 {
			t.Fatalf("a confirmed consumption was restored: %s", freshStamps(got))
		}
	})
	t.Run("the codex-account park restores the exact claim", func(t *testing.T) {
		p, id, claimed := seed(t)
		if _, err := p.e.q.ParkRunCodexAccountUnavailable(p.e.ctx, store.ParkRunCodexAccountUnavailableParams{
			ID: id, WorkerID: pgtype.UUID{Bytes: p.wid, Valid: true}, ClaimGeneration: claimed.ClaimGeneration}); err != nil {
			t.Fatal(err)
		}
		got := p.run(t, id)
		if got.Status != "recovery_wait" || !got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Valid || p.freshAuditCount(t, id, "restored") != 1 {
			t.Fatalf("codex-account park: %s restored=%d", freshStamps(got), p.freshAuditCount(t, id, "restored"))
		}
	})
	// The crash/loss requeue writers release the claim without knowing whether the payload reached
	// the worker, so they must keep the consumption: only the exact-claim non-delivery writers restore.
	notRestored := func(t *testing.T, p policyLive, id uuid.UUID, writer string) {
		t.Helper()
		got := p.run(t, id)
		if got.Status != "queued" {
			t.Fatalf("%s did not requeue the run (status %s): the writer was not exercised", writer, got.Status)
		}
		if got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Int64 != 2 || got.PolicyFreshConfirmedGeneration.Valid || p.freshAuditCount(t, id, "restored") != 0 {
			t.Fatalf("%s restored the grant: %s", writer, freshStamps(got))
		}
	}
	t.Run("the claimed-never-started sweep never restores", func(t *testing.T) {
		p, id, _ := seed(t)
		// The sweep is global over a shared database: age only this claim far past any other row and sweep
		// with a cutoff between them, so no other test's claimed run is requeued.
		p.e.exec(t, "UPDATE runs SET claimed_at=now()-interval '5000 days' WHERE id=$1", id)
		cutoff := pgconv.Time(time.Now().Add(-1000 * 24 * time.Hour))
		if _, err := p.e.q.SweepClaimedNeverStarted(p.e.ctx, store.SweepClaimedNeverStartedParams{Cutoff: cutoff, WitnessCutoff: cutoff}); err != nil {
			t.Fatal(err)
		}
		notRestored(t, p, id, "SweepClaimedNeverStarted")
	})
	t.Run("the missing-from-snapshot requeue never restores", func(t *testing.T) {
		p, id, _ := seed(t)
		// The worker started the run, then dropped it from its snapshot.
		p.e.exec(t, "UPDATE runs SET status='running',status_since=now()-interval '1 hour' WHERE id=$1", id)
		if _, err := p.e.q.RequeueRunsMissingFromSnapshot(p.e.ctx, store.RequeueRunsMissingFromSnapshotParams{
			WorkerID:             pgtype.UUID{Bytes: p.wid, Valid: true},
			MissingCutoff:        pgconv.Time(time.Now().Add(-60 * time.Second)),
			MaxRequeues:          5,
			Now:                  pgconv.Time(time.Now()),
			GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
		}); err != nil {
			t.Fatal(err)
		}
		notRestored(t, p, id, "RequeueRunsMissingFromSnapshot")
	})
}

// Retirement: a new hold and a fresh_from_saved_work resume both end any one-claim Fresh history,
// confirmed or not, and later ordinary claims carry no Fresh facts.
func TestPolicyFreshHistoryRetiredLiveDB(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprintf("confirmed=%v", confirmed), func(t *testing.T) {
			p := newPolicyLive(t)
			id, _ := p.freshHeld(t)
			claimed := p.reclaim(t, id) // generation 2 consumes
			if confirmed {
				gen, sid := int64(2), "new-conversation"
				if _, applied, err := p.svc.SetState(p.e.ctx, p.worker, id, StateRequest{State: "running", ClaimGeneration: &gen, SessionID: &sid}); err != nil || !applied {
					t.Fatalf("confirm: %v %v", applied, err)
				}
			}
			// The resumed claim is now unusable on a compatible lane and re-holds as an attempt-path episode.
			p.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", id)
			req := p.mustHold(t, id, holdSpec{claim: claimed.ClaimGeneration, mode: "fresh_from_saved_work", rehold: "attempt_path"})
			held := p.run(t, id)
			if held.PolicyPendingFresh || held.PolicyFreshConsumedGeneration.Valid || held.PolicyFreshConfirmedGeneration.Valid || !held.PolicyPreserveRequired {
				t.Fatalf("a new hold must retire the Fresh history: %s", freshStamps(held))
			}
			if _, err := p.resume(id, PolicyResumeFreshFromSavedWork, req.OutcomeID, ""); err != nil {
				t.Fatal(err)
			}
			resumed := p.run(t, id)
			if resumed.PolicyPendingFresh || resumed.PolicyFreshConsumedGeneration.Valid || resumed.PolicyFreshConfirmedGeneration.Valid || resumed.PolicyPreserveRequired {
				t.Fatalf("fresh_from_saved_work resume: %s", freshStamps(resumed))
			}
			for gen := int64(3); gen <= 4; gen++ {
				next := p.releaseAndReclaim(t, id)
				if next.ClaimGeneration != gen || policyFreshSession(next) || policyFreshUnconfirmed(next) || next.PolicyPendingFresh || next.PolicyFreshConsumedGeneration.Valid {
					t.Fatalf("ordinary claim %d carries Fresh facts: %s", gen, freshStamps(next))
				}
			}
		})
	}
}

// Resuming a compat episode with fresh_one_claim twice across episodes spends at most one grant per
// episode: the second episode's retirement leaves the first consumption unable to confirm.
func TestPolicyFreshNewEpisodeRetiresUnconfirmedLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	id, _ := p.freshHeld(t)
	claimed := p.reclaim(t, id) // generation 2 consumed, never confirmed
	p.e.exec(t, "UPDATE runs SET status='running' WHERE id=$1", id)
	// The unconfirmed claim's re-hold: a metadata-free resume_rejected hold of the same compat lane.
	req := p.mustHold(t, id, holdSpec{claim: claimed.ClaimGeneration, rehold: "fresh_start_uncertain"})
	got := p.run(t, id)
	if got.PolicyHoldCause.String != "fresh_start_uncertain" || got.PolicyFreshConsumedGeneration.Valid || got.PolicyPendingFresh || !got.PolicyPreserveRequired {
		t.Fatalf("fresh_start_uncertain re-hold: cause=%s %s", got.PolicyHoldCause.String, freshStamps(got))
	}
	// The owner can choose Fresh again, which is a new single-claim grant.
	if _, err := p.resume(id, PolicyResumeFreshOneClaim, req.OutcomeID, ""); err != nil {
		t.Fatal(err)
	}
	again := p.reclaim(t, id)
	if !policyFreshSession(again) || again.PolicyFreshConsumedGeneration.Int64 != again.ClaimGeneration {
		t.Fatalf("second grant: %s", freshStamps(again))
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
