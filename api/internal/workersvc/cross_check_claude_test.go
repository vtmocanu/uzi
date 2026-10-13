package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestOppositeHarness(t *testing.T) {
	for _, tc := range []struct {
		lead string
		want Harness
		ok   bool
	}{
		{"claude", HarnessCodex, true},
		{"codex", HarnessClaude, true},
		{"", "", false},
		{"gemini", "", false},
		{"Claude", "", false},
	} {
		got, ok := oppositeHarness(tc.lead)
		if got != tc.want || ok != tc.ok {
			t.Errorf("oppositeHarness(%q) = (%q, %v), want (%q, %v)", tc.lead, got, ok, tc.want, tc.ok)
		}
		if isCrossCheckLeadHarness(tc.lead) != tc.ok {
			t.Errorf("isCrossCheckLeadHarness(%q) disagrees with oppositeHarness", tc.lead)
		}
	}
}

// Single-family custody: a child holds ONLY its own family's credential.
func TestSingleFamilyCheckerCustody(t *testing.T) {
	codex := &ClaimCodexSecrets{Capability: "cap"}
	for _, tc := range []struct {
		name, harness string
		anthropic     string
		codex         *ClaimCodexSecrets
		want          bool
	}{
		{"claude child with only the Anthropic token", "claude", "tok", nil, true},
		{"claude child leaking Codex credentials", "claude", "tok", codex, false},
		{"claude child with Codex credentials only", "claude", "", codex, false},
		{"claude child with no credential", "claude", "", nil, false},
		{"codex child with only Codex credentials", "codex", "", codex, true},
		{"codex child leaking the Anthropic token", "codex", "tok", codex, false},
		{"codex child with the Anthropic token only", "codex", "tok", nil, false},
		{"codex child with no credential", "codex", "", nil, false},
		{"unknown family", "gemini", "tok", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := &ClaimPayload{}
			payload.Secrets.AnthropicOAuthToken, payload.Secrets.Codex = tc.anthropic, tc.codex
			if got := singleFamilyCheckerCustody(tc.harness, payload); got != tc.want {
				t.Fatalf("singleFamilyCheckerCustody(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A Codex lead needs cross_check_codex_lead_v1 on its worker to take another round; a Claude
// lead's flow predates it and an unknown lead never qualifies.
func TestLeadCrossCheckCapable(t *testing.T) {
	with := []string{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1}
	without := []string{capability.CrossCheckV1, capability.CrossCheckRoundsV1}
	for _, tc := range []struct {
		lead string
		caps []string
		want bool
	}{
		{"codex", with, true}, {"codex", without, false}, {"codex", nil, false},
		{"claude", with, true}, {"claude", without, true}, {"claude", nil, true},
		{"gemini", with, false}, {"", with, false},
	} {
		if got := leadCrossCheckCapable(tc.lead, tc.caps); got != tc.want {
			t.Errorf("leadCrossCheckCapable(%q, %v) = %v, want %v", tc.lead, tc.caps, got, tc.want)
		}
	}
}

// A Claude checker child's credential failure becomes the bare checker-unavailable sentinel
// BEFORE the transient classification; nothing else changes.
func TestClaudeCheckerClaimErrorMapping(t *testing.T) {
	claudeChild := store.Run{Kind: "cross_check", Harness: "claude"}
	codexChild := store.Run{Kind: "cross_check", Harness: "codex"}
	issue := store.Run{Kind: "issue", Harness: "claude"}
	wrapped := func(err error) error { return fmt.Errorf("%w: detail the owner must not see", err) }
	for _, tc := range []struct {
		name   string
		run    store.Run
		err    error
		mapped bool
	}{
		{"unavailable", claudeChild, wrapped(errCredentialUnavailable), true},
		{"disabled", claudeChild, wrapped(errCredentialDisabled), true},
		{"empty auto pool", claudeChild, errAutoPoolEmpty, true},
		{"vault locked is transient", claudeChild, errVaultLocked, false},
		{"vault locked wrapping unavailable is still transient", claudeChild, fmt.Errorf("%w: %w", errCredentialUnavailable, errVaultLocked), false},
		{"pin capability gap is transient", claudeChild, errCrossCheckPinsCapabilityMissing, false},
		{"unrelated error", claudeChild, errors.New("boom"), false},
		{"no error", claudeChild, nil, false},
		{"codex child unavailable", codexChild, wrapped(errCredentialUnavailable), false},
		{"codex child disabled", codexChild, wrapped(errCredentialDisabled), false},
		{"codex child empty pool", codexChild, errAutoPoolEmpty, false},
		{"issue run unavailable", issue, wrapped(errCredentialUnavailable), false},
		{"issue run disabled", issue, wrapped(errCredentialDisabled), false},
		{"issue run empty pool", issue, errAutoPoolEmpty, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeCheckerClaimError(tc.run, tc.err)
			if tc.mapped {
				if got != errCheckerPinUnavailable {
					t.Fatalf("got %v, want the bare checker-unavailable sentinel", got)
				}
				if got.Error() != "plan cross-check: checker unavailable" {
					t.Fatalf("reason %q leaks detail", got.Error())
				}
				if claimAssemblyOrigin(got) != "guardrail_blocked" {
					t.Fatalf("origin = %q, want guardrail_blocked", claimAssemblyOrigin(got))
				}
				return
			}
			if got != tc.err {
				t.Fatalf("got %v, want the error unchanged (%v)", got, tc.err)
			}
		})
	}
}

// A queued Claude checker child waits for a worker advertising cross_check_codex_lead_v1; the
// reason is named only when that is the one capability the owner's fleet lacks.
func TestHealthClaudeCheckerChildCodexLeadCapability(t *testing.T) {
	base := []string{capability.CrossCheckV1}
	withLead := append(append([]string{}, base...), capability.CrossCheckCodexLeadV1)
	for _, tc := range []struct {
		name    string
		harness string
		workers []store.ListWorkersByUserRow
		want    string
	}{
		{"claude child, capable worker", "claude", []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: withLead}}, reasonWaitingWorker},
		{"claude child, only an older worker", "claude", []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: base}}, reasonNoCrossCheckCodexLeadCapableWorker},
		{"claude child, the only capable worker is offline", "claude", []store.ListWorkersByUserRow{
			{Status: "offline", ProtocolCapabilities: withLead}}, reasonNoCrossCheckCapableWorker},
		{"claude child, split fleet (older online, capable offline)", "claude", []store.ListWorkersByUserRow{
			{Status: "online", ProtocolCapabilities: base}, {Status: "offline", ProtocolCapabilities: withLead}}, reasonNoCrossCheckCodexLeadCapableWorker},
		{"claude child, no cross-check worker at all", "claude", []store.ListWorkersByUserRow{{Status: "online", ProtocolCapabilities: nil}}, reasonNoCrossCheckCapableWorker},
		{"codex child ignores the capability", "codex", []store.ListWorkersByUserRow{{Status: "online",
			ProtocolCapabilities: []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}}}, reasonWaitingWorker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1,
				crossCheckRun: store.Run{ID: r.ID, Kind: "cross_check", Harness: tc.harness}, crossCheckWorkers: tc.workers}
			svc := healthSvc(fs, defaultHealthSettings())
			svc.q = &roundsHealthStore{healthFakeStore: fs, round: 1}
			svc.detectRunHealth(context.Background(), t0)
			got := lastWrite(t, fs, r.ID)
			if got.Health != healthWaitingWorker || got.HealthReason.String != tc.want {
				t.Fatalf("health=%s reason=%q want=%q", got.Health, got.HealthReason.String, tc.want)
			}
		})
	}
}
