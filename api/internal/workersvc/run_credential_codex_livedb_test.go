package workersvc

import (
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestOwnerCodexPendingTargetLiveDB(t *testing.T) {
	f := newApplyFx(t)
	run, _ := f.run(t, "running")
	old := f.env.seedStaticAPIKey(t, f.o.userID, "old-api", "old-key")
	target := f.env.seedStaticAPIKey(t, f.o.userID, "target-api", "target-key")
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, codexAuthModeAPIKey); err != nil {
		t.Fatal(err)
	}
	before := mustRun(t, f.env, run)
	// Claude negotiation alone must refuse without changing any run state.
	snap := f.snap(t, run)
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target); err == nil {
		t.Fatal("Claude negotiation accepted Codex request")
	}
	if f.snap(t, run) != snap {
		t.Fatal("refused owner request changed state")
	}
	f.env.exec("UPDATE workers SET credential_switch_apply_nonce=snapshot_register_nonce, protocol_capabilities = array_append(array_append(protocol_capabilities, 'codex_switch_stopped_v1'), $2) WHERE id = $1", f.o.workerID, capability.CodexCredentialSwitchV1)
	got, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Run
	if !r.CodexSwitchTargetSecretID.Valid || uuid.UUID(r.CodexSwitchTargetSecretID.Bytes) != target ||
		r.CodexSwitchTargetOpID != r.CredentialSwitchOpID ||
		!r.CodexSwitchTargetGeneration.Valid || r.CodexSwitchTargetGeneration.Int64 != f.gen {
		t.Fatalf("pending target not bound to request: %+v", r.ID)
	}
	if r.CredentialOverrideMode != before.CredentialOverrideMode || r.CredentialOverrideSecretID != before.CredentialOverrideSecretID ||
		r.CodexSecretID != before.CodexSecretID || r.CodexMaterialRevision != before.CodexMaterialRevision {
		t.Fatal("owner request changed override or active binding")
	}
	for _, mode := range []string{"auto", "default", "inherit"} {
		snap := f.snap(t, run)
		if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, mode, nil); err == nil {
			t.Fatalf("%s accepted", mode)
		}
		if f.snap(t, run) != snap {
			t.Fatalf("%s refusal wrote state", mode)
		}
	}
	// All clear writers share the trigger, including old binaries.
	f.env.exec("UPDATE runs SET credential_switch_requested_at = NULL, credential_switch_generation = NULL WHERE id = $1", run)
	r = mustRun(t, f.env, run)
	if r.CodexSwitchTargetSecretID.Valid || r.CodexSwitchTargetGeneration.Valid || r.CodexSwitchTargetOpID.Valid {
		t.Fatal("stamp clear retained pending target")
	}
}
