package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestTUISplitConfigModeAndUsageError(t *testing.T) {
	store := uzicli.NewStore(t.TempDir())
	for _, tc := range []struct {
		value, want string
		invalid     bool
	}{
		{"", "auto", false},
		{"auto", "auto", false},
		{"off", "off", false},
		{"on", "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if err := store.SaveConfig(&uzicli.Config{TUI: uzicli.TUIConfig{Split: tc.value}}); err != nil {
				t.Fatal(err)
			}
			got, err := tuiSplitConfigMode(store)
			if !tc.invalid {
				if err != nil || got != tc.want {
					t.Fatalf("mode=%q err=%v, want %q", got, err, tc.want)
				}
				return
			}
			if uzicli.ExitCodeFor(err) != uzicli.ExitUsage || !strings.Contains(err.Error(), "config.toml") || !strings.Contains(err.Error(), "[tui] split") || !strings.Contains(err.Error(), "auto") || !strings.Contains(err.Error(), "off") {
				t.Fatalf("invalid mode error = %v", err)
			}
			cmd := newTUICmd(Env{Store: store, StdoutTTY: true}, &globalFlags{})
			if err := cmd.RunE(cmd, nil); uzicli.ExitCodeFor(err) != uzicli.ExitUsage {
				t.Fatalf("tui exit = %d, want usage 2 (err=%v)", uzicli.ExitCodeFor(err), err)
			}
		})
	}
	if mode, err := tuiSplitConfigMode(nil); err != nil || mode != "auto" {
		t.Fatalf("nil store mode=%q err=%v", mode, err)
	}
	for _, value := range []string{"true", "42", "[\"auto\"]"} {
		t.Run("invalid TOML type "+value, func(t *testing.T) {
			body := "[tui]\nsplit = " + value + "\n"
			if err := os.WriteFile(filepath.Join(store.Dir(), "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := newTUICmd(Env{Store: store, StdoutTTY: true}, &globalFlags{})
			err := cmd.RunE(cmd, nil)
			if uzicli.ExitCodeFor(err) != uzicli.ExitUsage || !strings.Contains(err.Error(), "[tui] split") || !strings.Contains(err.Error(), "config.toml") || !strings.Contains(err.Error(), "auto") || !strings.Contains(err.Error(), "off") {
				t.Fatalf("invalid TOML type %s: %v", value, err)
			}
		})
	}
	for _, value := range []string{"auto", "off"} {
		t.Run("unrelated config error with "+value, func(t *testing.T) {
			body := "current = 42\n[tui]\nsplit = \"" + value + "\"\n"
			if err := os.WriteFile(filepath.Join(store.Dir(), "config.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := tuiSplitConfigMode(store)
			if err == nil || uzicli.ExitCodeFor(err) == uzicli.ExitUsage || strings.Contains(err.Error(), "accepted values") {
				t.Fatalf("unrelated config decode error misreported as split: %v", err)
			}
		})
	}
}

func TestTUISplitOffAndSessionToggle(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = resizeSplit(m, 120, splitMinHeight+2)
	if m.splitDrawn() || m.splitEligible() {
		t.Fatal("off config drew split")
	}
	m = press(t, m, "s")
	if m.splitOff || m.splitNote != "" || strings.Contains(stripANSI(m.View().Content), "s split") {
		t.Fatal("off config changed session state or footer")
	}

	m.splitMode = "auto"
	if m.splitOff || !m.splitDrawn() {
		t.Fatal("auto mode did not draw split")
	}
	m = press(t, m, "s")
	if !m.splitOff || m.splitDrawn() || m.view != viewBoard || !strings.Contains(stripANSI(m.View().Content), "s split") {
		t.Fatal("s did not collapse with visible restore hint")
	}
	m = resizeSplit(m, 120, splitMinHeight+3)
	if m.splitDrawn() {
		t.Fatal("resize re-enabled session-off split")
	}
	m = press(t, m, "s")
	if m.splitOff || !m.splitDrawn() {
		t.Fatal("s did not restore split")
	}
	m = resizeSplit(m, 120, splitMinHeight-1)
	m = press(t, m, "s")
	if m.splitOff || m.splitNote != "terminal too small to split" || !strings.Contains(stripANSI(m.View().Content), m.splitNote) {
		t.Fatal("undersized s did not show one-key note")
	}
}

func TestTUISplitConfigSurvivesContextAndLoginWrites(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{}, TUI: uzicli.TUIConfig{Split: "off"}}
	env := seedStore(t, cfg, &uzicli.Credentials{Contexts: map[string]uzicli.Credential{}})
	if _, stderr, code := runCLI(t, env, "context", "set", "work", "--url", "https://work.example"); code != uzicli.ExitOK {
		t.Fatalf("context set exit=%d stderr=%s", code, stderr)
	}
	if err := finishLogin(env, &globalFlags{quiet: true}, "work", "https://work.example", uzicli.CLIAuthPollResult{Token: "uzc_x"}); err != nil {
		t.Fatalf("finishLogin: %v", err)
	}
	got, err := env.Store.LoadConfig()
	if err != nil || got.TUI.Split != "off" || got.Contexts["work"].URL != "https://work.example" {
		t.Fatalf("config after writes=%+v err=%v", got, err)
	}
}
