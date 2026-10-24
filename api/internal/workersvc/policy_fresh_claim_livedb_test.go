package workersvc

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The owner grants exactly G2 a fresh conversation. A crash before or after
// persisting its replacement session must not turn that grant into authority for G3.
func TestPolicyFreshClaimCrashReclaimLiveDB(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprintf("persisted=%t", persisted), func(t *testing.T) {
			fx := newCodexClaimFix(t, setupCodexLiveDB(t), true)
			env := fx.env
			if fx.holdA == uuid.Nil {
				t.Fatal("missing G1 original-worker custody")
			}
			fx.svc.p.PolicyHoldNegotiated = true
			fx.svc.p.PolicyHoldSnapshotEnabled = true
			fx.svc.p.TerminalPendingLease = time.Hour

			// Persist admission prerequisites: Claim re-reads the worker row, and
			// owner resume checks the stored fleet's policy/harness capabilities.
			worker := wkrRow(t, env, fx.workerA)
			worker.ProtocolCapabilities = []string{
				capability.CodexHarnessV1, capability.CodexRuntimeV2,
				capability.CodexCustomModelV1, capability.RecoveryArchiveV1,
				capability.CredentialSwitchV1, capability.ProviderPolicyHoldV1,
			}
			env.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1",
				worker.ID, worker.ProtocolCapabilities)
			worker = wkrRow(t, env, worker.ID)
			// Restore the helper's requeued G1 to its live reporting state.
			env.exec("UPDATE runs SET status='running',session_id='refused-policy-session' WHERE id=$1", fx.runID)
			held, disposition, err := fx.svc.ReportPolicyHold(env.ctx, worker, fx.runID, policyRequest())
			if err != nil || disposition != PolicyHoldApplied || held.Status != "paused" || held.PolicyRefusalCount != 1 {
				t.Fatalf("G1 hold: status=%s count=%d disposition=%s err=%v",
					held.Status, held.PolicyRefusalCount, disposition, err)
			}
			if !held.SessionID.Valid || held.SessionID.String != "refused-policy-session" {
				t.Fatal("hold lost the refused session before the owner choice")
			}
			resumed, err := fx.svc.ResumePolicyHold(env.ctx, fx.userID, fx.runID,
				PolicyHoldActionToken(held), PolicyResumeRequest{Mode: "fresh"})
			if err != nil {
				t.Fatalf("owner fresh resume: %v", err)
			}

			assertStored := func(stage, status string, generation int64, session string) store.Run {
				t.Helper()
				run := mustRun(t, env, fx.runID)
				if run.Status != status || run.ClaimGeneration != generation || run.PolicyRefusalCount != 1 ||
					!run.PolicyFreshClaimGeneration.Valid || run.PolicyFreshClaimGeneration.Int64 != 2 {
					t.Fatalf("%s: status=%s generation=%d count=%d grant=%v", stage,
						run.Status, run.ClaimGeneration, run.PolicyRefusalCount, run.PolicyFreshClaimGeneration)
				}
				if run.SessionID.Valid != (session != "") || run.SessionID.String != session {
					t.Fatalf("%s: session=%v want=%q", stage, run.SessionID, session)
				}
				return run
			}
			if resumed.Status != "queued" || resumed.SessionID.Valid ||
				!resumed.PolicyFreshClaimGeneration.Valid || resumed.PolicyFreshClaimGeneration.Int64 != 2 {
				t.Fatal("fresh resume did not queue a cleared session with a G2 grant")
			}
			assertStored("owner resume", "queued", 1, "")

			assertClaim := func(generation int64, fresh bool, session string) {
				t.Helper()
				payload, err := fx.svc.Claim(env.ctx, worker, nil)
				if err != nil {
					t.Fatalf("G%d Claim: %v", generation, err)
				}
				if payload == nil {
					t.Fatalf("G%d Claim returned idle", generation)
				}
				if payload.RunID != fx.runID.String() || payload.ClaimGeneration != generation ||
					!payload.PolicyHoldResume || payload.PolicyHoldFreshSession != fresh {
					t.Fatalf("claim: run=%s generation=%d marker=%t fresh=%t; want G%d fresh=%t",
						payload.RunID, payload.ClaimGeneration, payload.PolicyHoldResume,
						payload.PolicyHoldFreshSession, generation, fresh)
				}
				if session == "" {
					if payload.SessionID != nil {
						t.Fatalf("G%d delivered unexpected session %q", generation, *payload.SessionID)
					}
				} else if payload.SessionID == nil || *payload.SessionID != session {
					t.Fatalf("G%d did not deliver the persisted replacement session", generation)
				}
				assertStored("claim", "claimed", generation, session)
			}
			assertClaim(2, true, "")
			g2 := int64(2)
			if _, applied, err := fx.svc.SetState(env.ctx, worker, fx.runID,
				StateRequest{State: "running", ClaimGeneration: &g2}); err != nil || !applied {
				t.Fatalf("G2 running: applied=%t err=%v", applied, err)
			}
			assertStored("G2 running", "running", 2, "")
			session := ""
			if persisted {
				session = "replacement-policy-session"
				if _, applied, err := fx.svc.SetState(env.ctx, worker, fx.runID,
					StateRequest{State: "running", ClaimGeneration: &g2, SessionID: &session}); err != nil || !applied {
					t.Fatalf("G2 session: applied=%t err=%v", applied, err)
				}
			}
			assertStored("before crash", "running", 2, session)
			// Model the crash requeue only; neither the grant nor generation is
			// consumed/reset by fixture SQL. Both subsequent claims use Service.Claim.
			env.exec("UPDATE runs SET status='queued',status_since=now() WHERE id=$1", fx.runID)
			assertStored("crash requeue", "queued", 2, session)
			assertClaim(3, false, session)

			before := mustRun(t, env, fx.runID)
			delayedSession := "delayed-G2-session"
			if _, applied, err := fx.svc.SetState(env.ctx, worker, fx.runID,
				StateRequest{State: "running", ClaimGeneration: &g2, SessionID: &delayedSession}); !errors.Is(err, ErrStaleClaim) || applied {
				t.Fatalf("delayed G2 session: applied=%t err=%v", applied, err)
			}
			after := assertStored("after stale G2 report", "claimed", 3, session)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("stale G2 session report mutated the G3 run")
			}
		})
	}
}
