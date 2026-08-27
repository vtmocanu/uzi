package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// The docs verbs read the corpus embedded in the binary, so these tests need no
// fake client wiring beyond fakeEnv's default (they never call env.client).

func TestDocsListJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []struct {
		Slug     string `json:"slug"`
		Title    string `json:"title"`
		Audience string `json:"audience"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	if len(items) == 0 {
		t.Fatalf("docs list --json returned no docs")
	}
	sawGettingStarted := false
	for _, it := range items {
		if it.Slug == "" || it.Title == "" || it.Audience == "" {
			t.Errorf("element missing a required field: %+v", it)
		}
		if it.Slug == "README" {
			t.Errorf("README must never be listed:\n%s", out)
		}
		// Default (no --audience) is user-only.
		if it.Audience != "user" {
			t.Errorf("default list must be audience=user only, got %q for %q", it.Audience, it.Slug)
		}
		if it.Slug == "getting-started" {
			sawGettingStarted = true
		}
	}
	if !sawGettingStarted {
		t.Errorf("docs list did not include getting-started:\n%s", out)
	}
}

func TestDocsListAudienceAllJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "list", "--audience", "all", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []struct {
		Slug     string `json:"slug"`
		Audience string `json:"audience"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	sawNonUser := false
	for _, it := range items {
		if it.Slug == "README" {
			t.Errorf("README must never be listed even under --audience all:\n%s", out)
		}
		if it.Audience != "user" {
			sawNonUser = true
		}
	}
	if !sawNonUser {
		t.Errorf("--audience all must include non-user audiences, saw only user:\n%s", out)
	}
}

func TestDocsListInvalidAudienceExit2(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "list", "--audience", "bogus")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "audience") {
		t.Errorf("usage error should mention audience:\n%s", errOut)
	}
}

func TestDocsShowBodyOnly(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "show", "getting-started")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// A stable literal from the body (the H1 heading).
	if !strings.Contains(out, "# Getting started") {
		t.Errorf("docs show did not print the doc body:\n%s", out)
	}
	// The frontmatter fence and its keys must be stripped — body only.
	for _, fm := range []string{"title: Getting started", "audience: user", "order: 10"} {
		if strings.Contains(out, fm) {
			t.Errorf("frontmatter line %q leaked into the body output:\n%s", fm, out)
		}
	}
}

func TestDocsShowJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "show", "getting-started", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got struct {
		Slug string `json:"slug"`
		Meta struct {
			Title    string `json:"title"`
			Audience string `json:"audience"`
		} `json:"meta"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}
	if got.Slug != "getting-started" {
		t.Errorf("slug = %q, want getting-started", got.Slug)
	}
	if got.Meta.Title == "" || got.Body == "" {
		t.Errorf("meta/body missing: %+v", got)
	}
}

func TestDocsShowUnknownExit4WithSuggestion(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "show", "no-such-doc")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
	if !strings.Contains(errOut, "did you mean") {
		t.Errorf("unknown-slug error should carry a suggestion:\n%s", errOut)
	}
}

func TestDocsShowNoArgExit2(t *testing.T) {
	if _, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "show"); code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

// Success Criterion 1: "connect a forge" is a verbatim H2 in getting-started.md, so
// a substring search over the body returns getting-started.
func TestDocsSearchFindsGettingStarted(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "search", "connect a forge", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	found := false
	for _, it := range items {
		if it.Slug == "getting-started" {
			found = true
		}
	}
	if !found {
		t.Errorf("search %q did not return getting-started:\n%s", "connect a forge", out)
	}
}

func TestDocsSearchNoResultsEmptyArray(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "docs", "search", "zzzznotfoundquery", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	if len(items) != 0 {
		t.Errorf("a no-hit search must return an empty array, got %d:\n%s", len(items), out)
	}
}
