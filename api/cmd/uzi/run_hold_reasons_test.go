package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// holdPresentationCase is one row of fixtures/hold-reasons/presentation.json, the table the web
// (NON_COMPLETION_HOLDS in web/src/lib/runBadge.ts) and this CLI (nonCompletionHolds) both pin.
type holdPresentationCase struct {
	Reason         string `json:"reason"`
	NonCompletion  bool   `json:"non_completion"`
	OwnerResumable bool   `json:"owner_resumable"`
	CLINotice      string `json:"cli_notice"`
	AnnouncesEntry bool   `json:"announces_entry"`
}

func TestHoldReasonPresentationFixture(t *testing.T) {
	// fixtures/ sits above the api module, so a fixture-only edit is invisible to the Go build cache.
	raw, err := os.ReadFile("../../../fixtures/hold-reasons/presentation.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var cases []holdPresentationCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	visited := 0
	seen := map[string]bool{}
	for _, c := range cases {
		visited++
		seen[c.Reason] = true
		if got := isNonCompletionHold(c.Reason); got != c.NonCompletion {
			t.Errorf("%s: isNonCompletionHold = %v, want %v", c.Reason, got, c.NonCompletion)
		}
		if got := isOwnerResumableHold(c.Reason); got != c.OwnerResumable {
			t.Errorf("%s: isOwnerResumableHold = %v, want %v", c.Reason, got, c.OwnerResumable)
		}
		if got := holdAnnouncesEntry(c.Reason); got != c.AnnouncesEntry {
			t.Errorf("%s: holdAnnouncesEntry = %v, want %v", c.Reason, got, c.AnnouncesEntry)
		}
		reason := c.Reason
		r := apitypes.RunDTO{ID: "run-1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
		if got := pausedHoldNotice(r); got != c.CLINotice {
			t.Errorf("%s: pausedHoldNotice = %q, want %q", c.Reason, got, c.CLINotice)
		}
	}
	if visited != len(cases) {
		t.Errorf("visited %d of %d fixture cases", visited, len(cases))
	}
	// Every hold the CLI treats as non-completion must be a fixture row, so the two sets cannot drift.
	for reason := range nonCompletionHolds {
		if !seen[reason] {
			t.Errorf("nonCompletionHolds has %q but the fixture does not", reason)
		}
	}
	// The completion, wall and weekly-reserve holds are not in the set.
	for _, reason := range []string{holdCompletionBlocked, holdBudgetExhausted, holdWeeklyReserve, "", "unknown_future_hold"} {
		if isNonCompletionHold(reason) || isOwnerResumableHold(reason) {
			t.Errorf("%q must not be a non-completion hold", reason)
		}
	}
}

func TestHoldRowRunOverrideUnavailable(t *testing.T) {
	reason := holdRunOverrideUnavailable
	r := apitypes.RunDTO{ID: "run-1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
	row := holdRow(r, statusEnteredAt(r))
	if len(row) != 2 || row[0] != "HOLD" {
		t.Fatalf("holdRow = %v", row)
	}
	if !strings.HasPrefix(row[1], "model choice unavailable · parked ") || !strings.HasSuffix(row[1], " · "+runOverrideAction) {
		t.Errorf("HOLD cell = %q, want the label first and the owner action last", row[1])
	}
}

// TestHoldEntryNoticesKeepPreExistingBehavior pins which holds announce themselves when a run
// enters them under `run wait` and `run logs --follow`: codex_policy_hold and
// repo_agent_source_unavailable never did, and only run_override_unavailable is new.
func TestHoldEntryNoticesKeepPreExistingBehavior(t *testing.T) {
	orig := logsPollInterval
	logsPollInterval = time.Millisecond
	t.Cleanup(func() { logsPollInterval = orig })
	for _, reason := range []string{
		holdCodexPolicy, holdRepoAgentSource, holdRunOverrideUnavailable, holdProviderUnavailable,
		holdIterationExhausted, holdBaseAttestation, holdCredentialsReset, holdCredentialSwitchUnsettled,
	} {
		for _, follow := range []bool{false, true} {
			name := reason + "/wait"
			if follow {
				name = reason + "/follow"
			}
			t.Run(name, func(t *testing.T) {
				held := heldRun("r1", reason, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
				notice := pausedHoldNotice(held)
				runs := []apitypes.RunDTO{runAt("running"), held, held, runAt("completed")}
				polls := 0
				srv := providerSequenceServer(t, runs, &polls)
				defer srv.Close()
				args := []string{"run", "wait", "r1", "--interval", "1ms", "--timeout", "2s", "--until", "completed"}
				if follow {
					args = []string{"run", "logs", "r1", "--follow"}
				}
				_, stderr, code := runCLI(t, httpEnv(srv), args...)
				if code != 0 {
					t.Fatalf("exit=%d: %s", code, stderr)
				}
				if got, want := strings.Contains(stderr, notice), holdAnnouncesEntry(reason); got != want {
					t.Fatalf("notice %q present=%v, want %v; stderr: %s", notice, got, want, stderr)
				}
			})
		}
	}
}
