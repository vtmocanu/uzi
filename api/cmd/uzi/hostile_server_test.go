package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// These drive REAL commands against a fake server that answers with the hostile
// payloads issue #180 was filed with. The unit tests in internal/uzicli prove the
// boundary strips what it is handed; these prove the commands actually route through
// it — which is the half that was broken, since the package's sanitizers existed all
// along and most render paths simply did not call one.
//
// Every case asserts on CONTENT: the negative (no control byte survived) is paired with
// a positive (the printable text and the row's other columns are still there), because
// an absence alone cannot distinguish a working sanitizer from a command that printed
// nothing at all.
const (
	esc2J    = "\x1b[2J\x1b[1;1H" // clear screen + home cursor
	oscTitle = "\x1b]0;pwned\x07" // rewrite the window title
)

// assertNoTerminalControl is the shared negative. It names the offending byte rather
// than the whole sequence: a partial strip leaving a bare ESC behind would satisfy a
// "does not contain \x1b[2J" check while still driving the terminal.
func assertNoTerminalControl(t *testing.T, where, out string) {
	t.Helper()
	for _, bad := range []struct {
		r    rune
		name string
	}{
		{0x1b, "ESC"}, {0x07, "BEL"}, {0x7f, "DEL"}, {0x202e, "U+202E RTL override"},
	} {
		if strings.ContainsRune(out, bad.r) {
			t.Errorf("%s: %s reached the terminal: %q", where, bad.name, out)
		}
	}
}

func TestHostileServerCannotDriveTheTerminal(t *testing.T) {
	past := time.Now().Add(-time.Hour)

	t.Run("admin-cli-tokens", func(t *testing.T) {
		// #169's second half: a CLI-token name renders into a CROSS-TENANT admin table,
		// beside its owner's email, and cli_tokens.go validates length only. The name here
		// also carries a newline, which is the row-forging half of that issue.
		fc := &uzicli.FakeClient{AdminCLITokens: []apitypes.AdminCLITokenDTO{{
			OwnerEmail:  "victim@example.com",
			TokenPrefix: "uzc_1234",
			Name:        "laptop" + esc2J + "\nuzc_9999  admin@example.com",
			Scope:       "user",
			LastUsedAt:  &past,
		}}}
		out, _, code := runCLI(t, fakeEnv(fc), "admin", "cli-tokens")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		assertNoTerminalControl(t, "admin cli-tokens", out)
		// Positive: the row still renders, with every other column intact.
		for _, want := range []string{"victim@example.com", "uzc_1234", "laptop", "user"} {
			if !strings.Contains(out, want) {
				t.Errorf("admin cli-tokens lost %q: %q", want, out)
			}
		}
		// The forged row must have been folded into the NAME cell, not become a line.
		if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")); n != 2 {
			t.Errorf("admin cli-tokens rendered %d lines, want 2 (header + 1 row) — the embedded newline forged a row:\n%q", n, out)
		}
	})

	t.Run("admin-users", func(t *testing.T) {
		// An email is the last field anyone would think to sanitize, which is exactly why
		// the boundary rather than a call-site audit is the fix.
		fc := &uzicli.FakeClient{AdminUsers: []apitypes.UserDTO{
			{ID: "u1", Email: "real@example.com"},
			{ID: "u2", Email: "spoof" + oscTitle + "@example.com", IsAdmin: true},
		}}
		out, _, code := runCLI(t, fakeEnv(fc), "admin", "users")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		assertNoTerminalControl(t, "admin users", out)
		for _, want := range []string{"real@example.com", "u2", "true"} {
			if !strings.Contains(out, want) {
				t.Errorf("admin users lost %q: %q", want, out)
			}
		}
	})

	t.Run("run-list-title", func(t *testing.T) {
		title := "fix the thing" + esc2J
		fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
			{RunDTO: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Title: &title}},
		}}
		out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		assertNoTerminalControl(t, "run list", out)
		for _, want := range []string{"r1", "issue", "running", "fix the thing"} {
			if !strings.Contains(out, want) {
				t.Errorf("run list lost %q: %q", want, out)
			}
		}
	})

	t.Run("error-message", func(t *testing.T) {
		// The broadest path in the CLI and the one furthest from the Printer: statusError
		// folds the server's {"error": "..."} body into the message, and Main prints it
		// with a fixed "uzi: " prefix. A newline here would print a prefix-less second line
		// in the CLI's own voice, which is why root.go uses CellText and not SanitizeTTY.
		fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitNotFound,
			"%s", "no such run"+esc2J+"\nuzi: all runs deleted")}
		out, errOut, code := runCLI(t, fakeEnv(fc), "run", "list")
		if code != uzicli.ExitNotFound {
			t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, uzicli.ExitNotFound, out, errOut)
		}
		assertNoTerminalControl(t, "error message", errOut)
		if !strings.Contains(errOut, "uzi: no such run") {
			t.Errorf("the error lost its legitimate text: %q", errOut)
		}
		if n := strings.Count(errOut, "\n"); n != 1 {
			t.Errorf("the error printed %d newlines, want 1 — a server message forged a second line:\n%q", n, errOut)
		}
	})

	t.Run("error-message-bounded", func(t *testing.T) {
		// The strip alone is not enough here: uzicli.CellText does NOT truncate, and the
		// server error is read via a 32 MiB io.LimitReader, so a megabyte-long body would
		// print in full. root.go uses the LOCAL cellText, whose 200-char cap bounds it (#220).
		fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitNotFound, "%s", "no such run: "+strings.Repeat("A", 1<<20))}
		out, errOut, code := runCLI(t, fakeEnv(fc), "run", "list")
		if code != uzicli.ExitNotFound {
			t.Fatalf("exit = %d, want %d; stdout=%q", code, uzicli.ExitNotFound, out)
		}
		if len(errOut) > 4096 {
			t.Errorf("stderr is %d bytes; the server error string reached the terminal unbounded", len(errOut))
		}
		// Positive: the legitimate prefix rendered, so the payload actually reached this path.
		if !strings.Contains(errOut, "uzi: no such run") {
			t.Errorf("the error lost its legitimate text: %q", errOut)
		}
	})

	t.Run("json-mode-is-untouched", func(t *testing.T) {
		// The other side of the contract, asserted through a real command rather than the
		// Printer: --json must carry the server's bytes verbatim so an agent sees what was
		// actually sent. ESC arrives escaped by encoding/json; the RTL override arrives
		// RAW, because json.Marshal does not escape Cf — which is precisely what a
		// sanitizer on this path would have eaten.
		title := "fix" + esc2J + " the " + string(rune(0x202E)) + " thing"
		fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
			{RunDTO: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "running", Title: &title}},
		}}
		out, _, code := runCLI(t, fakeEnv(fc), "run", "list", "--json")
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
		if !strings.Contains(out, "u001b") {
			t.Errorf("--json did not carry the ESC as an escape: %q", out)
		}
		if !strings.ContainsRune(out, 0x202e) {
			t.Errorf("--json stripped the RTL override; the machine channel must be byte-faithful: %q", out)
		}
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("--json emitted a RAW ESC, which encoding/json should have escaped: %q", out)
		}
	})
}
