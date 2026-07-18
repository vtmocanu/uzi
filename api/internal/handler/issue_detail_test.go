package handler

import (
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
)

func TestBuildIssueDetail(t *testing.T) {
	position := map[string]int{"In Progress": 0, "Human Review": 1, "Later": 2}

	t.Run("opened issue resolves its single column and carries description + author", func(t *testing.T) {
		dto := buildIssueDetail(forge.Issue{
			IID:         42,
			Title:       "ship it",
			State:       "opened",
			Labels:      []string{"PRD", "In Progress"},
			WebURL:      "https://gitlab.example.com/x/-/issues/42",
			Description: "Do the thing. See prds/12-board.md",
			Author:      "vlad",
		}, position, "forgejo")

		if dto.ForgeType != "forgejo" {
			t.Fatalf("forge_type should be carried onto the issue detail, got %q", dto.ForgeType)
		}

		if dto.IID != 42 || dto.Title != "ship it" || dto.State != "opened" {
			t.Fatalf("scalar fields wrong: %+v", dto)
		}
		if dto.Column != "In Progress" || dto.Closed || dto.Conflict {
			t.Fatalf("column resolution wrong: col=%q closed=%v conflict=%v", dto.Column, dto.Closed, dto.Conflict)
		}
		if !dto.HasPRDLink {
			t.Fatal("a description linking prds/*.md must set has_prd_link")
		}
		if dto.Description != "Do the thing. See prds/12-board.md" {
			t.Fatalf("description not carried verbatim: %q", dto.Description)
		}
		if dto.Author == nil || *dto.Author != "vlad" {
			t.Fatalf("author should be set to vlad, got %v", dto.Author)
		}
	})

	t.Run("closed issue goes to the implicit Closed column regardless of labels", func(t *testing.T) {
		dto := buildIssueDetail(forge.Issue{
			IID:    7,
			State:  "closed",
			Labels: []string{"In Progress"},
		}, position, "gitlab")
		if !dto.Closed || dto.Column != "" {
			t.Fatalf("closed issue should be Closed with empty column, got closed=%v col=%q", dto.Closed, dto.Column)
		}
	})

	t.Run("multiple column labels flag a conflict, highest position wins", func(t *testing.T) {
		dto := buildIssueDetail(forge.Issue{
			IID:    9,
			State:  "opened",
			Labels: []string{"In Progress", "Later"},
		}, position, "gitlab")
		if !dto.Conflict {
			t.Fatal("two column labels must flag conflict")
		}
		if dto.Column != "Later" {
			t.Fatalf("highest-positioned column should win, got %q", dto.Column)
		}
	})

	t.Run("no PRD link and no author leave the flags/pointer clear; nil labels normalize to empty", func(t *testing.T) {
		dto := buildIssueDetail(forge.Issue{
			IID:         1,
			State:       "opened",
			Description: "just a note, no link",
			Labels:      nil,
		}, position, "gitlab")
		if dto.HasPRDLink {
			t.Fatal("a description with no prds/*.md link must not set has_prd_link")
		}
		if dto.Author != nil {
			t.Fatalf("empty author must map to nil, got %v", *dto.Author)
		}
		if dto.Labels == nil {
			t.Fatal("nil labels must normalize to a non-nil empty slice (JSON [] not null)")
		}
		if len(dto.Labels) != 0 {
			t.Fatalf("expected empty labels, got %v", dto.Labels)
		}
		if dto.Column != "" || dto.Closed || dto.Conflict {
			t.Fatalf("an unlabeled open issue is Open, got col=%q closed=%v conflict=%v", dto.Column, dto.Closed, dto.Conflict)
		}
	})
}
