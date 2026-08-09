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
				ID:         "r1",
				Status:     "completed",
				MrWebURL:   sptr("https://example.com/mr/7"),
				MrState:    nil, // a null field
				Milestones: []apitypes.Milestone{{ID: "m1", Title: "one"}},
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

func TestRunGetFieldNullIsEmptyLine(t *testing.T) {
	stdout, _, code := runCLI(t, fakeEnv(fieldFake()), "run", "get", "r1", "--field", "mr_state")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if stdout != "\n" {
		t.Errorf("a null field must print a single empty line, got %q", stdout)
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
