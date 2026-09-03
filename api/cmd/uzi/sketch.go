package main

// The sketch harness — a throwaway surface for previewing a not-yet-built TUI feature
// (PRD #1061). A `sketch` is authored once and yields both a live interactive preview
// (`uzi tui --sketch <name>`) and, via the uxlab generator, light/dark screenshots.
//
// Sketches render PURE STRINGS / local state and are passed NO uzicli.Client by
// construction: the keepable half (the lipgloss View shape/layout) is meant to be
// lifted into the real tuiModel, at which point the sketch is DELETED, not maintained.
// That "no client" structure is what keeps this from becoming a second maintained TUI
// (the retired `factoryui`). Exactly one permanent `template` sketch ships as the
// copyable example; every other sketch is a branch-local throwaway.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// sketch is one prototype. Tier A supplies static `frames`; Tier B optionally supplies
// its own `model` for key-driven feel. `frames` may be nil only for a Tier-B-only sketch.
type sketch struct {
	title  string
	frames func(dark bool) []string // Tier A: static frames; nil only for a Tier-B-only sketch
	model  func() tea.Model         // Tier B: optional; when set, the live preview runs this
}

// sketches is the registry both the runtime `--sketch` command and the uxlab screenshot
// generator consume, so a sketch authored once is previewable live and screenshotted.
var sketches = map[string]sketch{
	"template": {
		title:  "template",
		frames: templateFrames,
	},
}

// sketchNames returns the registered sketch keys in sorted order (for `--sketch` listing).
func sketchNames() []string {
	names := make([]string, 0, len(sketches))
	for name := range sketches {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---- layout primitives ----------------------------------------------------
//
// Small lipgloss helpers that source their colours from the SHIPPED palette
// (newPalette(dark), tui_render.go) so a sketch inherits the real TUI's palette and
// cannot drift into ad-hoc colours. They exist to be copied into a new sketch.

// header renders a bold tungsten title line.
func header(p palette, title string) string {
	return p.title.Render(title)
}

// pane renders body inside the palette's bordered, padded structural box.
func pane(p palette, body string) string {
	return p.box.Render(body)
}

// statusbar renders a faint key-hint line.
func statusbar(p palette, hint string) string {
	return p.faint.Render(hint)
}

// templateFrames is the permanent minimal copyable example: a header, a pane with a
// couple of lines, and a statusbar, built ONLY from the primitives + newPalette(dark).
// Two frames so the frame-host paging is demonstrable.
func templateFrames(dark bool) []string {
	p := newPalette(dark)
	frame := func(status string, lines ...string) string {
		return strings.Join([]string{
			header(p, "sketch: template"),
			"",
			pane(p, strings.Join(lines, "\n")),
			"",
			statusbar(p, status),
		}, "\n")
	}
	return []string{
		frame("n/→ next · p/← prev · t theme · q quit",
			"This is a sketch frame.",
			"Copy this template to prototype a new TUI view.",
			p.sel.Render("Colours come from the shipped palette."),
		),
		frame("frame 2 of 2 · p/← prev · t theme · q quit",
			"A second frame demonstrates the host's paging.",
			p.faint.Render("Everything here is static text — no client, no API."),
		),
	}
}

// ---- generic Tier A host ---------------------------------------------------

// sketchHost renders a Tier A sketch's current frame fullscreen with paging and a
// light/dark toggle. It holds no client and issues no commands.
type sketchHost struct {
	frames        func(dark bool) []string
	dark          bool
	cur           []string // frames(dark), re-evaluated on a theme toggle
	frameIdx      int
	width, height int
	showHelp      bool
}

var _ tea.Model = sketchHost{}

// newSketchHost builds a host over a sketch's frames-producing closure, defaulting to
// the dark palette (the TUI's own default).
func newSketchHost(frames func(dark bool) []string) sketchHost {
	dark := true
	return sketchHost{frames: frames, dark: dark, cur: frames(dark)}
}

func (h sketchHost) Init() tea.Cmd { return nil }

func (h sketchHost) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h.width, h.height = msg.Width, msg.Height
		return h, nil
	case tea.KeyPressMsg:
		return h.handleKey(keyString(msg))
	}
	return h, nil
}

func (h sketchHost) handleKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case keyQuit, keyEsc, keyCtrlC:
		return h, tea.Quit
	case "n", keyRight:
		if h.frameIdx < len(h.cur)-1 {
			h.frameIdx++
		}
	case "p", keyLeft:
		if h.frameIdx > 0 {
			h.frameIdx--
		}
	case "t":
		h.dark = !h.dark
		h.cur = h.frames(h.dark)
		if h.frameIdx > len(h.cur)-1 {
			h.frameIdx = len(h.cur) - 1
		}
	case keyHelp:
		h.showHelp = !h.showHelp
	}
	return h, nil
}

func (h sketchHost) View() tea.View {
	var v tea.View
	v.AltScreen = true
	v.WindowTitle = "uzi sketch"

	body := "(sketch has no frames)"
	switch {
	case h.showHelp:
		body = strings.Join([]string{
			"n / →      next frame",
			"p / ←      previous frame",
			"t          toggle light / dark",
			"?          this help",
			"q / esc    quit",
		}, "\n")
	case len(h.cur) > 0:
		idx := h.frameIdx
		if idx < 0 {
			idx = 0
		}
		if idx > len(h.cur)-1 {
			idx = len(h.cur) - 1
		}
		body = h.cur[idx]
	}
	v.SetContent(body)
	return v
}

// ---- dispatch --------------------------------------------------------------

// sketchModel returns the tea.Model to run for a sketch: its own Tier-B model when
// one is supplied, otherwise the generic Tier-A frame host over its frames.
func sketchModel(sk sketch) tea.Model {
	if sk.model != nil {
		return sk.model()
	}
	return newSketchHost(sk.frames)
}

// runTUISketch runs the named sketch, or — for the "list" sentinel, an empty name, or an
// unknown key — prints the registered sketch names and returns nil (a discovery
// affordance, not an error). Sketches are passed NO uzicli.Client.
func runTUISketch(ctx context.Context, env Env, name string) error {
	sk, ok := sketches[name]
	if name == "list" || name == "" || !ok {
		printSketchList(env)
		return nil
	}
	m := sketchModel(sk)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(env.Stdin), tea.WithOutput(env.Stdout))
	if _, err := p.Run(); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "tui sketch: %v", err)
	}
	return nil
}

// printSketchList writes the sorted sketch names to env.Stdout, one per line.
func printSketchList(env Env) {
	_, _ = fmt.Fprintln(env.Stdout, "available sketches:")
	for _, name := range sketchNames() {
		_, _ = fmt.Fprintln(env.Stdout, "  "+name)
	}
}
