---
paths:
  - api/cmd/uzi/tui_*.go
  - api/cmd/uzi/sketch.go
  - api/cmd/uzi/uxlab/**
---

# TUI + the uxlab render harness

Loaded when you touch the `uzi` CLI TUI or the render harness. The repo-wide
map is the root `CLAUDE.md`.

The TUI is bubbletea v2 + lipgloss v2 (`api/cmd/uzi/tui_*.go`; the shared palette
and render primitives are `tui_render.go`). **You cannot look at a terminal, so
review the TUI through rendered images**, not by reading code.

**Regenerate + read the screenshots.** `cd api/cmd/uzi/uxlab && devbox run build`
drives the SHIPPED `tuiModel` offline (env-gated `uxlab_gen_test.go`, no
server/PTY) to ANSI frames, then freezes them to PNG (light + dark, ~19 scenes)
under `uxlab/png/` (gitignored). Read `png/<scene>-dark.png` /
`png/<scene>-light.png`. `devbox run gen` / `render` are the two halves; after a
code change run the full `build` and confirm the PNG mtimes are newer than the
frames — a timed-out render leaves stale PNGs beside current frames, and a stale
screenshot lies silently.

**Interactive drive-test, no server.** `cd api && go run ./cmd/uzi tui --demo`
(a hidden flag that runs the real model over seeded fixtures + a fake stream).

**One source of truth.** The demo, the screenshots, and the shipped views all run
the same `tuiModel` (the factoryui prototype was retired at PRD #325 M7), so
changing a view moves both. Do not reintroduce a parallel mock model.

**Exception: the sketch harness (`api/cmd/uzi/sketch.go`, PRD #1061), gated and throwaway.**
For previewing a feature that does not render yet, hand-built frames (Tier A) or a
local-state `tea.Model` (Tier B) are allowed via `uzi tui --sketch <name>`, passed
NO client. It is delete-when-done, not maintained: only the permanent `template`
sketch ships on `main`; every other sketch is branch-local and gets deleted once
its keepable `View` shape is lifted into the real `tuiModel`. Do not wire a real
client into a sketch, and do not let a feature sketch merge to `main`.

**Terminal-injection guard (D7).** Untrusted cells (titles, owner email, agent
output) MUST be drawn through `m.renderer.Plain` (= `capCell(cellText(s))`;
`cellText` strips control bytes, a bare `capCell` does NOT). The guard is
`d7UntrustedFields` (`tui_d7_guard_test.go`) + a hostile-value render test
(`tui_model_test.go`). A new untrusted field drawn raw passes every clean-fixture
screenshot — the test is the only guard, so add the field to `d7UntrustedFields`
and extend the render test.

**NO_COLOR.** A colour-only signal (the status spine fill) vanishes under an
Ascii/NoTTY profile; it needs a text/glyph fallback (the spine carries a
per-status glyph). Verify through the colorprofile downgrade, not the truecolor
frame.

**Automatable check.** The `Update→msg` / `View()→string` seam
(`tui_render_test.go` / `tui_model_test.go`) with substring / SGR-escape
assertions is the deterministic CI check for a colour / layout / banner property;
a screenshot cannot gate.

The **`tui-ux`** dev-team agent (`.claude/agents/tui-ux.md`) owns TUI review;
dispatch it on any TUI change (it is the terminal analogue of `web-ux`).
