---
paths:
  - api/cmd/uzi/tui_*.go
  - api/cmd/uzi/sketch.go
  - api/cmd/uzi/uxlab/**
---

# TUI + the uxlab render harness

Loaded when you touch the `uzi` CLI TUI or the render harness. The repo-wide
map is the root `CLAUDE.md`.

- The TUI is bubbletea v2 + lipgloss v2 (`api/cmd/uzi/tui_*.go`); the shared palette and render primitives are `tui_render.go`.
- You cannot look at a terminal, so review the TUI through rendered images, not by reading code.
- `cd api/cmd/uzi/uxlab && devbox run build` drives the SHIPPED `tuiModel` offline (env-gated `uxlab_gen_test.go`, no server, no PTY) to ANSI frames, then freezes them to PNG (light + dark, ~19 scenes) under `uxlab/png/` (gitignored). Read `png/<scene>-dark.png` / `png/<scene>-light.png`; `devbox run gen` / `devbox run render` are the two halves.
- After a code change run the full `build` and confirm the PNG mtimes are newer than the frames. A timed-out render leaves stale PNGs beside current frames, and a stale screenshot lies silently.
- Interactive drive-test with no server: `cd api && go run ./cmd/uzi tui --demo`, a hidden flag running the real model over seeded fixtures and a fake stream.
- The demo, the screenshots and the shipped views all run the same `tuiModel`, so changing a view moves both. Do not reintroduce a parallel mock model.
- Exception, gated and throwaway: the sketch harness (`api/cmd/uzi/sketch.go`, PRD #1061) previews a feature that does not render yet, as hand-built frames (Tier A) or a local-state `tea.Model` (Tier B), via `uzi tui --sketch <name>` and passed NO client.
- Only the permanent `template` sketch ships on `main`. Every other sketch is branch-local and gets deleted once its keepable `View` shape is lifted into the real `tuiModel`. Never wire a real client into a sketch, and never let a feature sketch merge to `main`.
- Terminal-injection guard (D7): untrusted cells (titles, owner email, agent output) MUST be drawn through `m.renderer.Plain` (= `capCell(cellText(s))`); `cellText` strips control bytes, a bare `capCell` does not.
- The guard is `d7UntrustedFields` (`tui_d7_guard_test.go`) plus a hostile-value render test (`tui_model_test.go`). A new untrusted field drawn raw passes every clean-fixture screenshot, so add the field to `d7UntrustedFields` and extend the render test.
- A colour-only signal such as the status spine fill vanishes under an Ascii/NoTTY profile and needs a text/glyph fallback (the spine carries a per-status glyph). Verify through the colorprofile downgrade, not the truecolor frame.
- The deterministic CI check for a colour / layout / banner property is the `Update→msg` / `View()→string` seam (`tui_render_test.go` / `tui_model_test.go`) with substring / SGR-escape assertions. A screenshot cannot gate.
- The `tui-ux` dev-team agent (`.claude/agents/tui-ux.md`) owns TUI review; dispatch it on any TUI change, as the terminal analogue of `web-ux`.
