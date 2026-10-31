package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #3719: the Codex select reason on the CLI and TUI surfaces.

func codexRunRow(t *testing.T, reason *string, id *string) string {
	t.Helper()
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	label := "work-login"
	r := apitypes.RunDTO{ID: "run-codex", Kind: "issue", Harness: "codex", Status: "completed", Health: "ok",
		CodexSecretLabel: &label, CodexSecretID: id, CodexSelectReason: reason}
	if err := renderRunDetail(p, r); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "CODEX_ALIAS") {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	t.Fatalf("no CODEX_ALIAS row in %q", buf.String())
	return ""
}

func TestRunGetCodexAliasRowNamesTheReason(t *testing.T) {
	id := "alias-id"
	for _, tc := range []struct {
		name   string
		reason *string
		id     *string
		want   string
	}{
		{"auto", sptr("auto"), &id, "CODEX_ALIAS work-login — auto (most limit headroom)"},
		{"default", sptr("default"), &id, "CODEX_ALIAS work-login — default"},
		{"deleted alias keeps the reason", sptr("pool_exhausted"), nil, "CODEX_ALIAS work-login (deleted) — auto (every login at capacity; soonest to release)"},
		{"NULL reason is not recorded", nil, &id, "CODEX_ALIAS work-login — not recorded"},
		{"empty reason is not recorded", sptr(""), &id, "CODEX_ALIAS work-login — not recorded"},
		{"an unknown reason prints as itself", sptr("future_reason"), &id, "CODEX_ALIAS work-login — future_reason"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexRunRow(t, tc.reason, tc.id); got != tc.want {
				t.Fatalf("row = %q, want %q", got, tc.want)
			}
		})
	}
	// An unknown reason is not from this binary's closed set, so it is folded like any untrusted cell.
	if got := codexRunRow(t, sptr("evil\x1b[31m\nreason"), &id); strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("hostile reason reached the terminal: %q", got)
	}
}

// TestCodexSelectReasonTextCoversEveryReason enumerates the closed vocabulary rather than a
// list typed here: every reason must render a non-empty, distinct phrase, and the TUI tag must
// be non-empty and distinct too. Go's switch is not exhaustive over a string type, so a reason
// added to codexselect without a phrase fails here, not in front of a user.
func TestCodexSelectReasonTextCoversEveryReason(t *testing.T) {
	text, tag := map[string]codexselect.Reason{}, map[string]codexselect.Reason{}
	for _, reason := range codexselect.AllReasons() {
		phrase := codexSelectReasonText(reason)
		if phrase == "" || phrase == string(reason) && reason != codexselect.ReasonDefault {
			t.Errorf("reason %q has no human phrase (got %q)", reason, phrase)
		}
		if prev, dup := text[phrase]; dup {
			t.Errorf("%q and %q render the same phrase %q", prev, reason, phrase)
		}
		text[phrase] = reason
		short := codexReasonTag(sptr(string(reason)))
		if short == "" || short == string(reason) && strings.Contains(string(reason), "_") {
			t.Errorf("reason %q has no short TUI tag (got %q)", reason, short)
		}
		if prev, dup := tag[short]; dup {
			t.Errorf("%q and %q render the same tag %q", prev, reason, short)
		}
		tag[short] = reason
	}
	if codexReasonTag(nil) != "" || codexReasonTag(sptr("")) != "" {
		t.Error("no reason must draw no tag")
	}
}

func TestSettingsCodexModeCommand(t *testing.T) {
	for _, mode := range []string{"auto", "default"} {
		fc := &uzicli.FakeClient{}
		out, _, code := runCLI(t, fakeEnv(fc), "settings", "codex-mode", mode)
		if code != 0 || fc.LastCodexMode != mode || fc.CodexModeCalls != 1 || strings.TrimSpace(out) != "Codex account mode: "+mode {
			t.Fatalf("%s: code=%d sent=%q calls=%d out=%q", mode, code, fc.LastCodexMode, fc.CodexModeCalls, out)
		}
	}
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "codex-mode", "auto", "--json")
	if code != 0 || !strings.Contains(out, `"codex_account_mode": "auto"`) {
		t.Fatalf("json: code=%d out=%q", code, out)
	}
	for _, args := range [][]string{{"settings", "codex-mode", "pool"}, {"settings", "codex-mode"}, {"settings", "codex-mode", "auto", "extra"}} {
		fc := &uzicli.FakeClient{}
		if _, _, code := runCLI(t, fakeEnv(fc), args...); code == 0 || fc.CodexModeCalls != 0 {
			t.Fatalf("%v: code=%d calls=%d, want a refusal with no write", args, code, fc.CodexModeCalls)
		}
	}
}

func TestSettingsGetShowsCodexMode(t *testing.T) {
	fc := &uzicli.FakeClient{Settings: apitypes.UserSettingsDTO{CodexAccountMode: "auto"}}
	out, _, code := runCLI(t, fakeEnv(fc), "settings", "get")
	if code != 0 || !strings.Contains(out, "Codex account mode: auto\n") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "settings", "get", "--json")
	if code != 0 || !strings.Contains(out, `"codex_account_mode": "auto"`) {
		t.Fatalf("json code=%d out=%q", code, out)
	}
}

func TestRailCodexSnapshotShowsReasonTag(t *testing.T) {
	id := "alias-id"
	label := "old-login"
	for _, tc := range []struct {
		name   string
		reason *string
		id     *string
		want   string
	}{
		{"tag beside the label", sptr("best_of_pool"), &id, "old-login · best of pool"},
		{"tag then deleted marker", sptr("auto"), nil, "old-login · auto (deleted)"},
		{"no reason draws nothing", nil, &id, "old-login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tuiTestModel(t, &uzicli.FakeClient{}, "codex-run")
			m.width, m.height = 100, 40
			m = applyDetail(m, apitypes.RunDTO{ID: "codex-run", Harness: "codex", Status: "completed",
				CodexSecretID: tc.id, CodexSecretLabel: &label, CodexSelectReason: tc.reason}, nil)
			out := stripANSI(m.railRateMeters(time.Now(), 3))
			if !strings.Contains(out, tc.want) || strings.Count(out, "\n") != 1 {
				t.Fatalf("rail = %q, want it to contain %q on one entry row", out, tc.want)
			}
		})
	}
	// A reason from a newer server is untrusted: sanitized, and the entry stays inside the rail.
	m := tuiTestModel(t, &uzicli.FakeClient{}, "codex-run")
	m.width, m.height = 100, 40
	hostile := "evil\x1b[31m\u202e" + strings.Repeat("z", 200)
	m = applyDetail(m, apitypes.RunDTO{ID: "codex-run", Harness: "codex", Status: "completed",
		CodexSecretID: &id, CodexSecretLabel: &label, CodexSelectReason: &hostile}, nil)
	block := m.railRateMeters(time.Now(), 3)
	for _, line := range strings.Split(stripANSI(block), "\n") {
		if visualWidth(line) > m.railWidth() {
			t.Fatalf("rail line %q is wider than the rail (%d)", line, m.railWidth())
		}
	}
	if strings.Contains(block, "\x1b[31m") || strings.Contains(block, "\u202e") {
		t.Fatalf("hostile reason reached the rail: %q", block)
	}
}
