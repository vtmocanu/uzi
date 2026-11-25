package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
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

// Job and cross-check claims (and an isolated profile-bound one) refuse a provider-bound row with
// provider_lane_unsupported before anything is opened: the store is a nil-embedding fake, so any
// credential read would panic. The judge row is refused here too, but only because the zero Worker
// lacks claude_provider_v1 / claude_provider_judge_v1 (a Claude judge is provider-bound since PRD
// #4322 M1a); a Codex-bound judge is the one refused as an unsupported lane.
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
	_, err := svc.assembleProviderBinding(context.Background(), store.Worker{ProtocolCapabilities: []string{capability.ClaudeProviderV1}}, providerBoundRow(runkind.Issue))
	if !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("err = %v, want errCredentialUnavailable", err)
	}
}

// bindingReadSpy fails the test on any provider binding read; every other store method is the
// nil-embedding fake's, which panics, so a vault open would fail loudly too.
type bindingReadSpy struct {
	*fakeStore
	t *testing.T
}

func (s bindingReadSpy) GetModelProviderRevisionForOwner(context.Context, store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error) {
	s.t.Error("the provider revision was read for a worker without claude_provider_v1")
	return store.GetModelProviderRevisionForOwnerRow{}, errors.New("unexpected read")
}

func (s bindingReadSpy) GetProviderBindingKeyByID(context.Context, store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error) {
	s.t.Error("the provider key row was read for a worker without claude_provider_v1")
	return store.GetProviderBindingKeyByIDRow{}, errors.New("unexpected read")
}

// A provider-bound run assembled for a worker that did not report claude_provider_v1 is refused
// terminally before the binding is read or the key is opened, on the run lane (issue) and the chat
// lane: a Go-side backstop to the SQL claim clauses.
func TestProviderBoundAssemblyRefusesAWorkerWithoutTheCapability(t *testing.T) {
	svc := New(bindingReadSpy{fakeStore: &fakeStore{}, t: t}, newBox(t), testParams())
	for _, caps := range [][]string{nil, {}, {capability.CrossCheckGatedV1, capability.RecoveryArchiveV1}} {
		wkr := store.Worker{ID: uuid.New(), ProtocolCapabilities: caps}
		run := providerBoundRow(runkind.Issue)
		wkr.UserID = run.UserID
		if _, err := svc.assembleProviderBinding(context.Background(), wkr, run); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
			t.Errorf("caps %v: assembleProviderBinding err = %v, want terminal provider_lane_unsupported", caps, err)
		}
		chat := providerBoundRow(runkind.Chat)
		if _, err := svc.assembleChatClaim(context.Background(), wkr, chat); !errors.Is(err, ErrProviderLaneUnsupported) {
			t.Errorf("caps %v: assembleChatClaim err = %v", caps, err)
		}
	}
}
