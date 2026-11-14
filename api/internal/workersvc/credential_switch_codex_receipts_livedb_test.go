package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

// Exercise the actual receipt query and public service, not just the capability
// predicate. A default/incorrect harness projection must fail this regression.
func TestCodexPendingSwitchReceiptDrainLiveDB(t *testing.T) {
	f := newApplyFx(t)
	nonce := f.register(t, []string{capability.CodexCredentialSwitchV1, capability.InputReceiptsV1})
	run, _ := f.run(t, "running")
	old := f.env.seedStaticAPIKey(t, f.o.userID, "receipt-old", "receipt-old-key")
	target := f.env.seedStaticAPIKey(t, f.o.userID, "receipt-target", "receipt-target-key")
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, codexAuthModeAPIKey); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.recordCodexEpoch(f.env.ctx, mustRun(t, f.env, run)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	f.env.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'follow_up','keep this control')", run)
	drained, err := f.svc.ConsumeInputs(f.env.ctx, f.worker(t), run)
	if err != nil || !drained.Receipts || len(drained.Inputs) != 1 || drained.CredentialSwitch == nil {
		t.Fatal("Codex pending receipt drain failed", err)
	}
	id := drained.Inputs[0].ID
	ack, err := f.svc.AckInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id})
	if err != nil || !ack.Active || ack.Reason != "" {
		t.Fatal("Codex pending ACK failed", err)
	}
	replayed := func() bool {
		t.Helper()
		rows, err := f.svc.q.ListReplayRunInputs(f.env.ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.ID == id {
				return true
			}
		}
		return false
	}
	var conflict *InputReceiptConflictError
	if _, err := f.svc.ApplyInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id}); !errors.As(err, &conflict) || conflict.Reason != ReceiptSwitchPending {
		t.Fatal("pending follow-up APPLIED was not fenced", err)
	}
	// Existing verdict-specific DISCARD exceptions do not apply to follow_up.
	if _, err := f.svc.DiscardInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id}); !errors.As(err, &conflict) || conflict.Reason != ReceiptSwitchPending {
		t.Fatal("pending follow-up DISCARD was not fenced", err)
	}
	if !replayed() {
		t.Fatal("ACK removed a pending control from replay")
	}

	// Only a committed switch permits the ordinary follow-up to become APPLIED.
	req := f.req(f.opOf(t, run))
	req.RegisterNonce = nonce
	applied, err := f.svc.ApplyCredentialSwitch(f.env.ctx, f.worker(t), run, req)
	if err != nil || applied.Outcome != ApplyOutcomeApplied {
		t.Fatal("switch did not settle", err)
	}
	result, err := f.svc.ApplyInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id})
	if err != nil || !result.Active || replayed() {
		t.Fatal("settled switch did not release receipt application", err)
	}

	// A later pending request retains its controls across a stale registration.
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &old); err != nil {
		t.Fatal(err)
	}
	f.env.exec("INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,'follow_up','survive registration skew')", run)
	drained, err = f.svc.ConsumeInputs(f.env.ctx, f.worker(t), run)
	if err != nil || len(drained.Inputs) != 1 {
		t.Fatal("second pending drain failed", err)
	}
	id = drained.Inputs[0].ID
	if _, err := f.svc.AckInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id}); err != nil {
		t.Fatal(err)
	}
	f.oldReplicaRegister(t, "receipt-older-replica")
	stale, err := f.svc.ConsumeInputs(f.env.ctx, f.worker(t), run)
	if err != nil || stale.CredentialSwitch == nil || len(stale.Inputs) != 0 {
		t.Fatal("stale registration drained pending controls", err)
	}
	if _, err := f.svc.AckInputs(f.env.ctx, f.worker(t), run, f.gen, []int64{id}); !errors.As(err, &conflict) || conflict.Reason != ReceiptSwitchPending {
		t.Fatal("stale Codex ACK accepted", err)
	}
	if !replayed() {
		t.Fatal("stale registration lost buffered control")
	}
}
