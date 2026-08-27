package schedtmpl_test

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/schedtmpl"
)

// catalogSlugs is the exact set of default scheduled jobs the product ships
// (PRD #589 M1, extended by PRD #590 M1 with self-improve). Keep it sorted —
// Catalog() returns its slice sorted by slug and TestCatalogSetIsExactlySeven
// compares index-for-index.
var catalogSlugs = []string{
	"bug-hunt", "bug-triage", "docs-hygiene", "feature-bingo", "planned-sweep", "self-improve", "test-improvement",
}

func TestCatalogSetIsExactlySeven(t *testing.T) {
	got := schedtmpl.Catalog()
	if len(got) != len(catalogSlugs) {
		t.Fatalf("got %d catalog entries, want %d", len(got), len(catalogSlugs))
	}
	for i, slug := range catalogSlugs {
		if got[i].Slug != slug {
			t.Errorf("catalog[%d] = %q, want %q (should be sorted by slug)", i, got[i].Slug, slug)
		}
	}
}

// TestCatalogParseAndValid asserts every embedded entry parses into a sane job:
// unique slug, non-empty name/description, a known target with the payload that
// target requires, a valid 5-field cron, a loadable timezone, and a valid model
// (empty allowed => inherit the owner default).
func TestCatalogParseAndValid(t *testing.T) {
	seen := make(map[string]bool)
	for _, j := range schedtmpl.Catalog() {
		if j.Slug == "" {
			t.Errorf("catalog entry has empty slug: %+v", j)
		}
		if seen[j.Slug] {
			t.Errorf("duplicate catalog slug %q", j.Slug)
		}
		seen[j.Slug] = true

		if strings.TrimSpace(j.Name) == "" {
			t.Errorf("catalog %q has empty name", j.Slug)
		}
		if strings.TrimSpace(j.Description) == "" {
			t.Errorf("catalog %q has empty description", j.Slug)
		}

		// Cron must be a valid 5-field standard expression. Reuse the repo's own
		// parser so this test cannot drift from what the scheduler accepts.
		if err := schedsvc.ValidateCron(j.Cron); err != nil {
			t.Errorf("catalog %q has invalid cron %q: %v", j.Slug, j.Cron, err)
		}

		// Timezone must load as an IANA location.
		if _, err := time.LoadLocation(j.Timezone); err != nil {
			t.Errorf("catalog %q has invalid timezone %q: %v", j.Slug, j.Timezone, err)
		}

		// Model against the shared, authoritative rule (empty => inherit).
		if _, err := agenttmpl.ValidateModel(j.Model); err != nil {
			t.Errorf("catalog %q has an invalid model %q: %v", j.Slug, j.Model, err)
		}

		switch j.Target {
		case "prompt":
			if strings.TrimSpace(j.Prompt) == "" {
				t.Errorf("catalog %q (prompt) has an empty prompt", j.Slug)
			}
			if len(j.Labels) != 0 {
				t.Errorf("catalog %q (prompt) must not carry labels, got %v", j.Slug, j.Labels)
			}
		case "sweep":
			if len(j.Labels) == 0 {
				t.Errorf("catalog %q (sweep) has no labels", j.Slug)
			}
			for _, l := range j.Labels {
				if l != strings.TrimSpace(l) || l == "" {
					t.Errorf("catalog %q (sweep) has a blank or padded label %q", j.Slug, l)
				}
			}
			if j.Prompt != "" {
				t.Errorf("catalog %q (sweep) must not carry a prompt, got %q", j.Slug, j.Prompt)
			}
			if j.MaxIssues < 0 {
				t.Errorf("catalog %q (sweep) has negative max_issues %d", j.Slug, j.MaxIssues)
			}
		case "self_improve":
			// Promptless and label-less (PRD #590 M1): the directive is worker-side and the
			// tracking issue is resolved at fire time, so a self_improve entry must carry
			// neither a prompt/guidance body nor labels.
			if strings.TrimSpace(j.Prompt) != "" {
				t.Errorf("catalog %q (self_improve) must not carry a prompt, got %q", j.Slug, j.Prompt)
			}
			if len(j.Labels) != 0 {
				t.Errorf("catalog %q (self_improve) must not carry labels, got %v", j.Slug, j.Labels)
			}
			if strings.TrimSpace(j.Guidance) != "" {
				t.Errorf("catalog %q (self_improve) must not carry guidance, got %q", j.Slug, j.Guidance)
			}
		default:
			t.Errorf("catalog %q has unknown target %q", j.Slug, j.Target)
		}
	}
}

// TestCatalogTimezoneDefaultsToUTC pins that an entry that omits the timezone key
// still parses with a concrete timezone (UTC), never an empty string that
// time.LoadLocation would reject.
func TestCatalogTimezoneDefaultsToUTC(t *testing.T) {
	for _, j := range schedtmpl.Catalog() {
		if j.Timezone == "" {
			t.Errorf("catalog %q has an empty timezone; it should default to %q", j.Slug, schedtmpl.DefaultTimezone)
		}
	}
}

// TestBySlug covers the accessor and its defensive copy: a known slug resolves,
// an unknown one does not, and mutating a returned job's Labels does not leak
// back into the catalog.
func TestBySlug(t *testing.T) {
	if _, ok := schedtmpl.BySlug("does-not-exist"); ok {
		t.Error("BySlug returned ok for a slug that does not exist")
	}
	j, ok := schedtmpl.BySlug("bug-triage")
	if !ok {
		t.Fatal("BySlug(bug-triage) not found")
	}
	if len(j.Labels) == 0 {
		t.Fatal("bug-triage should carry at least one label")
	}
	j.Labels[0] = "mutated"
	again, _ := schedtmpl.BySlug("bug-triage")
	if again.Labels[0] == "mutated" {
		t.Error("mutating a returned job's Labels leaked back into the catalog")
	}
}
