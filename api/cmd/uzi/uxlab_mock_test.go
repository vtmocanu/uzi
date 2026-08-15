package main

// UX-LAB MOCK — static PNGs of the PROPOSED redesign, for a side-by-side against the
// shipped frames. These render through the SAME factoryui package the live demo (uxlab/demo)
// uses, so the screenshots and the runnable demo are guaranteed to be the same design — a
// change to factoryui moves both. The redesign is DEMO-ONLY; the shipped TUI is untouched.
//
// Gated behind UZI_UXLAB_GEN=1, like the real-frame generator. Writes mock-*.ansi into
// uxlab/frames/, which render.sh turns into mock-*.png.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gitlab.example.com/vtmocanu/uzi/api/cmd/uzi/uxlab/factoryui"
)

// runByStatus returns the first seeded run in a given status, for the detail scenes.
func runByStatus(status string) factoryui.Run {
	for _, r := range factoryui.SeedRuns() {
		if r.Status == status {
			return r
		}
	}
	return factoryui.SeedRuns()[0]
}

func TestGenerateUXLabMocks(t *testing.T) {
	if os.Getenv("UZI_UXLAB_GEN") != "1" {
		t.Skip("set UZI_UXLAB_GEN=1 to (re)generate the ux-lab mock frames")
	}
	outDir := filepath.Join("uxlab", "frames")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	scenes := map[string]func(dark bool) string{
		"mock-board": func(dark bool) string {
			return factoryui.RenderBoard(factoryui.NewPalette(dark), factoryui.SeedRuns(), 1, false, false, "", frameWidth)
		},
		"mock-board-admin": func(dark bool) string {
			return factoryui.RenderBoard(factoryui.NewPalette(dark), factoryui.SeedRuns(), 0, true, false, "", frameWidth)
		},
		// Detail as it OPENS: the CREW rail focused (up/down moves between agents); the
		// transcript still follows live in the background.
		"mock-detail-running": func(dark bool) string {
			return factoryui.RenderDetail(factoryui.NewPalette(dark), runByStatus("running"), factoryui.FocusRail, 0, 0, true, frameWidth, 30)
		},
		// After →: the transcript focused (up/down scrolls it), FOLLOWING.
		"mock-detail-focus-transcript": func(dark bool) string {
			return factoryui.RenderDetail(factoryui.NewPalette(dark), runByStatus("running"), factoryui.FocusTranscript, 0, 0, true, frameWidth, 30)
		},
		// Detail scrolled back: follow detached, PAUSED with a "new below" count. Rendered
		// at a shorter height so the transcript overflows the viewport and paging is real.
		"mock-detail-paused": func(dark bool) string {
			return factoryui.RenderDetail(factoryui.NewPalette(dark), runByStatus("running"), factoryui.FocusTranscript, 0, 4, false, frameWidth, 20)
		},
		"mock-detail-awaiting-approval": func(dark bool) string {
			return factoryui.RenderDetail(factoryui.NewPalette(dark), runByStatus("awaiting_approval"), factoryui.FocusTranscript, 0, 0, false, frameWidth, 30)
		},
		"mock-detail-limit-wait": func(dark bool) string {
			return factoryui.RenderDetail(factoryui.NewPalette(dark), runByStatus("limit_wait"), factoryui.FocusTranscript, 0, 0, false, frameWidth, 30)
		},
		"mock-review": func(dark bool) string {
			return factoryui.RenderReview(factoryui.NewPalette(dark), runByStatus("completed"), factoryui.SeedReview(), 0, frameWidth)
		},
	}

	names := make([]string, 0, len(scenes))
	for n := range scenes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, dark := range []bool{true, false} {
			theme := "dark"
			if !dark {
				theme = "light"
			}
			body := scenes[name](dark)
			path := filepath.Join(outDir, fmt.Sprintf("%s-%s.ansi", name, theme))
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s (%d bytes)", path, len(body))
		}
	}
}
