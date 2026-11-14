package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexSwitchOwnerStateRefusals(t *testing.T) {
	var svc Service
	for _, status := range []string{"queued", "paused", "limit_wait", "pool_wait", "recovery_wait", "completed", "failed", "cancelled", "claimed"} {
		_, err := svc.requestCodexCredentialSwitch(context.Background(), store.Run{Status: status}, BindModePinned, nil)
		want := ErrCodexSwitchIneligible
		if status == "claimed" {
			want = ErrCredentialSwitchClaimAssembling
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", status, err, want)
		}
	}
}

func TestCodexSwitchNegotiation(t *testing.T) {
	w := store.Worker{
		SnapshotRegisterNonce: pgtype.Text{String: "registration", Valid: true},
		ProtocolCapabilities:  []string{capability.CredentialSwitchInPlaceV1},
	}
	if codexSwitchNegotiated(w) {
		t.Fatal("Claude negotiation authorized Codex")
	}
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.CodexCredentialSwitchV1)
	if !codexSwitchNegotiated(w) {
		t.Fatal("explicit Codex negotiation refused")
	}
	w.SnapshotRegisterNonce.Valid = false
	if codexSwitchNegotiated(w) {
		t.Fatal("missing registration accepted")
	}
}

func TestCodexSwitchHistoricalBaseline(t *testing.T) {
	id := uuid.New()
	r := store.Run{
		ClaimGeneration:       7,
		CodexSecretID:         pgtype.UUID{Bytes: id, Valid: true},
		CodexAuthMode:         pgtype.Text{String: codexAuthModeAPIKey, Valid: true},
		CodexMaterialRevision: pgtype.Int8{Int64: 3, Valid: true},
	}
	b := store.RunCredentialEpoch{ClaimGeneration: 7, SecretID: r.CodexSecretID}
	if !codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("nullable historical snapshots rejected")
	}
	b.CodexMaterialRevision = pgtype.Int8{Int64: 4, Valid: true}
	if codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("recorded revision mismatch accepted")
	}
	b.CodexMaterialRevision.Valid = false
	b.CodexAuthMode = pgtype.Text{String: codexAuthModeSubscription, Valid: true}
	if codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("recorded mode mismatch accepted")
	}
	if codexSwitchBaselineMatches(r, nil, nil) {
		t.Fatal("missing ordinal zero accepted")
	}
}

func TestCodexSwitchTargetShapes(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
		linked, want bool
	}{
		{store.KindOpenAIAPIKey, "static", false, true},
		{store.KindCodexAuth, "linked", true, true},
		{store.KindOpenAIAPIKey, "static", true, false},
		{store.KindCodexAuth, "linked", false, false},
		{store.KindAnthropicToken, "static", false, false},
	} {
		if got := codexSwitchTargetUsable(tc.kind, tc.status, tc.linked); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func TestCodexSwitchPolicyFence(t *testing.T) {
	for _, r := range []store.Run{
		{HoldReason: pgtype.Text{String: "codex_policy_hold", Valid: true}},
		{PolicyOutcomeID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		{PolicyPendingFresh: true},
	} {
		if !codexSwitchPolicyBlocked(r) {
			t.Fatal("policy fence accepted blocked episode")
		}
	}
	if codexSwitchPolicyBlocked(store.Run{PolicyPreserveRequired: true, PolicyRefusalCount: 1}) {
		t.Fatal("historical resolved policy constraint treated as an unresolved episode")
	}
	if codexSwitchPolicyBlocked(store.Run{}) {
		t.Fatal("empty episode blocked")
	}
}

func TestCodexSwitchOwnerLaneRefusals(t *testing.T) {
	var svc Service
	for _, kind := range []string{"chat", "judge", "self_improve", "job", "cross_check", "task"} {
		run := store.Run{Status: "running", Kind: kind}
		if kind == "task" {
			run.ReviewTargetRunID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		}
		_, err := svc.requestCodexCredentialSwitch(context.Background(), run, BindModePinned, nil)
		if !errors.Is(err, ErrCredentialOverrideLaneNotSwitchable) {
			t.Fatalf("%s: got %v, want lane refusal", kind, err)
		}
	}
}
