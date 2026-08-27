// Package schedtmpl is the single source of truth for the product's builtin
// "default scheduled jobs" catalog (PRD #589). Each entry is a generic,
// repo-agnostic schedule the seeder can materialize as a run_schedules row with
// origin='default' on any repo — a weekly test pass, a docs-hygiene sweep, a bug
// hunt, and so on. The catalog is versioned in git, embedded in the binary, and
// parsed once at package init; a parse failure is a build-time-embedded-file
// bug, so it panics rather than being silently skipped.
//
// It deliberately mirrors agenttmpl/builtins.go: a catalog/ directory of
// Markdown-with-frontmatter files embedded via go:embed, a hand-rolled
// frontmatter reader, and Catalog()/BySlug() accessors that return defensive
// copies so callers cannot mutate package state.
//
// Frontmatter format. Every catalog file opens with a `---`-fenced frontmatter
// block of `key: value` lines, then a single blank line, then a free-text body:
//
//	---
//	slug: some-slug
//	name: Human name
//	description: One-line description.
//	target: prompt            # prompt | sweep
//	cron: 0 8 * * 1           # 5-field standard cron
//	timezone: UTC             # optional, defaults to UTC
//	model: fable              # optional, empty => inherit the owner default
//	labels: bug, Planned      # sweep only, comma-separated
//	max_issues: 3             # sweep only, 0/absent => unset
//	---
//
//	<body>
//
// For a prompt target the body is the run prompt; for a sweep target the body is
// the per-issue guidance. auto_approve and wait_on_limit are NOT per-file: every
// default job is created with both true (see AutoApprove / WaitOnLimit).
package schedtmpl

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// AutoApprove and WaitOnLimit are the fixed run flags every default scheduled
// job is seeded with. They are constants rather than per-file frontmatter keys
// because a default job must run unattended (auto-approve its own plan) and ride
// out an Anthropic usage limit (wait rather than fail) on every repo alike.
const (
	AutoApprove = true
	WaitOnLimit = true
)

// DefaultTimezone is applied to a catalog entry whose frontmatter omits the
// timezone key.
const DefaultTimezone = "UTC"

// DefaultJob is one entry of the builtin default-schedule catalog.
type DefaultJob struct {
	Slug        string
	Name        string
	Description string
	Target      string // "prompt" | "sweep" | "self_improve"
	Cron        string
	Timezone    string
	Model       string // "" => inherit the owner default

	// Prompt is the run prompt for a "prompt" target (the file body). Empty for
	// a "sweep" target.
	Prompt string

	// Labels and Guidance carry a "sweep" target's selector and per-issue
	// guidance (the file body). Empty for a "prompt" target.
	Labels   []string
	Guidance string

	// MaxIssues caps how many issues one sweep fire may start (sweep only); 0
	// means unset.
	MaxIssues int
}

//go:embed catalog/*.md
var catalogFS embed.FS

// catalog is the parsed set of default jobs, sorted by slug. Parsing happens
// once at package init; a parse failure is a build-time-embedded-file bug, so it
// panics rather than being silently skipped.
var catalog []DefaultJob

func init() {
	entries, err := fs.ReadDir(catalogFS, "catalog")
	if err != nil {
		panic(fmt.Sprintf("schedtmpl: read catalog dir: %v", err))
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		raw, err := catalogFS.ReadFile(path.Join("catalog", e.Name()))
		if err != nil {
			panic(fmt.Sprintf("schedtmpl: read catalog %s: %v", e.Name(), err))
		}
		job, err := parse(raw)
		if err != nil {
			panic(fmt.Sprintf("schedtmpl: parse catalog %s: %v", e.Name(), err))
		}
		catalog = append(catalog, job)
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Slug < catalog[j].Slug })
}

// Catalog returns the embedded default jobs, sorted by slug. The returned slice
// (and each job's Labels slice) is a copy so callers cannot mutate package
// state.
func Catalog() []DefaultJob {
	out := make([]DefaultJob, len(catalog))
	for i, j := range catalog {
		out[i] = cloneJob(j)
	}
	return out
}

// BySlug returns the default job with the given slug.
func BySlug(slug string) (DefaultJob, bool) {
	for _, j := range catalog {
		if j.Slug == slug {
			return cloneJob(j), true
		}
	}
	return DefaultJob{}, false
}

// cloneJob deep-copies the mutable Labels slice so a returned job shares nothing
// with the package-level catalog.
func cloneJob(j DefaultJob) DefaultJob {
	if j.Labels != nil {
		labels := make([]string, len(j.Labels))
		copy(labels, j.Labels)
		j.Labels = labels
	}
	return j
}

// parse reads one catalog Markdown file into a DefaultJob. It is fed only the
// embedded catalog files, never user input, and its contract is: a `---`-fenced
// frontmatter block of `key: value` lines, a single blank line, then the body.
// It validates enough shape that a malformed entry panics at init rather than
// seeding a broken schedule.
func parse(raw []byte) (DefaultJob, error) {
	const delim = "---\n"
	content := string(raw)
	if !strings.HasPrefix(content, delim) {
		return DefaultJob{}, fmt.Errorf("missing opening frontmatter delimiter")
	}
	rest := content[len(delim):]
	idx := strings.Index(rest, "\n"+delim)
	if idx < 0 {
		return DefaultJob{}, fmt.Errorf("missing closing frontmatter delimiter")
	}
	frontmatter := rest[:idx]
	afterClose := rest[idx+len("\n"+delim):]
	if !strings.HasPrefix(afterClose, "\n") {
		return DefaultJob{}, fmt.Errorf("missing blank line after frontmatter")
	}
	body := strings.TrimSpace(afterClose[len("\n"):])

	j := DefaultJob{Timezone: DefaultTimezone}
	for _, line := range strings.Split(frontmatter, "\n") {
		key, val, ok := strings.Cut(line, ": ")
		if !ok {
			return DefaultJob{}, fmt.Errorf("malformed frontmatter line: %q", line)
		}
		val = strings.TrimSpace(val)
		switch key {
		case "slug":
			j.Slug = val
		case "name":
			j.Name = val
		case "description":
			j.Description = val
		case "target":
			j.Target = val
		case "cron":
			j.Cron = val
		case "timezone":
			if val != "" {
				j.Timezone = val
			}
		case "model":
			j.Model = val
		case "labels":
			for _, l := range strings.Split(val, ",") {
				if l = strings.TrimSpace(l); l != "" {
					j.Labels = append(j.Labels, l)
				}
			}
		case "max_issues":
			n, err := strconv.Atoi(val)
			if err != nil {
				return DefaultJob{}, fmt.Errorf("invalid max_issues %q: %w", val, err)
			}
			j.MaxIssues = n
		default:
			return DefaultJob{}, fmt.Errorf("unknown frontmatter key: %q", key)
		}
	}

	if j.Slug == "" {
		return DefaultJob{}, fmt.Errorf("frontmatter missing slug")
	}
	if j.Name == "" {
		return DefaultJob{}, fmt.Errorf("catalog %q missing name", j.Slug)
	}
	if j.Description == "" {
		return DefaultJob{}, fmt.Errorf("catalog %q missing description", j.Slug)
	}
	if j.Cron == "" {
		return DefaultJob{}, fmt.Errorf("catalog %q missing cron", j.Slug)
	}
	switch j.Target {
	case "prompt":
		if body == "" {
			return DefaultJob{}, fmt.Errorf("catalog %q (prompt) has an empty body", j.Slug)
		}
		j.Prompt = body
	case "sweep":
		if len(j.Labels) == 0 {
			return DefaultJob{}, fmt.Errorf("catalog %q (sweep) has no labels", j.Slug)
		}
		j.Guidance = body
	case "self_improve":
		// Promptless and label-less: the whole directive is worker-side and the tracking
		// issue is resolved at fire time (PRD #590 M1). A self_improve entry carries neither
		// a body prompt nor labels, so nothing off the body is stored.
	default:
		return DefaultJob{}, fmt.Errorf("catalog %q has unknown target %q", j.Slug, j.Target)
	}
	return j, nil
}
