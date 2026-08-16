package workersvc

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"unicode/utf16"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// storeRow is the generated query row, aliased only to keep the fixture types legible.
// The fixture decodes into THIS type, so there is no hand-written Go mapper to drift.
type storeRow = store.ListJudgeRecommendationRowsForUserRow

// The mock/server fidelity golden fixture (PRD #98 seam 6). This file is the GO HALF; the
// vitest half is web/src/mocks/judgeBacklogFidelity.test.ts. Neither reads the other, and
// neither generates the fixture: each compares its OWN output against the same third
// artifact, so a failure names the side that drifted. A direct Go-vs-JS diff can only ever
// report that they disagree.
//
// There is deliberately NO -update flag here, for the same reason the vitest half refuses
// toMatchSnapshot(): a golden that any run can rewrite is a snapshot, and a snapshot of a
// regression is green.
//
// This file is ASCII-ONLY on purpose. Every exotic character it needs is built by code
// point (rune(0x00A0) and friends) rather than pasted, because a pasted glyph corrupts
// silently in transit and the corruption reads as a passing test. One occurred while this
// fixture was being authored: a literal U+0020 arrived as U+00A0.
//
// 🔴 RUN THIS PACKAGE WITH -count=1 AFTER ANY FIXTURE-ONLY EDIT. GO'S TEST CACHE CANNOT SEE
// THE FIXTURE, AND A NEUTERED FIXTURE THEREFORE PRINTS "ok (cached)".
//
// Go's test cache hashes the files a test opens, but ONLY those inside the module root --
// cmd/go's own comment is "Do not recheck files outside the module, GOPATH, or GOROOT root".
// fidelityDir points ABOVE api/, deliberately (see the fixture README: repo root, owned by
// neither runtime), so every byte of cases.json and expected.json is outside this module and
// contributes NOTHING to this package's cache key.
//
// MEASURED at 6002d808, and the whole point is that it produced output indistinguishable
// from success:
//
//	delete an entire case from cases.json
//	cd api && go test ./internal/workersvc/   -> ok (cached)     <- the fixture is gutted
//	cd api && go test -count=1 ./...          -> FAIL, "fixture broken: cases.json has no
//	                                             case ..." and the orphaned-golden message
//
// THE ASYMMETRY IS THE FINDING, because the README's "each suite stands alone" reads as
// symmetric and is not: the vitest half has no such cache and reddened on the SAME tree with
// no flag at all. So "Go green + vitest red means Go drifted" -- the rule both files state
// at their failure sites -- has a THIRD explanation nobody had written down: Go never ran.
//
// WHICH GATE IS ACTUALLY EXPOSED, narrowed rather than left as "the Go half":
//   - ./e2e/run-store-it.sh was NEVER at risk, for two independent reasons: it passes
//     -count=1 (line 72), and it runs -run 'LiveDB$' over ./internal/store/... and
//     ./internal/handler/... only, so it never reaches this package at all.
//   - CI's test:controller was never at risk either -- it already passes -count=1, with this
//     exact mechanism written out in .gitlab-ci.yml for the api goldens IT reads across the
//     same module boundary.
//   - The exposed gates were the repo's own `cd api && go test ./...` (CLAUDE.md's api
//     section) and CI's test:api, which ran it bare while .go_job persists .gocache/ between
//     pipelines. test:api now passes -count=1 for the reason its controller sibling already
//     did.
//
// DO NOT "FIX" THIS BY MOVING THE FIXTURE UNDER api/. That reintroduces the regenerator
// gravity the design rejected: testdata/ is where a -update flag gets added, and a golden any
// run can rewrite is a snapshot.
const fidelityDir = "../../../fixtures/judge-fidelity"

type fidelityCase struct {
	Name      string `json:"name"`
	Proves    string `json:"proves"`
	DoNotTidy string `json:"do_not_tidy"`
	Bucket    string `json:"bucket"`
	// Rows decode STRAIGHT into the generated query row struct. There is no hand-written
	// mapper between the fixture and the code under test, so there is none to drift.
	Rows []storeRow `json:"rows"`
}

type fidelityDoc struct {
	Cases []fidelityCase `json:"cases"`
}

// fidRead loads a fixture file. A missing or unreadable fixture is a FATAL, never a skip:
// a skip here is the exact false-green shape the repo already records for the live-DB
// suites, where a suite that ran nothing prints ok.
func fidRead(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fidelityDir, name))
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- seam 6 asserts nothing without it, and skipping would look identical to passing", name, err)
	}
	return b
}

// fidDecode decodes with DisallowUnknownFields. That rejects a key the Go structs do not
// have, so a RENAMED or REMOVED query column reddens by name. It does NOT catch an ADDED
// field: a new struct field the fixture omits decodes to its zero value in silence.
func fidDecode(t *testing.T, name string, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("fixture %s does not decode into the types under test: %v", name, err)
	}
}

func fidCases(t *testing.T) fidelityDoc {
	t.Helper()
	var doc fidelityDoc
	fidDecode(t, "cases.json", fidRead(t, "cases.json"), &doc)
	if len(doc.Cases) == 0 {
		t.Fatal("fixture broken: cases.json defines no cases -- every assertion below would then pass over an empty range")
	}
	return doc
}

func fidExpectedAny(t *testing.T) map[string][]any {
	t.Helper()
	var out map[string][]any
	if err := json.Unmarshal(fidRead(t, "expected.json"), &out); err != nil {
		t.Fatalf("expected.json is not valid JSON: %v", err)
	}
	return out
}

// fidExpectedTyped is a SECOND decode of the same bytes, into the DTO. The golden
// comparison itself runs against the untyped form (so an extra or missing JSON key is a
// failure); this one exists for the output-side self-check, and its DisallowUnknownFields
// additionally catches a key in expected.json that the DTO does not ship.
func fidExpectedTyped(t *testing.T) map[string][]apitypes.JudgeRecommendationGroupDTO {
	t.Helper()
	var out map[string][]apitypes.JudgeRecommendationGroupDTO
	fidDecode(t, "expected.json", fidRead(t, "expected.json"), &out)
	return out
}

func fidCase(t *testing.T, doc fidelityDoc, name string) fidelityCase {
	t.Helper()
	for _, c := range doc.Cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("fixture broken: cases.json has no case %q -- the self-check below is what makes that case load-bearing, so removing the case must redden here rather than quietly shrink the fixture", name)
	return fidelityCase{}
}

func fidGroups(t *testing.T, exp map[string][]apitypes.JudgeRecommendationGroupDTO, name string) []apitypes.JudgeRecommendationGroupDTO {
	t.Helper()
	g, ok := exp[name]
	if !ok {
		t.Fatalf("fixture broken: expected.json has no golden output for case %q", name)
	}
	return g
}

// fidRung classifies ONE fixture row on the #94 ladder. It deliberately does NOT call
// BucketOf. A self-check that ran the implementation could be talked into agreeing by a
// mutated implementation, and immunity to that is the whole reason this layer exists.
//
// The honest cost of the duplication: this classifier cannot notice a LADDER change, only
// a fixture change. Catching a ladder change is the golden comparison's job.
func fidRung(r storeRow) string {
	switch r.DispositionStatus.String {
	case "dismissed":
		return "dismissed"
	case "done":
		return "done"
	}
	if r.FiledSettled {
		return "filed"
	}
	return "todo"
}

func fidRunes(s string) int { return len([]rune(s)) }
func fidUnits(s string) int { return len(utf16.Encode([]rune(s))) }
func fidCoord(r storeRow) coord {
	return coord{category: r.Category, target: r.Target}
}

// fidFirstSeen returns each coordinate's first row index, which is the pre-sort group order
// the stable sort must preserve within a tie.
func fidFirstSeen(rows []storeRow) map[coord]int {
	out := map[coord]int{}
	for i, r := range rows {
		if _, ok := out[fidCoord(r)]; !ok {
			out[fidCoord(r)] = i
		}
	}
	return out
}

func fidIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "<unmarshalable>"
	}
	return string(b)
}

// TestJudgeGrouperMatchesFidelityGolden runs the REAL grouper and filter over each case's
// rows and compares the marshalled result against expected.json. The comparison is on
// parsed JSON, not on structs, so a json-tag change on either side is a failure -- which is
// the point: the tags are the wire contract the mock mirrors.
func TestJudgeGrouperMatchesFidelityGolden(t *testing.T) {
	doc := fidCases(t)
	expected := fidExpectedAny(t)

	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if seen[c.Name] {
			t.Fatalf("fixture broken: cases.json defines %q twice -- the second one would silently win every lookup in the self-check", c.Name)
		}
		seen[c.Name] = true

		want, ok := expected[c.Name]
		if !ok {
			t.Errorf("fixture broken: cases.json defines case %q with no golden in expected.json -- an ungolden case runs the grouper and asserts nothing", c.Name)
			continue
		}
		if !ValidJudgeBacklogBucket(c.Bucket) {
			t.Errorf("fixture broken: case %q declares bucket %q, which the handler would reject with a 400 -- the case then exercises a filter value the API cannot receive", c.Name, c.Bucket)
			continue
		}

		raw, err := json.Marshal(filterGroups(GroupJudgeRecommendations(c.Rows), c.Bucket))
		if err != nil {
			t.Fatalf("case %q: marshalling the grouper output failed: %v", c.Name, err)
		}
		var got []any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("case %q: re-parsing the grouper output failed: %v", c.Name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("case %q: the GO grouper disagrees with fixtures/judge-fidelity/expected.json.\n"+
				"If the vitest half (web/src/mocks/judgeBacklogFidelity.test.ts) is GREEN, this side drifted.\n"+
				"got:\n%s\nwant:\n%s", c.Name, fidIndent(got), fidIndent(want))
		}
	}

	for name := range expected {
		if !seen[name] {
			t.Errorf("fixture broken: expected.json carries golden output for %q but cases.json no longer defines it -- an orphaned golden is never compared against anything and reads as coverage", name)
		}
	}
}

// TestJudgeFidelityCasesDiscriminate is the INPUT-side self-check: one predicate per
// reimplemented behaviour, asserting the case can actually tell a correct implementation
// from a wrong one. It touches neither implementation.
func TestJudgeFidelityCasesDiscriminate(t *testing.T) {
	doc := fidCases(t)

	t.Run("dedup-across-runs", func(t *testing.T) {
		c := fidCase(t, doc, "dedup-across-runs")
		runs := map[coord]map[string]bool{}
		for _, r := range c.Rows {
			k := fidCoord(r)
			if runs[k] == nil {
				runs[k] = map[string]bool{}
			}
			runs[k][r.RunID.String()] = true
		}
		for _, seen := range runs {
			if len(seen) >= 2 {
				return
			}
		}
		t.Fatal("fixture broken: no coordinate in this case occurs in two different runs -- otherwise it proves nothing about deduping ACROSS runs, and run_count 1 would satisfy it")
	})

	t.Run("occurrences-exceed-run-count", func(t *testing.T) {
		c := fidCase(t, doc, "occurrences-exceed-run-count")
		type k3 struct {
			c   coord
			run string
		}
		n := map[k3]int{}
		for _, r := range c.Rows {
			n[k3{fidCoord(r), r.RunID.String()}]++
		}
		for _, v := range n {
			if v >= 2 {
				return
			}
		}
		t.Fatal("fixture broken: no (category, target, run_id) triple appears twice -- otherwise occurrences can never outnumber run_count and the SQLSTATE 21000 shape is not in the fixture at all")
	})

	t.Run("partial-settle", func(t *testing.T) {
		c := fidCase(t, doc, "partial-settle")
		open, settled := map[coord]bool{}, map[coord]bool{}
		for _, r := range c.Rows {
			if fidRung(r) == BucketTodo {
				open[fidCoord(r)] = true
			} else {
				settled[fidCoord(r)] = true
			}
		}
		for k := range open {
			if settled[k] {
				return
			}
		}
		t.Fatal("fixture broken: no coordinate has BOTH an open member and a settled one -- otherwise the open_count short-circuit in the rollup is never exercised")
	})

	t.Run("rollup-precedence-pairs", func(t *testing.T) {
		c := fidCase(t, doc, "rollup-precedence-pairs")
		rungs := map[coord]map[string]bool{}
		for _, r := range c.Rows {
			k := fidCoord(r)
			if rungs[k] == nil {
				rungs[k] = map[string]bool{}
			}
			rungs[k][fidRung(r)] = true
		}
		pairs := [][2]string{
			{"dismissed", "done"}, {"dismissed", "filed"}, {"dismissed", BucketTodo},
			{"done", "filed"}, {"done", BucketTodo}, {"filed", BucketTodo},
		}
		for _, p := range pairs {
			found := false
			for _, have := range rungs {
				if !have[p[0]] || !have[p[1]] {
					continue
				}
				// For a pair that does not involve todo, a stray todo member would push
				// OpenCount above zero, the rollup would short-circuit, and topRung would
				// never be consulted -- the coordinate would look like coverage and be none.
				if p[0] != BucketTodo && p[1] != BucketTodo && have[BucketTodo] {
					continue
				}
				found = true
				break
			}
			if !found {
				t.Errorf("fixture broken: no coordinate carries members on BOTH the %s and %s rungs with no stray todo member -- otherwise the %s/%s half of the precedence ladder is never chosen between", p[0], p[1], p[0], p[1])
			}
		}
	})

	t.Run("sort-tie-first-seen-order", func(t *testing.T) {
		c := fidCase(t, doc, "sort-tie-first-seen-order")
		type rc struct{ runs, open int }
		agg := map[coord]*rc{}
		runsSeen := map[coord]map[string]bool{}
		for _, r := range c.Rows {
			k := fidCoord(r)
			if agg[k] == nil {
				agg[k] = &rc{}
				runsSeen[k] = map[string]bool{}
			}
			if !runsSeen[k][r.RunID.String()] {
				runsSeen[k][r.RunID.String()] = true
				agg[k].runs++
			}
			if fidRung(r) == BucketTodo {
				agg[k].open++
			}
		}
		tally := map[rc]int{}
		for _, v := range agg {
			tally[*v]++
		}
		tied := 0
		for _, n := range tally {
			if n >= 2 {
				tied = n
			}
		}
		if tied < 2 {
			t.Fatal("fixture broken: no two coordinates tie on (run_count, open_count) -- otherwise the sort never has to break a tie and the first-seen ordering guarantee is untested")
		}
		if len(tally) < 2 {
			t.Fatal("fixture broken: every coordinate has the same (run_count, open_count) -- with nothing to order, a comparator that always returned 0 would pass")
		}
	})

	t.Run("bucket-filter", func(t *testing.T) {
		byRows := map[string][]fidelityCase{}
		for _, c := range doc.Cases {
			k := string(fidJSON(t, c.Rows))
			byRows[k] = append(byRows[k], c)
		}
		exp := fidExpectedTyped(t)
		for _, group := range byRows {
			if len(group) < 2 {
				continue
			}
			buckets, counts := map[string]bool{}, map[int]bool{}
			for _, c := range group {
				buckets[c.Bucket] = true
				counts[len(fidGroups(t, exp, c.Name))] = true
			}
			if len(buckets) >= 2 && len(counts) >= 2 {
				return
			}
		}
		t.Fatal("fixture broken: no two cases share an identical row set while declaring different ?bucket= values AND expecting different group counts -- otherwise a filterGroups that ignored the bucket entirely would pass every case")
	})

	t.Run("preview-ascii-cut", func(t *testing.T) {
		c := fidCase(t, doc, "preview-ascii-cut")
		longEnough := false
		for _, r := range c.Rows {
			if fidRunes(r.RationaleMd) > RationalePreviewMaxRunes {
				longEnough = true
			}
		}
		if !longEnough {
			t.Error("fixture broken: no row exceeds the preview cap -- otherwise the truncation branch never runs and the case only proves short strings pass through")
		}
		first := fidFirstSeen(c.Rows)
		nonFirst := false
		for _, idx := range first {
			if idx > 0 {
				nonFirst = true
			}
		}
		if !nonFirst {
			t.Error("fixture broken: every coordinate's first row is row 0 -- otherwise 'the preview comes from the GROUP's first row' is indistinguishable from 'the preview comes from the case's first row'")
		}
		perCoord := map[coord]int{}
		for _, r := range c.Rows {
			perCoord[fidCoord(r)]++
		}
		multi := false
		for _, n := range perCoord {
			if n >= 2 {
				multi = true
			}
		}
		if !multi {
			t.Error("fixture broken: no coordinate has a second row -- otherwise 'the preview comes from the FIRST row' cannot be distinguished from 'from the last row'")
		}
	})

	t.Run("preview-multibyte-cut", func(t *testing.T) {
		c := fidCase(t, doc, "preview-multibyte-cut")
		for _, r := range c.Rows {
			if fidRunes(r.RationaleMd) != fidUnits(r.RationaleMd) && fidRunes(r.RationaleMd) > RationalePreviewMaxRunes {
				return
			}
		}
		t.Fatal("fixture broken: no row has a rune count differing from its UTF-16 code-unit count AND exceeding the cap -- otherwise the cut lands where the two counts agree and a code-unit implementation is indistinguishable from a rune one")
	})

	t.Run("preview-multibyte-no-cut", func(t *testing.T) {
		c := fidCase(t, doc, "preview-multibyte-no-cut")
		for _, r := range c.Rows {
			if fidUnits(r.RationaleMd) > RationalePreviewMaxRunes && fidRunes(r.RationaleMd) <= RationalePreviewMaxRunes {
				return
			}
		}
		t.Fatal("fixture broken: no row is over the cap in UTF-16 code units while under it in runes -- this is the ONLY shape that separates a rune implementation from a code-unit one, because past 280 runes both cut and cut identically")
	})

	t.Run("preview-trim-boundary", func(t *testing.T) {
		c := fidCase(t, doc, "preview-trim-boundary")
		want := map[rune]bool{rune(0x00A0): false, rune(0xFEFF): false, rune(0x2028): false, rune(0x0020): false}
		for _, r := range c.Rows {
			runes := []rune(r.RationaleMd)
			if len(runes) <= RationalePreviewMaxRunes {
				t.Errorf("fixture broken: a trim-boundary row is under the cap, so it is never cut and its rune %d is never at the boundary at all", RationalePreviewMaxRunes)
				continue
			}
			if _, ok := want[runes[RationalePreviewMaxRunes-1]]; ok {
				want[runes[RationalePreviewMaxRunes-1]] = true
			}
		}
		for ch, ok := range want {
			if !ok {
				t.Errorf("fixture broken: no row places U+%04X at rune %d -- that character is in JS's \\s and not in Go's TrimRight cutset, so without it the two trim sets are never asked to disagree (U+0020 is the positive control: it must be trimmed by BOTH, otherwise 'these characters survive' is satisfied by never trimming)", ch, RationalePreviewMaxRunes)
			}
		}
	})

	t.Run("sort-stability-13-groups", func(t *testing.T) {
		c := fidCase(t, doc, "sort-stability-13-groups")
		var order []coord
		seenCoord := map[coord]bool{}
		runs := map[coord]map[string]bool{}
		for _, r := range c.Rows {
			k := fidCoord(r)
			if !seenCoord[k] {
				seenCoord[k] = true
				order = append(order, k)
				runs[k] = map[string]bool{}
			}
			runs[k][r.RunID.String()] = true
			if fidRung(r) == BucketTodo {
				t.Fatal("fixture broken: a row in the stability case is open, so open_count can break a tie the sort was supposed to leave alone -- every member must be settled")
			}
		}
		if len(order) < 13 {
			t.Fatalf("fixture broken: %d groups, need at least 13 -- Go's pdqsort insertion-sorts below n=12, so an unstable sort produces byte-identical output at any smaller size", len(order))
		}
		// The PRE-SORT sequence of run_counts, in first-seen order.
		seq := make([]int, len(order))
		for i, k := range order {
			seq[i] = len(runs[k])
		}
		distinct := map[int]int{}
		for _, v := range seq {
			distinct[v]++
		}
		if len(distinct) < 2 {
			t.Fatal("fixture broken: every group has the same run_count -- an all-tied run is already sorted, pdqsort short-circuits on it, and the unstable sort goes green at every size")
		}
		for v, n := range distinct {
			if n < 2 {
				t.Fatalf("fixture broken: run_count %d appears on only %d group -- with no tie at that value there is no ordering for stability to preserve", v, n)
			}
		}
		sorted := true
		for i := 1; i < len(seq); i++ {
			if seq[i] > seq[i-1] {
				sorted = false
			}
		}
		if sorted {
			t.Fatal("fixture broken: the groups are already in run_count DESC order before the sort runs -- pdqsort detects an ordered input and short-circuits, so this case would go green under an unstable sort. Interleave them")
		}
	})
}

// TestJudgeFidelityGoldenStillDiscriminates is the OUTPUT-side self-check, and it is the
// half that defeats regeneration. Regenerating expected.json from a REGRESSED grouper
// produces a golden that no longer has the properties each case exists to demonstrate, and
// these predicates depend on neither implementation, so no regeneration can talk them into
// agreeing.
//
// Its honest limit: a regression that happens to preserve every declared property below
// regenerates cleanly. The declared list IS the coverage.
func TestJudgeFidelityGoldenStillDiscriminates(t *testing.T) {
	doc := fidCases(t)
	exp := fidExpectedTyped(t)
	ell := string(rune(0x2026))

	t.Run("occurrences-exceed-run-count", func(t *testing.T) {
		for _, g := range fidGroups(t, exp, "occurrences-exceed-run-count") {
			if len(g.Occurrences) > g.RunCount {
				return
			}
		}
		t.Fatal("fixture broken: no expected group has more occurrences than run_count -- this case no longer describes the shape it is named for, which is what a golden regenerated from a RunCount that escaped the runsSeen guard would look like")
	})

	t.Run("rollup-precedence-pairs", func(t *testing.T) {
		want := map[string]string{
			"pair-dismissed-done":  "dismissed",
			"pair-dismissed-filed": "dismissed",
			"pair-done-filed":      "done",
			"pair-dismissed-todo":  BucketTodo,
			"pair-done-todo":       BucketTodo,
			"pair-filed-todo":      BucketTodo,
		}
		got := map[string]string{}
		for _, g := range fidGroups(t, exp, "rollup-precedence-pairs") {
			got[g.Target] = g.Bucket
		}
		for target, rung := range want {
			if got[target] != rung {
				t.Errorf("fixture broken: the golden rolls %s up to %q, not %q -- a golden that agrees with the ladder about every pair EXCEPT the one it is named for is a regenerated golden, not an authored one", target, got[target], rung)
			}
		}
	})

	t.Run("sort-tie-first-seen-order", func(t *testing.T) {
		c := fidCase(t, doc, "sort-tie-first-seen-order")
		assertFirstSeenWithinTies(t, c, fidGroups(t, exp, c.Name))
	})

	t.Run("preview-ascii-cut", func(t *testing.T) {
		cut := false
		for _, g := range fidGroups(t, exp, "preview-ascii-cut") {
			r := []rune(g.RationalePreview)
			if len(r) == RationalePreviewMaxRunes+1 && string(r[len(r)-1]) == ell {
				cut = true
			}
		}
		if !cut {
			t.Fatalf("fixture broken: no expected preview is exactly %d runes ending in an ellipsis -- the case no longer shows a cut at all", RationalePreviewMaxRunes+1)
		}
	})

	t.Run("preview-multibyte-cut", func(t *testing.T) {
		for _, g := range fidGroups(t, exp, "preview-multibyte-cut") {
			p := g.RationalePreview
			if fidRunes(p) != RationalePreviewMaxRunes+1 {
				t.Errorf("fixture broken: expected preview for %s is %d runes, want %d", g.Target, fidRunes(p), RationalePreviewMaxRunes+1)
			}
			if fidRunes(p) == fidUnits(p) {
				t.Errorf("fixture broken: expected preview for %s has equal rune and UTF-16 counts, so it is effectively ASCII -- this is what someone 'simplifying' the case produces, and it silently stops separating a rune implementation from a code-unit one", g.Target)
			}
		}
	})

	t.Run("preview-multibyte-no-cut", func(t *testing.T) {
		c := fidCase(t, doc, "preview-multibyte-no-cut")
		byCoord := map[coord]string{}
		for _, r := range c.Rows {
			if _, ok := byCoord[fidCoord(r)]; !ok {
				byCoord[fidCoord(r)] = r.RationaleMd
			}
		}
		for _, g := range fidGroups(t, exp, c.Name) {
			src := byCoord[coord{category: g.Category, target: g.Target}]
			if g.RationalePreview != src {
				t.Errorf("fixture broken: the expected preview for %s is not byte-identical to its input rationale_md -- the no-cut branch is what this case exists to pin, and a golden that shows a cut here was regenerated from a code-unit implementation", g.Target)
			}
			if fidUnits(g.RationalePreview) <= RationalePreviewMaxRunes {
				t.Errorf("fixture broken: the expected preview for %s is under the cap in UTF-16 code units too, so a code-unit implementation would also have returned it whole", g.Target)
			}
		}
	})

	t.Run("preview-trim-boundary", func(t *testing.T) {
		survivors := map[rune]bool{rune(0x00A0): false, rune(0xFEFF): false, rune(0x2028): false}
		trimmed := false
		for _, g := range fidGroups(t, exp, "preview-trim-boundary") {
			r := []rune(g.RationalePreview)
			if len(r) < 2 || string(r[len(r)-1]) != ell {
				t.Errorf("fixture broken: expected preview for %s does not end in an ellipsis, so it was not cut and the trim never ran", g.Target)
				continue
			}
			last := r[len(r)-2]
			if _, ok := survivors[last]; ok {
				survivors[last] = true
				if len(r) != RationalePreviewMaxRunes+1 {
					t.Errorf("fixture broken: expected preview for %s kept U+%04X but is %d runes, want %d", g.Target, last, len(r), RationalePreviewMaxRunes+1)
				}
			} else if len(r) == RationalePreviewMaxRunes {
				trimmed = true
			}
		}
		for ch, ok := range survivors {
			if !ok {
				t.Errorf("fixture broken: no expected preview ends (before the ellipsis) in U+%04X -- the golden no longer shows the server KEEPING a character JS's \\s strips, which is precisely what a golden regenerated from the \\s implementation looks like", ch)
			}
		}
		if !trimmed {
			t.Fatalf("fixture broken: no expected preview is %d runes -- the U+0020 control is what proves the trim runs at all, and without it 'these three characters survive' is satisfied by never trimming", RationalePreviewMaxRunes)
		}
	})

	t.Run("sort-stability-13-groups", func(t *testing.T) {
		c := fidCase(t, doc, "sort-stability-13-groups")
		groups := fidGroups(t, exp, c.Name)
		if len(groups) < 13 {
			t.Fatalf("fixture broken: the golden holds %d groups, need at least 13 for an unstable sort to be observable", len(groups))
		}
		assertFirstSeenWithinTies(t, c, groups)
	})
}

// assertFirstSeenWithinTies checks that, within each (run_count, open_count) value, the
// golden's groups appear in the order their coordinates FIRST appear in the case's rows.
// That is the property sort.SliceStable buys and sort.Slice does not, and it is stated
// against the fixture's own input rather than against either implementation.
func assertFirstSeenWithinTies(t *testing.T, c fidelityCase, groups []apitypes.JudgeRecommendationGroupDTO) {
	t.Helper()
	first := fidFirstSeen(c.Rows)
	type key struct{ runs, open int }
	last := map[key]int{}
	checked := 0
	for _, g := range groups {
		k := key{g.RunCount, g.OpenCount}
		idx, ok := first[coord{category: g.Category, target: g.Target}]
		if !ok {
			t.Fatalf("fixture broken: golden group %s/%s has no row in cases.json", g.Category, g.Target)
		}
		if prev, seen := last[k]; seen {
			checked++
			if idx < prev {
				t.Errorf("fixture broken: within the (run_count %d, open_count %d) tie the golden puts %s at input index %d after a group at index %d -- ties must keep first-seen order, and a golden regenerated from an UNSTABLE sort is exactly what this looks like", k.runs, k.open, g.Target, idx, prev)
			}
		}
		last[k] = idx
	}
	if checked == 0 {
		t.Fatal("fixture broken: the golden contains no two groups sharing a (run_count, open_count) value, so nothing here constrains tie ordering at all")
	}
}

func fidJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
