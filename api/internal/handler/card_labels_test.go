package handler

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// cardDTO.Labels must never reach the wire as JSON null (PRD #102 M6).
//
// The chain, which is why the assertions below are on the MARSHALLED bytes rather
// than on the slice: issues.labels is `jsonb NOT NULL DEFAULT '[]'`, and SQL NOT
// NULL does not exclude the jsonb scalar `null`. json.Unmarshal decodes that into
// a nil slice and returns NO error, so an error-branch default never fires; a nil
// slice then marshals back to `null`, and the web calls .filter on it in
// chipLabels and .includes at two Board.tsx sites.
//
// Pre-existing and unreachable until M6: the sync filter guaranteed every cached
// issue carried the PRD label, so labels was never empty. The additive fetch is
// exactly what caches an issue with no labels at all, which is the ordinary shape
// of a freshly filed one.

func labelsFieldOf(t *testing.T, card cardDTO) string {
	t.Helper()
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	var probe struct {
		Labels json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	return string(probe.Labels)
}

func TestCardLabelsNeverMarshalAsNull(t *testing.T) {
	cases := []struct {
		name string
		// stored is the literal jsonb value in the issues row.
		stored string
		want   string
	}{
		// The case that matters. Note it is NOT a decode error, which is why the
		// pre-existing error branch could not catch it.
		{"jsonb null", `null`, `[]`},
		{"empty array", `[]`, `[]`},
		{"labels present", `["PRD","bug"]`, `["PRD","bug"]`},
		// A genuinely broken value still has to produce a renderable card.
		{"undecodable", `{not json`, `[]`},
		{"empty column value", ``, `[]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := store.Issue{ForgeIssueIid: 1, Title: "t", State: "opened", Labels: []byte(tc.stored)}

			t.Run("issueToCard", func(t *testing.T) {
				if got := labelsFieldOf(t, issueToCard(is, nil, "gitlab")); got != tc.want {
					t.Fatalf("labels = %s, want %s", got, tc.want)
				}
			})
			t.Run("assembleCards", func(t *testing.T) {
				cards := assembleCards([]store.Issue{is}, nil, nil, nil, uuid.Nil, "gitlab")
				if len(cards) != 1 {
					t.Fatalf("expected one card, got %d", len(cards))
				}
				if got := labelsFieldOf(t, cards[0]); got != tc.want {
					t.Fatalf("labels = %s, want %s", got, tc.want)
				}
			})
		})
	}
}

// TestNonNilLabelsGuardsTheThirdCardBuilder covers handler/issues.go's create
// path, which builds a cardDTO inline from the FORGE's slice rather than from the
// cache and so never passes through decodeLabels. It is the site the M6 handover
// notes did not name, and a fix applied only to board.go's two builders leaves it
// shipping null.
func TestNonNilLabelsGuardsTheThirdCardBuilder(t *testing.T) {
	if got := labelsFieldOf(t, cardDTO{Labels: nonNilLabels(nil)}); got != `[]` {
		t.Fatalf("labels = %s, want []", got)
	}
	if got := labelsFieldOf(t, cardDTO{Labels: nonNilLabels([]string{"PRD"})}); got != `["PRD"]` {
		t.Fatalf("labels = %s, want [\"PRD\"]", got)
	}
}

// TestDecodeLabelsIsTheOnlyDecodePath is a structural guard, not a behavioural
// one: it fails if a future card builder re-introduces a bare json.Unmarshal into
// a []string, which is how this bug is reintroduced. Both existing builders read
// their labels through decodeLabels.
func TestDecodeLabelsIsTheOnlyDecodePath(t *testing.T) {
	src := readBoardSource(t)
	if n := strings.Count(src, "json.Unmarshal(is.Labels"); n != 0 {
		t.Fatalf("found %d direct unmarshal(s) of a cached issue's labels in board.go; route them through decodeLabels so the non-nil guarantee holds", n)
	}
}

// readBoardSource reads board.go from disk. The file is the artifact under test
// here, so reading it is the point rather than a shortcut.
func readBoardSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("board.go")
	if err != nil {
		t.Fatalf("read board.go: %v", err)
	}
	return string(b)
}
