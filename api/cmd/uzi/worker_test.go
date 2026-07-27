package main

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerRm(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastDeletedWorkerID != "w1" {
		t.Fatalf("rm called DeleteWorker(%q), want w1", fc.LastDeletedWorkerID)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("rm output = %q, want a 'removed' confirmation", out)
	}
}

func TestWorkerRmJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"deleted": true`) || !strings.Contains(out, `"id": "w1"`) {
		t.Errorf("rm --json = %q, want deleted/id", out)
	}
}

// A worker with active runs is a 409 (exit 5); the CLI must surface it.
func TestWorkerRmConflict(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "worker has active runs")}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm", "w1")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict)", code, uzicli.ExitConflict)
	}
}

func TestWorkerRmRequiresArg(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "worker", "rm")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if fc.LastDeletedWorkerID != "" {
		t.Error("rm with no id must not call DeleteWorker")
	}
}

// -------------------------------------------------------------------------
// worker set-token (PRD #104 M3)
// -------------------------------------------------------------------------

func TestWorkerSetToken(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "console-key")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "console-key" {
		t.Fatalf("set-token called SetWorkerBindMode(%q,_,%q), want (w1,_,console-key)",
			fc.LastSetTokenWorkerID, fc.LastSetTokenLabel)
	}
	// The MODE rides with the label since PRD #111 M3. Asserted because the label
	// alone no longer determines what the server does: a label sent with mode
	// "default" or "auto" is a 400, not a pin.
	if fc.LastSetTokenMode != "pinned" {
		t.Errorf("a label sent mode %q, want pinned", fc.LastSetTokenMode)
	}
	if !strings.Contains(out, "console-key") {
		t.Errorf("set-token output = %q, want it to name the token", out)
	}
}

// --default clears the binding, which the client expresses as an empty label plus
// the explicit mode.
func TestWorkerSetTokenDefault(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "--default")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenLabel != "" || fc.LastSetTokenMode != "default" {
		t.Fatalf("--default called SetWorkerBindMode(%q,%q,%q), want (w1,default,\"\")",
			fc.LastSetTokenWorkerID, fc.LastSetTokenMode, fc.LastSetTokenLabel)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("--default output = %q, want it to say the worker uses the default", out)
	}
}

// --auto is PRD #111 M3's third mode. It sends NO label: the server refuses a
// label alongside a non-pinned mode rather than quietly dropping one of them, so a
// client that sent both would be rejected, not silently reconciled.
func TestWorkerSetTokenAuto(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "--auto")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastSetTokenWorkerID != "w1" || fc.LastSetTokenMode != "auto" || fc.LastSetTokenLabel != "" {
		t.Fatalf("--auto called SetWorkerBindMode(%q,%q,%q), want (w1,auto,\"\")",
			fc.LastSetTokenWorkerID, fc.LastSetTokenMode, fc.LastSetTokenLabel)
	}
	// The confirmation must say POOL, not "default": a user who cannot tell the two
	// apart from the output has no way to know whether --auto took effect.
	if !strings.Contains(out, "auto-selects") || !strings.Contains(out, "pool") {
		t.Errorf("--auto output = %q, want it to say the worker auto-selects from the pool", out)
	}
}

// A label AND --default is ambiguous; so is neither. Both are usage errors rather
// than a silent choice of one meaning over the other.
func TestWorkerSetTokenAmbiguousArgs(t *testing.T) {
	for _, args := range [][]string{
		{"worker", "set-token", "w1", "console-key", "--default"},
		{"worker", "set-token", "w1"},
		// PRD #111 M3 makes three choices, so every pair is a usage error and so is
		// all three. Enumerated rather than sampled: the pair a pairwise check
		// forgets is always the one added last.
		{"worker", "set-token", "w1", "console-key", "--auto"},
		{"worker", "set-token", "w1", "--default", "--auto"},
		{"worker", "set-token", "w1", "console-key", "--default", "--auto"},
	} {
		fc := &uzicli.FakeClient{}
		_, _, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Fatalf("%v: exit = %d, want %d (usage)", args, code, uzicli.ExitUsage)
		}
		if fc.LastSetTokenWorkerID != "" {
			t.Errorf("%v: must not reach the API", args)
		}
	}
}

// An unknown label is a 400 → exit 3; an unknown worker is a 404 → exit 4. Both
// must reach the caller as their documented codes rather than a generic failure.
func TestWorkerSetTokenErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown label", uzicli.Exitf(uzicli.ExitAuth, "no Anthropic token with that label"), uzicli.ExitAuth},
		{"unknown worker", uzicli.Exitf(uzicli.ExitNotFound, "worker not found"), uzicli.ExitNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{Err: tc.err}
			_, _, code := runCLI(t, fakeEnv(fc), "worker", "set-token", "w1", "some-label")
			if code != tc.want {
				t.Fatalf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}

// TestWorkerListShowsBindMode is PRD #111 M5, and it closes a CLI-parity hole M3
// opened: `uzi worker set-token --auto` is a WRITE the CLI can perform, and until
// this column there was no human-readable READ to confirm it — only `--json`. A
// three-way user choice you can set and cannot see is worse than one you cannot set.
//
// The fixture stages all three modes at once because the failure that matters is a
// renderer that collapses two of them. One mode per test would pass against a
// function that returned the same word for `default` and `auto`.
//
// MUTATION THIS CATCHES: returning w.AnthropicBindMode verbatim for `pinned` — the
// pinned row then reads "pinned" instead of naming the token, which is the fact a
// user needs and the argument `uzi worker set-token` takes back.
func TestWorkerListShowsBindMode(t *testing.T) {
	label := "console-key"
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "alpha", Status: "online", AnthropicBindMode: "default"},
		{ID: "w2", Name: "bravo", Status: "online", AnthropicBindMode: "auto"},
		{
			ID: "w3", Name: "charlie", Status: "online", AnthropicBindMode: "pinned",
			AnthropicSecretID: sptr("sec-1"), AnthropicSecretLabel: &label,
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "TOKEN") {
		t.Fatalf("worker list is missing the TOKEN column: %q", out)
	}
	cellOf := func(name string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, name) {
				f := strings.Fields(line)
				return f[len(f)-1]
			}
		}
		t.Fatalf("no row for %s in %q", name, out)
		return ""
	}
	if got := cellOf("alpha"); got != "default" {
		t.Errorf("default worker's TOKEN = %q, want default", got)
	}
	if got := cellOf("bravo"); got != "auto" {
		t.Errorf("auto worker's TOKEN = %q, want auto — an auto worker has no fixed token, and "+
			"naming one would present a snapshot as a setting", got)
	}
	// The LABEL, not the word "pinned": it is what the user set and what
	// `uzi worker set-token` takes back.
	if got := cellOf("charlie"); got != "console-key" {
		t.Errorf("pinned worker's TOKEN = %q, want the label console-key", got)
	}
}

// TestBindModeCellUnknownPassesThrough: the CLI ships separately from the API, so a
// newer server can send a fourth mode. Printing it as itself is honest; mapping it to
// "default" would state something false about where a worker's money goes.
func TestBindModeCellUnknownPassesThrough(t *testing.T) {
	got := bindModeCell(apitypes.WorkerDTO{AnthropicBindMode: "some_future_mode"})
	if got != "some_future_mode" {
		t.Fatalf("cell = %q, want the unrecognised mode verbatim", got)
	}
}

// TestBindModeCellPinnedWithNoLabel is belt-and-braces against a DTO that
// contradicts itself. The server reports the EFFECTIVE mode, so `pinned` always
// arrives with a label — D9's pinned-with-a-deleted-token case is mapped to
// `default` upstream. If one ever arrives anyway, "default" is what such a worker
// would actually spend, so it is the honest cell.
func TestBindModeCellPinnedWithNoLabel(t *testing.T) {
	if got := bindModeCell(apitypes.WorkerDTO{AnthropicBindMode: "pinned"}); got != "default" {
		t.Fatalf("cell = %q, want default — a pin with no credential resolves as the default (D9)", got)
	}
}
