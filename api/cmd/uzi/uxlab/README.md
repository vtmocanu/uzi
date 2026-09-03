# uzi TUI — UX lab

A repeatable harness for **seeing** how `uzi tui` renders, in both light and dark terminal
themes and across every screen and state, without standing up the API + DB. Any agent (or
human) can regenerate the images and Read the PNGs to critique the visuals. It also carries a
**runnable demo** of the TUI.

The redesign that this lab was built to develop (PRD #325, the "factory shift board") is now
**the shipped TUI**. There is no separate prototype any more: the demo and the screenshots
both drive the real `tuiModel`, so what you see here is what users get. The one deliberate,
gated exception is the throwaway **sketch harness** for previewing a feature that does not
exist yet — see "Prototyping a new TUI feature (sketches)" below.

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
| `../uxlab_gen_test.go` | the screenshot generator: drives the shipped `tuiModel` into every scene (and, per sketch, `sketches`) |
| `../demo.go` | the `uzi tui --demo` showcase (shipped model + fake client + live ticker) |
| `../sketch.go` | the sketch harness: the `sketches` registry, its lipgloss primitives, and the Tier-A frame host |
| `gen.sh` / `render.sh` | regenerate ANSI frames / render them to PNG |
| `devbox.json` | pins `freeze`; scripts `demo`, `gen`, `render`, `build` |

## Prototyping a new TUI feature (sketches)

Everything above renders the **shipped** TUI — the `scenes` map and `--demo` both drive the
real `tuiModel`, so they can only show a view that already exists. A **sketch** is for the
step before that: previewing a feature whose render code does not exist yet, to answer "does
it look right, does it feel right to drive" before you build it for real.

Two rungs, pick the cheaper one first:

- **Tier A — static frames (the common case).** Supply a `frames func(dark bool) []string`
  that builds a few frames from the shared lipgloss primitives (`header`/`pane`/`statusbar`)
  and the shipped palette (`newPalette(dark)`). A generic host renders them fullscreen with
  paging (`n`/`p` or `←`/`→`), a light/dark toggle (`t`), help (`?`), and quit (`q`/esc) — no
  per-sketch UI code. This covers look, states, and both palettes in ~20 lines.
- **Tier B — a real `tea.Model` (opt-in).** Supply a `model func() tea.Model` for key-driven
  feel that static frames can't show. Use it only when the interaction itself is the thing
  you're prototyping.

**How to add one.** Add an entry to the `sketches` registry in `../sketch.go` — copy the
permanent `template` entry as a starting point, and build its `frames` only from the
`header`/`pane`/`statusbar` primitives plus `newPalette(dark)` so it inherits the shipped
palette rather than drifting into ad-hoc colours. Then:

```sh
cd api && go run ./cmd/uzi tui --sketch <name>   # live preview, no server
```

A bare `uzi tui --sketch` (or `--sketch list`, or an unknown name) prints the registered
sketch names. `devbox run build` picks up any Tier-A sketch automatically — no generator
edit needed — and produces its `sketch-<name>-<frameIndex>-<theme>.png` alongside the shipped
scenes; a Tier-B-only sketch (no `frames`) is live-preview-only and isn't screenshotted.

**The point of a sketch is to be deleted.** A sketch is passed no `uzicli.Client` by
construction — wiring the real API into one defeats the purpose and is not supported. Once a
sketch has answered the "should we build this" question, lift its keepable lipgloss `View`
shape into the real `tuiModel` and **delete the sketch** rather than maintaining it. Feature
sketches are branch-local throwaways that should not merge to `main`: the harness ships
exactly one permanent entry, `template`, as the copyable example. This discipline is what
keeps the harness from becoming a second maintained TUI, the fate of the retired
`factoryui` prototype.

## Adding a state

Add a builder to the `scenes` map in `../uxlab_gen_test.go` (drive the real model with the
same message/key helpers the other builders use), then `devbox run build`. Name frames
`<screen>-<state>-<theme>.png`.
