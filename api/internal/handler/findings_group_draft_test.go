package handler

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/issuedraft"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestParseFindingGroupIDs(t *testing.T) {
	first, second := uuid.New(), uuid.New()
	ids, ok := parseFindingGroupIDs(first.String() + "," + second.String() + "," + first.String())
	if !ok || len(ids) != 2 || ids[0] != first || ids[1] != second {
		t.Fatalf("dedupe: %v %v", ids, ok)
	}
	for _, raw := range []string{"", first.String() + ",", uuid.Nil.String(), "invalid"} {
		if _, ok := parseFindingGroupIDs(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = uuid.NewString()
	}
	if _, ok := parseFindingGroupIDs(strings.Join(many, ",")); ok {
		t.Error("accepted 51 ids")
	}
	if _, ok := parseFindingGroupIDs(strings.Repeat(first.String()+",", 20000) + first.String()); ok {
		t.Error("accepted oversized duplicate input")
	}
}

func TestComposeFindingGroupDraftKeepsEveryMember(t *testing.T) {
	parts := make([]groupDraftPart, 50)
	for i := range parts {
		parts[i] = groupDraftPart{title: "Title " + strconv.Itoa(i), location: issuedraft.SafeInlineCode("src/file" + strconv.Itoa(i)), evidence: strings.Repeat("é", workersvc.MaxIssueDescriptionBytes)}
	}
	title, body := composeFindingGroupDraft(parts)
	if !strings.Contains(title, "Findings (50)") {
		t.Fatalf("title %q", title)
	}
	if len(body) > workersvc.MaxIssueDescriptionBytes || !utf8.ValidString(body) {
		t.Fatalf("invalid bounded body: %d bytes", len(body))
	}
	for i, p := range parts {
		if !strings.Contains(body, issuedraft.SafeInlineCode(p.title)+" — "+p.location) {
			t.Errorf("missing member %d", i)
		}
	}
	if issuedraft.SanitizeFiledBody(body) != body {
		t.Error("final body is not sanitized")
	}
}

func TestComposeFindingGroupDraftInertMemberTitle(t *testing.T) {
	payload := "![x](https://example.com/pixel)"
	_, body := composeFindingGroupDraft([]groupDraftPart{{
		title: payload, location: issuedraft.SafeInlineCode("src/a.go"), evidence: "plain evidence",
	}})
	if strings.Contains(body, "1. "+payload+" — ") {
		t.Fatalf("active Markdown in member roster: %q", body)
	}
	if !strings.Contains(body, issuedraft.SafeInlineCode(payload)) {
		t.Fatalf("missing inert title: %q", body)
	}
}
