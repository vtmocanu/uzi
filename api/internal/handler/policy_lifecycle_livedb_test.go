package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type policyLifecycleDTO struct {
	PreserveRequired bool     `json:"preserve_required"`
	OutcomeID        string   `json:"outcome_id"`
	ResolvedAt       *string  `json:"resolved_at"`
	HoldCause        *string  `json:"hold_cause"`
	ResumeModes      []string `json:"resume_modes"`
}

// ownerDTO reads the owner's run detail over the real route and returns its policy_hold member.
func (fx *compactPolicyFx) ownerDTO(run uuid.UUID) policyLifecycleDTO {
	fx.t.Helper()
	rec := bearerReq(fx.router, http.MethodGet, "/api/runs/"+run.String(), fx.ownerUzc)
	var wrapped struct {
		Run struct {
			PolicyHold policyLifecycleDTO `json:"policy_hold"`
		} `json:"run"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &wrapped) != nil {
		fx.t.Fatalf("GET run = %d %s", rec.Code, rec.Body.String())
	}
	return wrapped.Run.PolicyHold
}

// rehold posts a worker policy re-hold (resume_rejected) with the given cause and continuation mode
// over the real worker route and returns the new episode id.
func (fx *compactPolicyFx) rehold(run uuid.UUID, gen int64, cause, mode, head string) string {
	fx.t.Helper()
	published, contains := true, true
	req := workersvc.PolicyHoldRequest{OutcomeID: uuid.NewString(), SourceGeneration: gen, ClaimGeneration: gen, Detail: "resume_rejected", Cause: cause,
		ContinuationMode: mode, Head: &head, Published: published, CheckpointContainsLatest: &contains}
	b, err := json.Marshal(req)
	if err != nil {
		fx.t.Fatal(err)
	}
	rec := bearerReqBody(fx.router, http.MethodPost, "/api/worker/runs/"+run.String()+"/policy-hold", fx.workerToken, string(b))
	var reply workersvc.PolicyHoldReply
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.Disposition != "applied" {
		fx.t.Fatalf("worker re-hold = %d %s", rec.Code, rec.Body.String())
	}
	return req.OutcomeID
}

// realClaim drives the real worker claim route. The shared fixture seeds no Codex binding, so the
// run is claimed as a claude-harness run (the claim-path device TestPolicyFreshOneClaimChainLiveDB
// uses; the policy claim facts are harness-independent) and returned to codex so the worker policy
// routes accept the next report.
func (fx *compactPolicyFx) realClaim(run uuid.UUID) map[string]any {
	fx.t.Helper()
	fx.exec("UPDATE runs SET harness='claude' WHERE id=$1", run)
	payload := fx.workerClaim(run)
	fx.exec("UPDATE runs SET harness='codex' WHERE id=$1", run)
	return payload
}

func (fx *compactPolicyFx) rowFacts(run uuid.UUID) string {
	return fx.query(`SELECT jsonb_build_object('status',status,'hold',hold_reason,'session',session_id,'preserve',policy_preserve_required,
		'resolved',policy_resolved_at IS NOT NULL,'required',policy_required_head,'verified',policy_recovery_verified_head,
		'outcome',policy_outcome_id,'gen',claim_generation)::text FROM runs WHERE id=$1`, run)
}

func (fx *compactPolicyFx) facts(run uuid.UUID) map[string]any {
	fx.t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(fx.rowFacts(run)), &got); err != nil {
		fx.t.Fatal(err)
	}
	return got
}

// The whole compact policy-hold lifecycle through the real owner and worker routes: a compatible-lane
// hold, a preserve Resume, an attempt-path re-hold, the stale and cross-mode refusals, the
// fresh_from_saved_work Resume with its saved-work verification, the unconstrained ordinary claim
// afterwards, and a new refusal that the old consent cannot resolve.
func TestPolicyHoldLifecycleLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := fx.seedRun()
	ctx := context.Background()

	sealed, err := fx.h.box.Seal([]byte("anthropic-3607-token-abcdef1234567890"))
	if err != nil {
		t.Fatal(err)
	}
	fx.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
		VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), fx.owner, "anthropic-"+uuid.NewString(), sealed)
	pat, err := fx.h.box.Seal([]byte("bot-pat-3607-abcdef1234567890"))
	if err != nil {
		t.Fatal(err)
	}
	fx.exec("UPDATE forge_connections SET token_ciphertext=$2 WHERE id=(SELECT connection_id FROM repos WHERE id=$1)", fx.repo, pat)

	refuse := func(what, mode, episode, guidance string, want int) {
		t.Helper()
		before := fx.state(run)
		got := fx.resumeAs("bearer", run, resumeBody(mode, episode, guidance))
		if got.Code != want {
			t.Fatalf("%s = %d, want %d: %s", what, got.Code, want, got.Body.String())
		}
		if fx.state(run) != before {
			t.Fatalf("%s mutated the run, session, barrier, inputs or queue", what)
		}
	}

	// 1. A compatible-lane refusal with a published head.
	head1 := strings.Repeat("a", 40)
	e1 := fx.hold(run, 1, "same_conversation", false, head1)
	if f := fx.facts(run); f["status"] != "paused" || f["hold"] != "codex_policy_hold" || f["preserve"] != true || f["resolved"] != false {
		t.Fatalf("after the hold: %s", fx.rowFacts(run))
	}
	dto := fx.ownerDTO(run)
	if dto.HoldCause == nil || *dto.HoldCause != "refused" || dto.OutcomeID != e1 || !slices.Equal(dto.ResumeModes, []string{"preserve", "fresh_one_claim"}) {
		t.Fatalf("episode 1 DTO = %+v", dto)
	}

	// 2. The owner preserves, with guidance.
	resp := fx.resumeAs("bearer", run, resumeBody("preserve", e1, "keep the scope narrow"))
	var resumed struct {
		Run struct {
			Status string `json:"status"`
		} `json:"run"`
	}
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &resumed) != nil || resumed.Run.Status != "queued" {
		t.Fatalf("preserve resume = %d %s", resp.Code, resp.Body.String())
	}
	if f := fx.facts(run); f["status"] != "queued" || f["preserve"] != true || f["resolved"] != true || f["required"] != head1 {
		t.Fatalf("after preserve: %s", fx.rowFacts(run))
	}
	if g := fx.query("SELECT string_agg(body,'|') FROM run_user_inputs WHERE run_id=$1 AND kind='follow_up'", run); g != "keep the scope narrow" {
		t.Fatalf("recorded guidance = %q", g)
	}

	// 3. The real claim is preserve-only and carries the expected head.
	claim := fx.realClaim(run)
	if claim["claim_generation"] != float64(2) {
		t.Fatalf("claim generation = %v", claim["claim_generation"])
	}
	if got, want := policyClaimKeys(claim), readClaimFixture(t, "policy-preserve-claim.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("preserve claim policy keys %v, fixture %v", got, want)
	}

	// 4. The worker cannot continue the conversation: an attempt-path re-hold, episode E2.
	head2 := strings.Repeat("b", 40)
	e2 := fx.rehold(run, 2, "attempt_path", "fresh_from_saved_work", head2)
	if e2 == e1 {
		t.Fatal("re-hold reused the episode")
	}
	if f := fx.facts(run); f["status"] != "paused" || f["hold"] != "codex_policy_hold" || f["resolved"] != false {
		t.Fatalf("after the re-hold: %s", fx.rowFacts(run))
	}
	dto = fx.ownerDTO(run)
	if dto.HoldCause == nil || *dto.HoldCause != "attempt_path" || dto.OutcomeID != e2 || !slices.Equal(dto.ResumeModes, []string{"fresh_from_saved_work"}) {
		t.Fatalf("episode 2 DTO = %+v", dto)
	}
	savedWork, _ := fx.facts(run)["required"].(string)
	if savedWork == "" {
		t.Fatalf("no recovery head after the re-hold: %s", fx.rowFacts(run))
	}

	// 5. The consent for the old episode resolves nothing, in any mode.
	for _, mode := range []string{"preserve", "fresh_from_saved_work", "fresh_one_claim"} {
		refuse("old episode "+mode, mode, e1, "old consent", http.StatusConflict)
	}

	// A request that never commits leaves the run paused: validation fails before any write.
	refuse("NUL guidance", "fresh_from_saved_work", e2, "a\x00b", http.StatusBadRequest)
	if f := fx.facts(run); f["status"] != "paused" || f["hold"] != "codex_policy_hold" || f["resolved"] != false {
		t.Fatalf("after the uncommitted request: %s", fx.rowFacts(run))
	}

	// 6. The attempt-path Resume.
	resp = fx.resumeAs("bearer", run, resumeBody("fresh_from_saved_work", e2, "start from the saved work"))
	if resp.Code != http.StatusOK {
		t.Fatalf("fresh_from_saved_work resume = %d %s", resp.Code, resp.Body.String())
	}
	f := fx.facts(run)
	if f["status"] != "queued" || f["hold"] != nil || f["preserve"] != false || f["session"] != nil || f["resolved"] != true || f["required"] != savedWork {
		t.Fatalf("after fresh_from_saved_work: %s", fx.rowFacts(run))
	}
	settled := fx.state(run)
	// Commit followed by a lost response: the exact replay acks and records nothing.
	var replayed struct {
		Replay bool `json:"replay"`
	}
	replay := fx.resumeAs("bearer", run, resumeBody("fresh_from_saved_work", e2, "start from the saved work"))
	if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &replayed) != nil || !replayed.Replay || fx.state(run) != settled {
		t.Fatalf("exact replay = %d %s", replay.Code, replay.Body.String())
	}
	// A different request for the resolved episode is a conflict that records nothing.
	refuse("different guidance", "fresh_from_saved_work", e2, "something else", http.StatusConflict)
	refuse("cross-mode fresh_one_claim", "fresh_one_claim", e2, "start from the saved work", http.StatusConflict)

	// Queued but unclaimed: the run stays resolved and nothing else is pending until a worker picks it up.
	if f := fx.facts(run); f["status"] != "queued" || f["resolved"] != true || f["hold"] != nil {
		t.Fatalf("queued, unclaimed: %s", fx.rowFacts(run))
	}

	// 7. The next pickup is ordinary except for the saved-work verification.
	claim = fx.realClaim(run)
	if claim["claim_generation"] != float64(3) {
		t.Fatalf("claim generation = %v", claim["claim_generation"])
	}
	if got := policyClaimKeys(claim); !reflect.DeepEqual(got, map[string]any{"policy_saved_work_head": savedWork}) {
		t.Fatalf("saved-work claim policy keys %v, want only policy_saved_work_head=%s", got, savedWork)
	}
	if _, present := claim["policy_preserve_only"]; present {
		t.Fatal("the saved-work claim is preserve-only")
	}
	verified := bearerReqBody(fx.router, http.MethodPost, "/api/worker/runs/"+run.String()+"/policy-recovery-verified", fx.workerToken,
		`{"claim_generation":3,"required_head":"`+savedWork+`"}`)
	var vreply workersvc.PolicyRecoveryVerifiedReply
	if verified.Code != http.StatusOK || json.Unmarshal(verified.Body.Bytes(), &vreply) != nil || vreply.Disposition != "recorded" {
		t.Fatalf("verification = %d %s", verified.Code, verified.Body.String())
	}
	if f := fx.facts(run); f["required"] != nil {
		t.Fatalf("verification left the required head: %s", fx.rowFacts(run))
	}

	// 8. Later ordinary claims are unconstrained. The fixture has one worker capability set, so a
	// worker without attempt paths is not distinguished here.
	n, err := fx.h.q.RequeueClaimAssemblyExact(ctx, store.RequeueClaimAssemblyExactParams{
		ID: run, WorkerID: pgconv.UUID(fx.workerID), ClaimGeneration: 3,
	})
	if err != nil || n != 1 {
		t.Fatalf("requeue after verification = %d %v", n, err)
	}
	claim = fx.realClaim(run)
	if claim["claim_generation"] != float64(4) {
		t.Fatalf("claim generation = %v", claim["claim_generation"])
	}
	if got := policyClaimKeys(claim); len(got) != 0 {
		t.Fatalf("ordinary claim carries policy constraints: %v", got)
	}

	// 9. A new refusal holds again, as a new unresolved episode.
	e3 := fx.hold(run, 4, "same_conversation", false, strings.Repeat("c", 40))
	if e3 == e1 || e3 == e2 {
		t.Fatal("new refusal reused an episode")
	}
	if f := fx.facts(run); f["status"] != "paused" || f["hold"] != "codex_policy_hold" || f["resolved"] != false || f["outcome"] != e3 {
		t.Fatalf("after the new refusal: %s", fx.rowFacts(run))
	}
	dto = fx.ownerDTO(run)
	if dto.OutcomeID != e3 || dto.ResolvedAt != nil || !slices.Equal(dto.ResumeModes, []string{"preserve", "fresh_one_claim"}) {
		t.Fatalf("episode 3 DTO = %+v", dto)
	}

	// 10. The old consent cannot resolve it.
	refuse("old episode against the new refusal", "fresh_from_saved_work", e2, "start from the saved work", http.StatusConflict)
	refuse("older episode against the new refusal", "preserve", e1, "keep the scope narrow", http.StatusConflict)
	if f := fx.facts(run); f["status"] != "paused" || f["resolved"] != false {
		t.Fatalf("old consent changed the run: %s", fx.rowFacts(run))
	}
}
