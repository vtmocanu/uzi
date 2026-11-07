package runquestion

import (
	"encoding/json"
	"strings"
	"testing"
)

func payloadJSON(t *testing.T, id string, qs ...Item) []byte {
	t.Helper()
	b, err := json.Marshal(Payload{QuestionID: id, Questions: qs})
	if err != nil {
		t.Fatalf("marshal question payload: %v", err)
	}
	return b
}

// A payload the notifier cannot use — unparseable, identity-less, or carrying no
// question with text — yields no post rather than a park nobody can explain or answer.
func TestParseRejectsUnusable(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"not json", []byte("{oops")},
		{"no question id", payloadJSON(t, "", Item{Question: "what?"})},
		{"no questions", payloadJSON(t, "q-1")},
		{"question with no text", payloadJSON(t, "q-1", Item{Header: "h", Question: "   "})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := Parse(tc.raw); ok {
				t.Fatalf("payload must be rejected: %s", tc.raw)
			}
		})
	}
	if p, ok := Parse(payloadJSON(t, " q-1 ",
		Item{Question: "a?"}, Item{Question: " "})); !ok ||
		p.QuestionID != "q-1" || len(p.Questions) != 1 {
		t.Fatalf("a usable payload must keep only questions carrying text, with a trimmed id: %+v %v", p, ok)
	}
}

func TestExcerpt(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"plain", payloadJSON(t, "q", Item{Question: "Should the cap count queued exports?"}), "Should the cap count queued exports?"},
		{"newline collapses before strip", payloadJSON(t, "q", Item{Question: "a\nb"}), "a b"},
		{"tab collapses before strip", payloadJSON(t, "q", Item{Question: "a\tb"}), "a b"},
		{"whitespace run", payloadJSON(t, "q", Item{Question: "a \n\t  b"}), "a b"},
		{"bidi override removed", payloadJSON(t, "q", Item{Question: "Should ‮we go?"}), "Should we go?"},
		{"zero-width removed", payloadJSON(t, "q", Item{Question: "ab​cd"}), "abcd"},
		{"exactly 280 unchanged", payloadJSON(t, "q", Item{Question: long(280)}), long(280)},
		{"281 capped to 280 with ellipsis", payloadJSON(t, "q", Item{Question: long(281)}), long(279) + "…"},
		{"trailing space before ellipsis trimmed", payloadJSON(t, "q", Item{Question: long(278) + " " + long(10)}), long(278) + "…"},
		{"more suffix", payloadJSON(t, "q", Item{Question: "first?"}, Item{Question: "second?"}, Item{Question: "third?"}), "first? (+2 more)"},
		{"more suffix outside the cap", payloadJSON(t, "q", Item{Question: long(500)}, Item{Question: "b"}), long(279) + "… (+1 more)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Excerpt(tc.raw)
			if !ok || got != tc.want {
				t.Fatalf("Excerpt = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
	if got, _ := Excerpt(payloadJSON(t, "q", Item{Question: long(281)})); len([]rune(got)) != ExcerptMaxRunes {
		t.Fatalf("capped excerpt must be %d runes, got %d", ExcerptMaxRunes, len([]rune(got)))
	}
}

func TestExcerptRejects(t *testing.T) {
	cases := map[string][]byte{
		"malformed json":      []byte("{oops"),
		"empty":               nil,
		"missing question id": payloadJSON(t, "", Item{Question: "what?"}),
		"no text":             payloadJSON(t, "q", Item{Question: "  "}),
		"no questions":        payloadJSON(t, "q"),
		"sanitizes to empty":  payloadJSON(t, "q", Item{Question: "‮​\u0007"}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := Excerpt(raw); ok || got != "" {
				t.Fatalf("Excerpt = %q, %v; want no excerpt", got, ok)
			}
		})
	}
}
