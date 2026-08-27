# uzi TUI — UX lab

A repeatable harness for **seeing** how `uzi tui` renders, in both light and dark terminal
themes and across every screen and state, without standing up the API + DB. Any agent (or
human) can regenerate the images and Read the PNGs to critique the visuals. It also carries a
**runnable demo** of the TUI.

The redesign that this lab was built to develop (PRD #325, the "factory shift board") is now
**the shipped TUI**. There is no separate prototype any more: the demo and the screenshots
both drive the real `tuiModel`, so what you see here is what users get.

## Run the TUI live (no server)

```sh
cd api && go run ./cmd/uzi tui --demo      # or, from here: devbox run demo
```

Boots the SHIPPED TUI over seeded fixtures with a ticker that injects live frames — no API,
no DB, no auth, no network — in your terminal's own theme. `--demo` is a hidden flag on
`uzi tui` (`demo.go`): it swaps the real API client for an in-memory `uzicli.FakeClient` and
wraps the shipped model with a live-frame ticker, so the board, the plan gate, the focusable
panes and follow-live are all drivable.

Keys — board: `j`/`k` or arrows move, `enter` opens a run, `/` filters, `a` toggles the
all-crews board, `r` refresh, `q` quit. Detail: `←`/`→` (or `h`/`l`, or `tab`) focus the crew
rail / the transcript, `↑`/`↓` (or `j`/`k`) move within the focused pane (agents · scroll),
`g` follow live (jump to newest), `f` follow-up, `v` review, `y`/`n` approve/reject at a plan
gate, `esc` back, `?` help.

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

- **Offline render, not a live binary.** The TUI is a `bubbletea` model whose `Update` takes
  a message and whose `View()` returns a plain string. `../uxlab_gen_test.go` drives that seam
  directly (the same one `../tui_model_test.go` tests use) into a range of states, in both
  palettes, and writes each `View().Content` as raw ANSI. No server, no PTY, fully
  deterministic — and it reaches states like `limit_wait`, a stream degradation or a paused
  transcript that a live binary could not produce on demand.
- **In package `main`.** `tuiModel` and its render helpers are unexported and Go forbids
  importing a `main` package (see the header of `../tui.go`), so the generator lives beside
  the TUI as a `_test.go`, gated behind `UZI_UXLAB_GEN=1` (so `task gate:api` compiles it and
  skips it instantly). The demo lives in the same package for the same reason (`demo.go`), as
  a hidden `uzi tui --demo` flag rather than a `_test.go`, so the gate compiles and lints it.
- **lipgloss emits truecolor into the string.** In lipgloss v2 `Style.Render` writes 24-bit
  SGR escapes unconditionally; the profile downgrade happens only at write time, so the
  offline frame already carries the exact colours the TUI would draw.
- **freeze, fed the ANSI.** `render.sh` runs `freeze -l ansi`. Dark frames use freeze's
  default (light-on-dark) text colour; light frames pass `-t github` purely to get a dark
  DEFAULT foreground, because freeze's default is light and would wash out the board's
  un-styled text on a light background (a real light terminal draws it near-black).

## What's in `png/`

Every scene is rendered from the SHIPPED model, `-dark` and `-light`.

| Frame | State it captures |
|---|---|
| `board-populated` / `board-empty` / `board-admin` / `board-filter` | the run board: mixed statuses/health, empty state, factory-wide admin (OWNER), and the `/` filter |
| `detail-running` | detail, live, crew rail focused, ⇣ following |
| `detail-focus-transcript` | detail with the transcript pane focused |
| `detail-paused` | transcript scrolled back: ⏸ N new · g ⇣ |
| `detail-stalled` | a stalled run (▲ stalled cue in the header) |
| `detail-awaiting-approval` / `detail-awaiting-input` | the two attention banners |
| `detail-limit-wait` / `detail-degraded` | usage-limit park / stream fell back to polling |
| `detail-steer-typing` / `detail-steer-confirm` / `detail-steer-queue` | steer states |
| `review-overlay` / `review-pending` | the judge review overlay (severity chip) |
| `help`, `quit` | the help overlay and quit modal |

## Files

| Path | What |
|---|---|
| `../uxlab_gen_test.go` | the screenshot generator: drives the shipped `tuiModel` into every scene |
| `../demo.go` | the `uzi tui --demo` showcase (shipped model + fake client + live ticker) |
| `gen.sh` / `render.sh` | regenerate ANSI frames / render them to PNG |
| `devbox.json` | pins `freeze`; scripts `demo`, `gen`, `render`, `build` |

## Adding a state

Add a builder to the `scenes` map in `../uxlab_gen_test.go` (drive the real model with the
same message/key helpers the other builders use), then `devbox run build`. Name frames
`<screen>-<state>-<theme>.png`.
