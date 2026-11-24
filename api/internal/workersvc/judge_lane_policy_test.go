package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// judge_lane_policy_test.go: the judge's lane decision and its fake-store admission (PRD #4322
// M1a), without a database. The real-lock and atomicity proofs are the *LiveDB tests in
// judge_create_policy_livedb_test.go.

func uuidOf(b byte) pgtype.UUID {
	var id uuid.UUID
	id[0] = b
	return pgtype.UUID{Bytes: id, Valid: true}
}

func TestDecideJudgeLane(t *testing.T) {
	claudeSrc, codexSrc, secret := uuidOf(1), uuidOf(2), uuidOf(3)
	pol := func(mode string, pin, claude, codex pgtype.UUID) store.GetUserJudgeLanePolicyRow {
		return store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: mode, JudgeAnthropicSecretID: pin, ClaudeProviderID: claude, CodexProviderID: codex}
	}
	none := pgtype.UUID{}
	for _, tc := range []struct {
		name       string
		h          Harness
		p          store.GetUserJudgeLanePolicyRow
		wantVendor bool
		wantErr    error
	}{
		{"claude/no source", HarnessClaude, pol(BindModeDefault, none, none, none), true, nil},
		{"claude/source", HarnessClaude, pol(BindModeDefault, none, claudeSrc, none), false, nil},
		{"claude/auto mode with source", HarnessClaude, pol(BindModeAuto, none, claudeSrc, none), false, nil},
		{"claude/pinned with a credential beats the source", HarnessClaude, pol(BindModePinned, secret, claudeSrc, none), true, nil},
		{"claude/pinned with NULL secret is no pin", HarnessClaude, pol(BindModePinned, none, claudeSrc, none), false, nil},
		{"claude/non-pinned mode with a stale id is no pin", HarnessClaude, pol(BindModeDefault, secret, claudeSrc, none), false, nil},
		{"claude/codex source only", HarnessClaude, pol(BindModeDefault, none, none, codexSrc), true, nil},
		{"claude/both sources", HarnessClaude, pol(BindModeDefault, none, claudeSrc, codexSrc), false, nil},
		{"codex/no source", HarnessCodex, pol(BindModeDefault, none, none, none), true, nil},
		{"codex/claude source only is vendor", HarnessCodex, pol(BindModeDefault, none, claudeSrc, none), true, nil},
		{"codex/pinned claude judge policy is irrelevant", HarnessCodex, pol(BindModePinned, secret, claudeSrc, none), true, nil},
		{"codex/codex source refuses", HarnessCodex, pol(BindModeDefault, none, none, codexSrc), false, ErrProviderLaneUnsupported},
		{"codex/both sources refuses", HarnessCodex, pol(BindModeDefault, none, claudeSrc, codexSrc), false, ErrProviderLaneUnsupported},
		{"unknown harness", Harness("unknown"), pol(BindModeDefault, none, none, none), false, ErrProviderLaneUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideJudgeLane(tc.h, tc.p)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Vendor != tc.wantVendor || got.Policy != tc.p {
				t.Fatalf("decision = %+v, %v; want vendor=%t over the same policy", got, err, tc.wantVendor)
			}
		})
	}
}

// TestJudgePolicyDriftPrecedence pins the comparison order of the locked re-reads: the frozen
// provider against the decision's first, then the pin policy; equal reads pass.
func TestJudgePolicyDriftPrecedence(t *testing.T) {
	a, b, secret := uuidOf(1), uuidOf(2), uuidOf(3)
	base := store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: BindModeDefault, ClaudeProviderID: a}
	dec := judgeLaneDecision{Policy: base}
	withPin := base
	withPin.JudgeAnthropicBindMode, withPin.JudgeAnthropicSecretID = BindModePinned, secret
	otherMode := base
	otherMode.JudgeAnthropicBindMode = BindModeAuto
	otherSecret := base
	otherSecret.JudgeAnthropicSecretID = secret

	frozenA, frozenB := uuid.UUID(a.Bytes), uuid.UUID(b.Bytes)
	for _, tc := range []struct {
		name   string
		frozen uuid.UUID
		now    store.GetUserJudgeLanePolicyRow
		dec    judgeLaneDecision
		want   error
	}{
		{"unchanged", frozenA, base, dec, nil},
		{"source changed to another provider", frozenB, base, dec, ErrJudgePolicyChanged},
		{"pin mode changed", frozenA, otherMode, dec, ErrJudgePolicyChanged},
		{"pin secret changed", frozenA, otherSecret, dec, ErrJudgePolicyChanged},
		{"pinned", frozenA, withPin, dec, ErrJudgePolicyChanged},
		{"both changed", frozenB, withPin, dec, ErrJudgePolicyChanged},
		{"decision had no source", frozenA, base, judgeLaneDecision{Policy: store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: BindModeDefault}}, ErrJudgePolicyChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := judgePolicyDrift(tc.dec, tc.frozen, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("drift = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestJudgeSkipReasonMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		provider   string
		credential string
	}{
		{"source unusable typed", &ProviderSourceUnusableError{Reason: SourceUnusableRetired}, JudgeSkipProviderSourceUnusable, ""},
		{"source unusable wrapped", fmt.Errorf("x: %w", ErrProviderSourceUnusable), JudgeSkipProviderSourceUnusable, ""},
		{"model unmapped typed", &ProviderModelUnmappedError{Model: "opus", Source: "judge"}, JudgeSkipProviderModelUnmapped, ""},
		{"model unmapped sentinel", ErrProviderModelUnmapped, JudgeSkipProviderModelUnmapped, ""},
		{"lane unsupported", ErrProviderLaneUnsupported, JudgeSkipProviderLaneUnsupported, ""},
		{"policy changed records nothing", ErrJudgePolicyChanged, "", ""},
		{"no anthropic token", ErrNoAnthropicToken, "", JudgeSkipNoUsableCredential},
		{"credential disabled", ErrHarnessCredentialDisabled, "", JudgeSkipCredentialDisabled},
		{"no credential for harness", ErrNoCredentialForHarness, "", JudgeSkipNoUsableCredential},
		{"pool unavailable", ErrCodexPoolUnavailable, "", JudgeSkipCodexPoolUnavailable},
		{"unrelated", errors.New("boom"), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := judgeProviderSkipReasonFor(tc.err); got != tc.provider || ok != (tc.provider != "") {
				t.Fatalf("provider reason = %q,%t want %q", got, ok, tc.provider)
			}
			if got, ok := judgeSkipReasonFor(tc.err); got != tc.credential || ok != (tc.credential != "") {
				t.Fatalf("credential reason = %q,%t want %q", got, ok, tc.credential)
			}
		})
	}
}

// judgeLaneCfg is the scripted part of judgeLaneFake (a table row must not copy the embedded store).
type judgeLaneCfg struct {
	policy    store.GetUserJudgeLanePolicyRow
	policyErr error
	noToken   bool
	tokenErr  error
}

// judgeLaneFake is the in-memory store for the judge lane's fake admission: it counts the policy
// and default-token reads the decision makes.
type judgeLaneFake struct {
	fakeStore
	policy        store.GetUserJudgeLanePolicyRow
	policyErr     error
	noToken       bool
	tokenErr      error
	policyReads   int
	tokenReads    int
	policyOwner   uuid.UUID
	laneSourceHit int
}

func (f *judgeLaneFake) GetUserJudgeLanePolicy(_ context.Context, owner uuid.UUID) (store.GetUserJudgeLanePolicyRow, error) {
	f.policyReads++
	f.policyOwner = owner
	return f.policy, f.policyErr
}

func (f *judgeLaneFake) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	f.tokenReads++
	if f.tokenErr != nil {
		return store.GetUserSecretCiphertextRow{}, f.tokenErr
	}
	if f.noToken {
		return store.GetUserSecretCiphertextRow{}, pgx.ErrNoRows
	}
	return store.GetUserSecretCiphertextRow{Ciphertext: []byte{1}}, nil
}

// GetUserProviderLaneSources must never be read by a judge: the policy statement carries both
// sources, so a read here would be the old pre-transaction refusal coming back.
func (f *judgeLaneFake) GetUserProviderLaneSources(context.Context, uuid.UUID) (store.GetUserProviderLaneSourcesRow, error) {
	f.laneSourceHit++
	return store.GetUserProviderLaneSourcesRow{ClaudeProviderID: uuidOf(9), CodexProviderID: uuidOf(9)}, nil
}

func TestJudgeLaneFakeCreationAdmission(t *testing.T) {
	src, secret := uuidOf(1), uuidOf(3)
	boom := errors.New("read failed")
	for _, tc := range []struct {
		name     string
		explicit *Harness
		f        judgeLaneCfg
		want     error
		policy   int
		token    int
		inserts  int
	}{
		{name: "missing explicit", want: ErrProviderLaneUnsupported},
		{name: "unknown explicit", explicit: laneHarness("unknown"), want: ErrProviderLaneUnsupported},
		{name: "claude vendor", explicit: laneHarness(HarnessClaude), policy: 1, token: 1, inserts: 1},
		{name: "claude vendor without a token", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{noToken: true}, want: ErrNoAnthropicToken, policy: 1, token: 1},
		{name: "claude token read error", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{tokenErr: boom}, want: boom, policy: 1, token: 1},
		{name: "claude provider branch fails closed before any token read", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: src}}, want: errProviderCreateRequiresTx, policy: 1},
		{name: "claude pinned with a credential is vendor despite a source", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{policy: store.GetUserJudgeLanePolicyRow{JudgeAnthropicBindMode: BindModePinned, JudgeAnthropicSecretID: secret, ClaudeProviderID: src}}, policy: 1, token: 1, inserts: 1},
		{name: "claude with a codex source only is vendor", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{policy: store.GetUserJudgeLanePolicyRow{CodexProviderID: src}}, policy: 1, token: 1, inserts: 1},
		{name: "codex source refuses a codex judge", explicit: laneHarness(HarnessCodex), f: judgeLaneCfg{policy: store.GetUserJudgeLanePolicyRow{CodexProviderID: src}}, want: ErrProviderLaneUnsupported, policy: 1},
		{name: "claude source does not refuse a codex judge (fake credential refusal)", explicit: laneHarness(HarnessCodex), f: judgeLaneCfg{policy: store.GetUserJudgeLanePolicyRow{ClaudeProviderID: src}}, want: errNoCredentialForHarness, policy: 1},
		{name: "policy read error", explicit: laneHarness(HarnessClaude), f: judgeLaneCfg{policyErr: boom}, want: boom, policy: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &judgeLaneFake{policy: tc.f.policy, policyErr: tc.f.policyErr, noToken: tc.f.noToken, tokenErr: tc.f.tokenErr}
			owner := uuid.New()
			inserts := 0
			_, err := (&Service{q: f}).createRunResolved(context.Background(), owner, tc.explicit, func(Store, resolvedHarness) (store.Run, error) {
				inserts++
				return store.Run{}, nil
			}, harnessCreationOrigin{JudgeLane: true})
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if f.policyReads != tc.policy || f.tokenReads != tc.token || inserts != tc.inserts || f.laneSourceHit != 0 {
				t.Fatalf("policy reads=%d token reads=%d inserts=%d lane-source reads=%d, want %d/%d/%d/0",
					f.policyReads, f.tokenReads, inserts, f.laneSourceHit, tc.policy, tc.token, tc.inserts)
			}
			if f.policyReads > 0 && f.policyOwner != owner {
				t.Fatalf("policy read for %s, want %s", f.policyOwner, owner)
			}
		})
	}
}

// A guarded origin that is NOT the judge (jobs, checkers) keeps refusing any Claude source: the
// judge lane must not have relaxed RefuseProviderOwner. (Entry-point proofs for CreateJobRun and the
// plan and code checker creations are TestProviderOwnerGetsNoJobLiveDB,
// TestProviderCrossCheckFailsClosedToTheHumanGateLiveDB and TestProviderCheckerFrozenLeadBeforeVendorLiveDB.)
func TestRefuseProviderOwnerOriginStillRefusesAClaudeSource(t *testing.T) {
	f := &laneCreationStore{lanePresenceReader: lanePresenceReader{row: store.GetUserProviderLaneSourcesRow{ClaudeProviderID: uuidOf(1)}}}
	inserts := 0
	_, err := (&Service{q: f}).createRunResolved(context.Background(), uuid.New(), laneHarness(HarnessClaude), func(Store, resolvedHarness) (store.Run, error) {
		inserts++
		return store.Run{}, nil
	}, harnessCreationOrigin{RefuseProviderOwner: true})
	if !errors.Is(err, ErrProviderLaneUnsupported) || inserts != 0 {
		t.Fatalf("err = %v inserts = %d, want provider_lane_unsupported and no insert", err, inserts)
	}
}
