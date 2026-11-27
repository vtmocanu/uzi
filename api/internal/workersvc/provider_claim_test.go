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

// Cross-check claims (and an isolated profile-bound one) refuse a provider-bound row with
// provider_lane_unsupported before anything is opened: the store is a nil-embedding fake, so any
// credential read would panic. The judge and job rows are refused here too, but only because the zero
// Worker lacks claude_provider_v1 / claude_provider_judge_v1 / claude_provider_job_v1 (a Claude judge
// is provider-bound since PRD #4322 M1a, a Claude job since M1b); a Codex-bound judge is likewise
// refused only for a missing capability (PRD #4574 M1), while a Codex-bound job is the one refused
// as an unsupported lane (its worker below advertises every Claude capability).
func TestProviderBoundRowsRefuseUnsupportedLanes(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	for _, kind := range []string{runkind.Judge, runkind.Job, runkind.CrossCheck} {
		_, err := svc.assembleClaim(context.Background(), store.Worker{}, providerBoundRow(kind))
		if !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
			t.Errorf("%s: err = %v, want terminal provider_lane_unsupported", kind, err)
		}
	}
	// A Codex-bound job is refused as an unsupported lane whatever the worker advertises: the worker
	// below advertises every Claude and Codex capability, so the capability check cannot be what
	// refuses it (were the lane restriction removed, the claim would proceed to the nil store and panic).
	allCaps := store.Worker{ProtocolCapabilities: []string{
		capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1, capability.ClaudeProviderJobV1,
		capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3, capability.CodexProviderJudgeV1,
	}}
	codexJob := providerBoundRow(runkind.Job)
	codexJob.Harness = harnessCodex
	if _, err := svc.assembleClaim(context.Background(), allCaps, codexJob); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
		t.Errorf("codex-bound job: err = %v, want terminal provider_lane_unsupported", err)
	}
	// A Codex-bound judge is supported; this worker advertises no Codex capability, so that alone refuses it.
	allClaude := store.Worker{ProtocolCapabilities: []string{capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1, capability.ClaudeProviderJobV1}}
	codexJudge := providerBoundRow(runkind.Judge)
	codexJudge.Harness = harnessCodex
	if _, err := svc.assembleClaim(context.Background(), allClaude, codexJudge); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
		t.Errorf("codex-bound judge: err = %v, want terminal provider_lane_unsupported", err)
	}
	iso := providerBoundRow(runkind.Issue)
	iso.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if _, err := svc.assembleClaim(context.Background(), store.Worker{IsolatedLane: true}, iso); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("isolated: err = %v, want provider_lane_unsupported", err)
	}
	// A profile-bound provider JOB is refused by the isolated term even on a fully capable lane worker.
	isoJob := providerBoundRow(runkind.Job)
	isoJob.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	laneWorker := allClaude
	laneWorker.IsolatedLane = true
	if _, err := svc.assembleClaim(context.Background(), laneWorker, isoJob); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("isolated job: err = %v, want provider_lane_unsupported", err)
	}
	// The lane assemblers themselves refuse too (defence in depth for a direct call).
	if _, err := svc.assembleJudgeClaim(context.Background(), store.Worker{}, providerBoundRow(runkind.Judge)); !errors.Is(err, ErrProviderLaneUnsupported) {
		t.Errorf("assembleJudgeClaim: err = %v", err)
	}
	// The job assembler no longer refuses a provider-bound row outright: it claims against the frozen
	// binding, so a worker without claude_provider_v1 or claude_provider_job_v1 is what refuses, before
	// any binding read or vault open (the store would panic on one).
	jobSvc := New(&roleLaneStore{fakeStore: &fakeStore{}}, newBox(t), testParams())
	for _, caps := range [][]string{nil, {capability.ClaudeProviderV1}, {capability.ClaudeProviderJobV1}} {
		_, err := jobSvc.assembleJobClaim(context.Background(), store.Worker{ProtocolCapabilities: caps}, providerBoundRow(runkind.Job))
		if !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
			t.Errorf("assembleJobClaim caps %v: err = %v, want terminal provider_lane_unsupported", caps, err)
		}
	}
	if _, err := jobSvc.assembleJobClaim(context.Background(), allClaude, codexJob); !errors.Is(err, errCredentialUnavailable) {
		t.Errorf("assembleJobClaim codex: err = %v, want errCredentialUnavailable", err)
	}
	if singleFamilyCheckerCustody(string(HarnessClaude), &ClaimPayload{Secrets: ClaimSecrets{AnthropicOAuthToken: "t", Provider: &ClaimProviderBinding{}}}) {
		t.Error("a checker child holding a provider binding passed the custody rule")
	}
}

// A Codex provider judge needs codex_provider_judge_v1 on top of the Codex provider trio and
// codex_provider_v1 (PRD #4574 M1); an ordinary Codex provider run does not.
func TestWorkerSupportsProviderCodexJudge(t *testing.T) {
	base := []string{capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}
	withJudge := append(append([]string{}, base...), capability.CodexProviderJudgeV1)
	onlyJudge := []string{capability.CodexProviderJudgeV1}
	for _, tc := range []struct {
		name string
		caps []string
		kind string
		want bool
	}{
		{"full trio without the judge capability, judge", base, runkind.Judge, false},
		{"full trio without the judge capability, issue", base, runkind.Issue, true},
		{"full trio with the judge capability, judge", withJudge, runkind.Judge, true},
		{"full trio with the judge capability, issue", withJudge, runkind.Issue, true},
		{"judge capability alone, judge", onlyJudge, runkind.Judge, false},
		{"no capabilities, judge", nil, runkind.Judge, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerSupportsProvider(store.Worker{ProtocolCapabilities: tc.caps}, harnessCodex, tc.kind); got != tc.want {
				t.Fatalf("workerSupportsProvider(codex, %s) = %t, want %t", tc.kind, got, tc.want)
			}
		})
	}
}

// The Go backstop in assembleClaim: a provider-bound Codex judge reaches the capability check (no
// longer refused by kind) and a worker without codex_provider_judge_v1 is refused terminally
// before any credential read (the store is a nil-embedding fake, so a read would panic).
func TestProviderBoundCodexJudgeRefusedWithoutJudgeCapability(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	run := providerBoundRow(runkind.Judge)
	run.Harness = harnessCodex
	wkr := store.Worker{ProtocolCapabilities: []string{capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}}
	if _, err := svc.assembleClaim(context.Background(), wkr, run); !errors.Is(err, ErrProviderLaneUnsupported) || !errors.Is(err, errCredentialUnavailable) {
		t.Fatalf("err = %v, want terminal provider_lane_unsupported", err)
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
