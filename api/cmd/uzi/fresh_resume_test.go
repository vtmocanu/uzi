package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func availableFreshPreview() apitypes.FreshResumePreviewDTO {
	return apitypes.FreshResumePreviewDTO{
		Status: apitypes.FreshResumeStatusAvailable, SelectionToken: "tok1",
		EvidenceGeneration: 3, ClaimGeneration: 5, Kind: apitypes.FreshResumeKindCapture,
		CaptureID: "cap-1", Sha: "abc123", UncoveredGenerations: []int64{4, 5},
	}
}

func TestFreshResumeShowsCutoffAndS6Text(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d stderr=%q", code, errb)
	}
	for _, want := range []string{
		"evidence generation (Ge): 3", "capture", "abc123",
		"No saved evidence exists for generation 4, 5; any edits made in it are not included.",
		"Edits made after the saved evidence may be missing",
		"original worker's source and custody are kept",
		"Unsaved edits may be lost if the original worker's volume is gone",
		"Each attempt keeps its own custody hold",
		"counts one extra admission unit until its new hold is released or the run ends",
		"only a third eligible hosted worker can take a repeat fresh resume",
		"the original worker never takes this run again",
		"only two workers whose new worker is lost must add a worker or cancel",
	} {
		if !strings.Contains(errb, want) {
			t.Errorf("preview missing %q\n%s", want, errb)
		}
	}
	if len(fc.FreshResumeStartCalls) != 1 {
		t.Fatalf("start calls = %+v", fc.FreshResumeStartCalls)
	}
	c := fc.FreshResumeStartCalls[0]
	if c.RunID != "run1" || c.Request.SelectionToken != "tok1" || len(c.Request.RequestID) != 36 {
		t.Errorf("start call = %+v", c)
	}
}

func TestFreshResumeNewConfirmNewRequestID(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if len(fc.FreshResumeStartCalls) != 2 || fc.FreshResumeStartCalls[0].Request.RequestID == fc.FreshResumeStartCalls[1].Request.RequestID {
		t.Fatalf("two confirms must use two request ids: %+v", fc.FreshResumeStartCalls)
	}
}

func TestFreshResumeTransportRetryReusesRequestID(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview(),
		FreshResumeStartErr: uzicli.Exitf(uzicli.ExitUnreachable, "connection reset")}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if code != uzicli.ExitUnreachable {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitUnreachable)
	}
	if len(fc.FreshResumeStartCalls) != 3 {
		t.Fatalf("calls = %d, want 1 + %d bounded retries", len(fc.FreshResumeStartCalls), freshResumeStartRetries)
	}
	id := fc.FreshResumeStartCalls[0].Request.RequestID
	for _, c := range fc.FreshResumeStartCalls {
		if c.Request.RequestID != id {
			t.Errorf("retry changed the request id: %+v", fc.FreshResumeStartCalls)
		}
	}
	if !strings.Contains(errb, "--request-id "+id) {
		t.Errorf("error should name the request id for a manual retry: %q", errb)
	}
}

func TestFreshResumeRequestIDFlagReused(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	const id = "11111111-2222-4333-8444-555555555555"
	runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes", "--request-id", id)
	if len(fc.FreshResumeStartCalls) != 1 || fc.FreshResumeStartCalls[0].Request.RequestID != id {
		t.Fatalf("calls = %+v", fc.FreshResumeStartCalls)
	}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes", "--request-id", "nope")
	if code != uzicli.ExitUsage {
		t.Errorf("bad request id exit = %d", code)
	}
}

func TestFreshResumeNonTTYWithoutYesRefuses(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	env := fakeEnv(fc)
	env.StdinTTY = false
	_, errb, code := runCLI(t, env, "run", "fresh-resume", "run1")
	if code != uzicli.ExitUsage || len(fc.FreshResumeStartCalls) != 0 || !strings.Contains(errb, "--yes") {
		t.Fatalf("exit=%d calls=%+v stderr=%q", code, fc.FreshResumeStartCalls, errb)
	}
}

func TestFreshResumeDeclinedSendsNothing(t *testing.T) {
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("n\n")
	_, errb, code := runCLI(t, env, "run", "fresh-resume", "run1")
	if code != uzicli.ExitOK || len(fc.FreshResumeStartCalls) != 0 || !strings.Contains(errb, "aborted") {
		t.Fatalf("exit=%d calls=%+v stderr=%q", code, fc.FreshResumeStartCalls, errb)
	}
	env.Stdin = strings.NewReader("y\n")
	_, _, code = runCLI(t, env, "run", "fresh-resume", "run1")
	if code != uzicli.ExitOK || len(fc.FreshResumeStartCalls) != 1 {
		t.Fatalf("confirmed: exit=%d calls=%+v", code, fc.FreshResumeStartCalls)
	}
}

func TestFreshResumeNonAvailableStatusesExplained(t *testing.T) {
	for status, want := range map[string]string{
		apitypes.FreshResumeStatusNoDestination: "No other persistent hosted worker",
		apitypes.FreshResumeStatusCustodyLimit:  "custody hold limit",
		apitypes.FreshResumeStatusNoEvidence:    "no saved evidence",
		apitypes.FreshResumeStatusExpired:       "expired",
		apitypes.FreshResumeStatusDisabled:      "not enabled",
		apitypes.FreshResumeStatusAmbiguous:     "none contains the others",
		"something_new":                         "does not know",
	} {
		fc := &uzicli.FakeClient{FreshResumePreviewResult: apitypes.FreshResumePreviewDTO{Status: status}}
		_, errb, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
		if code != uzicli.ExitConflict || !strings.Contains(strings.ToLower(errb), strings.ToLower(want)) {
			t.Errorf("%s: exit=%d stderr=%q", status, code, errb)
		}
		if strings.Contains(errb, "something_new") {
			t.Errorf("an unknown status must never print raw: %q", errb)
		}
		if len(fc.FreshResumeStartCalls) != 0 {
			t.Errorf("%s: a non-available preview must not start", status)
		}
	}
}

func TestFreshResumeEach409ReasonRendered(t *testing.T) {
	for reason, want := range map[string]string{
		apitypes.FreshResumeReasonStaleSelection:   "selection changed",
		apitypes.FreshResumeReasonRequestCompleted: "already finished",
		"fresh_resume_in_progress":                 "already in progress",
	} {
		fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview(),
			FreshResumeStartErr: &uzicli.ExitError{Code: uzicli.ExitConflict, Err: errors.New("conflict"), Reason: reason}}
		_, errb, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
		if code != uzicli.ExitConflict || !strings.Contains(errb, want) {
			t.Errorf("%s: exit=%d stderr=%q", reason, code, errb)
		}
		if len(fc.FreshResumeStartCalls) != 1 {
			t.Errorf("%s: a 409 must not be retried: %d calls", reason, len(fc.FreshResumeStartCalls))
		}
	}
}

func TestFreshResumeUntrustedFieldsSanitized(t *testing.T) {
	p := availableFreshPreview()
	p.Sha = "abc\x1b[31mred"
	fc := &uzicli.FakeClient{FreshResumePreviewResult: p}
	_, errb, _ := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if strings.Contains(errb, "\x1b") {
		t.Errorf("escape byte reached the terminal: %q", errb)
	}
}

func TestRecoveryDecisionLineHintsFreshResumeOnlyWhenHeld(t *testing.T) {
	cause := retainedRecoveryExhaustedCause
	held := apitypes.RunDTO{ID: "run9", Status: statusRecoveryWait, RecoveryWaitCause: &cause, RecoveryRetryNotBeforePresent: true}
	if got := recoveryDecisionLine(held); !strings.Contains(got, "uzi run fresh-resume run9") {
		t.Errorf("held line lacks the hint: %q", got)
	}
}
