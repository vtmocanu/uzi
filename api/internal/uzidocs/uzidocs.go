// Package uzidocs is the offline docs corpus that the `uzi docs` CLI verbs
// consume. It embeds a COMMITTED MIRROR of the repo's root docs/*.md into the
// binary via go:embed, so an agent helping a user get started from a terminal
// can read the product's conceptual/onboarding docs with no server and no token
// (like `uzi version`). See PRD #567.
//
// Why a mirror rather than embedding docs/ in place: docs/ sits at the repo root,
// a sibling of the api/ Go module (api/go.mod), and a go:embed pattern cannot
// reference a path outside the embedding package's directory tree (no `..`). So
// the docs are copied into embed/ by `task docs:sync` and committed; the drift
// guard TestEmbeddedDocsMatchSource keeps that mirror byte-identical to docs/*.md.
//
// The frontmatter parser and slug/order/audience semantics here DELIBERATELY
// mirror web/src/lib/docs.ts (parseFrontmatter, slugOf, sortDocsForIndex) so the
// terminal answer and the /docs web page cannot disagree. When you change one,
// change the other: this file is a Go port of that TypeScript (the closing-fence
// search is a substring scan for "\n---", not a full-line scan; unknown/malformed
// frontmatter falls back to audience "design"; the README slug is skipped in the
// loader). The port matches docs.ts for the repo's actual corpus, whose `order:`
// values are all plain integers; it is NOT a bit-exact reimplementation of JS
// Number() for exotic `order:` forms — see the `case "order"` note below. Keep
// this package a stdlib-only leaf.
package uzidocs

import (
	"embed"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// embeddedDocs holds the committed mirror of docs/*.md. `task docs:sync` populates
// embed/ and the files are committed, so `go build` (and `brew install`, which
// builds from a plain source tarball with no generate step) embeds them unchanged.
//
//go:embed embed/*.md
var embeddedDocs embed.FS

// Audience is the closed set of doc audiences, matching web/src/lib/docs.ts:28-29.
const (
	AudienceUser        = "user"
	AudienceOperator    = "operator"
	AudienceDesign      = "design"
	AudienceContributor = "contributor"
)

var audiences = map[string]bool{
	AudienceUser:        true,
	AudienceOperator:    true,
	AudienceDesign:      true,
	AudienceContributor: true,
}

// DocMeta is the parsed frontmatter of a doc. Order is a pointer so "no order"
// (nil) is distinct from order 0, mirroring the `number | null` in docs.ts.
type DocMeta struct {
	Title    string
	Order    *int
	Audience string
}

// Doc is a single embedded doc: its slug (filename without .md), parsed
// frontmatter, and the body (raw markdown, post-frontmatter).
type Doc struct {
	Slug string
	Meta DocMeta
	Body string
}

// SearchResult is one Search hit: enough context for the caller to render a
// snippet. InTitle reports whether the query matched the title (title hits rank
// above body-only hits).
type SearchResult struct {
	Slug     string
	Title    string
	Audience string
	Excerpt  string
	InTitle  bool
}

// frontmatterKey matches a single `key: value` frontmatter line, mirroring the
// JS regex /^([A-Za-z]+):\s*(.*)$/ applied to a trimmed line.
var frontmatterKey = regexp.MustCompile(`^([A-Za-z]+):\s*(.*)$`)

// parseFrontmatter is a Go port of web/src/lib/docs.ts:parseFrontmatter. Only a
// leading `---\n` fence at byte 0 is consumed; the closing fence is found by a
// SUBSTRING search for "\n---" starting at offset 4 (not a full-line scan), so a
// `---` later in the body (e.g. inside a code fence) is content. A file with no
// leading fence, or a malformed one, falls back to audience "design" with the
// whole raw text as the body rather than erroring.
func parseFrontmatter(raw string) (DocMeta, string) {
	fallback := DocMeta{Title: "", Order: nil, Audience: AudienceDesign}
	if !strings.HasPrefix(raw, "---\n") {
		return fallback, raw
	}

	// JS: raw.indexOf("\n---", 4) — search from offset 4, absolute index.
	rel := strings.Index(raw[4:], "\n---")
	if rel == -1 {
		return fallback, raw
	}
	closeIdx := rel + 4

	yaml := raw[4:closeIdx]

	// Body starts after the closing fence's own line (which may carry trailing
	// whitespace). JS: afterFence = raw.indexOf("\n", close+4).
	var body string
	if arel := strings.Index(raw[closeIdx+4:], "\n"); arel == -1 {
		body = ""
	} else {
		afterFence := closeIdx + 4 + arel
		body = raw[afterFence+1:]
	}
	// JS .replace(/^\n+/, "") — strip leading newline chars only (not other
	// whitespace).
	body = strings.TrimLeft(body, "\n")

	meta := DocMeta{Title: "", Order: nil, Audience: AudienceDesign}
	for _, line := range strings.Split(yaml, "\n") {
		m := frontmatterKey.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		key, value := m[1], strings.TrimSpace(m[2])
		switch key {
		case "title":
			meta.Title = value
		case "order":
			// JS: meta.order = value !== "" && Number.isFinite(Number(value)) ? n : null.
			// Reset each line so a later malformed order line nulls an earlier one.
			//
			// Parity caveat: JS Number() accepts floats/hex/underscored forms that
			// ParseFloat+int() would truncate or reject (e.g. "1.5"->1 here vs 1.5 in
			// web; "0x10" rejected here vs 16 in web). The corpus uses only plain
			// integers, so this never diverges in practice; a future fractional/hex
			// order could sort differently in the CLI vs web — reject non-integer
			// `order:` at authoring time instead.
			meta.Order = nil
			if value != "" {
				if n, err := strconv.ParseFloat(value, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
					o := int(n)
					meta.Order = &o
				}
			}
		case "audience":
			// Set only when the value is one of the closed audience set.
			if audiences[value] {
				meta.Audience = value
			}
		}
	}
	return meta, body
}

// docsBySlug is the loaded corpus keyed by slug, README excluded (mirrors
// docs.ts:113). Built once at package init from the embedded bytes.
var docsBySlug = loadDocs()

func loadDocs() map[string]Doc {
	m := make(map[string]Doc)
	entries, err := embeddedDocs.ReadDir("embed")
	if err != nil {
		return m
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		slug := strings.TrimSuffix(name, ".md")
		if slug == "README" { // never surfaced by the loader (docs.ts:113)
			continue
		}
		raw, err := embeddedDocs.ReadFile("embed/" + name)
		if err != nil {
			continue
		}
		meta, body := parseFrontmatter(string(raw))
		m[slug] = Doc{Slug: slug, Meta: meta, Body: body}
	}
	return m
}

// sortDocs orders docs the way web does (sortDocsForIndex): by order ascending
// with nil order LAST, then slug ascending as a stable tiebreak.
func sortDocs(docs []Doc) {
	sort.SliceStable(docs, func(i, j int) bool {
		oi, oj := docs[i].Meta.Order, docs[j].Meta.Order
		switch {
		case oi != nil && oj != nil:
			if *oi != *oj {
				return *oi < *oj
			}
			return docs[i].Slug < docs[j].Slug
		case oi != nil: // i has an order, j does not -> i first
			return true
		case oj != nil: // j has an order, i does not -> j first
			return false
		default:
			return docs[i].Slug < docs[j].Slug
		}
	})
}

// List returns the docs matching audience (one of the four audiences, or "all"
// for every non-README doc), ordered by order-asc-nils-last then slug. README is
// never included.
func List(audience string) []Doc {
	var out []Doc
	for _, d := range docsBySlug {
		if audience == "all" || d.Meta.Audience == audience {
			out = append(out, d)
		}
	}
	sortDocs(out)
	return out
}

// Get looks up a doc by slug. README is not in the corpus, so Get("README") is
// (Doc{}, false).
func Get(slug string) (Doc, bool) {
	d, ok := docsBySlug[slug]
	return d, ok
}

// Search does a whole-query, case-insensitive SUBSTRING match (not tokenized)
// over title + body, within the audience filter (or "all"). Title hits rank above
// body-only hits; within each group the order is the List order (order then
// slug), so the result is deterministic. Each result carries a short excerpt
// around the first body match (or the body start for a title-only hit).
func Search(query, audience string) []SearchResult {
	if query == "" {
		return nil
	}
	q := strings.ToLower(query)
	docs := List(audience) // already filtered and ordered by order,slug

	var titleHits, bodyHits []SearchResult
	for _, d := range docs {
		inTitle := strings.Contains(strings.ToLower(d.Meta.Title), q)
		bodyIdx := strings.Index(strings.ToLower(d.Body), q)
		switch {
		case inTitle:
			titleHits = append(titleHits, makeResult(d, len(q), bodyIdx, true))
		case bodyIdx >= 0:
			bodyHits = append(bodyHits, makeResult(d, len(q), bodyIdx, false))
		}
	}
	return append(titleHits, bodyHits...)
}

const excerptContext = 60

func makeResult(d Doc, qLen, bodyIdx int, inTitle bool) SearchResult {
	return SearchResult{
		Slug:     d.Slug,
		Title:    d.Meta.Title,
		Audience: d.Meta.Audience,
		Excerpt:  excerpt(d.Body, qLen, bodyIdx),
		InTitle:  inTitle,
	}
}

// excerpt returns a whitespace-collapsed snippet around the first body match. For
// a title-only hit (bodyIdx < 0) it snips from the body start instead.
func excerpt(body string, qLen, bodyIdx int) string {
	start := 0
	var end int
	if bodyIdx >= 0 {
		start = bodyIdx - excerptContext
		if start < 0 {
			start = 0
		}
		end = bodyIdx + qLen + excerptContext
	} else {
		end = 2*excerptContext + qLen
	}
	if end > len(body) {
		end = len(body)
	}
	// bodyIdx is a byte offset into strings.ToLower(body), which is NOT always
	// length-preserving: U+023A 'Ⱥ' (2 bytes) lowercases to U+2C65 'ⱥ' (3 bytes) under
	// Go's strings.ToLower, so a body with such runes lowers LONGER than the original and
	// the offset can land past the ORIGINAL body, making start exceed end. Clamp so the
	// slice below can never panic with start > end. The corpus is ASCII today, so this is
	// defensive against a future non-ASCII doc (CodeRabbit #656).
	if start > end {
		start = end
	}
	// Byte slicing may cut a multi-byte rune at either edge; drop any broken
	// bytes rather than emit invalid UTF-8, then collapse whitespace/newlines.
	snip := strings.ToValidUTF8(body[start:end], "")
	return strings.Join(strings.Fields(snip), " ")
}

// SuggestSlug returns the known slug nearest to the given (unknown) slug by
// Levenshtein distance, for a "did you mean" hint. Ties resolve to the
// lexicographically smallest slug, so the result is deterministic. Returns "" when
// the corpus is empty.
func SuggestSlug(slug string) string {
	slugs := make([]string, 0, len(docsBySlug))
	for s := range docsBySlug {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)

	best := ""
	bestDist := -1
	for _, s := range slugs {
		d := levenshtein(slug, s)
		if bestDist == -1 || d < bestDist {
			bestDist = d
			best = s
		}
	}
	return best
}

// levenshtein is the standard edit distance over runes.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr := make([]int, len(rb)+1)
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev = curr
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
