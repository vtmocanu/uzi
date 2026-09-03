# PRD #1061: TUI sketch harness — preview a new TUI feature before implementing it

**Issue**: #1061
**Priority**: Medium (developer velocity / internal tooling)
**Status**: Implemented — M1–M5 landed on branch `agent/issue-1061` (2026-09-03), `task gate:api` green. SC2–SC6 are covered by `api/cmd/uzi/sketch_test.go` (registry frames property, the Tier-A host render + paging/theme clamps pinned by an asymmetric-frame mutation test, the Tier-B dispatch via a stub model, the CLI list affordance on a TTY, and the non-TTY `ExitUsage` guard) plus the generator/screenshot wiring in `uxlab_gen_test.go`. SC1 (the interactive `uzi tui --sketch template` session) is the documented manual/pty acceptance, not a gate unit test — matching the untested `--demo` precedent. The `devbox run build` PNG-render step is CI/local-devbox-only (freeze cannot be fetched in the uzi worker); the `.ansi` frame emission is proven and `render.sh` is unchanged.

## Problem

Seeing how a not-yet-built TUI feature would *look and feel* is slow today. There are only two ways in:

1. **Edit the real `tuiModel`** — `api/cmd/uzi/tui.go` plus ~15 `tui_*.go` files and a 108 KB `tui_model_test.go`, all of which must keep compiling and passing `task gate:api` — then boot the stack (or `uzi tui --demo`) to look at it.
2. **Add a screenshot scene** in `api/cmd/uzi/uxlab_gen_test.go`. But every scene there drives the *real* model (`m.View().Content`), so the feature's render code must already exist. The lab is excellent for iterating on states of a thing that already renders and useless for previewing a feature you have not decided to build.

So there is no cheap surface to prototype a new View + key-handler and answer the only two questions that matter at the "should we build this" stage: *does it look right* and *does it feel right to drive*. The team previously built a **separate** mock TUI (`factoryui`) for exactly this and **retired it** because a second implementation drifts from what ships (see `api/cmd/uzi/demo.go` header and `uxlab/README.md`). We want the speed of a throwaway mock without re-introducing that drift.

## Solution (one line)

A throwaway **sketch** surface layered on the existing `uxlab` harness: one sketch definition, authored once, yields **both** a live interactive preview (`uzi tui --sketch <name>`) and light/dark PNGs via the existing `freeze` pipeline — with sketches rendering pure strings / local state only, never the real client/API, so the keepable half (the lipgloss View shape/layout) lifts into the real `tuiModel` and the sketch is then deleted rather than maintained.

## Solution detail

- **One definition, two outputs.** A `sketch` type and a `sketches` registry live in a new **non-test** file `api/cmd/uzi/sketch.go` (non-test so the runtime `--sketch` command can reach it; Go forbids importing a `main` package, so `_test.go` cannot be the home for runtime code). Both the runtime `--sketch` command and the `_test.go` screenshot generator consume the same registry, so a sketch authored once is previewable live and screenshotted in both themes with no per-sketch generator code.
- **Two tiers, most sketches are Tier A.**
  - **Tier A (static frames)** — a sketch supplies `frames func(dark bool) []string`. A generic host model renders the current frame fullscreen with paging and a light/dark toggle. Covers *look* + multiple states + both palettes with ~20 lines and zero bubbletea plumbing.
  - **Tier B (interactive)** — a sketch optionally supplies its own `model func() tea.Model` (real `View` + `Update`) when you need key-driven *feel* (focus movement, navigation). Opt-in.
- **Reusable layout primitives.** Small lipgloss helpers (`header`, `pane`, `statusbar`) that pull colours from the real `newPalette(dark)` (`api/cmd/uzi/tui_render.go`), so a sketch inherits the shipped palette and cannot drift into ad-hoc colours.
- **Gated + throwaway.** `--sketch` is a hidden flag parallel to the existing hidden `--demo`, dispatched after the same non-TTY guard. Sketches are ephemeral: add one while prototyping, delete it when the feature lands (its View code lifts into the real model) or is dropped. Exactly one permanent minimal `template` sketch ships as the copyable example and to keep the registry/helpers non-dead against the zero-tolerance dead-code gate.

## Verified technical facts (read locally; no open-web needed)

All seams below were read in this checkout at PRD authoring time. The implementer can re-verify every one offline (codebase read only).

- **`--demo` is a hidden bool flag on the `tui` command, dispatched after a non-TTY guard.** `api/cmd/uzi/tui.go`:
  - `newTUICmd(env Env, gf *globalFlags)` (`tui.go:661`) builds `&cobra.Command{Use: "tui [run-id]", …}` (the `Use:` line is `tui.go:664`).
  - The `RunE` first rejects a non-TTY stdout with a `uzicli.ExitUsage` message (`tui.go:673-677`), **then** `if demo { return runTUIDemo(cmd.Context(), env) }` (`tui.go:680-682`).
  - Flag registration: `cmd.Flags().BoolVar(&demo, "demo", false, …)` then `cmd.Flags().MarkHidden("demo")` (`tui.go:708-709`). `--sketch` mirrors this shape.
- **The demo drives the real model over a fake client, no server/DB/auth.** `api/cmd/uzi/demo.go`: `runTUIDemo` builds `demoModel{tuiModel: newTUIModel(ctx, newDemoClient(), "")}` and runs `tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(env.Stdin), tea.WithOutput(env.Stdout))`. `newDemoClient()` returns a `*uzicli.FakeClient`. The sketch host uses the same `tea.NewProgram(...)` wiring but **passes no client at all** — a sketch has no data source by construction.
- **`newTUIModel(ctx, c uzicli.Client, startRun string) tuiModel`** is at `tui.go:211`; **`newPalette(dark bool)`** (`api/cmd/uzi/tui_render.go:161`) and **`newTUIRenderer(width, dark)`** (`api/cmd/uzi/tui_render.go:38`) — NOT in `tui.go` — are what `uxlab_gen_test.go`'s `uxModel` uses (`uxlab_gen_test.go:44-49`). The layout primitives source their colours from `newPalette`.
- **The screenshot generator is a `_test.go` in `package main`, gated behind `UZI_UXLAB_GEN=1`.** `api/cmd/uzi/uxlab_gen_test.go:80` declares `scenes := map[string]func(dark bool) string{…}`, iterates `for _, dark := range []bool{true, false}` (`:113`), and writes each `scenes[name](dark)` as raw ANSI (`:118`). Because the builder signature is just `func(dark bool) string`, a builder may return **any** string — it need not drive the real model. This is the seam the sketch screenshots fold into.
- **The render pipeline is `devbox`-scripted and derives images from committed source.** `api/cmd/uzi/uxlab/`: `gen.sh` (drives the generator → `frames/*.ansi`), `render.sh` (`freeze -l ansi` → `png/*.png`; dark uses freeze's default, light passes `-t github`), `devbox.json` pins `freeze` and scripts `demo`/`gen`/`render`/`build`. `frames/` and `png/` are gitignored (regenerated, not stored) — see `uxlab/.gitignore`.
- **Toolchain.** `charm.land/bubbletea/v2 v2.0.9` and `charm.land/lipgloss/v2 v2.0.6` (`api/go.mod`). lipgloss v2 `Style.Render` writes 24-bit SGR unconditionally, so an offline frame already carries the exact colours the TUI would draw (per `uxlab/README.md`).
- **Gate reachability / dead-code.** `task gate:api` compiles and lints non-test `sketch.go` and compiles the `_test.go` (skipping the generator body without `UZI_UXLAB_GEN=1`). The dead-code slot gates at **zero** against an empty baseline (`.claude/rules/go.md`), so every shipped function must be reachable: the registry reference makes each sketch reachable, `runTUISketch` is reached by the `--sketch` dispatch, and the permanent `template` sketch keeps the layout primitives live.
- **No workflow files are involved.** Implementation and validation touch only `api/cmd/uzi/**` and `api/cmd/uzi/uxlab/README.md`; `.github/workflows/**` is not edited (see Workflow-scope note).

## Milestones

### M1 — Sketch definition, registry, and layout primitives (`api/cmd/uzi/sketch.go`, new)
- A `sketch` type: `title string`, `frames func(dark bool) []string` (Tier A), optional `model func() tea.Model` (Tier B).
- A package-level `sketches` registry (`map[string]sketch`), ordered listing helper for the `--sketch` list output.
- Reusable lipgloss primitives (`header`, `pane`, `statusbar`) that source colours from `newPalette(dark)` so sketches match the shipped palette.
- Exactly one permanent minimal `template` sketch demonstrating the primitives, so the registry + helpers are non-dead and the pattern is copyable.
- **Validation**: `template` is registered; `sketches["template"].frames(true/false)` each return ≥1 non-empty frame.

### M2 — Interactive `uzi tui --sketch <name>` (hidden flag on the `tui` command)
- Add a hidden string flag `--sketch`, dispatched **after** the existing non-TTY guard and parallel to `--demo` (`tui.go:673-709`).
- **Bare `--sketch` (no value) must not error.** A cobra `StringVar` normally rejects a valueless flag with "flag needs an argument" *before* `RunE` runs. Set `cmd.Flags().Lookup("sketch").NoOptDefVal = "list"` so a bare `--sketch` yields the `list` sentinel and reaches the dispatch.
- `runTUISketch(ctx, env, name)`: if the named sketch has a `model`, run it (Tier B); else run a generic host model over its `frames` (Tier A) with keys `n`/`p`/←/→ page frames, `t` toggle light/dark, `q`/esc quit, `?` help.
- The `list` sentinel (bare `--sketch`), `--sketch list`, or an unknown name prints the registered sketch names and exits 0 (a discovery affordance, not an error).
- The host passes **no `uzicli.Client`** — sketches have no data source.
- **The discovery/list path is behind the non-TTY guard, matching `--demo`.** So a piped `uzi tui --sketch | cat` returns `ExitUsage` (not a list); the list affordance is a real-terminal convenience, and its M5 unit test drives it with `env.StdoutTTY = true` (precedent: `run_field_test.go`). Keeping the guard first avoids drawing escape codes into a pipe, the exact reason the guard exists.
- **Validation**: `uzi tui --sketch template` runs and quits cleanly over no server/DB/auth (manual/pty check — see M5); with `StdoutTTY=true`, bare `uzi tui --sketch` lists names and exits 0; a non-TTY `--sketch` honours the guard with `ExitUsage`.

### M3 — Screenshot integration (extend the generator to also walk the registry)
- The generator gains a second loop that iterates the shared `sketches` registry and, for each sketch with `frames` and each `dark ∈ {true,false}`, writes `sketch-<name>-<frameIndex>-<theme>.ansi` (theme **last**, matching `render.sh`'s `*-light` suffix detection and the existing `<screen>-<state>-<theme>` convention). This is additive generator code alongside the existing `scenes` map, not a literal merge into it (a scene is one string; a Tier A sketch is many frames, hence the `<frameIndex>`).
- No per-sketch generator code: adding a sketch to the registry automatically produces its PNGs on `devbox run build`.
- **Tier B without `frames` is live-preview-only** and is not screenshotted (there is no static frame to render). Such a sketch is reachable from the live preview but not the PNG path; SC4's "reachable from both" therefore applies to Tier A (and to a Tier B sketch that also supplies a representative `frames`).
- **Validation**: with `UZI_UXLAB_GEN=1`, the generator emits `sketch-template-*-{dark,light}.ansi`; `devbox run build` renders them to `png/`.

### M4 — Docs (`api/cmd/uzi/uxlab/README.md` **and** `.claude/rules/tui.md`)
- `uxlab/README.md`: a "prototyping a new TUI feature" section — the rung-1 (static frames → screenshot) / rung-2 (Tier B interactive) ladder, how to add a sketch (one registry entry), and the **delete-when-done** discipline.
- State the anti-pattern guard explicitly: a sketch must never wire the real `uzicli.Client`/API, its lipgloss View *shape/layout* is meant to be lifted into the real `tuiModel` (then the sketch is deleted, not maintained), and **feature sketches are branch-local throwaways that should not merge to `main`** — the harness ships exactly one permanent `template`. This is what keeps the harness from becoming a second maintained TUI (`factoryui`).
- **`.claude/rules/tui.md` must be updated in the same change** to resolve a direct contradiction: its "One source of truth" paragraph (lines ~28-30) says *"Do not reintroduce a parallel mock model."* Add a carve-out naming the sketch harness as the deliberate, gated, throwaway exception (hand-built frames / local-state Tier B model, no client, delete-when-done, only `template` on `main`), so the rule and the shipped feature do not conflict. (Required by the repo's conflicting-instructions rule; `tui.md`'s `paths:` already covers `api/cmd/uzi/uxlab/**`, so the rule loads when this area is touched.)
- **Validation**: `uxlab/README.md` documents the workflow and the guard; `.claude/rules/tui.md` no longer forbids the harness it now ships. No user-facing `docs/*.md` page is added (this is dev tooling, so `web/scripts/check-docs.mjs` is unaffected).

### M5 — Tests + green gate (`api/cmd/uzi/sketch_test.go`, new)
- Registry/property tests: every registered sketch with `frames` yields ≥1 non-empty frame in **both** themes; the generic Tier A host builds and its `View()` renders without panic; the **Tier B dispatch is exercised too** (construct a Tier B host over a `model`-bearing sketch and assert it builds and renders without panic — so the interactive path is not left un-tested and un-gated); `--sketch <unknown>` and bare `--sketch` list and exit 0 **with `env.StdoutTTY = true`** (precedent `run_field_test.go`); a non-TTY `--sketch` returns `ExitUsage`.
- Assert **properties, not the roster** (mirrors the repo convention that no test pins a roster by name): do not hard-code the set of sketch names beyond the permanent `template`.
- **Scope of automated coverage.** "Runs interactively and quits cleanly" (SC1) needs a pty and is a manual acceptance check — the parallel `--demo` path has no automated test either. The gate covers host-builds-and-`View()`-renders-without-panic and the flag/guard dispatch, not a driven interactive session.
- **Validation**: `task gate:api` green (fmt-check, ratcheted lint, dead-code zero, `-race -count=1` tests). No `.github/workflows/**` touched.

## Success criteria

1. `uzi tui --sketch template` runs interactively with **no** server, DB, or auth, toggles light/dark, and quits cleanly. *(Manual/pty acceptance — not a `gate:api` unit test, matching the untested `--demo` precedent.)*
2. In a terminal, bare `uzi tui --sketch` lists the registered sketches and exits 0; a non-TTY invocation returns `ExitUsage` (the list affordance is behind the TTY guard, like `--demo`).
3. `devbox run build` in `uxlab/` produces `sketch-template-*-{dark,light}.png` from the shared registry, with **no** per-sketch generator edit.
4. A new **Tier A** sketch (or a Tier B sketch that also supplies `frames`) is added by editing **one** registry entry (+ its builder) and is thereby reachable from **both** the live preview and the screenshots. A Tier-B-only sketch is live-preview-only, by design.
5. Sketches never import or call `uzicli.Client`/the API — the host passes no client, and the pattern/docs forbid it.
6. `task gate:api` is green and **no** workflow files are touched. `.claude/rules/tui.md` no longer contradicts the shipped harness.

## Decision Log

- **D1 — Sketch surface, not a separate app.** Reject a standalone subcommand hosting its own prototype framework, and reject reviving `factoryui`: a second maintained TUI drifts from what ships. Sketches are throwaway, gated behind a hidden flag, render pure strings / local state, and their View code is designed to be lifted into the real model — so the harness cannot become a parallel implementation.
- **D2 — One definition, two outputs.** The `sketches` registry lives in non-test `sketch.go` so both the `_test.go` screenshot generator and the runtime `--sketch` command consume it. Authoring a sketch once yields a live preview and light/dark PNGs; there is no test/non-test duplication and nothing to keep in sync by hand.
- **D3 — Two tiers.** Tier A (a `frames(dark)` builder) covers look + states + themes with no bubbletea plumbing and is the default; Tier B (an own `tea.Model`) is opt-in for real key-driven feel. This keeps the common case ~20 lines while still allowing interaction prototyping.
- **D4 — Hidden flag, parallel to `--demo`, after the TTY guard.** Reuse the exact dispatch shape at `tui.go:673-709` (guard, then flag branch, then `MarkHidden`). `--sketch` is discovery-friendly: no/unknown name lists rather than errors.
- **D5 — One permanent `template` sketch; feature sketches never merge to `main`.** The `template` sketch is the copyable example and keeps the registry + layout primitives non-dead against the zero-tolerance dead-code gate. Every other sketch is **branch-local**: added during prototyping, and deleted when its View shape graduates into the real model or the idea is dropped — because the screenshots (PNGs) are gitignored and regenerated per branch, a feature sketch has no reason to land on `main`. This tightens "delete when done" into "do not merge," which is the clean anti-drift position.
- **D6 — Enforcement is docs + a Tier B test, not a hard gate (this release).** A mechanical "only `template` in the registry on `main`" check was considered (reviewer-proposed) and deliberately left as documented discipline plus M5's Tier B coverage, for two reasons: a hard gate would redden the moment a developer adds a sketch *on their own branch* (the normal prototyping state), and the repo already keeps a permanent hidden showcase (`--demo`) as precedent that a deliberate second permanent example is acceptable. A non-gating `nudge:sketches` (mirroring `nudge:roles`) is recorded as a future option in Risks rather than shipped here, to keep this PRD to one Go package and no CI-wiring change.

## Risks & mitigations

- **Rot: a graveyard of dead sketches.** Mitigation: only `template` is permanent; feature sketches are branch-local (D5, never merged), ~20 lines, isolated in `sketch.go`, and the docs mandate delete-when-done. **Future option (not shipped, D6):** a non-gating `task nudge:sketches` mirroring `nudge:roles` that flags any registered sketch other than `template` on `main` — converts the discipline into a visible signal without breaking local prototyping. Recorded here so it is a deliberate follow-up, not a gap.
- **Tier B is the one path that could become a maintained parallel model.** Its `Update` complexity is unbounded and a hoarded Tier B sketch is the smaller cousin of `factoryui`. Mitigation: the "no client" structural guard means it can never be a *usable* app; M5 exercises the Tier B dispatch so it is tested not un-gated; and D5 keeps it branch-local. Residual risk is a developer keeping a Tier B sketch past graduation, which the docs and (optionally) the nudge address.
- **Second-implementation drift (the `factoryui` failure).** Mitigation: sketches render pure strings / local state and are passed **no** client; the keepable half (lipgloss) is meant to be lifted into the real model, at which point the sketch is deleted, not maintained. The docs state this as the harness's reason for existing.
- **Dead-code gate false-positive on an unwired sketch.** Mitigation: registry membership makes every sketch reachable, and `template` keeps the helpers live; a sketch removed from the registry must be deleted whole (no orphan builders).
- **Palette drift** (a sketch using ad-hoc colours won't match the real TUI). Mitigation: the layout primitives source colours from `newPalette(dark)`, so sketches inherit the shipped palette by default.

## Files touched (for reviewer file-disjointness checks)

- `api/cmd/uzi/sketch.go` **(new)** — `sketch` type, `sketches` registry, layout primitives, `runTUISketch`, the `template` sketch. (M1, M2)
- `api/cmd/uzi/tui.go` — hidden `--sketch <name>` flag + dispatch after the TTY guard. (M2)
- `api/cmd/uzi/uxlab_gen_test.go` — fold the `sketches` registry into scene generation. (M3)
- `api/cmd/uzi/sketch_test.go` **(new)** — registry/host property tests, incl. the Tier B dispatch. (M5)
- `api/cmd/uzi/uxlab/README.md` — prototyping workflow + anti-pattern guard. (M4)
- `.claude/rules/tui.md` — carve out the sketch exception to "no parallel mock model." (M4)

## Workflow-scope note (uzi worker)

This PRD touches **no** `.github/workflows/**` file in either its implementation or its validation — all changes live under `api/cmd/uzi/**` and `api/cmd/uzi/uxlab/README.md`. The worker PAT lacks `workflow` scope, so this constraint keeps the branch pushable in one shot. No user-facing `docs/*.md` is added, so `web/scripts/check-docs.mjs` is unaffected.
