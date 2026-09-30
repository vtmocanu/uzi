package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// titleAcceptedByService reports whether the service's create validation accepts title. A zero
// Service is enough: validation runs first, and a product caller without a token id is refused
// with a "caller" field right after it, so a title that passes surfaces that error and a title
// that fails surfaces its own.
func titleAcceptedByService(t *testing.T, title string) bool {
	t.Helper()
	pid := uuid.New()
	_, err := (&workersvc.Service{}).CreateJobRun(context.Background(), workersvc.CreateJobParams{
		Caller:  workersvc.JobCaller{UserID: uuid.New(), ProductID: &pid},
		JobType: "research",
		Title:   title,
		Prompt:  "p",
	})
	var inv *workersvc.JobInvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("title %q: want a JobInvalidError, got %v", title, err)
	}
	return inv.Field != "title"
}

// TestDerivedJobTitleFitsServiceLimit is the regression for a create with no title whose first
// prompt line is short in runes but long in bytes: the derived title must still pass the
// service's byte limit, so an omitted title never fails an otherwise valid request.
func TestDerivedJobTitleFitsServiceLimit(t *testing.T) {
	for name, prompt := range map[string]string{
		"cjk":        strings.Repeat("漢", 100),
		"emoji":      strings.Repeat("😀", 100),
		"cjk second": "\n  \n" + strings.Repeat("漢字", 60) + "\nrest",
		"ascii":      strings.Repeat("a", 500),
		// A cut that lands right after a space leaves trailing whitespace, which the
		// service refuses; the derived title must trim it instead of falling back.
		"rune cut after space": strings.Repeat("a", 79) + " bcdef",
		"byte cut after space": strings.Repeat("😀", 49) + " " + strings.Repeat("😀", 5),
	} {
		t.Run(name, func(t *testing.T) {
			title := derivedJobTitle("research", prompt)
			if len(title) > v1JobTitleMaxBytes || utf8.RuneCountInString(title) > v1JobTitleFromPromptRune || !utf8.ValidString(title) {
				t.Fatalf("derived title has %d bytes, %d runes, valid=%v", len(title), utf8.RuneCountInString(title), utf8.ValidString(title))
			}
			if title == "" || title == "research job" {
				t.Fatalf("derived title fell back: %q", title)
			}
			if !titleAcceptedByService(t, title) {
				t.Fatalf("service refused derived title %q (%d bytes)", title, len(title))
			}
		})
	}
}

// TestDerivedJobTitleTrimsTrailingSpace pins the exact derived text when a cut lands after a space.
func TestDerivedJobTitleTrimsTrailingSpace(t *testing.T) {
	if got, want := derivedJobTitle("research", strings.Repeat("a", 79)+" bcdef"), strings.Repeat("a", 79); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := derivedJobTitle("research", strings.Repeat("😀", 49)+" "+strings.Repeat("😀", 5)); strings.HasSuffix(got, " ") || got == "research job" {
		t.Fatalf("got %q", got)
	}
}

// TestV1JobTitleMaxBytesMatchesService pins the local constant to the service's limit.
func TestV1JobTitleMaxBytesMatchesService(t *testing.T) {
	if !titleAcceptedByService(t, strings.Repeat("a", v1JobTitleMaxBytes)) {
		t.Fatalf("service refuses a title of exactly %d bytes", v1JobTitleMaxBytes)
	}
	if titleAcceptedByService(t, strings.Repeat("a", v1JobTitleMaxBytes+1)) {
		t.Fatalf("service accepts a title of %d bytes: raise v1JobTitleMaxBytes", v1JobTitleMaxBytes+1)
	}
}
