package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// workerClaim posts the real worker claim route and returns the decoded claim payload as a raw map.
func (fx *compactPolicyFx) workerClaim(run uuid.UUID) map[string]any {
	fx.t.Helper()
	rec := bearerReqBody(fx.router, http.MethodPost, "/api/worker/runs/claim", fx.workerToken, "{}")
	var payload map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload["run_id"] != run.String() {
		fx.t.Fatalf("worker claim = %d %s (run %s)", rec.Code, rec.Body.String(), fx.query("SELECT status||'/'||COALESCE(hold_reason,'-')||'/'||COALESCE(failure_reason,'-') FROM runs WHERE id=$1", run))
	}
	return payload
}

func policyClaimKeys(payload map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range payload {
		if strings.HasPrefix(k, "policy_") {
			out[k] = v
		}
	}
	return out
}

func readClaimFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/claims/" + name) //nolint:gosec // G304: test reads a fixed fixtures/claims file named by the test itself
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

// The owner's one-claim Fresh through the real owner and worker routes: the first claim carries the
// grant and no session; when its response is lost (no running report) and the claimed-never-started
// sweep returns the run, the next claim carries only the unconfirmed flag, exactly as the shared
// fixture the agent also reads.
func TestPolicyFreshOneClaimChainLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := fx.seedRun()
	head := strings.Repeat("a", 40)
	episode := fx.hold(run, 1, "same_conversation", false, head)
	// The claim path needs no Codex binding when the harness is claude: the policy facts are
	// harness-independent claim inputs (the same device as TestPolicyResumeClaimPayloadLiveDB).
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
	fx.exec("UPDATE runs SET harness='claude' WHERE id=$1", run)

	if resp := fx.resumeAs("bearer", run, resumeBody("fresh_one_claim", episode, "start over")); resp.Code != http.StatusOK {
		t.Fatalf("owner fresh_one_claim = %d %s", resp.Code, resp.Body.String())
	}
	// Competing owner mutations on the same episode are refused and change nothing.
	settled := fx.state(run)
	for _, mode := range []string{"preserve", "fresh_from_saved_work"} {
		if resp := fx.resumeAs("cookie", run, resumeBody(mode, episode, "other")); resp.Code != http.StatusConflict {
			t.Fatalf("%s after fresh_one_claim = %d %s", mode, resp.Code, resp.Body.String())
		}
	}
	if fx.state(run) != settled {
		t.Fatal("competing owner mutations changed the run")
	}

	first := fx.workerClaim(run)
	if first["claim_generation"] != float64(2) || first["session_id"] != nil {
		t.Fatalf("first claim: generation=%v session=%v", first["claim_generation"], first["session_id"])
	}
	wantFirst := readClaimFixture(t, "policy-fresh-session-claim.json")
	wantFirst["policy_resume_inputs_pending"] = true  // owner guidance has not been delivered
	wantFirst["policy_resume_context_pending"] = true // the fresh session has not been reported
	if got := policyClaimKeys(first); !reflect.DeepEqual(got, wantFirst) {
		t.Fatalf("first claim policy keys %v, fixture %v", got, wantFirst)
	}
	if got := fx.query("SELECT policy_pending_fresh::text||'/'||policy_fresh_consumed_generation::text FROM runs WHERE id=$1", run); got != "false/2" {
		t.Fatalf("consumption = %s, want false/2", got)
	}

	// The response is lost: no running report ever arrives. The sweep returns the claimed run.
	// The shared test database holds other runs claimed long ago, so age only this claim far past any
	// other row and sweep with a cutoff between them: the sweep is global and must touch only this run.
	fx.exec("UPDATE runs SET claimed_at = now() - interval '5000 days' WHERE id=$1", run)
	cutoff := pgconv.Time(time.Now().Add(-1000 * 24 * time.Hour))
	reset, err := fx.h.q.SweepClaimedNeverStarted(context.Background(), store.SweepClaimedNeverStartedParams{Cutoff: cutoff, WitnessCutoff: cutoff})
	if err != nil || len(reset) != 1 || reset[0].ID != run {
		t.Fatalf("sweep = %v %v", reset, err)
	}
	if got := fx.query("SELECT status||'/'||policy_pending_fresh::text||'/'||policy_fresh_consumed_generation::text FROM runs WHERE id=$1", run); got != "queued/false/2" {
		t.Fatalf("after the sweep = %s, want queued/false/2 (the sweep never restores the grant)", got)
	}

	second := fx.workerClaim(run)
	if second["claim_generation"] != float64(3) || second["session_id"] != nil {
		t.Fatalf("second claim: generation=%v session=%v", second["claim_generation"], second["session_id"])
	}
	wantSecond := readClaimFixture(t, "policy-fresh-unconfirmed-claim.json")
	wantSecond["policy_resume_inputs_pending"] = true // the lost claim did not consume its guidance
	wantSecond["policy_resume_context_pending"] = true
	if got := policyClaimKeys(second); !reflect.DeepEqual(got, wantSecond) {
		t.Fatalf("G+1 claim policy keys %v, fixture %v", got, wantSecond)
	}
	if n := fx.query("SELECT count(*)::text FROM run_user_inputs WHERE run_id=$1 AND kind='resume' AND body LIKE '{\"policy_fresh\":\"consumed\",%'", run); n != "1" {
		t.Fatalf("consumption audit rows = %s, want 1", n)
	}
}

// A one-claim Fresh restored by a non-delivered claim (here the empty-pool requeue) sits on an
// already-resolved episode. The owner's routes must not dead-end it: the empty-body resume-now
// promotes the pool_wait hold (the mode-bearing resume refuses a resolved episode), the restored
// grant survives for the next claim, and set-token's refusal carries no pointer to that refused
// mode-bearing resume.
func TestPolicyFreshRestoredOrdinaryResumeLiveDB(t *testing.T) {
	fx := newCompactPolicyFx(t)
	run := fx.seedRun()
	episode := fx.hold(run, 1, "same_conversation", false, strings.Repeat("a", 40))
	if resp := fx.resumeAs("bearer", run, resumeBody("fresh_one_claim", episode, "start over")); resp.Code != http.StatusOK {
		t.Fatalf("owner fresh_one_claim = %d %s", resp.Code, resp.Body.String())
	}
	claimed := fx.claim(run)
	n, err := fx.h.q.RequeueClaimAssemblyExact(context.Background(), store.RequeueClaimAssemblyExactParams{
		PoolWait: true, ID: run, WorkerID: pgconv.UUID(fx.workerID), ClaimGeneration: claimed.ClaimGeneration,
	})
	if err != nil || n != 1 {
		t.Fatalf("empty-pool requeue = %d %v", n, err)
	}
	restored := "SELECT status||'/'||policy_pending_fresh::text||'/'||COALESCE(policy_fresh_consumed_generation::text,'-')||'/'||(policy_resolved_at IS NOT NULL)::text FROM runs WHERE id=$1"
	if got := fx.query(restored, run); got != "pool_wait/true/-/true" {
		t.Fatalf("after the restore = %s, want pool_wait/true/-/true", got)
	}

	rec := bearerReqBody(fx.router, http.MethodPost, "/api/runs/"+run.String()+"/credential", fx.ownerUzc, `{"mode":"auto"}`)
	var msg struct {
		Error string `json:"error"`
	}
	if rec.Code != http.StatusUnprocessableEntity || json.Unmarshal(rec.Body.Bytes(), &msg) != nil ||
		msg.Error != "a credential override is not supported on the codex harness" {
		t.Fatalf("set-token on the restored run = %d %s", rec.Code, rec.Body.String())
	}

	if resp := fx.resumeAs("bearer", run, ""); resp.Code != http.StatusOK {
		t.Fatalf("empty-body resume-now on the restored run = %d %s", resp.Code, resp.Body.String())
	}
	if got := fx.query(restored, run); got != "queued/true/-/true" {
		t.Fatalf("after resume-now = %s, want queued/true/-/true (the restored grant is kept)", got)
	}
}
