package schedtmpl

import (
	"strings"
	"testing"
)

// White-box tests of parse's PRD #929 M1 `output:` frontmatter key. parse is unexported
// (the catalog is embedded, never user input) and panics at init on error, so its
// error/normalization paths for output mode can only be exercised in-package.

// mustFrontmatterOutput builds a minimal catalog file for the given target with an optional
// `output:` frontmatter key, so the output-mode paths can be exercised across targets.
func mustFrontmatterOutput(target, output, labels, body string) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("slug: t\n")
	b.WriteString("name: T\n")
	b.WriteString("description: A test job.\n")
	b.WriteString("target: " + target + "\n")
	b.WriteString("cron: 0 2 * * *\n")
	if output != "" {
		b.WriteString("output: " + output + "\n")
	}
	if labels != "" {
		b.WriteString("labels: " + labels + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	b.WriteString("\n")
	return []byte(b.String())
}

func TestParsePromptOutputIssues(t *testing.T) {
	j, err := parse(mustFrontmatterOutput("prompt", OutputModeIssues, "", "do the thing"))
	if err != nil {
		t.Fatalf("output: issues on a prompt must parse: %v", err)
	}
	if j.Output != OutputModeIssues {
		t.Fatalf("Output = %q, want %q", j.Output, OutputModeIssues)
	}
	if got := j.OutputMode(); got != OutputModeIssues {
		t.Fatalf("OutputMode() = %q, want %q", got, OutputModeIssues)
	}
}

func TestParsePromptOutputMR(t *testing.T) {
	j, err := parse(mustFrontmatterOutput("prompt", OutputModeMR, "", "do the thing"))
	if err != nil {
		t.Fatalf("output: mr on a prompt must parse: %v", err)
	}
	if j.Output != OutputModeMR {
		t.Fatalf("Output = %q, want %q", j.Output, OutputModeMR)
	}
	if got := j.OutputMode(); got != OutputModeMR {
		t.Fatalf("OutputMode() = %q, want %q", got, OutputModeMR)
	}
}

func TestParsePromptOutputOmittedDefaultsToMR(t *testing.T) {
	j, err := parse(mustFrontmatterOutput("prompt", "", "", "do the thing"))
	if err != nil {
		t.Fatalf("an omitted output on a prompt must parse: %v", err)
	}
	if j.Output != "" {
		t.Fatalf("Output = %q, want empty (unset)", j.Output)
	}
	// The helper resolves an unset output to the default, never leaving callers with "".
	if got := j.OutputMode(); got != OutputModeMR {
		t.Fatalf("OutputMode() = %q, want %q (the default)", got, OutputModeMR)
	}
	if DefaultOutputMode != OutputModeMR {
		t.Fatalf("DefaultOutputMode = %q, want %q", DefaultOutputMode, OutputModeMR)
	}
}

func TestParsePromptOutputInvalidRejected(t *testing.T) {
	_, err := parse(mustFrontmatterOutput("prompt", "bogus", "", "do the thing"))
	if err == nil {
		t.Fatal("an unknown output value must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown output") {
		t.Fatalf("error = %v, want an 'unknown output' message", err)
	}
}

func TestParseSweepOutputRejected(t *testing.T) {
	_, err := parse(mustFrontmatterOutput("sweep", OutputModeIssues, "bug", "guidance"))
	if err == nil {
		t.Fatal("output set on a sweep target must be rejected (prompt-only)")
	}
	if !strings.Contains(err.Error(), "must not set output") {
		t.Fatalf("error = %v, want a 'must not set output' message", err)
	}
}

func TestParseSelfImproveOutputRejected(t *testing.T) {
	_, err := parse(mustFrontmatterOutput("self_improve", OutputModeMR, "", ""))
	if err == nil {
		t.Fatal("output set on a self_improve target must be rejected (prompt-only)")
	}
	if !strings.Contains(err.Error(), "must not set output") {
		t.Fatalf("error = %v, want a 'must not set output' message", err)
	}
}
