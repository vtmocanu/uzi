package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Every registered Tier-A sketch must yield at least one non-empty frame in BOTH
// themes. This is a property over the whole registry, not the roster — a new
// branch-local sketch is covered automatically, and a sketch that ships an empty
// frame (a broken copy of the template) reddens here.
func TestSketchRegistryFramesNonEmpty(t *testing.T) {
	for name, sk := range sketches {
		if sk.frames == nil {
			continue // Tier-B-only sketch: no static frames to check.
		}
		for _, dark := range []bool{true, false} {
			frames := sk.frames(dark)
			if len(frames) == 0 {
				t.Errorf("sketch %q frames(%v) returned no frames", name, dark)
				continue
			}
			for i, f := range frames {
				if strings.TrimSpace(f) == "" {
					t.Errorf("sketch %q frames(%v)[%d] is empty", name, dark, i)
				}
			}
		}
	}
}

// The permanent `template` sketch is the copyable example and must always be
// registered with frames and a title. This reads the `title` field, which is
// otherwise write-only. It deliberately does NOT assert the registry size or any
// other name, so a branch-local sketch cannot break it.
func TestSketchTemplateRegistered(t *testing.T) {
	sk, ok := sketches["template"]
	if !ok {
		t.Fatal(`sketches["template"] is not registered`)
	}
	if sk.frames == nil {
		t.Error("template sketch has nil frames")
	}
	if sk.title == "" {
		t.Error("template sketch has an empty title")
	}
}

// content reads the rendered string out of a tea.View, mirroring how the uxlab
// generator does it (m.View().Content).
func content(v tea.View) string { return v.Content }

// The Tier-A host renders without panic across a key sequence that exercises the
// paging clamps and the theme toggle. Driving many `n` past the last frame then
// rendering must not index out of range, and the theme toggle must re-clamp.
func TestSketchHostRendersAndClamps(t *testing.T) {
	h := newSketchHost(sketches["template"].frames)

	if got := content(h.View()); strings.TrimSpace(got) == "" {
		t.Fatalf("initial host View() is empty")
	}

	// press drives one key through handleKey and re-renders, asserting no panic and
	// (with a non-empty frame slice) non-empty content.
	press := func(k string) {
		t.Helper()
		next, _ := h.handleKey(k)
		var ok bool
		h, ok = next.(sketchHost)
		if !ok {
			t.Fatalf("handleKey(%q) returned a non-sketchHost model %T", k, next)
		}
		if got := content(h.View()); len(h.cur) > 0 && strings.TrimSpace(got) == "" {
			t.Fatalf("View() after key %q is empty", k)
		}
	}

	// Advance well past the last frame: the paging must clamp, not panic or overflow.
	for i := 0; i < 10; i++ {
		press("n")
	}
	press(keyRight)
	// Go back to the start.
	for i := 0; i < 10; i++ {
		press("p")
	}
	press(keyLeft)
	// Toggle theme (re-evaluates frames and re-clamps), then page again.
	press("t")
	for i := 0; i < 10; i++ {
		press("n")
	}
	press("t")
	// Open help.
	press(keyHelp)
}

// The paging clamp and the theme re-clamp are pinned here against a sketch whose two
// themes return DIFFERENT frame counts (dark→3, light→1). Asserting on the host's
// frameIdx STATE — not the rendered content — is what makes these branches
// falsifiable: View() clamps the index defensively before rendering, so a rendered
// frame stays valid even if handleKey's clamps were removed; only the state exposes
// the regression. (template has 2 frames in both themes and cannot exercise this.)
func TestSketchHostPagingAndThemeReclamp(t *testing.T) {
	asym := func(dark bool) []string {
		if dark {
			return []string{"dark-0", "dark-1", "dark-2"}
		}
		return []string{"light-0"}
	}
	h := newSketchHost(asym) // defaults dark → 3 frames

	step := func(k string) {
		t.Helper()
		next, _ := h.handleKey(k)
		h = next.(sketchHost)
	}

	// Over-page: 5×"n" on a 3-frame slice must clamp frameIdx at len-1 == 2.
	// (Drop handleKey's "n" clamp and frameIdx would run to 5 here.)
	for i := 0; i < 5; i++ {
		step("n")
	}
	if h.frameIdx != 2 {
		t.Fatalf("after over-paging, frameIdx = %d, want 2 (clamped at len-1)", h.frameIdx)
	}

	// Toggle to the light theme (1 frame). frameIdx was 2 and must re-clamp to 0.
	// (Drop the "t" re-clamp and frameIdx would stay 2 with only 1 frame present.)
	step("t")
	if h.frameIdx != 0 {
		t.Fatalf("after theme toggle to a 1-frame theme, frameIdx = %d, want 0 (re-clamped)", h.frameIdx)
	}
	if got := content(h.View()); got != "light-0" {
		t.Fatalf("after re-clamp, View() = %q, want the sole light frame %q", got, "light-0")
	}
}

// stubSketchModel is a minimal tea.Model used to prove the Tier-B dispatch path.
type stubSketchModel struct{}

func (stubSketchModel) Init() tea.Cmd { return nil }
func (m stubSketchModel) Update(tea.Msg) (tea.Model, tea.Cmd) {
	return m, nil
}
func (stubSketchModel) View() tea.View {
	var v tea.View
	v.SetContent("stub sketch model")
	return v
}

// sketchModel dispatches to a sketch's own Tier-B model when one is supplied, and to
// the generic Tier-A host otherwise. Both arms are exercised so the dispatch is
// tested, not merely un-gated.
func TestSketchModelDispatch(t *testing.T) {
	// Tier B: a sketch supplying its own model.
	sk := sketch{title: "stub", model: func() tea.Model { return stubSketchModel{} }}
	m := sketchModel(sk)
	if _, ok := m.(stubSketchModel); !ok {
		t.Fatalf("sketchModel with a model closure returned %T, want stubSketchModel", m)
	}
	if got := content(m.View()); strings.TrimSpace(got) == "" {
		t.Errorf("Tier-B model View() is empty")
	}

	// Tier A: a sketch with only frames dispatches to the generic host.
	a := sketchModel(sketch{frames: sketches["template"].frames})
	if _, ok := a.(sketchHost); !ok {
		t.Fatalf("sketchModel with only frames returned %T, want sketchHost", a)
	}
}

// A bare `--sketch` on a TTY lists the registered sketches and exits OK, rather than
// launching a program. The list always contains the permanent `template`.
func TestSketchCLIListsOnTTY(t *testing.T) {
	env := fakeEnv(&uzicli.FakeClient{})
	env.StdoutTTY = true
	out, _, code := runCLI(t, env, "tui", "--sketch")
	if code != uzicli.ExitOK {
		t.Fatalf("bare --sketch exit = %d, want %d", code, uzicli.ExitOK)
	}
	if !strings.Contains(out, "template") {
		t.Errorf("bare --sketch did not list the template sketch:\n%s", out)
	}
}

// An unknown sketch name lists rather than errors — the discovery affordance.
func TestSketchCLIUnknownNameLists(t *testing.T) {
	env := fakeEnv(&uzicli.FakeClient{})
	env.StdoutTTY = true
	out, _, code := runCLI(t, env, "tui", "--sketch", "definitely-not-a-real-sketch")
	if code != uzicli.ExitOK {
		t.Fatalf("unknown --sketch exit = %d, want %d", code, uzicli.ExitOK)
	}
	if !strings.Contains(out, "template") {
		t.Errorf("unknown --sketch name did not fall back to listing:\n%s", out)
	}
}

// On a non-TTY the tui guard fires BEFORE any dispatch, so even a valid sketch name
// returns a usage error and never launches a program (which would hang on the empty
// test stdin).
func TestSketchCLINonTTYGuard(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "tui", "--sketch", "template")
	if code != uzicli.ExitUsage {
		t.Fatalf("non-TTY --sketch exit = %d, want %d (usage guard)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "terminal") {
		t.Errorf("non-TTY guard message should mention the terminal requirement:\n%s", errOut)
	}
}
