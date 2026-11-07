package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const policyEpisode = "0f8fad5b-d9cb-469f-a165-70867728950e"

// policyResumeServer records the resume-now request body and answers with status/reply.
func policyResumeServer(t *testing.T, status int, reply any, got *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/runs/r1/resume-now" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		*got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPolicyResumePostsExactBody(t *testing.T) {
	var body string
	srv := policyResumeServer(t, 200, map[string]any{"run": apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "queued"}}, &body)
	_, errOut, code := runCLI(t, httpEnv(srv), "run", "resume-now", "r1",
		"--policy-mode", "fresh_one_claim", "--episode", policyEpisode, "--guidance", "go on", "--yes")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := `{"mode":"fresh_one_claim","expected_episode_id":"` + policyEpisode + `","guidance":"go on"}`
	if strings.TrimSpace(body) != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	for _, s := range []string{apitypes.PolicyResumeModeLabel("fresh_one_claim"), apitypes.PolicyResumeModeConsequence("fresh_one_claim")} {
		if !strings.Contains(errOut, s) {
			t.Errorf("stderr missing %q: %s", s, errOut)
		}
	}
}

func TestPolicyResumeGuidanceFileAndEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.txt")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var body string
	srv := policyResumeServer(t, 200, map[string]any{"run": apitypes.RunDTO{ID: "r1", Status: "queued"}}, &body)
	if _, e, code := runCLI(t, httpEnv(srv), "run", "resume-now", "r1", "--policy-mode", "preserve",
		"--episode", policyEpisode, "--guidance-file", path, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if !strings.Contains(body, `"guidance":"from file"`) {
		t.Errorf("body = %s", body)
	}
	if _, e, code := runCLI(t, httpEnv(srv), "run", "resume-now", "r1", "--policy-mode", "preserve",
		"--episode", policyEpisode, "--yes"); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if !strings.Contains(body, `"guidance":""`) {
		t.Errorf("empty guidance must still be sent: %s", body)
	}
}

func TestPolicyResumeReplayOutput(t *testing.T) {
	var body string
	srv := policyResumeServer(t, 200, map[string]any{"run": apitypes.RunDTO{ID: "r1", Status: "queued"}, "replay": true}, &body)
	out, e, code := runCLI(t, httpEnv(srv), "run", "resume-now", "r1", "--policy-mode", "preserve", "--episode", policyEpisode, "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if !strings.Contains(out, "already resumed (this request was applied earlier)") {
		t.Errorf("replay line missing: %s", out)
	}
}

func TestPolicyResume409Passthrough(t *testing.T) {
	for _, msg := range []string{
		"already resumed with different guidance; the new guidance was not recorded",
		"policy hold changed or does not match this mode; refresh the run and choose again",
	} {
		var body string
		srv := policyResumeServer(t, 409, map[string]string{"error": msg}, &body)
		_, e, code := runCLI(t, httpEnv(srv), "run", "resume-now", "r1", "--policy-mode", "preserve", "--episode", policyEpisode, "--yes")
		if code != uzicli.ExitConflict || !strings.Contains(e, msg) {
			t.Errorf("exit %d stderr %q, want conflict carrying %q", code, e, msg)
		}
	}
}

func TestPolicyResumeValidation(t *testing.T) {
	big := strings.Repeat("x", 16001)
	path := filepath.Join(t.TempDir(), "g.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown mode", []string{"--policy-mode", "bogus", "--episode", policyEpisode, "--yes"}, "preserve, fresh_from_saved_work, fresh_one_claim"},
		{"missing episode", []string{"--policy-mode", "preserve", "--yes"}, "--episode is required"},
		{"bad episode", []string{"--policy-mode", "preserve", "--episode", "nope", "--yes"}, "must be a UUID"},
		{"uppercase episode", []string{"--policy-mode", "preserve", "--episode", strings.ToUpper(policyEpisode), "--yes"}, "must be a UUID"},
		{"episode without mode", []string{"--episode", policyEpisode}, "only with --policy-mode"},
		{"both guidance", []string{"--policy-mode", "preserve", "--episode", policyEpisode, "--guidance", "a", "--guidance-file", path, "--yes"}, "mutually exclusive"},
		{"too long", []string{"--policy-mode", "preserve", "--episode", policyEpisode, "--guidance", big, "--yes"}, "limit is 16000 bytes"},
		{"nul", []string{"--policy-mode", "preserve", "--episode", policyEpisode, "--guidance", "a\x00b", "--yes"}, "NUL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, e, code := runCLI(t, fakeEnv(fc), append([]string{"run", "resume-now", "r1"}, tc.args...)...)
			if code != uzicli.ExitUsage || !strings.Contains(e, tc.want) {
				t.Errorf("exit %d stderr %q, want usage error containing %q", code, e, tc.want)
			}
			if fc.LastPolicyResume.Called || fc.LastResumeRunID != "" {
				t.Error("a rejected invocation must not reach the server")
			}
		})
	}
}

func TestPolicyResumeRefusesWithoutYesOrTTY(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, e, code := runCLI(t, fakeEnv(fc), "run", "resume-now", "r1", "--policy-mode", "preserve", "--episode", policyEpisode)
	if code != uzicli.ExitUsage || !strings.Contains(e, "pass --yes") {
		t.Errorf("exit %d stderr %q", code, e)
	}
	if fc.LastPolicyResume.Called {
		t.Error("refused invocation reached the server")
	}
}

func TestPolicyResumeTTYConfirmation(t *testing.T) {
	for _, tc := range []struct {
		answer string
		posted bool
	}{{"y\n", true}, {"\n", false}} {
		fc := &uzicli.FakeClient{ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued"}}
		env := fakeEnv(fc)
		env.StdinTTY = true
		env.Stdin = strings.NewReader(tc.answer)
		_, _, code := runCLI(t, env, "run", "resume-now", "r1", "--policy-mode", "fresh_from_saved_work", "--episode", policyEpisode)
		if code != 0 || fc.LastPolicyResume.Called != tc.posted {
			t.Errorf("answer %q: exit %d posted=%v, want posted=%v", tc.answer, code, fc.LastPolicyResume.Called, tc.posted)
		}
	}
}

func TestResumeNowWithoutPolicyFlagsStaysBodyless(t *testing.T) {
	fc := &uzicli.FakeClient{ResumedRun: apitypes.RunDTO{ID: "r1", Status: "queued", Kind: "issue"}}
	if _, e, code := runCLI(t, fakeEnv(fc), "run", "resume-now", "r1"); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if fc.LastResumeRunID != "r1" || fc.LastPolicyResume.Called {
		t.Errorf("bare resume-now must use ResumeRunNow only: %+v", fc.LastPolicyResume)
	}
}

func TestPolicyHoldRenderCauseModesAndPending(t *testing.T) {
	run := heldRun("r1", "codex_policy_hold", time.Now())
	run.PolicyHold = &apitypes.PolicyHoldDTO{
		OutcomeID: policyEpisode, HoldCause: sp("fresh_start_uncertain"), PendingFresh: true,
		ResumeModes: []string{"preserve", "fresh_from_saved_work"}, RecoveryRequired: true,
	}
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": run}}
	out, e, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{
		apitypes.PolicyHoldCauseText(sp("fresh_start_uncertain")),
		"preserve (" + apitypes.PolicyResumeModeLabel("preserve") + ")",
		"fresh_from_saved_work (" + apitypes.PolicyResumeModeLabel("fresh_from_saved_work") + ")",
		"one-claim fresh start pending",
		"--episode " + policyEpisode,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "uzi run stop") {
		t.Errorf("claims a Stop action: %s", out)
	}
}
