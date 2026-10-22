package store_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestManualCordonTextTerminalSequencesAndRunes(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		limit       int
	}{
		{" \x1b[31mFix\x1b[0m\nthis\t\u202e now ", "Fix this  now", 500},
		{"\x1b]0;hidden title\aVisible\x1b]8;;https://example.com\x1b\\ link\x1b]8;;\x1b\\", "Visible link", 500},
		{strings.Repeat("😀", 200) + "X", strings.Repeat("😀", 200), 200},
		{strings.Repeat("🚀", 500) + "X", strings.Repeat("🚀", 500), 500},
		{"\x1b]0;hidden\a\u202e", "", 200},
	} {
		if got := store.ManualCordonText(tc.input, tc.limit); got != tc.want {
			t.Fatalf("text=%q want=%q", got, tc.want)
		}
	}
}
