package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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
	old := freshResumeSleep
	freshResumeSleep = func(context.Context, time.Duration) {}
	defer func() { freshResumeSleep = old }()
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
		// The two reasons that tell the owner to run the command again name it.
		if reason != "fresh_resume_in_progress" && !strings.Contains(errb, "`uzi run fresh-resume`") {
			t.Errorf("%s: message must name the command `uzi run fresh-resume`: %q", reason, errb)
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

func TestFreshResumeUntrustedCheckpointRefAndCaptureSanitized(t *testing.T) {
	p := availableFreshPreview()
	p.CheckpointRef = "refs/x\x1b[31mred"
	p.CaptureID = "cap\x1b[32mgreen"
	fc := &uzicli.FakeClient{FreshResumePreviewResult: p}
	_, errb, _ := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if strings.Contains(errb, "\x1b") || !strings.Contains(errb, "checkpoint ref:") || !strings.Contains(errb, "capture:") {
		t.Errorf("escape byte reached the terminal or ref missing: %q", errb)
	}
}

func TestFreshResumeCutoffShownBeforePromptEvenWhenQuietOrJSON(t *testing.T) {
	for _, args := range [][]string{{"--quiet"}, {"--json"}, {"--quiet", "--json"}} {
		fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
		env := fakeEnv(fc)
		env.StdinTTY = true
		env.Stdin = strings.NewReader("n\n")
		_, errb, _ := runCLI(t, env, append(append([]string{}, args...), "run", "fresh-resume", "run1")...)
		cut := strings.Index(errb, "Edits made after the saved evidence may be missing")
		prompt := strings.Index(errb, "[y/N]")
		if cut < 0 || prompt < 0 || cut > prompt {
			t.Errorf("%v: cutoff text must precede the prompt: %q", args, errb)
		}
	}
	// The --request-id plus --selection-token replay path shows it before its prompt too.
	{
		fc := &uzicli.FakeClient{}
		env := fakeEnv(fc)
		env.StdinTTY = true
		env.Stdin = strings.NewReader("n\n")
		_, errb, _ := runCLI(t, env, "--quiet", "run", "fresh-resume", "run1",
			"--request-id", "11111111-2222-4333-8444-555555555555", "--selection-token", "tokOld")
		cut := strings.Index(errb, "Edits made after the saved evidence may be missing")
		prompt := strings.Index(errb, "[y/N]")
		if cut < 0 || prompt < 0 || cut > prompt {
			t.Errorf("replay: cutoff text must precede the prompt: %q", errb)
		}
		if len(fc.FreshResumeStartCalls) != 0 {
			t.Errorf("replay declined must not start: %+v", fc.FreshResumeStartCalls)
		}
	}
	// With --yes there is no prompt, so --quiet stays quiet.
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview()}
	_, errb, _ := runCLI(t, fakeEnv(fc), "--quiet", "run", "fresh-resume", "run1", "--yes")
	if strings.Contains(errb, "Edits made after") {
		t.Errorf("--quiet --yes printed the cutoff: %q", errb)
	}
}

func TestFreshResumeRequestIDWithTokenPostsDirectly(t *testing.T) {
	// The preview would say in_progress while the transfer is active; the direct post must not consult it.
	fc := &uzicli.FakeClient{FreshResumePreviewResult: apitypes.FreshResumePreviewDTO{Status: apitypes.FreshResumeStatusInProgress}}
	const id = "11111111-2222-4333-8444-555555555555"
	_, _, code := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes", "--request-id", id, "--selection-token", "tokOld")
	if code != uzicli.ExitOK || len(fc.FreshResumeStartCalls) != 1 {
		t.Fatalf("exit=%d calls=%+v", code, fc.FreshResumeStartCalls)
	}
	c := fc.FreshResumeStartCalls[0]
	if c.Request.RequestID != id || c.Request.SelectionToken != "tokOld" {
		t.Errorf("call = %+v", c)
	}
	_, _, code = runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes", "--selection-token", "tokOld")
	if code != uzicli.ExitUsage {
		t.Errorf("token without request id exit = %d", code)
	}
}

func TestFreshResumeTransportErrorNamesRequestIDAndToken(t *testing.T) {
	old := freshResumeSleep
	freshResumeSleep = func(context.Context, time.Duration) {}
	defer func() { freshResumeSleep = old }()
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview(),
		FreshResumeStartErr: uzicli.Exitf(uzicli.ExitUnreachable, "connection reset")}
	_, errb, _ := runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	id := fc.FreshResumeStartCalls[0].Request.RequestID
	if !strings.Contains(errb, "--request-id "+id+" --selection-token tok1") {
		t.Errorf("hint = %q", errb)
	}
}

func TestFreshResumeRetryBacksOffHonoringRetryAfter(t *testing.T) {
	var waits []time.Duration
	old := freshResumeSleep
	freshResumeSleep = func(_ context.Context, d time.Duration) { waits = append(waits, d) }
	defer func() { freshResumeSleep = old }()
	fc := &uzicli.FakeClient{FreshResumePreviewResult: availableFreshPreview(),
		FreshResumeStartErr: &uzicli.ExitError{Code: uzicli.ExitUnreachable, Err: errors.New("shed"), RetryAfter: 2 * time.Second}}
	runCLI(t, fakeEnv(fc), "run", "fresh-resume", "run1", "--yes")
	if len(waits) != freshResumeStartRetries || waits[0] != 2*time.Second || waits[1] != 2*time.Second {
		t.Errorf("waits = %v", waits)
	}
	if got := freshResumeBackoff(&uzicli.ExitError{RetryAfter: time.Hour}, 0); got != freshResumeMaxBackoff {
		t.Errorf("unbounded Retry-After: %v", got)
	}
	if got := freshResumeBackoff(nil, 0); got <= 0 || got > freshResumeMaxBackoff {
		t.Errorf("default backoff: %v", got)
	}
}

func TestFreshResumeCutoffMatchesServerNotes(t *testing.T) {
	src, err := os.ReadFile("../../internal/workersvc/fresh_resume_select.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range freshResumeCutoffText {
		if !strings.Contains(string(src), strconv.Quote(line)) {
			t.Errorf("server freshNotes lacks the CLI cutoff line %q", line)
		}
	}
}

func freshHeldRun(cause string) apitypes.RunDTO {
	return apitypes.RunDTO{ID: "run9", Kind: "issue", Status: statusRecoveryWait, RecoveryWaitCause: &cause, RecoveryRetryNotBeforePresent: true}
}

func TestFreshResumeClaimedCodexAndUnclaimedCodex(t *testing.T) {
	gen := func(n int64) *int64 { return &n }
	r := freshHeldRun(workerRequeueExhaustedCause)
	r.Harness = "codex"
	r.ClaimGeneration = gen(0)
	if !freshResumeEligible(r) {
		t.Errorf("an unclaimed codex run stays eligible")
	}
	r.ClaimGeneration = gen(1)
	if freshResumeEligible(r) {
		t.Errorf("a claimed codex run must be refused")
	}
}

func TestRecoveryDecisionLineHintsFreshResumeOnlyWhenHeld(t *testing.T) {
	held := freshHeldRun(retainedRecoveryExhaustedCause)
	if got := recoveryDecisionLine(held); !strings.Contains(got, "uzi run fresh-resume run9") {
		t.Errorf("held line lacks the hint: %q", got)
	}
}

func TestFreshResumeHintFollowsServerEligibility(t *testing.T) {
	const hint = "uzi run fresh-resume run9"
	for _, cause := range []string{workerRequeueExhaustedCause, retainedRecoveryExhaustedCause, retainedRecoveryAdoptionFailedCause, retainedContinuationUnwitnessedCause} {
		if got := workerExhaustionLine(freshHeldRun(cause)); !strings.Contains(got, hint) {
			t.Errorf("%s: eligible hold lacks the hint: %q", cause, got)
		}
	}
	str := func(s string) *string { return &s }
	stamp := time.Now().Add(time.Hour)
	for name, r := range map[string]apitypes.RunDTO{
		"codex_idle_stall": freshHeldRun("codex_idle_stall"),
		"secret_remediation_active": func() apitypes.RunDTO {
			r := freshHeldRun(retainedRecoveryAdoptionFailedCause)
			r.RecoveryWaitDetail = str(recoveryDetailSecretRemediationActive)
			return r
		}(),
		"source_end_declined": func() apitypes.RunDTO {
			r := freshHeldRun(retainedRecoveryAdoptionFailedCause)
			r.RecoveryWaitDetail = str(recoveryDetailSourceEndDeclined)
			return r
		}(),
		"retry stamp": func() apitypes.RunDTO {
			r := freshHeldRun(retainedRecoveryExhaustedCause)
			r.RecoveryRetryNotBefore = &stamp
			return r
		}(),
		"requeue exhausted with retry stamp": func() apitypes.RunDTO {
			r := freshHeldRun(workerRequeueExhaustedCause)
			r.RecoveryRetryNotBefore = &stamp
			return r
		}(),
		"task kind": func() apitypes.RunDTO { r := freshHeldRun(workerRequeueExhaustedCause); r.Kind = "task"; return r }(),
		"claimed codex run": func() apitypes.RunDTO {
			r := freshHeldRun(workerRequeueExhaustedCause)
			one := int64(1)
			r.Harness, r.ClaimGeneration = "codex", &one
			return r
		}(),
		"codex_policy_hold": func() apitypes.RunDTO {
			r := freshHeldRun(workerRequeueExhaustedCause)
			r.HoldReason = str("codex_policy_hold")
			return r
		}(),
		"reserve pause": func() apitypes.RunDTO {
			r := freshHeldRun(workerRequeueExhaustedCause)
			r.PauseMode = str("reserve")
			return r
		}(),
		"retry key absent": func() apitypes.RunDTO {
			r := freshHeldRun(workerRequeueExhaustedCause)
			r.RecoveryRetryNotBeforePresent = false
			return r
		}(),
	} {
		if got := workerExhaustionLine(r); strings.Contains(got, hint) {
			t.Errorf("%s: refused hold must not carry the hint: %q", name, got)
		}
		if freshResumeEligible(r) {
			t.Errorf("%s: predicate must refuse", name)
		}
	}
}
