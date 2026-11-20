package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanCrossCheckCandidateMilestoneAdmissibility(t *testing.T) {
	token := "glpat-" + "abcdefghijklmnopqrst"
	entries := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`{"id":"m%d","title":"T"}`, i)
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	one := func(id, title string) string { return `[{"id":"` + id + `","title":"` + title + `"}]` }
	const (
		refused  = false
		accepted = true
	)
	for _, tc := range []struct {
		name       string
		milestones string
		want       bool
		base       string // "new": admitted on base; "base": already refused on base
	}{
		{"null entry", `[null]`, refused, "new"},
		{"empty object", `[{}]`, refused, "new"},
		{"missing id", `[{"title":"t"}]`, refused, "new"},
		{"missing title", `[{"id":"m1"}]`, refused, "new"},
		{"blank id", one("", "t"), refused, "new"},
		{"blank title", one("m1", ""), refused, "new"},
		{"whitespace id", one("   ", "t"), refused, "new"},
		{"whitespace title", one("m1", "   "), refused, "new"},
		{"blank id and title", `[{"id":"","title":""}]`, refused, "new"},
		{"extra key", `[{"id":"m1","title":"t","metadata":{"id":12}}]`, refused, "new"},
		{"capitalized keys", `[{"ID":"m1","Title":"t"}]`, refused, "new"},
		{"capitalized id key", `[{"ID":"m1","title":"t"}]`, refused, "new"},
		{"capitalized title key", `[{"id":"m1","Title":"t"}]`, refused, "new"},
		{"duplicate ids", `[{"id":"m1","title":"a"},{"id":"m1","title":"b"}]`, refused, "new"},
		{"interior space id", one("m 1", "t"), refused, "new"},
		{"slash id", one("a/b", "t"), refused, "new"},
		{"leading dot id", one(".m1", "t"), refused, "new"},
		{"token-shaped id", one(token, "t"), refused, "new"},
		{"51 entries", entries(51), refused, "new"},
		{"201-rune title", one("m1", strings.Repeat("a", 201)), refused, "new"},

		{"string entry", `["x"]`, refused, "base"},
		{"number entry", `[1]`, refused, "base"},
		{"65-char id", one(strings.Repeat("a", 65), "t"), refused, "base"},
		{"numeric id", `[{"id":12,"title":"t"}]`, refused, "base"},
		{"65 entries", entries(65), refused, "base"},

		{"empty list", `[]`, accepted, "base"},
		{"50 entries", entries(50), accepted, "base"},
		{"200-rune title", one("m1", strings.Repeat("a", 200)), accepted, "base"},
		{"multibyte title", one("m1", "Überprüfung 検証"), accepted, "base"},
		{"scrub-dependent title", one("m1", " "+token+" \u202e title "), accepted, "base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := PlanCrossCheckCandidate{PlanMd: "Plan", Milestones: json.RawMessage(tc.milestones),
				RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
				BaseCommit: strings.Repeat("a", 40)}
			got, err := NormalizePlanCrossCheckCandidate(c)
			if tc.want == refused {
				if !errors.Is(err, ErrCrossCheckRefused) {
					t.Fatalf("[%s] want ErrCrossCheckRefused, got %v", tc.base, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("admissible candidate refused: %v", err)
			}
			var decoded []Milestone
			if err := json.Unmarshal(c.Milestones, &decoded); err != nil {
				t.Fatal(err)
			}
			digest, err := got.Digest()
			if err != nil {
				t.Fatal(err)
			}
			caps, tools, size := []string{}, []string{}, "s"
			plan, gen := "Plan", int64(1)
			req := StateRequest{State: "running", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &decoded,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size,
				CandidateDigest: hex.EncodeToString(digest)}
			bound, err := normalizeBoundCrossCheckReport(store.Run{Kind: "issue"}, req, plan)
			if err != nil {
				t.Fatalf("state-write normalization refused an admitted candidate: %v", err)
			}
			if string(bound.candidate.Milestones) != string(got.Milestones) {
				t.Fatalf("bound milestones differ from submission: %.200s vs %.200s", bound.candidate.Milestones, got.Milestones)
			}
		})
	}
}

func TestPlanCrossCheckCandidateMilestoneBytesAndDigestPin(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`[]`, `[]`},
		{`[{"title":"T","id":"m1"}]`, `[{"id":"m1","title":"T"}]`},
	} {
		c := PlanCrossCheckCandidate{PlanMd: "Plan", Milestones: json.RawMessage(tc.in),
			RequiredCapabilities: []string{}, RequiredTools: []string{}, SizeClass: "s",
			BaseCommit: strings.Repeat("a", 40)}
		got, err := NormalizePlanCrossCheckCandidate(c)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Milestones) != tc.want {
			t.Fatalf("normalized %s = %s, want %s", tc.in, got.Milestones, tc.want)
		}
		digest, err := got.Digest()
		if err != nil {
			t.Fatal(err)
		}
		canonical := c
		canonical.Milestones = json.RawMessage(tc.want)
		again, err := NormalizePlanCrossCheckCandidate(canonical)
		if err != nil {
			t.Fatal(err)
		}
		againDigest, err := again.Digest()
		if err != nil || hex.EncodeToString(againDigest) != hex.EncodeToString(digest) {
			t.Fatalf("digest not stable for %s: %x vs %x err=%v", tc.in, againDigest, digest, err)
		}
	}
}
