package workersvc

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB proof of the PRD #1247 M5 rework's CHAT EXEMPTION: a
// credential_switch_v1 capability worker's CHAT runs are NEVER fenced. Chat has no
// claim-generation contract (its batcher sends generation 0 and the run may omit it
// entirely), so the fail-closed generation fence — which 409s a capability worker's
// mutating report / message batch that OMITS claim_generation — must not apply to chat.
// Skipped unless UZI_TEST_DATABASE_URL is set (setupCodexLiveDB skips).

// seedChatRun inserts one chat-kind run owned by o's worker in the given status. Chat's
// runs_kind_shape requires repo_id/issue_iid/branch to be NULL, so those columns are left
// unset; claim_generation is seeded at 0 to mirror chat's "generation 0" (which the report
// under test omits regardless). The run is NOT interlocked (completion_contract_version stays
// NULL) so a `completed` report takes the direct SetRunCompleted path, matching real chat runs.
func seedChatRun(t *testing.T, env codexTestEnv, o reevalOwner, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness,
	             status, status_since, worker_id, started_at, claim_generation, budget_paused_seconds)
	          VALUES ($1, $2, 'chat', 'chat title', 'chat prompt', 'chat title', 'chat', 'claude',
	                  $3, now() - interval '5 minutes', $4, now() - interval '20 minutes', 0, 0)`,
		id, o.userID, status, o.workerID)
	return id
}

// TestSetStateChatExemptFromGenerationFenceLiveDB is the report half of the chat exemption: a
// capability worker's chat run `running`, `completed`, and `failed` reports are ACCEPTED with NO
// claim_generation — no ErrMissingClaimGeneration (the fail-closed guard is skipped for chat) and
// no stale rejection. Each is seeded fresh because the transitions change status.
//
// MUTATION CHECK: removing ONLY the chat guard at the top of stateUsesGenerationFence reddens every
// sub-test here — stateUsesGenerationFence then returns true for running/failed and (for a
// non-interlocked run) completed, so SetState's fail-closed check refuses the unstamped report with
// ErrMissingClaimGeneration.
func TestSetStateChatExemptFromGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := fenceSvc(env)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	capWkr := store.Worker{ID: o.workerID, UserID: o.userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}

	t.Run("running report accepted with no claim_generation", func(t *testing.T) {
		id := seedChatRun(t, env, o, "running")
		run, applied, err := svc.SetState(env.ctx, capWkr, id, StateRequest{State: "running", ClaimGeneration: nil})
		if err != nil {
			t.Fatalf("chat running report: err = %v, want nil (chat is fence-exempt)", err)
		}
		if !applied || run.Status != "running" {
			t.Fatalf("applied=%v status=%q, want applied running", applied, run.Status)
		}
	})

	t.Run("completed report accepted with no claim_generation", func(t *testing.T) {
		id := seedChatRun(t, env, o, "running")
		run, applied, err := svc.SetState(env.ctx, capWkr, id, StateRequest{State: "completed", ClaimGeneration: nil})
		if err != nil {
			t.Fatalf("chat completed report: err = %v, want nil (chat is fence-exempt)", err)
		}
		if !applied || run.Status != "completed" {
			t.Fatalf("applied=%v status=%q, want applied completed", applied, run.Status)
		}
	})

	t.Run("failed report accepted with no claim_generation", func(t *testing.T) {
		id := seedChatRun(t, env, o, "running")
		run, applied, err := svc.SetState(env.ctx, capWkr, id, StateRequest{State: "failed", ClaimGeneration: nil})
		if err != nil {
			t.Fatalf("chat failed report: err = %v, want nil (chat is fence-exempt)", err)
		}
		if !applied || run.Status != "failed" {
			t.Fatalf("applied=%v status=%q, want applied failed", applied, run.Status)
		}
	})
}

// TestAppendMessagesChatExemptFromGenerationFenceLiveDB is the message-batch half of the chat
// exemption: a capability worker's chat message batch with NO claim_generation is ACCEPTED and
// PERSISTED — the appendMessages fail-closed guard does not fire for chat.
//
// MUTATION CHECK: removing ONLY the `run.Kind != runkind.Chat` conjunct in appendMessages reddens
// this test — the batch would be refused with ErrMissingClaimGeneration and persist nothing.
func TestAppendMessagesChatExemptFromGenerationFenceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	capWkr := store.Worker{ID: o.workerID, UserID: o.userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	id := seedChatRun(t, env, o, "running")

	countMsgs := func() int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		return n
	}

	msg := IncomingMessage{Seq: 1, Kind: "text", Payload: []byte(`{"t":"x"}`)}
	if err := svc.AppendMessagesForClaim(env.ctx, capWkr, id, []IncomingMessage{msg}, nil); err != nil {
		if errors.Is(err, ErrMissingClaimGeneration) {
			t.Fatalf("chat batch without claim_generation: err = %v, want nil (chat is fence-exempt)", err)
		}
		t.Fatalf("chat batch append: %v", err)
	}
	if countMsgs() != 1 {
		t.Fatalf("message count = %d, want 1 (a capability worker's chat batch persists unfenced)", countMsgs())
	}
}

// TestAppendMessagesChatMismatchedGenerationTreatedAsLegacyLiveDB is the DISCRIMINATOR the
// omission-only test above cannot catch: a capability worker's chat message batch carrying a
// MISMATCHED NONZERO claim_generation (the run is at generation 0) is still treated as LEGACY —
// accepted, persisted, and last_seq advanced — because appendMessages normalizes effectiveClaimGen
// to nil for a chat run BEFORE the InsertRunMessage / generation_live / UpdateRunLastSeq fence. The
// server must never fence chat regardless of what generation a buggy or old worker supplies.
//
// MUTATION CHECK: reverting the effectiveClaimGen normalization (passing the raw claimGen to
// InsertRunMessage) reddens this test — the insert fences out against the run's generation 0
// (generation_live == false) and appendMessages returns ErrStaleClaim, persisting nothing. The
// omission-only chat test above stays GREEN on that partial code, which is exactly why this
// nonzero-mismatch case is required.
func TestAppendMessagesChatMismatchedGenerationTreatedAsLegacyLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	capWkr := store.Worker{ID: o.workerID, UserID: o.userID, ProtocolCapabilities: []string{capability.CredentialSwitchV1}}
	id := seedChatRun(t, env, o, "running") // run is at claim_generation 0

	countMsgs := func() int {
		var n int
		if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		return n
	}
	lastSeq := func() int64 {
		var n int64
		if err := env.pool.QueryRow(env.ctx, `SELECT last_seq FROM runs WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read last_seq: %v", err)
		}
		return n
	}

	mismatched := int64(99) // NOT the run's generation (0); a supplied value chat must ignore
	msg := IncomingMessage{Seq: 1, Kind: "text", Payload: []byte(`{"t":"x"}`)}
	if err := svc.AppendMessagesForClaim(env.ctx, capWkr, id, []IncomingMessage{msg}, &mismatched); err != nil {
		if errors.Is(err, ErrStaleClaim) {
			t.Fatalf("chat batch with mismatched generation: err = %v, want nil (chat is legacy regardless of the supplied generation)", err)
		}
		t.Fatalf("chat batch append: %v", err)
	}
	if countMsgs() != 1 {
		t.Fatalf("message count = %d, want 1 (a chat batch persists unfenced even with a mismatched generation)", countMsgs())
	}
	if lastSeq() != 1 {
		t.Fatalf("last_seq = %d, want 1 (a chat batch advances last_seq unfenced with a mismatched generation)", lastSeq())
	}

	// The state-report half is already covered by stateUsesGenerationFence's chat guard; assert it
	// here too so the whole chat property is pinned in one place: a chat state report carrying a
	// mismatched nonzero generation is still accepted.
	run, applied, err := svc.SetState(env.ctx, capWkr, id, StateRequest{State: "completed", ClaimGeneration: &mismatched})
	if err != nil {
		t.Fatalf("chat completed report with mismatched generation: err = %v, want nil", err)
	}
	if !applied || run.Status != "completed" {
		t.Fatalf("applied=%v status=%q, want applied completed", applied, run.Status)
	}
}
