package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The failure-snapshot log scrubber (PRD #6) must strip every known token SHAPE a
// teammate's CI log might print, before the tail is frozen onto the run — the forge
// driver's error-only, connection-PAT-only redactor does not cover a success body.
func TestScrubKnownTokensRedactsTokenFamilies(t *testing.T) {
	const secret = "abcdef0123456789ABCDEFghij" //gitleaks:allow // fake token body: the scrubber must strip it, never a real secret
	cases := map[string]string{
		"glpat":         "leaked glpat-" + secret + " here",
		"gloas":         "gloas-" + secret,
		"glrt":          "glrt-" + secret,
		"glcbt":         "glcbt-" + secret,
		"glptt":         "glptt-" + secret,
		"glsoat":        "glsoat-" + secret,
		"gldt":          "gldt-" + secret,
		"sk-ant":        "ANTHROPIC_API_KEY=sk-ant-" + secret,
		"private-token": "PRIVATE-TOKEN: " + secret,
		"authorization": "Authorization: Bearer " + secret,
		"bare-bearer":   "sent Bearer " + secret + " upstream",
		// uzi's own Bearer credentials (PRD #64 Risk 14): the CLI PRD tells users to put
		// UZI_TOKEN in a GitLab CI variable, so a `uzi ...` invocation echoing its token
		// into a trace is exactly the path this snapshot ingests. uzc_ (user), uza_
		// (admin_ro) and uzw_ (worker) must all be stripped before the tail is frozen.
		"uzc": "UZI_TOKEN=uzc_" + secret + " in the env",
		"uza": "ran with uza_" + secret,
		"uzw": "worker joined as uzw_" + secret,
	}
	for name, in := range cases {
		out := ScrubKnownTokens(in)
		if strings.Contains(out, secret) {
			t.Errorf("%s: token body survived the scrub: %q", name, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s: expected a [REDACTED] placeholder, got %q", name, out)
		}
	}

	// A header line's whole value is redacted (not just the first word after the
	// colon), so "Authorization: Bearer <token>" never leaks the token tail.
	if strings.Contains(ScrubKnownTokens("Authorization: Bearer "+secret), secret) {
		t.Error("the Authorization header value must be redacted to end-of-line")
	}

	// Benign log text is untouched (no false positives on ordinary failure output).
	benign := "=== RUN TestFoo\n--- FAIL: TestFoo (nil guard removed)\nexit status 1\n"
	if got := ScrubKnownTokens(benign); got != benign {
		t.Errorf("benign log must be untouched, got %q", got)
	}
}

// fixSnapForge overrides only the two pipeline methods BuildFailureSnapshot calls;
// the rest of forge.Forge is the embedded (nil) interface — unreached in these tests.
type fixSnapForge struct {
	forge.Forge
	jobs []forge.Job
	log  string
}

func (f *fixSnapForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return f.jobs, nil
}
func (f *fixSnapForge) JobLogTail(context.Context, int64, int64, int) (string, error) {
	return f.log, nil
}

// TestBuildFailureSnapshotIncludesForgejoFailureJobs pins the failure filter: a
// Forgejo Actions job reports "failure", not GitLab's "failed", so a bare
// == "failed" filter dropped every failed Forgejo job and the fix agent got no
// failure context. The failed job must be snapshotted; a passing job must not.
func TestBuildFailureSnapshotIncludesForgejoFailureJobs(t *testing.T) {
	f := &fixSnapForge{
		jobs: []forge.Job{
			{ID: 1, Name: "build", Status: "failure"}, // Forgejo Actions failure
			{ID: 2, Name: "test", Status: "success"},  // a passing job must be excluded
		},
		log: "boom at line 5\nFAIL",
	}

	snap, err := BuildFailureSnapshot(context.Background(), f, 7, store.PipelineStatus{PipelineID: 4300}, 10, 4096)
	if err != nil {
		t.Fatalf("BuildFailureSnapshot: %v", err)
	}
	if len(snap.FailedJobs) != 1 {
		t.Fatalf("expected exactly the one Forgejo 'failure' job, got %d: %+v", len(snap.FailedJobs), snap.FailedJobs)
	}
	if snap.FailedJobs[0].Name != "build" {
		t.Fatalf("the 'failure' job must be snapshotted and the 'success' job excluded, got %+v", snap.FailedJobs)
	}
	if snap.FailedJobs[0].LogTail == "" {
		t.Error("the failed job's log tail must be captured")
	}
}

// TestFailureSignatureStableAcrossVolatileTokens pins PRD #71 design (b): the
// signature is the SAME for two runs of the identical failure whose logs differ only
// in volatile tokens (timestamps, durations, line numbers, hex addresses, runner
// paths), and DISTINCT for a different failing job set.
func TestFailureSignatureStableAcrossVolatileTokens(t *testing.T) {
	runA := FailureSnapshot{
		PipelineID: 4200, Ref: "main", SHA: "aaa",
		FailedJobs: []SnapshotJob{{
			Name: "unit", Stage: "test",
			LogTail: "2026-08-10T12:34:56Z starting job at /builds/team/proj/main\n" +
				"\x1b[31m--- FAIL: TestFoo (1.23s)\x1b[0m\n" +
				"panic: nil pointer at 0xdeadbeef line 4211\n" +
				"exit status 1\n",
		}},
	}
	// Same failure, later rerun: fresh timestamp, different duration, different line
	// number, different address, different runner path suffix.
	runB := FailureSnapshot{
		PipelineID: 4299, Ref: "main", SHA: "bbb",
		FailedJobs: []SnapshotJob{{
			Name: "unit", Stage: "test",
			LogTail: "2026-08-11T09:00:01Z starting job at /builds/team/proj/9f2c\n" +
				"\x1b[31m--- FAIL: TestFoo (0.88s)\x1b[0m\n" +
				"panic: nil pointer at 0xc0ffee42 line 5309\n" +
				"exit status 1\n",
		}},
	}
	sigA, sigB := FailureSignature(runA), FailureSignature(runB)
	if sigA != sigB {
		t.Fatalf("signature must be stable across volatile-token variation:\n a=%s\n b=%s", sigA, sigB)
	}
	if len(sigA) != 64 {
		t.Fatalf("expected a 64-hex-char SHA-256, got %d chars: %q", len(sigA), sigA)
	}

	// A genuinely different failing job set must produce a different signature.
	runC := FailureSnapshot{
		PipelineID: 4300, Ref: "main", SHA: "ccc",
		FailedJobs: []SnapshotJob{{
			Name: "lint", Stage: "check",
			LogTail: "golangci-lint: undefined: Foo\nexit status 1\n",
		}},
	}
	if FailureSignature(runC) == sigA {
		t.Fatal("a different failing job set must yield a distinct signature")
	}
}

// TestMergeCIConfigPaths pins the guard's watch-set union: defaults first, the
// project ci_config_path appended only when non-empty and new, empties dropped, and
// stable de-duplication.
func TestMergeCIConfigPaths(t *testing.T) {
	defaults := []string{".gitlab-ci.yml", ".gitlab/**", "", ".gitlab-ci.yml"}

	got := MergeCIConfigPaths(defaults, "ci/pipeline.yml")
	want := []string{".gitlab-ci.yml", ".gitlab/**", "ci/pipeline.yml"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("union mismatch: got %v want %v", got, want)
	}

	// An empty project path degrades to just the de-duplicated defaults.
	got = MergeCIConfigPaths(defaults, "")
	want = []string{".gitlab-ci.yml", ".gitlab/**"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("empty projectPath mismatch: got %v want %v", got, want)
	}

	// A project path already present as a default is not duplicated.
	got = MergeCIConfigPaths(defaults, ".gitlab-ci.yml")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("duplicate projectPath must not be re-added: got %v", got)
	}
}
