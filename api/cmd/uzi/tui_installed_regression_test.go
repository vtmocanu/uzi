package main

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"io"
	"strings"
	"testing"
)

func TestM2RejectUnqualifiedUpgrade(t *testing.T) {
	calls := 0
	env := Env{Stdout: io.Discard, Stderr: io.Discard, Brew: func(bool, ...string) (string, error) { calls++; return "", nil }}
	if err := runPendingUpgrade(env, []string{"upgrade", "uzi-cli"}, "v0.85.0"); err == nil || calls != 0 {
		t.Fatalf("unqualified argv accepted: err=%v calls=%d", err, calls)
	}
}
func TestM2ZeroExitNoopNotSuccess(t *testing.T) {
	var out strings.Builder
	env := Env{Stdout: &out, Stderr: io.Discard, Brew: func(bool, ...string) (string, error) { return "", nil }, InstalledVersion: func(string) (string, error) { return "v0.84.0", nil }}
	err := runPendingUpgrade(env, []string{"upgrade", "vtmocanu/tap/uzi-cli"}, "v0.85.0")
	if err == nil || strings.Contains(out.String(), "Update complete") {
		t.Fatalf("zero-exit no-op reported success: %v %q", err, out.String())
	}
}
func TestM2InstalledCurrentDoesNotPrompt(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	m.updatePrompt.owner = "uzi-cli"
	m.installedVersion = func(string) (string, error) { return "v0.85.0", nil }
	next, cmd := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
	m = next.(tuiModel)
	m = settleObservation(m, cmd)
	if m.updatePrompt.showing {
		t.Fatal("already-current installed binary still prompts")
	}
}
func TestM2LaterPollAfterNotNow(t *testing.T) {
	withVersion(t, "v0.83.0")
	m := updatePromptModel(t)
	m.updatePrompt.owner = "uzi-cli"
	calls := 0
	m.installedVersion = func(string) (string, error) { calls++; return "v0.84.0", nil }
	poll := func() {
		next, cmd := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: "v0.85.0"}})
		m = next.(tuiModel)
		m = settleObservation(m, cmd)
	}
	poll()
	next, _ := m.updatePromptKey(keyEsc)
	m = next.(tuiModel)
	poll()
	if calls != 2 {
		t.Fatalf("installed probe calls after Not now = %d, want 2", calls)
	}
}

// An open, unanswered offer must survive a newer release arriving mid-session: the new
// candidate re-shows it (after its installed check, for a brew owner) rather than the
// once-per-session latch hiding it for good. A closed offer stays closed.
func TestM2OpenOfferSurvivesNewerRelease(t *testing.T) {
	for _, owner := range []string{"uzi-cli", ""} {
		t.Run("owner="+owner, func(t *testing.T) {
			withVersion(t, "v0.83.0")
			m := updatePromptModel(t)
			m.updatePrompt.owner = owner
			m.installedVersion = func(string) (string, error) { return "v0.83.0", nil }
			poll := func(v string) {
				next, cmd := m.Update(buildInfoMsg{latest: &apitypes.LatestReleaseDTO{Version: v}})
				m = next.(tuiModel)
				m = settleObservation(m, cmd)
			}
			poll("v0.85.0")
			if !m.updatePrompt.showing {
				t.Fatal("first offer not shown")
			}
			poll("v0.86.0")
			if !m.updatePrompt.showing || m.updatePrompt.latestVersion != "v0.86.0" {
				t.Fatalf("open offer lost on newer release: showing=%v latest=%q", m.updatePrompt.showing, m.updatePrompt.latestVersion)
			}
			next, _ := m.updatePromptKey(keyEsc)
			m = next.(tuiModel)
			poll("v0.87.0")
			if m.updatePrompt.showing {
				t.Fatal("a closed offer re-opened on a newer release")
			}
		})
	}
}
