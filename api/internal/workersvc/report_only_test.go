package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

func boolp(b bool) *bool { return &b }

func TestClampWireReportOnly(t *testing.T) {
	if clampWireReportOnly(issueRun(), nil) {
		t.Error("nil declaration must be false")
	}
	if clampWireReportOnly(issueRun(), boolp(false)) {
		t.Error("&false must be false")
	}
	if !clampWireReportOnly(issueRun(), boolp(true)) {
		t.Error("&true on an issue run must be true")
	}
	// Kind gate: a &true on a non-issue run drops to false without panicking. runs.kind
	// is NOT NULL; the worker schema-gates too, but the server does not take its word.
	for _, kind := range []string{"ci_fix", "self_improve", "judge", "chat"} {
		run := store.Run{ID: uuid.New(), Kind: kind}
		if clampWireReportOnly(run, boolp(true)) {
			t.Errorf("kind %q: &true must still drop to false", kind)
		}
	}
}

func TestClampWireReportMdNilYieldsInvalid(t *testing.T) {
	if got := clampWireReportMd(issueRun(), nil, true); got.Valid {
		t.Errorf("nil must land NULL, got %q", got.String)
	}
}

func TestClampWireReportMdIssuePlainTextStored(t *testing.T) {
	const s = "All checks passed. No code change required."
	got := clampWireReportMd(issueRun(), strp(s), true)
	if !got.Valid || got.String != s {
		t.Errorf("plain text: got valid=%v %q, want valid=true %q", got.Valid, got.String, s)
	}
}

// report_md is stored ONLY on a report-only completion: with reportOnly=false it is
// dropped even on an issue run with text present, keeping the column's invariant
// (non-NULL only on a report_only run) true against an untrusted worker.
func TestClampWireReportMdDroppedWhenNotReportOnly(t *testing.T) {
	if got := clampWireReportMd(issueRun(), strp("findings"), false); got.Valid {
		t.Errorf("reportOnly=false must drop report_md, got %q", got.String)
	}
}

// A non-issue run can never reach reportOnly=true (clampWireReportOnly gates on kind),
// so report_md is dropped end-to-end for every non-issue kind — the composed behavior.
func TestClampWireReportMdDropsNonIssueKind(t *testing.T) {
	for _, kind := range []string{"ci_fix", "self_improve", "judge", "chat"} {
		run := store.Run{ID: uuid.New(), Kind: kind}
		reportOnly := clampWireReportOnly(run, boolp(true))
		if got := clampWireReportMd(run, strp("findings"), reportOnly); got.Valid {
			t.Errorf("kind %q: report_md must be dropped, got %q", kind, got.String)
		}
	}
}

func TestClampWireReportMdStripsControlAndFormatChars(t *testing.T) {
	// U+0007 BELL (Cc) and U+202E RIGHT-TO-LEFT OVERRIDE (Cf) must both be stripped;
	// \n and \t must survive so multi-line markdown is preserved.
	in := "line1\n\tline2\x07\u202emiddle"
	got := clampWireReportMd(issueRun(), strp(in), true)
	if !got.Valid {
		t.Fatalf("expected stored text, got NULL")
	}
	if strings.ContainsRune(got.String, '\x07') || strings.ContainsRune(got.String, '\u202e') {
		t.Errorf("control/format char survived: %q", got.String)
	}
	if !strings.Contains(got.String, "\n\tline2") {
		t.Errorf("newline/tab were not preserved: %q", got.String)
	}
	if !strings.Contains(got.String, "middle") {
		t.Errorf("expected surrounding text intact: %q", got.String)
	}
}

func TestClampWireReportMdScrubsSecrets(t *testing.T) {
	// Fabricated secret-shaped strings, never a real credential.
	for _, secret := range []string{
		"glpat-ABCDEFGHIJKLMNOPQRST", //gitleaks:allow fabricated GitLab PAT fixture — asserts report_md secret scrubbing (#279), never a real credential
		"sk-ant-abcdef0123456789ABCDEF",
	} {
		in := "the token was " + secret + " during the run"
		got := clampWireReportMd(issueRun(), strp(in), true)
		if !got.Valid {
			t.Fatalf("expected stored text, got NULL")
		}
		if strings.Contains(got.String, secret) {
			t.Errorf("secret survived scrub: %q", got.String)
		}
		if !strings.Contains(got.String, "[redacted]") {
			t.Errorf("expected redaction marker, got %q", got.String)
		}
	}
}

func TestClampWireReportMdBoundsToCap(t *testing.T) {
	in := strings.Repeat("a", ReviewSummaryMaxBytes+4096)
	got := clampWireReportMd(issueRun(), strp(in), true)
	if !got.Valid {
		t.Fatalf("expected stored text, got NULL")
	}
	if len(got.String) > ReviewSummaryMaxBytes {
		t.Errorf("stored %d bytes, want <= %d", len(got.String), ReviewSummaryMaxBytes)
	}
}

// A completed report from a run that produced a normal MR carries neither field: the
// worker sends no key rather than "" or false, so both decode nil and clamp to the
// no-report-only shape (report_only=false, report_md NULL), byte-identical to before.
func TestClampWireReportDefaultsForNormalCompletion(t *testing.T) {
	run := issueRun()
	if clampWireReportOnly(run, nil) {
		t.Error("normal completion must be report_only=false")
	}
	if got := clampWireReportMd(run, nil, false); got.Valid {
		t.Errorf("normal completion must be report_md NULL, got %q", got.String)
	}
}
