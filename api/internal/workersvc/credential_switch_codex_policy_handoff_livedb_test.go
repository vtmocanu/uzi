package workersvc

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

// The worker must hand a confirmed preserving resume rejection directly to policy
// retention. An ordinary uncertain pause first makes that endpoint ineligible.
func TestCodexCommittedPreserveRefusalPolicyHoldLiveDB(t *testing.T) {
	for _, pausedFirst := range []bool{false, true} {
		name := "running handoff"
		if pausedFirst {
			name = "ordinary pause refuses policy handoff"
		}
		t.Run(name, func(t *testing.T) {
			f, run, target := codexApplyFixture(t, true)
			savedSessionID := "12345678-1234-1234-1234-123456789abc"
			f.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,$2) WHERE id=$1",
				f.o.workerID, capability.ProviderPolicyHoldV1)
			// A resolved preserving episode keeps its conversation constraint while
			// permitting ordinary writers; an unresolved episode would refuse apply.
			f.env.exec(`UPDATE runs SET policy_outcome_id=$2, policy_preserve_required=true,
policy_continuation_mode='same_conversation', policy_applying_worker_id=worker_id,
policy_applying_generation=claim_generation-1, policy_source_generation=claim_generation-1,
policy_pending_fresh=false, policy_resolved_at=now(), session_id=$3 WHERE id=$1`,
				run, uuid.New(), savedSessionID)
			if UnresolvedPolicyEpisode(mustRun(t, f.env, run)) {
				t.Fatal("fixture did not resolve the previous preserving episode")
			}
			op := f.opOf(t, run)
			applied := f.apply(t, run, f.req(op))
			if applied.Outcome != ApplyOutcomeApplied || applied.Codex == nil {
				t.Fatal("preserving fixture did not commit B")
			}
			committed := mustRun(t, f.env, run)
			var committedJournal string
			if err := f.env.pool.QueryRow(f.env.ctx,
				"SELECT to_jsonb(e)::text FROM run_credential_switch_epochs e WHERE run_id=$1 AND op_id=$2",
				run, op).Scan(&committedJournal); err != nil {
				t.Fatal(err)
			}
			if !committed.CodexSecretID.Valid || uuid.UUID(committed.CodexSecretID.Bytes) != target {
				t.Fatal("fixture did not bind B")
			}
			if pausedFirst {
				// Model the state produced by credential_switch_uncertain, without
				// pretending its explicit acknowledgment can authorize policy POST.
				f.env.exec("UPDATE runs SET status='paused', hold_reason=NULL WHERE id=$1", run)
			}
			req := PolicyHoldRequest{
				OutcomeID: uuid.NewString(), SourceGeneration: f.gen, ClaimGeneration: f.gen,
				Detail: "resume_rejected", Cause: "provider_resume_rejected",
				ContinuationMode: "same_conversation", SessionID: &savedSessionID,
			}
			report := func() PolicyHoldReply {
				t.Helper()
				reply, err := f.svc.ReportPolicyHold(f.env.ctx, f.worker(t), run, req)
				if err != nil {
					t.Fatal(err)
				}
				return reply
			}
			if pausedFirst {
				f.noWrite(t, run, func() {
					reply := report()
					if reply.Disposition != "pending" || reply.Reason != "ineligible_status" {
						t.Fatal("ordinary pause unexpectedly admitted the policy transition", reply.Disposition, reply.Reason)
					}
				})
			} else {
				reply := report()
				if reply.Disposition != "applied" || reply.OutcomeID != req.OutcomeID ||
					reply.ObservedGeneration != f.gen || reply.ObservedStatus != "paused" || reply.HoldReason != "codex_policy_hold" {
					t.Fatal("running committed B did not enter policy retention", reply.Disposition, reply.Reason)
				}
				after := mustRun(t, f.env, run)
				if !UnresolvedPolicyEpisode(after) || !after.PolicyOutcomeID.Valid ||
					uuid.UUID(after.PolicyOutcomeID.Bytes).String() != req.OutcomeID ||
					!after.SessionID.Valid || after.SessionID.String != savedSessionID {
					t.Fatal("policy retention did not preserve the saved conversation candidate")
				}
			}
			after := mustRun(t, f.env, run)
			if after.ClaimGeneration != committed.ClaimGeneration || after.WorkerID != committed.WorkerID ||
				after.ClaimReleasedAt != committed.ClaimReleasedAt || after.CodexSecretID != committed.CodexSecretID ||
				after.CodexAuthMode != committed.CodexAuthMode || after.CodexMaterialRevision != committed.CodexMaterialRevision ||
				after.CodexClaimEpoch != committed.CodexClaimEpoch || !bytes.Equal(after.CodexCapHash, committed.CodexCapHash) ||
				after.CredentialSwitchOpID != committed.CredentialSwitchOpID || after.CredentialSwitchRequestedAt.Valid ||
				after.CredentialSwitchGeneration.Valid || after.CodexSwitchTargetSecretID.Valid {
				t.Fatal("policy handoff changed committed B or released its claim")
			}
			var children int
			if err := f.env.pool.QueryRow(f.env.ctx,
				"SELECT count(*) FROM run_credential_switch_epochs WHERE run_id=$1", run).Scan(&children); err != nil {
				t.Fatal(err)
			}
			if children != 1 {
				t.Fatal("policy handoff changed switch journal cardinality")
			}
			var afterJournal string
			if err := f.env.pool.QueryRow(f.env.ctx,
				"SELECT to_jsonb(e)::text FROM run_credential_switch_epochs e WHERE run_id=$1 AND op_id=$2",
				run, op).Scan(&afterJournal); err != nil {
				t.Fatal(err)
			}
			if afterJournal != committedJournal {
				t.Fatal("policy handoff changed the committed switch journal row")
			}
		})
	}
}
