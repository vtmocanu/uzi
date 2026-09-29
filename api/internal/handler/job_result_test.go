package handler

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func jp(s string) *string { return &s }
func ji(v int64) *int64   { return &v }

func jobBody(f ...workerJobFindingBody) workerJobResultRequest {
	return workerJobResultRequest{Status: "completed", ReportMd: "report", Findings: f}
}

// TestValidateAndScrubJobResult pins the pure ingest gate of the job-result route.
func TestValidateAndScrubJobResult(t *testing.T) {
	longMsg := strings.Repeat("m", workersvc.JobFindingMessageMaxBytes+1)
	tooManyFindings := make([]workerJobFindingBody, workersvc.JobResultMaxFindings+1)
	for i := range tooManyFindings {
		tooManyFindings[i] = workerJobFindingBody{Severity: "info", MessageMd: "x"}
	}
	bad := []struct {
		name string
		req  workerJobResultRequest
	}{
		{"empty status", workerJobResultRequest{}},
		{"status not a token", workerJobResultRequest{Status: "Not A Token"}},
		{"status too long", workerJobResultRequest{Status: strings.Repeat("a", 33)}},
		{"report over cap", workerJobResultRequest{Status: "completed", ReportMd: strings.Repeat("r", workersvc.JobResultReportMaxBytes+1)}},
		{"too many findings", workerJobResultRequest{Status: "completed", Findings: tooManyFindings}},
		{"bad severity", jobBody(workerJobFindingBody{Severity: "critical", MessageMd: "x"})},
		{"empty severity", jobBody(workerJobFindingBody{MessageMd: "x"})},
		{"empty message", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "  "})},
		{"message over cap", jobBody(workerJobFindingBody{Severity: "info", MessageMd: longMsg})},
		{"url and file", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("https://e.com"), File: jp("f")})},
		{"line without file", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", Line: ji(3)})},
		{"line with url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("https://e.com"), Line: ji(3)})},
		{"negative line", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", File: jp("f"), Line: ji(-1)})},
		{"huge line", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", File: jp("f"), Line: ji(1 << 40)})},
		{"javascript url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("javascript:alert(1)")})},
		{"data url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("data:text/html,x")})},
		{"file url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("file:///etc/passwd")})},
		{"scheme-relative url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("//e.com/x")})},
		{"relative url", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("/x")})},
		{"url without host", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("https://")})},
		{"url with credentials", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("https://u:p@e.com/")})},
		{"url over cap", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: jp("https://e.com/" + strings.Repeat("a", workersvc.JobFindingURLMaxBytes))})},
		{"file over cap", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", File: jp(strings.Repeat("a", workersvc.JobFindingFileMaxBytes+1))})},
		{"empty file", jobBody(workerJobFindingBody{Severity: "info", MessageMd: "x", File: jp("")})},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := validateAndScrubJobResult(tc.req); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}

	t.Run("accepts a full result", func(t *testing.T) {
		sub, err := validateAndScrubJobResult(jobBody(
			workerJobFindingBody{Severity: "info", MessageMd: "line one\nline two"},
			workerJobFindingBody{Severity: "warning", MessageMd: "w", URL: jp("http://e.com/a?b=1")},
			workerJobFindingBody{Severity: "error", MessageMd: "e", File: jp("a/b.go"), Line: ji(12)},
			workerJobFindingBody{Severity: "error", MessageMd: "e2", File: jp("a/b.go")},
		))
		if err != nil {
			t.Fatal(err)
		}
		if len(sub.Findings) != 4 || sub.Findings[0].MessageMD != "line one\nline two" {
			t.Fatalf("findings = %+v", sub.Findings)
		}
		if sub.Findings[2].Line == nil || *sub.Findings[2].Line != 12 || sub.Findings[3].Line != nil {
			t.Fatalf("lines = %v / %v", sub.Findings[2].Line, sub.Findings[3].Line)
		}
	})
	t.Run("a maximal report and message are accepted", func(t *testing.T) {
		sub, err := validateAndScrubJobResult(workerJobResultRequest{
			Status: "partial", ReportMd: strings.Repeat("r", workersvc.JobResultReportMaxBytes),
			Findings: []workerJobFindingBody{{Severity: "info", MessageMd: strings.Repeat("m", workersvc.JobFindingMessageMaxBytes)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(sub.ReportMD) > workersvc.JobResultReportMaxBytes || len(sub.Findings[0].MessageMD) > workersvc.JobFindingMessageMaxBytes {
			t.Fatalf("scrubbed sizes %d/%d exceed the caps", len(sub.ReportMD), len(sub.Findings[0].MessageMD))
		}
	})
	t.Run("multi-byte text stays inside the byte cap", func(t *testing.T) {
		sub, err := validateAndScrubJobResult(workerJobResultRequest{
			Status: "completed", ReportMd: strings.Repeat("€", workersvc.JobResultReportMaxBytes/3),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(sub.ReportMD) > workersvc.JobResultReportMaxBytes {
			t.Fatalf("report is %d bytes, over the cap", len(sub.ReportMD))
		}
	})
	t.Run("control characters are stripped and secrets scrubbed", func(t *testing.T) {
		// Assembled at runtime so no provider-token-shaped literal sits in the source.
		secret := "glpat-" + "AbCdEfGhIj" + "0123456789"
		sub, err := validateAndScrubJobResult(workerJobResultRequest{
			Status: "completed", ReportMd: "a\x1b[31mred\x00 " + secret,
			Findings: []workerJobFindingBody{{Severity: "info", MessageMd: "key " + secret, File: jp("f\x07.go")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{sub.ReportMD, sub.Findings[0].MessageMD} {
			if strings.Contains(s, secret) || strings.ContainsAny(s, "\x1b\x00") {
				t.Fatalf("not sanitized: %q", s)
			}
		}
		if f := *sub.Findings[0].File; strings.Contains(f, "\x07") {
			t.Fatalf("file not sanitized: %q", f)
		}
	})
}
