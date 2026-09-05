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
// (PRD #589 M1, extended by PRD #590 M1 with self-improve, PRD #767 M4 with
// assigned-sweep, and issue #928 with refactor-scout). Keep it sorted — Catalog()
// returns its slice sorted by slug and TestCatalogSetIsExactlyNine compares
// index-for-index.
var catalogSlugs = []string{
	"assigned-sweep", "bug-hunt", "bug-triage", "docs-hygiene", "feature-bingo", "planned-sweep", "refactor-scout", "self-improve", "test-improvement",
}

func TestCatalogSetIsExactlyNine(t *testing.T) {
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
			// PRD #767 M4: a sweep is label-selected XOR assignment-selected. A label
			// sweep must carry labels; an assigned sweep must carry NONE (it selects by
			// the uzi-bot assignee at fire time).
			switch j.SelectorKind {
			case schedtmpl.SelectorAssigned:
				if len(j.Labels) != 0 {
					t.Errorf("catalog %q (assigned sweep) must not carry labels, got %v", j.Slug, j.Labels)
				}
				if strings.TrimSpace(j.Guidance) == "" {
					t.Errorf("catalog %q (assigned sweep) has empty per-issue guidance", j.Slug)
				}
			case schedtmpl.SelectorLabel:
				if len(j.Labels) == 0 {
					t.Errorf("catalog %q (label sweep) has no labels", j.Slug)
				}
				for _, l := range j.Labels {
					if l != strings.TrimSpace(l) || l == "" {
						t.Errorf("catalog %q (sweep) has a blank or padded label %q", j.Slug, l)
					}
				}
			default:
				t.Errorf("catalog %q (sweep) has unexpected selector kind %q", j.Slug, j.SelectorKind)
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

// TestAssignedSweepCatalogEntry pins the PRD #767 M4 assigned-sweep default: it parses
// as a sweep with SelectorKind "assigned", carries NO labels (it selects by the uzi-bot
// assignee at fire time, not a label), and keeps a non-empty per-issue guidance body.
func TestAssignedSweepCatalogEntry(t *testing.T) {
	j, ok := schedtmpl.BySlug("assigned-sweep")
	if !ok {
		t.Fatal("assigned-sweep catalog entry missing")
	}
	if j.Target != "sweep" {
		t.Fatalf("assigned-sweep target = %q, want sweep", j.Target)
	}
	if j.SelectorKind != schedtmpl.SelectorAssigned {
		t.Fatalf("assigned-sweep selector kind = %q, want %q", j.SelectorKind, schedtmpl.SelectorAssigned)
	}
	if len(j.Labels) != 0 {
		t.Fatalf("assigned-sweep must carry no labels, got %v", j.Labels)
	}
	if strings.TrimSpace(j.Guidance) == "" {
		t.Fatal("assigned-sweep must carry non-empty per-issue guidance")
	}
}

// TestSweepSelectorDefaultsToLabel pins that every label sweep (and every non-sweep entry)
// resolves SelectorKind to "label", never "" — so callers can branch on the kind without a
// blank third case.
func TestSweepSelectorDefaultsToLabel(t *testing.T) {
	for _, j := range schedtmpl.Catalog() {
		if j.Slug == "assigned-sweep" {
			continue
		}
		if j.SelectorKind != schedtmpl.SelectorLabel {
			t.Errorf("catalog %q selector kind = %q, want %q (the default)", j.Slug, j.SelectorKind, schedtmpl.SelectorLabel)
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

// TestResolveOutputMode covers the shared resolver both the fire side (schedsvc) and the
// completion-filing side (workersvc) call (PRD #929 M3): a valid stored mode wins; a NULL
// mode with a known catalog slug resolves to that job's catalog default; a NULL mode with an
// unknown or empty slug falls back to DefaultOutputMode ("mr").
func TestResolveOutputMode(t *testing.T) {
	// A valid non-empty stored mode overrides the catalog default, whatever the slug.
	if got := schedtmpl.ResolveOutputMode("issues", true, "feature-bingo"); got != "issues" {
		t.Errorf("stored issues override = %q, want issues", got)
	}
	if got := schedtmpl.ResolveOutputMode("mr", true, "feature-bingo"); got != "mr" {
		t.Errorf("stored mr override = %q, want mr", got)
	}

	// NULL (invalid) stored mode with a KNOWN catalog slug resolves to the catalog default,
	// routed through BySlug — proven by equality with the job's own OutputMode().
	const knownSlug = "feature-bingo"
	job, ok := schedtmpl.BySlug(knownSlug)
	if !ok {
		t.Fatalf("catalog entry %q missing", knownSlug)
	}
	if got := schedtmpl.ResolveOutputMode("", false, knownSlug); got != job.OutputMode() {
		t.Errorf("NULL + known slug = %q, want the catalog default %q", got, job.OutputMode())
	}
	// A stored-but-empty value is treated as unset (storedValid true but "" is not a choice).
	if got := schedtmpl.ResolveOutputMode("", true, knownSlug); got != job.OutputMode() {
		t.Errorf("empty stored + known slug = %q, want the catalog default %q", got, job.OutputMode())
	}

	// NULL with an unknown or empty slug falls back to DefaultOutputMode.
	if got := schedtmpl.ResolveOutputMode("", false, "no-such-slug"); got != schedtmpl.DefaultOutputMode {
		t.Errorf("NULL + unknown slug = %q, want %q", got, schedtmpl.DefaultOutputMode)
	}
	if got := schedtmpl.ResolveOutputMode("", false, ""); got != schedtmpl.DefaultOutputMode {
		t.Errorf("NULL + empty slug = %q, want %q", got, schedtmpl.DefaultOutputMode)
	}
}

// TestFeatureBingoBodyModeNeutral pins the PRD #929 M4 body trim: the dedup para no longer
// names the mr-only "read the idea files under ideas/", and the close-out no longer says
// "open no merge request" (both would contradict issues mode, where delivery is overridden).
// The mr-delivery para 3 ("open a merge request") STAYS — mr mode relies on it verbatim and
// injects nothing. The catalog default remains mr, and the body still parses non-empty.
func TestFeatureBingoBodyModeNeutral(t *testing.T) {
	job, ok := schedtmpl.BySlug("feature-bingo")
	if !ok {
		t.Fatal("feature-bingo catalog entry missing")
	}
	if strings.TrimSpace(job.Prompt) == "" {
		t.Fatal("feature-bingo body is empty after the trim")
	}
	if job.OutputMode() != "mr" {
		t.Fatalf("feature-bingo catalog default = %q, want mr (unchanged)", job.OutputMode())
	}
	// The trimmed mode-contradicting phrasings must be gone.
	if strings.Contains(job.Prompt, "open no merge request") {
		t.Fatalf("feature-bingo body still carries the trimmed mr-only phrase %q", "open no merge request")
	}
	if strings.Contains(job.Prompt, "read the existing idea files") {
		t.Fatalf("feature-bingo body still carries the mr-only dedup phrasing %q", "read the existing idea files")
	}
	// The mr-delivery para that mr mode relies on verbatim must remain.
	if !strings.Contains(job.Prompt, "open a merge request") {
		t.Fatal("feature-bingo body lost its mr-delivery instruction (mr mode relies on it verbatim)")
	}
}
