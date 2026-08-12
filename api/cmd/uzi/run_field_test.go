package main

import (
	"strings"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #264 M2 — `uzi run get --field <name>` prints named top-level scalar(s) raw,
// one per line, so a poller never JSON-parses (the exact case that produced the
// zsh-`echo` footgun).

func fieldFake() *uzicli.FakeClient {
	return &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{
			"r1": {
				ID:           "r1",
				Status:       "completed",
				MrWebURL:     sptr("https://example.com/mr/7"),
				MrState:      nil, // a null field
				Model:        sptr("fable"),
				WaitOnLimit:  true,
				RequeueCount: 3,
				Milestones:   []apitypes.Milestone{{ID: "m1", Title: "one"}},
			},
			// A run whose schedule left the model unset — model freezes as null.
			"r2": {
				ID:     "r2",
				Status: "completed",
				Model:  nil, // a null field
			},
		},
	}
}

func TestRunGetFieldPresentScalar(t *testing.T) {
	stdout, stderr, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.TrimSpace(stdout) != "completed" {
		t.Errorf("stdout = %q, want raw 'completed'", stdout)
	}
	// Raw and unquoted: no JSON quotes.
	if strings.Contains(stdout, `"`) {
		t.Errorf("scalar must be unquoted, got %q", stdout)
	}
}

func TestRunGetFieldRepeatablePreservesOrder(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1",
		"--field", "mr_web_url", "--field", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	want := "https://example.com/mr/7\ncompleted\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// Non-string scalars (bool, number) print their raw JSON literal, unquoted — pinning
// scalarField's number/bool passthrough, which a string-only fixture would leave dead.
func TestRunGetFieldBoolAndNumberRaw(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1",
		"--field", "wait_on_limit", "--field", "requeue_count")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "true\n3\n" {
		t.Errorf("stdout = %q, want raw literals \"true\\n3\\n\"", stdout)
	}
}

func TestRunGetFieldNullIsEmptyLine(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "mr_state")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "\n" {
		t.Errorf("a null field must print a single empty line, got %q", stdout)
	}
}

// The frozen per-schedule run model (PRD #300 M1) is a top-level scalar on RunDTO, so
// `--field model` exposes it raw with no CLI change — this pins that read surface.
func TestRunGetFieldModelRaw(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "model")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "fable\n" {
		t.Errorf("stdout = %q, want raw \"fable\\n\" (unquoted, one line)", stdout)
	}
	if strings.Contains(stdout, `"`) {
		t.Errorf("model must be unquoted, got %q", stdout)
	}
}

// A run whose model was never overridden freezes as null — `--field model` prints a
// single empty line, mirroring the mr_state null-field contract.
func TestRunGetFieldModelNullIsEmptyLine(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r2", "--field", "model")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "\n" {
		t.Errorf("a null model must print a single empty line, got %q", stdout)
	}
}

// The frozen "apply model also to agents" flag (PRD #305) is a top-level bool on RunDTO,
// so `--field override_subagent_model` prints it raw (true/false, unquoted, one line) with
// no CLI change — this pins that read surface, both polarities.
func TestRunGetFieldOverrideSubagentModel(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"on":  {ID: "on", Status: "completed", OverrideSubagentModel: true},
		"off": {ID: "off", Status: "completed", OverrideSubagentModel: false},
	}}
	stdout, _, code := runCLI(t, fakeEnv(fc), "run", "get", "on", "--field", "override_subagent_model")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "true\n" {
		t.Errorf("stdout = %q, want raw \"true\\n\" (unquoted, one line)", stdout)
	}
	stdout2, _, code2 := runCLI(t, fakeEnv(fc), "run", "get", "off", "--field", "override_subagent_model")
	if code2 != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code2)
	}
	if stdout2 != "false\n" {
		t.Errorf("stdout = %q, want raw \"false\\n\"", stdout2)
	}
}

func TestRunGetFieldUnknownIsUsageError(t *testing.T) {
	_, stderr, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
	}
}

func TestRunGetFieldNonScalarIsUsageError(t *testing.T) {
	// milestones is an array — no meaningful one-line raw form (D5).
	_, stderr, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "milestones")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "not a scalar") {
		t.Errorf("stderr should explain the non-scalar rejection, got %q", stderr)
	}
}

// A forge-authored scalar (issue_title) carrying a raw ANSI/control sequence must be
// byte-exact on a PIPE (the agent poller contract) but stripped on a TTY, where an
// unescaped ESC would drive the terminal — the asymmetry `--json` avoids via its
// encoder. This pins printRunFields's TTY guard.
func TestRunGetFieldRawOnPipeSanitizedOnTTY(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", IssueTitle: "safe\x1b[2Jinjected"},
	}}
	// Pipe (non-TTY): raw, exact bytes.
	stdout, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--field", "issue_title")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, "\x1b[2J") {
		t.Errorf("pipe output must be raw, got %q", stdout)
	}
	// TTY: the ESC/CSI is stripped, printable text survives.
	env := fakeEnv(fc)
	env.StdoutTTY = true
	stdout2, _, code2 := runCLI(t, env, "run", "get", "r1", "--field", "issue_title")
	if code2 != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code2)
	}
	if strings.ContainsRune(stdout2, '\x1b') {
		t.Errorf("TTY output must be sanitized of ESC, got %q", stdout2)
	}
	if !strings.Contains(stdout2, "safe") || !strings.Contains(stdout2, "injected") {
		t.Errorf("printable text must survive sanitizing, got %q", stdout2)
	}
}

func TestRunGetFieldWithJSONRejected(t *testing.T) {
	_, stderr, code := runCLI(t, fakeEnv(fieldFake()), "--json", "run", "get", "r1", "--field", "status")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "--field cannot be combined with --json") {
		t.Errorf("stderr should name the conflict, got %q", stderr)
	}
}

// A validation failure in a multi-field call must print NOTHING before erroring, so a
// caller never sees a partial stream mistaken for the full answer.
func TestRunGetFieldValidateBeforePrint(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1",
		"--field", "status", "--field", "nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("nothing should be printed when a later field is invalid, got %q", stdout)
	}
}
