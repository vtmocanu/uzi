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
	cases := []struct {
		name       string
		caps       []string
		applyNonce string
		harness    string
		want       bool
	}{
		{
			name:       "codex_only_matching_nonce_drains_codex",
			caps:       []string{capability.CodexCredentialSwitchV1, capability.InputReceiptsV1},
			applyNonce: "current",
			harness:    harnessCodex,
			want:       true,
		},
		{
			name:       "codex_only_matching_nonce_does_not_drain_claude",
			caps:       []string{capability.CodexCredentialSwitchV1, capability.InputReceiptsV1},
			applyNonce: "current",
			harness:    harnessClaude,
			want:       false,
		},
		{
			name:       "codex_only_stale_nonce_does_not_drain_codex",
			caps:       []string{capability.CodexCredentialSwitchV1, capability.InputReceiptsV1},
			applyNonce: "previous",
			harness:    harnessCodex,
			want:       false,
		},
		{
			// Current legacy behavior drains Claude even with an old apply nonce.
			name:       "claude_only_stale_nonce_still_drains_claude",
			caps:       []string{capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1},
			applyNonce: "previous",
			harness:    harnessClaude,
			want:       true,
		},
		{
			name:       "claude_only_does_not_drain_codex",
			caps:       []string{capability.CredentialSwitchInPlaceV1, capability.InputReceiptsV1},
			applyNonce: "previous",
			harness:    harnessCodex,
			want:       false,
		},
		{
			name:       "both_caps_stale_nonce_does_not_drain_codex",
			caps:       []string{capability.CredentialSwitchInPlaceV1, capability.CodexCredentialSwitchV1, capability.InputReceiptsV1},
			applyNonce: "previous",
			harness:    harnessCodex,
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := store.Worker{
				ProtocolCapabilities:       tc.caps,
				SnapshotRegisterNonce:      pgtype.Text{String: "current", Valid: true},
				CredentialSwitchApplyNonce: pgtype.Text{String: tc.applyNonce, Valid: true},
			}
			if got := inPlaceSwitchDrains(w, tc.harness); got != tc.want {
				t.Fatalf("inPlaceSwitchDrains = %t, want %t", got, tc.want)
			}
		})
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
