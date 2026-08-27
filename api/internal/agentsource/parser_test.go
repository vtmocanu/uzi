package agentsource

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// goldenNote is one expected note in the differential golden.
type goldenNote struct {
	Reason string   `json:"reason"`
	Name   string   `json:"name"`
	Tools  []string `json:"tools,omitempty"`
}

// goldenEntry is the hand-authored expected outcome for one corpus file. It was
// authored by reasoning from repoagents.ts, NOT snapshotted from either parser.
type goldenEntry struct {
	OK          bool         `json:"ok"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	PromptBody  string       `json:"prompt_body"`
	Tools       []string     `json:"tools"`
	Model       string       `json:"model"`
	Skip        string       `json:"skip"`
	Notes       []goldenNote `json:"notes"`
}

const parityDir = "testdata/parity"

func loadGolden(t *testing.T) map[string]goldenEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(parityDir, "expected.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden map[string]goldenEntry
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	return golden
}

// TestParityGolden is the Go half of the differential test (architect N4). It runs
// the Go parser over the committed corpus and asserts deep equality with the
// hand-authored golden. agent/test/agentsource-parity.test.ts runs repoagents.ts
// over the SAME corpus and golden; the two agreeing is what proves the parsers agree.
func TestParityGolden(t *testing.T) {
	golden := loadGolden(t)

	entries, err := os.ReadDir(parityDir)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var mdFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			mdFiles = append(mdFiles, e.Name())
		}
	}
	if len(mdFiles) == 0 {
		t.Fatal("corpus has no .md files")
	}

	for _, name := range mdFiles {
		name := name
		t.Run(name, func(t *testing.T) {
			want, ok := golden[name]
			if !ok {
				t.Fatalf("no golden entry for fixture %q", name)
			}
			raw, err := os.ReadFile(filepath.Join(parityDir, name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			slug := safeLabel(strings.TrimSuffix(name, ".md"))
			got := ParseFile(raw, slug)

			if got.OK != want.OK {
				t.Fatalf("OK = %v, want %v (skip=%q name=%q)", got.OK, want.OK, got.Skip, got.Name)
			}
			if !want.OK {
				if string(got.Skip) != want.Skip {
					t.Errorf("skip reason = %q, want %q", got.Skip, want.Skip)
				}
				if got.Name != want.Name {
					t.Errorf("skip name = %q, want %q", got.Name, want.Name)
				}
				return
			}

			if got.Role.Name != want.Name {
				t.Errorf("name = %q, want %q", got.Role.Name, want.Name)
			}
			if got.Role.Description != want.Description {
				t.Errorf("description = %q, want %q", got.Role.Description, want.Description)
			}
			if got.Role.PromptBody != want.PromptBody {
				t.Errorf("prompt_body = %q, want %q", got.Role.PromptBody, want.PromptBody)
			}
			if got.Role.Model != want.Model {
				t.Errorf("model = %q, want %q", got.Role.Model, want.Model)
			}
			if !equalStrings(got.Role.Tools, want.Tools) {
				t.Errorf("tools = %#v, want %#v", got.Role.Tools, want.Tools)
			}
			assertNotes(t, got.Notes, want.Notes)
		})
	}
}

// equalStrings treats a nil slice and an absent (null) golden value as equal
// (both mean inherit), and otherwise compares element-wise.
func equalStrings(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

func assertNotes(t *testing.T, got []Note, want []goldenNote) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("notes = %#v, want %#v", got, want)
	}
	for i := range got {
		if string(got[i].Reason) != want[i].Reason {
			t.Errorf("note[%d].reason = %q, want %q", i, got[i].Reason, want[i].Reason)
		}
		if got[i].Name != want[i].Name {
			t.Errorf("note[%d].name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if !equalStrings(got[i].Tools, want[i].Tools) {
			t.Errorf("note[%d].tools = %#v, want %#v", i, got[i].Tools, want[i].Tools)
		}
	}
}

// TestParseSetCapsAndDedupe covers the set-level wrapper the M3-part-B caller uses:
// the 16-file cap (over_limit), the per-file 64KB cap (too_large), duplicate-name
// detection (first wins), and sorted output. Mirrors repoagents.ts detectRepoAgents.
func TestParseSetCapsAndDedupe(t *testing.T) {
	valid := func(name, body string) []byte {
		return []byte("---\nname: " + name + "\ndescription: ok.\n---\n\n" + body + "\n")
	}
	// roleName generates a unique, valid, lexically-sortable kebab-case name for the i-th
	// synthetic role file (a00, a01, … a99). Used to build rosters of a chosen size.
	roleName := func(i int) string {
		return "a" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}

	t.Run("duplicate name, first file by name wins", func(t *testing.T) {
		got := ParseSet([]SourceFile{
			{Name: "z-second.md", Data: valid("dup", "loser")},
			{Name: "a-first.md", Data: valid("dup", "winner")},
		})
		if len(got.Roles) != 1 {
			t.Fatalf("roles = %d, want 1", len(got.Roles))
		}
		if got.Roles[0].PromptBody != "winner\n" {
			t.Errorf("winner body = %q, want %q", got.Roles[0].PromptBody, "winner\n")
		}
		if len(got.Notes) != 1 || got.Notes[0].Reason != NoteDuplicate || got.Notes[0].Name != "dup" {
			t.Errorf("notes = %#v, want one duplicate note for dup", got.Notes)
		}
	})

	// too_large is kept Go-only rather than shared in the differential corpus: pinning
	// it there means committing a >64KB fixture, which is impractical to carry in the
	// shared corpus. The duplicate case above IS the shared set-level differential
	// (agent/test/agentsource-parity.test.ts asserts the same duplicate/first-wins
	// outcome through detectRepoAgents).
	t.Run("oversized file is skipped without parsing", func(t *testing.T) {
		big := []byte("---\nname: big\ndescription: ok.\n---\n\n" + strings.Repeat("x", MaxBytes+1))
		got := ParseSet([]SourceFile{
			{Name: "big.md", Data: big},
			{Name: "small.md", Data: valid("small", "body")},
		})
		if len(got.Roles) != 1 || got.Roles[0].Name != "small" {
			t.Fatalf("roles = %#v, want [small]", got.Roles)
		}
		if len(got.Notes) != 1 || got.Notes[0].Reason != NoteTooLarge || got.Notes[0].Name != "big" {
			t.Errorf("notes = %#v, want one too_large note for big", got.Notes)
		}
	})

	// A roster BELOW the cap (the published product-agents/ size, ~14-15 files) must
	// stage in full with NO over_limit note — the headroom guarantee issue #703 is about.
	t.Run("full published-size roster stages with no over_limit note", func(t *testing.T) {
		const rosterSize = 15 // > the published count, still comfortably < MaxFiles(32)
		if rosterSize > MaxFiles {
			t.Fatalf("test bug: rosterSize %d must be <= MaxFiles %d", rosterSize, MaxFiles)
		}
		var files []SourceFile
		for i := 0; i < rosterSize; i++ {
			n := roleName(i)
			files = append(files, SourceFile{Name: n + ".md", Data: valid(n, "body")})
		}
		got := ParseSet(files)
		if len(got.Roles) != rosterSize {
			t.Fatalf("roles = %d, want %d (whole roster kept)", len(got.Roles), rosterSize)
		}
		for _, note := range got.Notes {
			if note.Reason == NoteOverLimit {
				t.Errorf("unexpected over_limit note for a roster of %d (<= MaxFiles %d): %#v", rosterSize, MaxFiles, note)
			}
		}
		if len(got.Notes) != 0 {
			t.Errorf("notes = %#v, want none for an all-valid roster", got.Notes)
		}
	})

	// A roster ABOVE the cap still truncates to MaxFiles and reports the correct count,
	// so the DoS bound still guards after the raise.
	t.Run("caps at MaxFiles with one aggregated over_limit note, sorted output", func(t *testing.T) {
		const total = MaxFiles + 8
		var files []SourceFile
		for i := 0; i < total; i++ {
			n := roleName(i)
			files = append(files, SourceFile{Name: n + ".md", Data: valid(n, "body")})
		}
		got := ParseSet(files)
		if len(got.Roles) != MaxFiles {
			t.Fatalf("roles = %d, want %d", len(got.Roles), MaxFiles)
		}
		wantLast := roleName(MaxFiles - 1)
		if got.Roles[0].Name != roleName(0) || got.Roles[MaxFiles-1].Name != wantLast {
			t.Errorf("roles range = [%s..%s], want [%s..%s]", got.Roles[0].Name, got.Roles[MaxFiles-1].Name, roleName(0), wantLast)
		}
		if len(got.Notes) != 1 || got.Notes[0].Reason != NoteOverLimit || got.Notes[0].Count != total-MaxFiles {
			t.Errorf("notes = %#v, want one over_limit note count=%d", got.Notes, total-MaxFiles)
		}
	})

	t.Run("a skipped file surfaces its note; non-md files are ignored", func(t *testing.T) {
		got := ParseSet([]SourceFile{
			{Name: "good.md", Data: valid("good", "body")},
			{Name: "bad.md", Data: []byte("no frontmatter here\n")},
			{Name: "notes.txt", Data: valid("ignored", "body")},
		})
		if len(got.Roles) != 1 || got.Roles[0].Name != "good" {
			t.Fatalf("roles = %#v, want [good]", got.Roles)
		}
		if len(got.Notes) != 1 || got.Notes[0].Reason != NoteInvalid || got.Notes[0].Name != "bad" {
			t.Errorf("notes = %#v, want one invalid note for bad", got.Notes)
		}
	})

	// issue #715: a conventional non-role doc (README, no frontmatter) fails to parse as
	// a role and, because its basename is reserved, is dropped silently on the invalid
	// path AFTER ParseFile — so it never becomes an `invalid` note or counts toward the
	// "failed" total. The drop is SCOPED to reserved doc names on the invalid path: a
	// genuinely malformed role file with an ordinary name is STILL reported invalid, so
	// this is not a blanket "ignore every no-frontmatter file".
	t.Run("README is skipped silently while a malformed role file still fails", func(t *testing.T) {
		got := ParseSet([]SourceFile{
			{Name: "README.md", Data: []byte("# The roster\n\nJust docs, no frontmatter.\n")},
			{Name: "coder.md", Data: valid("coder", "body")},
			{Name: "broken.md", Data: []byte("no frontmatter here\n")},
		})
		if len(got.Roles) != 1 || got.Roles[0].Name != "coder" {
			t.Fatalf("roles = %#v, want [coder]", got.Roles)
		}
		// README yields NO note; the malformed role (broken) is still an invalid note.
		if len(got.Notes) != 1 || got.Notes[0].Reason != NoteInvalid || got.Notes[0].Name != "broken" {
			t.Fatalf("notes = %#v, want exactly one invalid note for broken (README excluded silently)", got.Notes)
		}
		// The "failed"-equivalent (invalid notes) is exactly the one malformed role, not README.
		failed := 0
		for _, n := range got.Notes {
			if n.Reason == NoteInvalid {
				failed++
			}
			if strings.EqualFold(n.Name, "readme") {
				t.Errorf("README produced a note %#v; it must be dropped on the invalid path, not surfaced", n)
			}
		}
		if failed != 1 {
			t.Errorf("invalid-note (failed) count = %d, want 1 (only the malformed role, README excluded)", failed)
		}
	})

	// The reserved-doc skip is case-insensitive and covers the curated basename set, so a
	// roster of ONLY reserved docs stages zero roles with ZERO notes — a clean sync reads
	// "0 failed" rather than one-per-doc.
	t.Run("reserved doc names are skipped case-insensitively, no notes", func(t *testing.T) {
		got := ParseSet([]SourceFile{
			{Name: "readme.md", Data: []byte("lower.\n")},
			{Name: "License.md", Data: []byte("mixed.\n")},
			{Name: "CONTRIBUTING.md", Data: []byte("upper.\n")},
			{Name: "CHANGELOG.md", Data: []byte("x.\n")},
			{Name: "Code_Of_Conduct.md", Data: []byte("x.\n")},
			{Name: "SECURITY.md", Data: []byte("x.\n")},
		})
		if len(got.Roles) != 0 {
			t.Fatalf("roles = %#v, want none (all reserved docs)", got.Roles)
		}
		if len(got.Notes) != 0 {
			t.Errorf("notes = %#v, want none — reserved docs never count as failed", got.Notes)
		}
	})

	// The name alone is NOT disqualifying: a reserved-named file that PARSES as a valid
	// role is kept. `security`, `readme`, … all pass AGENT_NAME_RE, so `<name>.md` is a
	// legitimate role file — the doc-drop fires only on the invalid-parse path. Here
	// security.md (valid frontmatter) is kept while README.md (no frontmatter) is
	// dropped, in the same set.
	t.Run("a reserved name with valid frontmatter is staged as a role", func(t *testing.T) {
		got := ParseSet([]SourceFile{
			{Name: "security.md", Data: valid("security", "the security reviewer")},
			{Name: "README.md", Data: []byte("# Docs, no frontmatter.\n")},
		})
		if len(got.Roles) != 1 || got.Roles[0].Name != "security" {
			t.Fatalf("roles = %#v, want [security] (valid reserved-named role kept)", got.Roles)
		}
		if len(got.Notes) != 0 {
			t.Errorf("notes = %#v, want none (README dropped silently, security kept)", got.Notes)
		}
	})
}

// TestParseFileRejectsInvalidUTF8 pins the deliberate api-side divergence #1 (see the
// doc comment on ParseFile): non-UTF-8 input is rejected as `invalid` BEFORE any
// rune-ranging, whereas repoagents.ts reads lossily (invalid bytes → U+FFFD) and
// parses on. This is a Go-only divergence, so it is NOT in the shared parity corpus.
// A lone C1 byte (0x9B) is the worked example: it would decode to U+FFFD and slip the
// description Cc/Cf check while the raw byte survived into the stored description — a
// control-byte bypass and a Postgres UTF-8 insert failure on a "parsed-OK" role.
func TestParseFileRejectsInvalidUTF8(t *testing.T) {
	// A well-formed role file with a single raw C1 (0x9B) byte spliced into the
	// description value — valid frontmatter shape, but not valid UTF-8.
	raw := []byte("---\nname: rogue\ndescription: bad\x9bbyte.\n---\n\nbody\n")
	if utf8.Valid(raw) {
		t.Fatal("test fixture is unexpectedly valid UTF-8")
	}
	got := ParseFile(raw, "rogue")
	if got.OK {
		t.Fatalf("OK = true, want false (non-UTF-8 must be rejected)")
	}
	if got.Skip != NoteInvalid {
		t.Errorf("skip = %q, want %q", got.Skip, NoteInvalid)
	}
	if got.Name != "rogue" {
		t.Errorf("name = %q, want %q (the slug)", got.Name, "rogue")
	}
}

// TestValidModelRejectsCf pins the deliberate api-side divergence #2 (see validModel):
// a Cf-bearing model becomes `model_ignored` (a fail-safe clamp — the role is KEPT and
// inherits the run default), whereas repoagents.ts's isValidModel honors Cf. Go-only,
// so not in the shared corpus.
func TestValidModelRejectsCf(t *testing.T) {
	// model: opus + U+200B (zero-width space, category Cf, not JS \s).
	raw := []byte("---\nname: cf-model\ndescription: model carries a Cf char.\nmodel: opus\u200b\n---\n\nbody\n")
	got := ParseFile(raw, "cf-model")
	if !got.OK {
		t.Fatalf("OK = false (skip=%q), want true — a Cf model clamps, not skips", got.Skip)
	}
	if got.Role.Model != "" {
		t.Errorf("model = %q, want empty (Cf model must be ignored)", got.Role.Model)
	}
	if len(got.Notes) != 1 || got.Notes[0].Reason != NoteModelIgnored || got.Notes[0].Name != "cf-model" {
		t.Errorf("notes = %#v, want one model_ignored note for cf-model", got.Notes)
	}
	// And the plain-ASCII control: a Cf-free full id is still honored.
	rawOK := []byte("---\nname: cf-model\ndescription: clean model.\nmodel: claude-opus-4-8\n---\n\nbody\n")
	okRes := ParseFile(rawOK, "cf-model")
	if !okRes.OK || okRes.Role.Model != "claude-opus-4-8" {
		t.Errorf("clean model: OK=%v model=%q, want OK=true model=%q", okRes.OK, okRes.Role.Model, "claude-opus-4-8")
	}
}
