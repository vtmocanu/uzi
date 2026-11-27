package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var codexProviderTrio = []string{capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}

// A Codex provider cross_check checker needs codex_provider_checker_v1 on top of the Codex provider
// trio (PRD #4574 M2); the judge term is unchanged and neither term leaks into the other kind.
func TestWorkerSupportsProviderCodexChecker(t *testing.T) {
	withChecker := append(append([]string{}, codexProviderTrio...), capability.CodexProviderCheckerV1)
	withJudge := append(append([]string{}, codexProviderTrio...), capability.CodexProviderJudgeV1)
	for _, tc := range []struct {
		name string
		caps []string
		kind string
		want bool
	}{
		{"trio without the checker capability, cross_check", codexProviderTrio, runkind.CrossCheck, false},
		{"trio with the checker capability, cross_check", withChecker, runkind.CrossCheck, true},
		{"checker capability alone, cross_check", []string{capability.CodexProviderCheckerV1}, runkind.CrossCheck, false},
		{"trio with only the judge capability, cross_check", withJudge, runkind.CrossCheck, false},
		{"trio without the checker capability, issue", codexProviderTrio, runkind.Issue, true},
		{"trio with only the checker capability, judge", withChecker, runkind.Judge, false},
		{"trio with only the judge capability, judge", withJudge, runkind.Judge, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workerSupportsProvider(store.Worker{ProtocolCapabilities: tc.caps}, harnessCodex, tc.kind); got != tc.want {
				t.Fatalf("workerSupportsProvider(codex, %s) = %t, want %t", tc.kind, got, tc.want)
			}
		})
	}
	// The Claude arm gains no checker lane.
	if !workerSupportsProvider(store.Worker{ProtocolCapabilities: []string{capability.ClaudeProviderV1}}, harnessClaude, runkind.CrossCheck) {
		t.Fatal("claude arm of workerSupportsProvider must not consult the Codex checker capability")
	}
}

// assembleClaim keeps refusing a provider-bound CLAUDE-harness cross_check even on a worker
// advertising every capability, while a Codex-harness one gets past the refusal term to the
// capability check: refused without codex_provider_checker_v1, and (with it) past every refusal and
// into the claim body, which a nil-embedding store cannot serve (it panics there).
func TestAssembleClaimProviderCrossCheckLane(t *testing.T) {
	allCaps := store.Worker{ProtocolCapabilities: append(append([]string{
		capability.ClaudeProviderV1, capability.ClaudeProviderJudgeV1, capability.ClaudeProviderJobV1,
	}, codexProviderTrio...), capability.CodexProviderJudgeV1, capability.CodexProviderCheckerV1)}

	claude := providerBoundRow(runkind.CrossCheck)
	if _, err := (&Service{}).assembleClaim(context.Background(), allCaps, claude); !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("claude-harness provider cross_check err = %v, want errProviderLaneClaimRefused", err)
	}

	codex := providerBoundRow(runkind.CrossCheck)
	codex.Harness = harnessCodex
	noChecker := store.Worker{ProtocolCapabilities: codexProviderTrio}
	if _, err := (&Service{}).assembleClaim(context.Background(), noChecker, codex); !errors.Is(err, errProviderLaneClaimRefused) {
		t.Fatalf("codex provider cross_check without the checker capability err = %v, want errProviderLaneClaimRefused", err)
	}

	var reached any
	func() {
		defer func() { reached = recover() }()
		_, _ = (&Service{}).assembleClaim(context.Background(), allCaps, codex)
	}()
	if reached == nil {
		t.Fatal("codex provider cross_check on a capable worker was refused before the claim body")
	}
}
