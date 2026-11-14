package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type codexApplyNonceRecorder struct {
	calls  int
	nonce  string
	worker uuid.UUID
}

func (q *codexApplyNonceRecorder) StampWorkerCredentialSwitchApplyNonce(_ context.Context, p store.StampWorkerCredentialSwitchApplyNonceParams) error {
	q.calls++
	q.nonce, q.worker = p.Nonce.String, p.ID
	return nil
}

func TestCodexSwitchDrainNegotiationKeepsHarnessesSeparate(t *testing.T) {
	w := store.Worker{
		ProtocolCapabilities:       []string{capability.CodexCredentialSwitchV1, capability.InputReceiptsV1},
		SnapshotRegisterNonce:      pgtype.Text{String: "current", Valid: true},
		CredentialSwitchApplyNonce: pgtype.Text{String: "current", Valid: true},
	}
	if !inPlaceSwitchDrains(w, harnessCodex) {
		t.Fatal("Codex receipt drain disabled")
	}
	if inPlaceSwitchDrains(w, harnessClaude) {
		t.Fatal("Codex capability changed legacy Claude draining")
	}
	w.CredentialSwitchApplyNonce.String = "previous"
	if inPlaceSwitchDrains(w, harnessCodex) {
		t.Fatal("stale registration drained pending Codex controls")
	}
	w.ProtocolCapabilities = []string{capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1}
	if !inPlaceSwitchDrains(w, harnessClaude) {
		t.Fatal("Claude receipt drain changed")
	}
	if inPlaceSwitchDrains(w, harnessCodex) {
		t.Fatal("Claude capability admitted Codex draining")
	}
}

func TestCodexOnlyRegistrationRetiresLegacySwitch(t *testing.T) {
	var svc Service
	q := &codexApplyNonceRecorder{}
	id := uuid.New()
	got, err := svc.stampApplyNonce(context.Background(), q, id, []string{capability.CodexCredentialSwitchV1}, "codex-registration")
	if err != nil || !got.Valid || got.String != "codex-registration" ||
		q.calls != 1 || q.nonce != got.String || q.worker != id {
		t.Fatalf("Codex-only negotiation did not stamp release retirement: calls=%d valid=%t error=%v", q.calls, got.Valid, err)
	}
	q = &codexApplyNonceRecorder{}
	got, err = svc.stampApplyNonce(context.Background(), q, id, []string{capability.CredentialSwitchV1}, "legacy-registration")
	if err != nil || got.Valid || q.calls != 0 {
		t.Fatal("legacy registration incorrectly retired")
	}
}
