package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
	if !strings.HasPrefix(row[1], "model choice unavailable · parked ") || !strings.HasSuffix(row[1], " · "+runOverrideAction(r)) {
		t.Errorf("HOLD cell = %q, want the label first and the owner action last", row[1])
	}
}

// A queued/claimed run after Resume keeps hold_reason until it starts, but it is no longer an owner hold.
func TestHoldRowRunOverrideUnavailableOnlyWhilePaused(t *testing.T) {
	reason := holdRunOverrideUnavailable
	for _, status := range []string{"queued", "claimed", "running"} {
		r := apitypes.RunDTO{ID: "run-1", Status: status, Kind: "issue", HoldReason: &reason}
		if row := holdRow(r, time.Now()); row != nil {
			t.Errorf("%s: holdRow = %v, want no HOLD row", status, row)
		}
	}
}

func TestHoldRowPhaseSwitchUnavailable(t *testing.T) {
	reason := holdPhaseSwitchUnavailable
	r := apitypes.RunDTO{ID: "run-1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
	row := holdRow(r, statusEnteredAt(r))
	if len(row) != 2 || row[0] != "HOLD" {
		t.Fatalf("holdRow = %v", row)
	}
	if !strings.HasPrefix(row[1], "planning switch unavailable · parked ") || !strings.HasSuffix(row[1], " · "+phaseSwitchAction(r)) {
		t.Errorf("HOLD cell = %q, want the label first and the owner action last", row[1])
	}
}

// A queued/claimed run after Resume keeps hold_reason until it starts, but it is no longer an owner hold.
func TestHoldRowPhaseSwitchUnavailableOnlyWhilePaused(t *testing.T) {
	reason := holdPhaseSwitchUnavailable
	for _, status := range []string{"queued", "claimed", "running"} {
		r := apitypes.RunDTO{ID: "run-1", Status: status, Kind: "issue", HoldReason: &reason}
		if row := holdRow(r, time.Now()); row != nil {
			t.Errorf("%s: holdRow = %v, want no HOLD row", status, row)
		}
	}
}

func TestRunOverrideNoticeCopy(t *testing.T) {
	reason := holdRunOverrideUnavailable
	r := apitypes.RunDTO{ID: "run-1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
	n := pausedHoldNotice(r)
	for _, part := range []string{"Resume retries only if the choice has become available", "Cancel and start a new run", "Resume: uzi run resume run-1; Cancel: uzi run cancel run-1"} {
		if !strings.Contains(n, part) {
			t.Errorf("notice %q lacks %q", n, part)
		}
	}
}

func TestHoldRowRunEnvHistoryUnavailable(t *testing.T) {
	reason := holdRunEnvHistoryUnavailable
	r := apitypes.RunDTO{ID: "run-1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
	row := holdRow(r, statusEnteredAt(r))
	if len(row) != 2 || row[0] != "HOLD" || !strings.HasPrefix(row[1], "variable history unavailable · parked ") ||
		!strings.HasSuffix(row[1], " · "+runEnvHistoryAction(r)) {
		t.Errorf("HOLD row = %v", row)
	}
	r.Status = "queued"
	if row := holdRow(r, time.Now()); row != nil {
		t.Errorf("a resumed run must not render the hold, got %v", row)
	}
}

func TestRenderRunDetailInjectedEnv(t *testing.T) {
	r := apitypes.RunDTO{ID: "run-1", Kind: "issue", Status: "completed", Health: "ok",
		InjectedEnv: []apitypes.InjectedEnvVarDTO{{Name: "API_URL", Scope: "repo"}, {Name: "TOKEN\x1b[31m", Scope: "user", Secret: true}}}
	out := renderDetail(t, r)
	if !strings.Contains(out, "INJECTED_ENV") || !strings.Contains(out, "API_URL (repo)") || !strings.Contains(out, "(user, secret)") {
		t.Errorf("INJECTED_ENV row missing or wrong:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a hostile variable name reached the terminal:\n%q", out)
	}
	none := renderDetail(t, apitypes.RunDTO{ID: "run-2", Kind: "issue", Status: "completed", Health: "ok"})
	if strings.Contains(none, "INJECTED_ENV") {
		t.Errorf("a run with no injected variables must render no row:\n%s", none)
	}
}

// The set drives the default `run wait` stop and the TUI owner band and count: membership is pinned.
func TestHoldSetStopAndBoardMembership(t *testing.T) {
	wantWait := map[string]bool{holdBaseAttestation: true, holdCredentialSwitchSettling: true, holdCredentialSwitchUnsettled: true, holdRunOverrideUnavailable: true,
		holdPhaseSwitchUnavailable: true, holdProviderBindingStale: true, holdRunEnvHistoryUnavailable: true}
	wantBoard := map[string]bool{holdCredentialDisabled: true, holdCredentialsReset: true, holdProviderUnavailable: true, holdIterationExhausted: true,
		holdCredentialSwitchSettling: true, holdCredentialSwitchUnsettled: true, holdRunOverrideUnavailable: true,
		holdPhaseSwitchUnavailable: true, holdProviderDestinationNotAllowed: true, holdProviderBindingStale: true, holdRunEnvHistoryUnavailable: true}
	for reason := range nonCompletionHolds {
		reason := reason
		r := apitypes.RunDTO{ID: "r1", Status: statusPaused, HoldReason: &reason}
		if got := stopsDefaultWait(r); got != wantWait[reason] {
			t.Errorf("%s: stopsDefaultWait = %v, want %v", reason, got, wantWait[reason])
		}
		if got := isBoardOwnerHold(r); got != wantBoard[reason] {
			t.Errorf("%s: isBoardOwnerHold = %v, want %v", reason, got, wantBoard[reason])
		}
		r.Status = "queued"
		if stopsDefaultWait(r) || isBoardOwnerHold(r) {
			t.Errorf("%s: a non-paused run must not match", reason)
		}
		// The board agrees with the helper for a paused run.
		item := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "r1", Status: statusPaused, HoldReason: &reason}}
		if wantBoard[reason] && runBandOf(item) != bandNeedsYou {
			t.Errorf("%s: runBandOf = %d, want needs-you", reason, runBandOf(item))
		}
		m := tuiModel{}
		m.board.runs = []apitypes.RunListItemDTO{item}
		if got := strings.Contains(m.boardSummary(), "⊘"); got != wantBoard[reason] {
			t.Errorf("%s: boardSummary %q owner-hold segment = %v, want %v", reason, m.boardSummary(), got, wantBoard[reason])
		}
	}
}

func TestRunWaitStopsOnRunOverrideHold(t *testing.T) {
	held := heldRun("r1", holdRunOverrideUnavailable, time.Now())
	fc := &uzicli.FakeClient{GetRunHook: scriptHook(okStep("running"), waitStep{run: held})}
	_, stderr, code := runCLI(t, fakeEnv(fc), "run", "wait", "r1", "--interval", "1ms", "--timeout", "2s")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
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

// PRD #4320 module 5: the two provider-binding holds read as their own tokens on the board and the
// detail (never the generic "paused"), and the stale hold's label names the binding, not only the key
// (a deleted provider or revision strands the run too).
func TestProviderBindingHoldTokensAndLabels(t *testing.T) {
	for _, tc := range []struct{ reason, word, label string }{
		{holdProviderDestinationNotAllowed, providerURLBlockedWord, "provider URL not allowed"},
		{holdProviderBindingStale, providerBindingStaleWord, "provider binding changed"},
	} {
		reason := tc.reason
		run := apitypes.RunDTO{ID: "r1", Status: statusPaused, Kind: "issue", HoldReason: &reason}
		item := apitypes.RunListItemDTO{RunDTO: run}
		if got := runStateWord(item); got != tc.word {
			t.Errorf("%s: runStateWord = %q, want %q", tc.reason, got, tc.word)
		}
		if tok := (palette{}).runStateToken(run, false); tok.word != tc.word || tok.glyph != credDisabledGlyph {
			t.Errorf("%s: runStateToken = %+v, want %s %s", tc.reason, tok, credDisabledGlyph, tc.word)
		}
		if got := holdReasonLabel(tc.reason); got != tc.label {
			t.Errorf("%s: holdReasonLabel = %q, want %q", tc.reason, got, tc.label)
		}
		run.Status = "queued"
		if tok := (palette{}).runStateToken(run, false); tok.word == tc.word {
			t.Errorf("%s: a non-paused run must not carry the hold token", tc.reason)
		}
	}
}
