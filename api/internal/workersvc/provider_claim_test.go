package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4320 module 5: the builder refuses a payload carrying a provider binding together with a
// vendor credential, and a vendor payload never emits a `provider` key.
func TestAssertClaimSecretsExclusive(t *testing.T) {
	binding := &ClaimProviderBinding{ProviderID: uuid.NewString(), Credential: "k"}
	for name, tc := range map[string]struct {
		s       ClaimSecrets
		wantErr bool
	}{
		"provider only":                {ClaimSecrets{Provider: binding}, false},
		"vendor only":                  {ClaimSecrets{AnthropicOAuthToken: "t"}, false},
		"provider and anthropic token": {ClaimSecrets{Provider: binding, AnthropicOAuthToken: "t"}, true},
		"provider and codex":           {ClaimSecrets{Provider: binding, Codex: &ClaimCodexSecrets{}}, true},
	} {
		if err := assertClaimSecretsExclusive(tc.s); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
	raw, err := json.Marshal(ClaimSecrets{ForgePAT: "p", AnthropicOAuthToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "provider") {
		t.Fatalf("a vendor claim's secrets carry a provider key: %s", raw)
	}
	chat, _ := json.Marshal(ChatClaimSecrets{AnthropicOAuthToken: "t"})
	if string(chat) != `{"anthropic_oauth_token":"t"}` {
		t.Fatalf("vendor chat secrets = %s", chat)
	}
	pchat, _ := json.Marshal(ChatClaimSecrets{Provider: binding})
	if !strings.HasPrefix(string(pchat), `{"anthropic_oauth_token":"","provider":{`) {
		t.Fatalf("provider chat secrets = %s", pchat)
	}
}

func providerBoundRow(kind string) store.Run {
	return store.Run{
		ID: uuid.New(), UserID: uuid.New(), Kind: kind, Harness: "claude", Status: "claimed",
		ModelProviderID:          pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ModelProviderRevision:    pgtype.Int4{Int32: 1, Valid: true},
		ModelProviderKeyMode:     pgtype.Text{String: "user_key", Valid: true},
		ModelProviderKeySecretID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ModelProviderKeyRev:      pgtype.Int8{Int64: 1, Valid: true},
	}
}

// Judge, job and cross-check claims (and an isolated profile-bound one) refuse a provider-bound row
// with provider_lane_unsupported before anything is opened: the store is a nil-embedding fake, so any
// credential read would panic.
func TestProviderBoundRowsRefuseUnsupportedLanes(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	for _, kind := range []string{runkind.Judge, runkind.Job, runkind.CrossCheck} {
		_, err := svc.assembleClaim(context.Background(), store.Worker{}, providerBoundRow(kind))
		if !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
			t.Errorf("%s: err = %v, want terminal provider_lane_unsupported", kind, err)
		}
	}
	iso := providerBoundRow(runkind.Issue)
	iso.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := svc.assembleClaim(context.Background(), store.Worker{IsolatedLane: true}, iso); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("isolated: err = %v, want provider_lane_unsupported", err)
	}
	// The lane assemblers themselves refuse too (defence in depth for a direct call).
	if _, err := svc.assembleJudgeClaim(context.Background(), store.Worker{}, providerBoundRow(runkind.Judge)); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("assembleJudgeClaim: err = %v", err)
	}
	if _, err := svc.assembleJobClaim(context.Background(), store.Worker{}, providerBoundRow(runkind.Job)); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("assembleJobClaim: err = %v", err)
	}
	if singleFamilyCheckerCustody(string(HarnessClaude), &ClaimPayload{Secrets: ClaimSecrets{AnthropicOAuthToken: "t", Provider: &ClaimProviderBinding{}}}) {
		t.Error("a checker child holding a provider binding passed the custody rule")
	}
}

// A store that cannot read provider bindings fails a provider-bound claim closed.
func TestProviderBoundClaimFailsClosedWithoutBindingReader(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	_, err := svc.assembleProviderBinding(context.Background(), providerBoundRow(runkind.Issue))
	if !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("err = %v, want errCredentialUnavailable", err)
	}
}
