# uzi TUI — UX lab

A repeatable harness for **seeing** how `uzi tui` renders, in both light and dark
terminal themes and across every screen and state, without standing up the API + DB.
Any agent (or human) can regenerate the images and Read the PNGs to critique the visuals.
It also carries a **runnable demo** of the proposed redesign.

## Run the redesign live

Boots the proposed "factory shift board" redesign in a real bubbletea event loop over
seeded fixtures — no API, no DB, no auth, no network. Runs in your terminal's own theme.

```sh
cd api/cmd/uzi/uxlab && go run ./demo      # or: devbox run demo
```

Navigation: `j`/`k` or arrows move, `enter` opens a run, `esc` backs out, `tab`/`h`/`l`
switch crew lane, `v` opens the judge review, `/` filters, `a` toggles the all-crews board,
`?` help, `q` quits. The seed spans running / awaiting_approval / completed / failed /
limit_wait / stalled so every status colour shows, plus a multi-lane crew in detail.

This is **demo-only**: the shipped TUI (`../tui_*.go`) is unchanged. The redesign lives in
`factoryui/` and is the reference for a later port into the real views. The same `factoryui`
package renders both this live demo and the static `mock-*` PNGs below, so they can't drift.

## Screenshots — one command

```sh
cd api/cmd/uzi/uxlab
devbox run build          # generate ANSI frames + render them to png/
```

`build` runs two steps you can also run alone:

- `devbox run gen` — drives the real `tuiModel` offline and writes `frames/*.ansi`
- `devbox run render` — turns `frames/*.ansi` into `png/*.png` with `freeze`

Outputs land in `png/`. Both `frames/` and `png/` are gitignored: they are large binaries
derived deterministically from the committed harness, so they are regenerated, not stored.

## How it works, and why this way

- **Offline render, not a live binary.** The TUI is a `bubbletea` model whose `Update`
  takes a message and whose `View()` returns a plain string. `../uxlab_gen_test.go` drives
  that seam directly (the same one `../tui_model_test.go` tests use) into a range of states,
  in both palettes, and writes each `View().Content` as raw ANSI. No server, no PTY, fully
  deterministic. Driving the live binary would need a running API with seeded data and could
  not reach states like `limit_wait` or a stream degradation on demand.
- **In package `main`.** `tuiModel` and its render helpers are unexported and Go forbids
  importing a `main` package (see the header of `../tui.go`), so the generator has to live
  beside the TUI as a `_test.go`. It is gated behind `UZI_UXLAB_GEN=1`, so `task gate:api`
  compiles it and skips it instantly; it never runs in CI.
- **lipgloss emits truecolor into the string.** In lipgloss v2 `Style.Render` writes 24-bit
  SGR escapes unconditionally; the profile downgrade happens only at write time. So the
  offline frame already carries the exact colours the TUI would draw.
- **freeze, fed the ANSI.** `render.sh` runs `freeze -l ansi`. Dark frames use freeze's
  default (light-on-dark) text colour; light frames pass `-t github` purely to get a dark
  DEFAULT foreground, because freeze's default is light and would wash out the board's
  un-styled text on a light background (a real light terminal draws it near-black).

## What's in `png/`

Each scene is rendered `-dark` and `-light`.

| Frame | State it captures |
|---|---|
| `board-populated` | the run board, mixed kinds/statuses/health |
| `board-empty` | empty state |
| `board-admin` | the factory-wide admin board |
| `board-filter` | the `/` filter active |
| `detail-running` | run detail, live, multi-lane transcript |
| `detail-stalled` | a run whose health is `stalled` |
| `detail-awaiting-approval` | parked at the plan gate (approve/reject) |
| `detail-limit-wait` | parked on an Anthropic usage limit |
| `detail-degraded` | the live stream fell back to REST polling |
| `detail-steer-typing` | composing a follow-up |
| `detail-steer-confirm` | the cancel confirmation |
| `detail-steer-queue` | the steer queue indicator |
| `review-overlay` | the judge review overlay |
| `review-pending` | a judge in flight, no verdict yet |
| `help`, `quit` | the help overlay and quit modal |
| `mock-*` | the **proposed redesign** (see below) |

## The redesign

The proposed "factory shift board" redesign lives in `factoryui/` — pure render functions
plus a small bubbletea model. It is the single source of truth for the proposal:

- `demo/` — the runnable demo (`go run ./demo`). Thin `main` that boots `factoryui.New()`.
- `factoryui/` — palette, fixtures, render functions, model. `factoryui/*_test.go` drive the
  model headless (navigation, banner, review) so the loop is proven without a PTY.
- `../uxlab_mock_test.go` — renders `factoryui` at a fixed size into `png/mock-*.png`, so the
  static screenshots and the live demo are the same design by construction.

It is **demo-only**; the shipped TUI (`../tui_*.go`) is untouched. Porting the design into
the real views, if approved, is a separate change.

## Files

| Path | What |
|---|---|
| `../uxlab_gen_test.go` | generates the SHIPPED-TUI frames (offline model drive) |
| `../uxlab_mock_test.go` | generates the REDESIGN frames (via `factoryui`) |
| `factoryui/` | the redesign: palette, data, render funcs, bubbletea model |
| `demo/` | the runnable redesign demo |
| `gen.sh` / `render.sh` | regenerate ANSI frames / render them to PNG |
| `devbox.json` | pins `freeze`; scripts `demo`, `gen`, `render`, `build` |

## Adding a state

For the shipped-TUI frames: add a builder to the `scenes` map in `../uxlab_gen_test.go`
(drive the real model with the same message/key helpers the other builders use). For the
redesign: extend `factoryui` (both the demo and the mock PNGs pick it up). Then
`devbox run build`. Name frames `<screen>-<state>-<theme>.png`.
