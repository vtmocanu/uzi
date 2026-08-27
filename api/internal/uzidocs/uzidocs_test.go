package uzidocs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantTitle string
		wantOrder *int
		wantAud   string
		wantBody  string
	}{
		{
			name:      "normal file",
			raw:       "---\ntitle: Getting started\norder: 10\naudience: user\n---\n\n# Getting started\n\nBody text.\n",
			wantTitle: "Getting started",
			wantOrder: intp(10),
			wantAud:   AudienceUser,
			wantBody:  "# Getting started\n\nBody text.\n",
		},
		{
			name:      "no leading fence falls back to design with whole body",
			raw:       "# Just a heading\n\nNo frontmatter here.\n",
			wantTitle: "",
			wantOrder: nil,
			wantAud:   AudienceDesign,
			wantBody:  "# Just a heading\n\nNo frontmatter here.\n",
		},
		{
			name:      "leading fence but no closing fence falls back to design",
			raw:       "---\ntitle: Broken\norder: 3\n\n# no close\n",
			wantTitle: "",
			wantOrder: nil,
			wantAud:   AudienceDesign,
			wantBody:  "---\ntitle: Broken\norder: 3\n\n# no close\n",
		},
		{
			name: "a non-line-anchored --- in the body is not a close when there is no real frontmatter",
			// Starts with "# ", so no leading fence -> whole thing is body,
			// audience design; the inline `---` must not be mistaken for a close.
			raw:       "# Title\n\nsome --- inline dashes\n\nmore text\n",
			wantTitle: "",
			wantOrder: nil,
			wantAud:   AudienceDesign,
			wantBody:  "# Title\n\nsome --- inline dashes\n\nmore text\n",
		},
		{
			name:      "order missing yields nil",
			raw:       "---\ntitle: No order\naudience: operator\n---\n\nBody.\n",
			wantTitle: "No order",
			wantOrder: nil,
			wantAud:   AudienceOperator,
			wantBody:  "Body.\n",
		},
		{
			name:      "order zero is distinct from missing",
			raw:       "---\ntitle: Zero\norder: 0\naudience: user\n---\n\nBody.\n",
			wantTitle: "Zero",
			wantOrder: intp(0),
			wantAud:   AudienceUser,
			wantBody:  "Body.\n",
		},
		{
			name:      "empty order value yields nil",
			raw:       "---\ntitle: Empty order\norder:\naudience: user\n---\n\nBody.\n",
			wantTitle: "Empty order",
			wantOrder: nil,
			wantAud:   AudienceUser,
			wantBody:  "Body.\n",
		},
		{
			name:      "non-numeric order yields nil",
			raw:       "---\ntitle: Bad order\norder: abc\naudience: user\n---\n\nBody.\n",
			wantTitle: "Bad order",
			wantOrder: nil,
			wantAud:   AudienceUser,
			wantBody:  "Body.\n",
		},
		{
			name:      "unknown audience falls back to design",
			raw:       "---\ntitle: Weird\norder: 5\naudience: aliens\n---\n\nBody.\n",
			wantTitle: "Weird",
			wantOrder: intp(5),
			wantAud:   AudienceDesign,
			wantBody:  "Body.\n",
		},
		{
			name:      "leading blank lines after fence are stripped",
			raw:       "---\ntitle: Blanks\naudience: user\n---\n\n\n\n# Heading\n",
			wantTitle: "Blanks",
			wantOrder: nil,
			wantAud:   AudienceUser,
			wantBody:  "# Heading\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, body := parseFrontmatter(tt.raw)
			if meta.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", meta.Title, tt.wantTitle)
			}
			if !eqIntp(meta.Order, tt.wantOrder) {
				t.Errorf("order = %v, want %v", fmtIntp(meta.Order), fmtIntp(tt.wantOrder))
			}
			if meta.Audience != tt.wantAud {
				t.Errorf("audience = %q, want %q", meta.Audience, tt.wantAud)
			}
			if body != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func eqIntp(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func fmtIntp(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

func TestListUserAudienceOnly(t *testing.T) {
	got := List(AudienceUser)
	if len(got) == 0 {
		t.Fatal("List(user) returned no docs")
	}
	for _, d := range got {
		if d.Meta.Audience != AudienceUser {
			t.Errorf("List(user) returned non-user doc %q (audience %q)", d.Slug, d.Meta.Audience)
		}
		if d.Slug == "README" {
			t.Error("List returned README")
		}
	}
}

func TestListAllExcludesReadmeAndCoversNonReadme(t *testing.T) {
	all := List("all")
	if len(all) == 0 {
		t.Fatal("List(all) returned no docs")
	}
	seen := make(map[string]bool)
	for _, d := range all {
		if d.Slug == "README" {
			t.Fatal("List(all) returned README")
		}
		seen[d.Slug] = true
	}

	// Every embedded .md except README must appear in List("all").
	entries, err := embeddedDocs.ReadDir("embed")
	if err != nil {
		t.Fatalf("ReadDir embed: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		if slug == "README" {
			if seen[slug] {
				t.Error("List(all) unexpectedly contained README")
			}
			continue
		}
		if !seen[slug] {
			t.Errorf("List(all) missing embedded doc %q", slug)
		}
	}
}

func TestListOrdering(t *testing.T) {
	docs := []Doc{
		{Slug: "b", Meta: DocMeta{Order: intp(20), Audience: AudienceUser}},
		{Slug: "z-noorder", Meta: DocMeta{Order: nil, Audience: AudienceUser}},
		{Slug: "a", Meta: DocMeta{Order: intp(10), Audience: AudienceUser}},
		{Slug: "a-noorder", Meta: DocMeta{Order: nil, Audience: AudienceUser}},
		{Slug: "c", Meta: DocMeta{Order: intp(10), Audience: AudienceUser}},
	}
	sortDocs(docs)
	want := []string{"a", "c", "b", "a-noorder", "z-noorder"}
	for i, w := range want {
		if docs[i].Slug != w {
			t.Errorf("position %d = %q, want %q (full: %v)", i, docs[i].Slug, w, slugsOf(docs))
		}
	}
}

func slugsOf(docs []Doc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Slug
	}
	return out
}

func TestGet(t *testing.T) {
	// A real user doc exists.
	if _, ok := Get("getting-started"); !ok {
		t.Error("Get(getting-started) not found")
	}
	// README is skipped by the loader.
	if _, ok := Get("README"); ok {
		t.Error("Get(README) should be false")
	}
	// Unknown slug.
	if _, ok := Get("no-such-doc"); ok {
		t.Error("Get(no-such-doc) should be false")
	}
}

func TestSearchWholeQuerySubstringCaseInsensitive(t *testing.T) {
	// "connect a forge" is a verbatim H2 in docs/getting-started.md.
	res := Search("CONNECT A FORGE", AudienceUser)
	if len(res) == 0 {
		t.Fatal("Search(connect a forge) returned nothing")
	}
	found := false
	for _, r := range res {
		if r.Slug == "getting-started" {
			found = true
		}
	}
	if !found {
		t.Errorf("Search(connect a forge) did not include getting-started; got %v", searchSlugs(res))
	}
}

func TestSearchTitleOverBodyRanking(t *testing.T) {
	// Force the corpus for a deterministic ranking assertion via the exported
	// Search over synthetic docs is not possible (Search reads package state),
	// so assert the property on the live corpus: within a result set, every
	// InTitle=true result precedes every InTitle=false result.
	res := Search("worker", AudienceUser)
	if len(res) < 2 {
		t.Skip("not enough hits to assert ranking")
	}
	seenBody := false
	for _, r := range res {
		if !r.InTitle {
			seenBody = true
		} else if seenBody {
			t.Errorf("title hit %q ranked after a body-only hit", r.Slug)
		}
	}
}

func TestSearchAudienceFilter(t *testing.T) {
	// A user-audience search must never return a non-user doc.
	res := Search("uzi", AudienceUser)
	userSlugs := make(map[string]bool)
	for _, d := range List(AudienceUser) {
		userSlugs[d.Slug] = true
	}
	for _, r := range res {
		if !userSlugs[r.Slug] {
			t.Errorf("Search(user) returned non-user doc %q", r.Slug)
		}
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	if res := Search("", AudienceUser); res != nil {
		t.Errorf("Search(\"\") = %v, want nil", searchSlugs(res))
	}
}

func searchSlugs(res []SearchResult) []string {
	out := make([]string, len(res))
	for i, r := range res {
		out[i] = r.Slug
	}
	return out
}

func TestSuggestSlug(t *testing.T) {
	// A near-miss typo of a real slug should suggest that slug.
	if got := SuggestSlug("getting-startd"); got != "getting-started" {
		t.Errorf("SuggestSlug(getting-startd) = %q, want getting-started", got)
	}
	// A plausible nearest is always returned for any input (non-empty corpus).
	if got := SuggestSlug("xyzzy"); got == "" {
		t.Error("SuggestSlug(xyzzy) returned empty for a non-empty corpus")
	}
}

// TestEmbeddedDocsMatchSource is the drift guard (PRD #567 D4). It reads the
// SOURCE docs across the module boundary at ../../../docs/*.md (the precedented
// cross-module read) and asserts byte-equality with the embed/ mirror in BOTH
// directions. Because it reads files the Go toolchain does not treat as source
// inputs, it is CACHE-INVISIBLE and must run under -count=1 (which task test:api
// carries). Re-run `task docs:sync` whenever you edit a docs/*.md, or this reddens.
func TestEmbeddedDocsMatchSource(t *testing.T) {
	sourceDir := filepath.Join("..", "..", "..", "docs")
	srcEntries, err := os.ReadDir(sourceDir)
	if err != nil {
		t.Fatalf("read source docs dir %s: %v", sourceDir, err)
	}

	// Every top-level docs/*.md (skip docs/img/) must have an identical mirror.
	sourceMD := make(map[string]bool)
	for _, e := range srcEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		sourceMD[e.Name()] = true

		want, err := os.ReadFile(filepath.Join(sourceDir, e.Name()))
		if err != nil {
			t.Fatalf("read source %s: %v", e.Name(), err)
		}
		got, err := embeddedDocs.ReadFile("embed/" + e.Name())
		if err != nil {
			t.Errorf("embed/%s missing (run `task docs:sync`): %v", e.Name(), err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("embed/%s differs from docs/%s (run `task docs:sync`)", e.Name(), e.Name())
		}
	}

	// The mirror must contain no .md whose source no longer exists.
	embedEntries, err := embeddedDocs.ReadDir("embed")
	if err != nil {
		t.Fatalf("read embed dir: %v", err)
	}
	for _, e := range embedEntries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if !sourceMD[name] {
			t.Errorf("embed/%s has no source docs/%s (stale mirror; run `task docs:sync`)", name, name)
		}
	}
}

// TestExcerptClampsOutOfRangeIndex guards the CodeRabbit #656 fix: bodyIdx is a byte
// offset into strings.ToLower(body), which is not length-preserving for every input
// (U+0130 'İ' → 3 bytes), so on such content the offset can exceed the ORIGINAL body and
// make start > end. excerpt must clamp rather than panic on the slice. A bodyIdx well past
// len(body) forces start = bodyIdx-excerptContext > end = len(body); before the clamp this
// panicked with "slice bounds out of range [start:end]".
func TestExcerptClampsOutOfRangeIndex(t *testing.T) {
	// Reproduce the REAL search mechanism, not a synthetic offset: bodyIdx is a
	// strings.Index into the LOWERED body (see Search), but excerpt slices the ORIGINAL
	// body. U+023A 'Ⱥ' (2 bytes) lowercases to U+2C65 'ⱥ' (3 bytes) under Go's
	// strings.ToLower, so a long Ⱥ prefix makes the lowered body longer than the original
	// and the match's lowered offset lands PAST the original body length. excerpt must
	// clamp rather than panic with start > end. (CodeRabbit #656)
	body := strings.Repeat("Ⱥ", 100) + "needle"
	const q = "needle"
	bodyIdx := strings.Index(strings.ToLower(body), q) // offset into the LONGER lowered body
	if bodyIdx <= len(body) {
		t.Fatalf("fixture did not expand: bodyIdx=%d must exceed len(body)=%d", bodyIdx, len(body))
	}
	// The offset is past the original body, so the only safe outcome is an empty excerpt;
	// the point of the assertion is that the call does not panic.
	if got := excerpt(body, len(q), bodyIdx); got != "" {
		t.Errorf("out-of-range bodyIdx must clamp to an empty excerpt (no panic), got %q", got)
	}
	// A normal in-range hit still returns a snippet around the match.
	if got := excerpt("the quick brown fox", 5, 4); got == "" {
		t.Error("an in-range match must still produce a non-empty excerpt")
	}
}
