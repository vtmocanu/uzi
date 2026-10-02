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

func TestComposeFiledFindingGroupMatchesOrdinaryPreview(t *testing.T) {
	op := uuid.New()
	parts := []groupDraftPart{
		{title: "one `tick`", location: "src/a`b.go", evidence: "first finding evidence"},
		{title: "second", location: "src/two.go", evidence: "second finding evidence"},
	}
	previewParts := make([]groupDraftPart, len(parts))
	for i, p := range parts {
		previewParts[i] = groupDraftPart{title: p.title, location: issuedraft.SafeInlineCode(p.location), evidence: p.evidence}
	}
	title, preview := composeFindingGroupDraft(previewParts)
	if title != "Findings (2): one `tick`" {
		t.Errorf("unexpected two-member title: %q", title)
	}
	marker := "\n\n<!-- uzi-finding-group-operation: " + op.String() + " -->"
	body, ok := composeFiledFindingGroup(parts, nil, op)
	if !ok {
		t.Fatal("ordinary preview did not fit")
	}
	if body != preview+marker {
		t.Errorf("default filing differs from complete preview plus marker:\nfiled=%q\npreview=%q", body, preview+marker)
	}
	for _, evidence := range []string{"first finding evidence", "second finding evidence"} {
		if !strings.Contains(body, evidence) {
			t.Errorf("missing member evidence %q", evidence)
		}
	}
	for _, p := range parts {
		line := issuedraft.SafeInlineCode(p.title) + " — " + issuedraft.SafeInlineCode(p.location)
		if !strings.Contains(body, line) {
			t.Errorf("missing transformed roster line %q", line)
		}
	}
}

func TestComposeFiledFindingGroupSanitizesRosterAndPreservesMemberEvidence(t *testing.T) {
	secretLocation := "src/glpat-" + "12345678901234567890.go"
	parts := []groupDraftPart{
		{title: "![pixel](https://example.com/p)", location: "src/a`b.go", evidence: "evidence for image title"},
		{title: "secret coordinate", location: secretLocation, evidence: "evidence for secret coordinate"},
	}
	op := uuid.New()
	body, ok := composeFiledFindingGroup(parts, nil, op)
	if !ok {
		t.Fatal("sanitized fixture did not fit")
	}
	if strings.Contains(body, "1. ![pixel]") || strings.Contains(body, secretLocation) {
		t.Errorf("unsafe roster content survived: %q", body)
	}
	if !strings.Contains(body, issuedraft.SafeInlineCode(parts[0].title)) {
		t.Errorf("image-like title missing from inert roster: %q", body)
	}
	for _, evidence := range []string{"evidence for image title", "evidence for secret coordinate"} {
		if strings.Count(body, evidence) != 1 {
			t.Errorf("member evidence %q occurs %d times", evidence, strings.Count(body, evidence))
		}
	}
	if !strings.HasSuffix(body, "\n\n<!-- uzi-finding-group-operation: "+op.String()+" -->") {
		t.Error("operation marker missing")
	}
}

func TestComposeFiledFindingGroupMatchesUTF8PreviewPrefixAtMarkerBoundary(t *testing.T) {
	op := uuid.New()
	parts := []groupDraftPart{
		{title: "multibyte", location: "src/near.go", evidence: "x" + strings.Repeat("é", workersvc.MaxIssueDescriptionBytes)},
		{title: "second", location: "src/two.go", evidence: "second evidence"},
	}
	previewParts := []groupDraftPart{
		{title: parts[0].title, location: issuedraft.SafeInlineCode(parts[0].location), evidence: parts[0].evidence},
		{title: parts[1].title, location: issuedraft.SafeInlineCode(parts[1].location), evidence: parts[1].evidence},
	}
	_, preview := composeFindingGroupDraft(previewParts)
	marker := "\n\n<!-- uzi-finding-group-operation: " + op.String() + " -->"
	limit := workersvc.MaxIssueDescriptionBytes - len(marker)
	if len(preview) <= limit {
		t.Fatalf("fixture does not cross marker boundary: preview=%d limit=%d", len(preview), limit)
	}
	end := limit
	for end > 0 && !utf8.RuneStart(preview[end]) {
		end--
	}
	if end == limit {
		t.Fatal("fixture did not split a multibyte rune at the marker boundary")
	}
	want := preview[:end] + marker
	body, ok := composeFiledFindingGroup(parts, nil, op)
	if !ok || !utf8.ValidString(body) || len(body) > workersvc.MaxIssueDescriptionBytes {
		t.Fatalf("invalid filed body: ok=%v bytes=%d valid=%v", ok, len(body), utf8.ValidString(body))
	}
	if body != want {
		t.Errorf("default filing differs from UTF-8-safe preview prefix at marker boundary: filed bytes=%d want bytes=%d", len(body), len(want))
	}
}

func TestReleaseFindingGroupOrReportReturnsRecoveryID(t *testing.T) {
	op := uuid.New()
	w := httptest.NewRecorder()
	called := false
	if releaseFindingGroupOrReport(w, op, []string{"member"}, func() bool {
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
