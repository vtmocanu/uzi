package handler

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

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

// TestDecodeJobResultBytesCapsFindingsWhileStreaming pins the streaming findings cap: a body of
// far more than JobResultMaxFindings elements is refused, and the refusal does not materialise
// them (a plain struct decode of 1.4M empty findings costs ~190 MiB of heap).
func TestDecodeJobResultBytesCapsFindingsWhileStreaming(t *testing.T) {
	body := []byte(`{"claim_generation":1,"status":"completed","findings":[` +
		strings.Repeat("{},", (4<<20)/3) + `{}]}`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := decodeJobResultBytes(body)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, errJobResultTooManyFindings) {
		t.Fatalf("err = %v, want errJobResultTooManyFindings", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Fatalf("refusing the over-count body allocated %d bytes; findings are being materialised", grew)
	}

	atCap := []byte(`{"claim_generation":1,"status":"completed","findings":[` +
		strings.Repeat(`{"severity":"info","message_md":"x"},`, workersvc.JobResultMaxFindings-1) +
		`{"severity":"info","message_md":"x"}]}`)
	req, err := decodeJobResultBytes(atCap)
	if err != nil || len(req.Findings) != workersvc.JobResultMaxFindings || req.ClaimGeneration == nil {
		t.Fatalf("a body at the cap must decode: %d findings, err %v", len(req.Findings), err)
	}
	for name, b := range map[string]string{
		"unknown top-level field": `{"status":"completed","extra":1}`,
		"unknown finding field":   `{"status":"completed","findings":[{"severity":"info","message_md":"x","bogus":1}]}`,
		"trailing value":          `{"status":"completed"} {}`,
		"not an object":           `[]`,
	} {
		if _, err := decodeJobResultBytes([]byte(b)); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

// TestJobFindingURLStaysInsideDBCap pins that the url is capped AFTER the scrub: growth from the
// redactor and a multibyte rune straddling the boundary must not push it past octet_length <= 2048.
func TestJobFindingURLStaysInsideDBCap(t *testing.T) {
	secret := "glpat-" + "AbCdEfGhIj" + "0123456789"
	prefix := "https://e.com/"
	inputs := map[string]string{
		"multibyte at boundary": prefix + strings.Repeat("€", (workersvc.JobFindingURLMaxBytes-len(prefix))/3),
		"secret packed":         prefix + "?k=" + strings.Repeat(secret+"&", (workersvc.JobFindingURLMaxBytes-len(prefix)-3)/(len(secret)+1)),
		"multibyte and secret":  prefix + strings.Repeat("é"+secret, (workersvc.JobFindingURLMaxBytes-len(prefix))/(len(secret)+2)),
		// Each 7-byte slack token scrubs to the 10-byte "[redacted]" plus its "/": the scrubbed
		// text is 2046 bytes when the 3-byte euro sign starts at byte 2046 and ends at 2049, past
		// the cap, while the raw URL is well inside it.
		"scrub growth then straddling rune": prefix + strings.Repeat("xoxb-1/", 100) + strings.Repeat("a", 932) + "\u20ac",
	}
	for name, u := range inputs {
		if len(u) > workersvc.JobFindingURLMaxBytes {
			t.Fatalf("%s: fixture is %d bytes, over the pre-scrub cap", name, len(u))
		}
		fs, err := validateAndScrubJobFinding(workerJobFindingBody{Severity: "info", MessageMd: "x", URL: &u})
		if err != nil {
			continue // a refusal is fine; a 500 is not, and only an over-cap value can cause one
		}
		if fs.URL == nil || len(*fs.URL) > workersvc.JobFindingURLMaxBytes || !utf8.ValidString(*fs.URL) {
			t.Fatalf("%s: stored url is %d bytes (valid utf8 %v)", name, len(*fs.URL), utf8.ValidString(*fs.URL))
		}
	}
}
