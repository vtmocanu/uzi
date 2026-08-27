package workersvc

import (
	"fmt"
	"strings"
	"testing"
)

// planChangedFilesParam is the persist-time trust boundary for the PRD #212
// plan-turn changed-file list: the paths originate in a cloned repo an attacker
// may control, so the helper must (a) preserve the repo_agents tri-state
// (nil pointer -> nil so the SQL COALESCE PRESERVES; a non-nil slice, even empty,
// -> a non-nil result so COALESCE REPLACES/clears — Decision 4), (b) strip
// control/bidi runes per line, and (c) cap the list with a synthetic truncation
// marker. The live-DB store test (internal/store) exercises the COALESCE at the
// SQL layer with raw slices; this table test gates the helper's own logic, which
// that test bypasses.
func TestPlanChangedFilesParam(t *testing.T) {
	t.Run("nil pointer stays nil (COALESCE preserves)", func(t *testing.T) {
		if got := planChangedFilesParam(nil); got != nil {
			t.Fatalf("nil pointer: got %#v, want nil (a nil param must COALESCE-preserve the column)", got)
		}
	})

	t.Run("non-nil empty stays non-nil empty (COALESCE clears)", func(t *testing.T) {
		empty := []string{}
		got := planChangedFilesParam(&empty)
		if got == nil {
			t.Fatalf("empty slice: got nil, want a non-nil empty slice (an empty non-nil param must COALESCE-REPLACE, i.e. clear a prior list)")
		}
		if len(got) != 0 {
			t.Fatalf("empty slice: got %#v, want empty", got)
		}
	})

	t.Run("non-empty passes through, order preserved", func(t *testing.T) {
		in := []string{" M src/app.ts", "?? notes.md", "A  x.go"}
		got := planChangedFilesParam(&in)
		if len(got) != len(in) {
			t.Fatalf("got %d lines, want %d: %#v", len(got), len(in), got)
		}
		for i := range in {
			if got[i] != in[i] {
				t.Fatalf("line %d: got %q, want %q", i, got[i], in[i])
			}
		}
	})

	t.Run("control and bidi runes are stripped per line", func(t *testing.T) {
		// An attacker-planted filename carrying a terminal escape (ESC) and a bidi
		// override (U+202E, RIGHT-TO-LEFT OVERRIDE) must not survive to storage.
		in := []string{" M src/a\x1b[31mb\u202ec.ts"}
		got := planChangedFilesParam(&in)
		if len(got) != 1 {
			t.Fatalf("got %d lines, want 1", len(got))
		}
		if strings.ContainsRune(got[0], '\x1b') || strings.ContainsRune(got[0], '\u202e') {
			t.Fatalf("control/bidi rune survived the clamp: %q", got[0])
		}
	})

	t.Run("length is clamped per line", func(t *testing.T) {
		in := []string{" M " + strings.Repeat("a", 5000)}
		got := planChangedFilesParam(&in)
		if len(got) != 1 {
			t.Fatalf("got %d lines, want 1", len(got))
		}
		if len(got[0]) > 512 {
			t.Fatalf("line not clamped: %d bytes, want <= 512", len(got[0]))
		}
	})

	t.Run("at the 200-line cap no marker is added", func(t *testing.T) {
		in := make([]string, 200)
		for i := range in {
			in[i] = fmt.Sprintf(" M f%d", i)
		}
		got := planChangedFilesParam(&in)
		if len(got) != 200 {
			t.Fatalf("got %d lines, want 200 (no marker at exactly the cap)", len(got))
		}
		if strings.Contains(got[len(got)-1], "more)") {
			t.Fatalf("unexpected truncation marker at exactly the cap: %q", got[len(got)-1])
		}
	})

	t.Run("over the cap truncates with an accurate marker", func(t *testing.T) {
		in := make([]string, 250)
		for i := range in {
			in[i] = fmt.Sprintf(" M f%d", i)
		}
		got := planChangedFilesParam(&in)
		// 200 real lines + 1 marker line.
		if len(got) != 201 {
			t.Fatalf("got %d lines, want 201 (200 kept + 1 marker)", len(got))
		}
		want := fmt.Sprintf("… (+%d more)", 250-200)
		if got[len(got)-1] != want {
			t.Fatalf("marker line = %q, want %q", got[len(got)-1], want)
		}
	})
}
