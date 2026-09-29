package handler

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestComposeFiledFindingGroupKeepsRosterAndMarker(t *testing.T) {
	op := uuid.New()
	parts := []groupDraftPart{
		{title: "first", location: "src/one.go", evidence: "first evidence"},
		{title: "second", location: "src/two.go", evidence: "second evidence"},
	}
	edited := strings.Repeat("é", workersvc.MaxIssueDescriptionBytes)
	body, ok := composeFiledFindingGroup(parts, &edited, op)
	if !ok || len(body) > workersvc.MaxIssueDescriptionBytes || !utf8.ValidString(body) {
		t.Fatalf("invalid bounded body: ok=%v bytes=%d", ok, len(body))
	}
	for _, part := range parts {
		want := issuedraft.SafeInlineCode(part.title) + " — " + issuedraft.SafeInlineCode(part.location)
		if !strings.Contains(body, want) {
			t.Fatalf("missing roster member %q", want)
		}
	}
	if !strings.HasSuffix(body, "<!-- uzi-finding-group-operation: "+op.String()+" -->") {
		t.Fatal("operation marker absent")
	}
	if issuedraft.SanitizeFiledBody(body) != body {
		t.Fatal("body is not sanitized")
	}
}

func TestComposeFiledFindingGroupStripsPreviewRoster(t *testing.T) {
	parts := []groupDraftPart{{title: "first", location: "src/one.go", evidence: "evidence"}}
	previewParts := []groupDraftPart{{title: "first", location: issuedraft.SafeInlineCode("src/one.go"), evidence: "evidence"}}
	_, preview := composeFindingGroupDraft(previewParts)
	body, ok := composeFiledFindingGroup(parts, &preview, uuid.New())
	if !ok || strings.Count(body, "## Findings") != 1 {
		t.Fatalf("preview roster duplicated: %q", body)
	}
	if !strings.Contains(body, "evidence") {
		t.Fatalf("preview evidence lost: %q", body)
	}
}

func TestComposeFiledFindingGroupStripsScrubbedPreviewRoster(t *testing.T) {
	parts := []groupDraftPart{{title: "first", location: "glpat-" + "12345678901234567890", evidence: "evidence"}}
	previewParts := []groupDraftPart{{title: "first", location: issuedraft.SafeInlineCode(parts[0].location), evidence: "evidence"}}
	_, preview := composeFindingGroupDraft(previewParts)
	body, ok := composeFiledFindingGroup(parts, &preview, uuid.New())
	if !ok || strings.Count(body, "## Findings") != 1 {
		t.Fatalf("scrubbed preview roster duplicated: %q", body)
	}
}

func TestComposeFiledFindingGroupStripsBidiEdit(t *testing.T) {
	parts := []groupDraftPart{{title: "first", location: "src/one.go"}}
	edited := "safe\u202etxt.exe"
	body, ok := composeFiledFindingGroup(parts, &edited, uuid.New())
	if !ok || strings.ContainsRune(body, '\u202e') {
		t.Fatalf("bidi edit survived: %q", body)
	}
}

func TestReleaseFindingGroupOrReportReturnsRecoveryID(t *testing.T) {
	op := uuid.New()
	w := httptest.NewRecorder()
	called := false
	if releaseFindingGroupOrReport(w, op, []string{"member"}, "pre_call", func() bool {
		called = true
		return false
	}) {
		t.Fatal("reported a refused release as successful")
	}
	if !called || w.Code != 202 || !strings.Contains(w.Body.String(), op.String()) || !strings.Contains(w.Body.String(), "member") {
		t.Fatalf("missing pending operation response: called=%v code=%d body=%s", called, w.Code, w.Body.String())
	}
}

func TestFindingGroupDeadlineHasPositiveFloor(t *testing.T) {
	now := time.Now()
	if got := findingGroupDeadline(now, -time.Minute, 3*time.Minute); got != now.Add(6*time.Minute) {
		t.Fatalf("deadline = %v", got)
	}
	if got := findingGroupDeadline(now, 0, 0); got != now.Add(2*time.Minute) {
		t.Fatalf("default deadline = %v", got)
	}
}

func TestParseFindingGroupBodyIDsDeduplicates(t *testing.T) {
	id := uuid.New()
	ids, ok := parseFindingGroupBodyIDs([]string{id.String(), id.String()})
	if !ok || len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids = %v, ok = %v", ids, ok)
	}
}
