# PRD #118 — Per-worker token picker renders as an unstyled native select

**Issue**: [#118](https://gitlab.example.com/vtmocanu/uzi/-/issues/118) · **Label**: PRD · **Priority**: Low
**Area**: `web/` — the `Select` UI primitive (`web/src/components/ui.tsx`) and its one styled-override caller, the Workers page token picker (PRD #104 lineage).
**Status**: **DONE** — merged `cbc660e7` via `f8b79b9f`, released in **v0.11.3**. Closed 2026-07-25.

> **Verified against the merged diff 2026-07-25.** M1: `ui.tsx:123` is `cx(INPUT_CLASS, className)` — and the same fix landed for `Input` and `Textarea` (`:119`, `:127`), which the PRD scoped to `Select` alone; the wider fix is correct and is noted here rather than left unrecorded. `ui.test.tsx:21-25` asserts both base tokens (`border-edge`, `bg-raised`) and the caller's classes (`h-8`, `custom-x`), the composition pin M1 asked for. M2: `WorkersSettings.test.tsx:602-603` asserts the picker keeps base-field styling. Web suite re-run at close: 959 tests green, `tsc` clean. The M2 in-app confirmation is ticked on shipped-and-live evidence (v0.11.3), not on a recorded manual pass.

## Problem

In **Settings → Workers**, the "Provision a hosted worker" card's **Size**
chooser renders as the app's styled field — rounded, `bg-raised` surface,
`border-edge`, brand-tinted focus. But the per-worker **Anthropic token picker**
in each "Your workers" row renders as a **raw native `<select>`**: dark,
borderless, no rounding, no padding, visually out of place next to the row's
badges and Delete button (observed 2026-07-22, screenshot on the issue). Two
controls of the same kind, one styled and one not, side by side on the same page.

### Root cause

`Select` is the **only** input primitive that clobbers a caller's `className`
instead of merging it. `Input` and `Textarea` both destructure `className` and
compose it: `cx(INPUT_CLASS, className)`. `Select` does not:

```tsx
// web/src/components/ui.tsx
export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={INPUT_CLASS} {...props} />;   // props.className WINS
}
```

Because `className` is inside the spread `{...props}` and the spread comes
**after** the literal `className={INPUT_CLASS}`, any caller-supplied `className`
replaces `INPUT_CLASS` wholesale rather than adding to it. Every other `<Select>`
in the app passes **no** `className`, so they all get the full `INPUT_CLASS` and
look right. The token picker is the sole exception — it passes
`className="h-8 max-w-[11rem] text-xs"` for compact inline sizing
(`web/src/pages/WorkersSettings.tsx:427`), which silently strips the entire
`INPUT_CLASS` — `w-full rounded-lg border border-edge bg-raised px-3 py-2 text-sm
text-fg placeholder:text-faint outline-none focus:border-brand/70` — and drops
the control back to the browser default.

The Size chooser looks fine precisely because it passes no `className`; the token
picker is the one call site that trips the clobber.

## Solution

Fix the primitive, not the call site: make `Select` **merge** `className` the
same way `Input`/`Textarea` already do.

```tsx
export function Select({ className = "", ...props }: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select className={cx(INPUT_CLASS, className)} {...props} />;
}
```

The token picker then keeps its `h-8 max-w-[11rem] text-xs` sizing overrides
**on top of** the base field styling, so it renders as a compact-but-styled field
consistent with the Size chooser and the rest of the app. Its bound value,
options, `aria-label`, and rebind behavior are untouched — this is the control's
appearance and, as one consequence of restoring the base class, its width: it
now inherits `INPUT_CLASS`'s `w-full` and so fills up to its `max-w-[11rem]` cap
inside the row's flex container rather than sizing to content. That is the
intended styled look; M2 eyeballs the row layout to confirm it sits well next to
the badges and Delete button.

### Approach chosen (and rejected)

- **Chosen — merge `className` in the `Select` primitive.** Fixes the actual
  defect (an outlier primitive that doesn't compose like its siblings), restores
  the token picker, and removes the footgun for any future styled `<Select>`.
  `Input`/`Textarea` already establish this exact pattern, so `Select` is being
  brought into line, not given a new one.
- **Rejected — inline the full `INPUT_CLASS` at the call site.** Would fix the
  one visible symptom while leaving `Select` a clobbering outlier, ready to bite
  the next caller that passes a `className` expecting it to add, not replace.
  Duplicating the base class string at the call site also splits its source of
  truth.
- **Rejected — drop the token picker's `className` and let it render full-size.**
  Makes it match by removing the compact sizing, but a full-size field inline in
  a worker row next to badges and a small Delete button is the wrong shape; the
  `h-8 max-w-[11rem] text-xs` sizing is deliberate and correct.

## User journey

- A user with more than one named Anthropic token opens **Settings → Workers**.
  Each worker row's token picker now renders as a styled, compact field matching
  the Size chooser and the rest of the app's inputs — same rounding, surface,
  border, and focus treatment, just smaller.
- Behavior is unchanged: choosing a token still rebinds that worker on its next
  claim; "default token" still clears the binding. Only the control's look
  changes.
- Users with zero or one token are unaffected — the picker only renders when
  `tokens.length > 1`, exactly as today.

## Technical scope

Two files, one of them a one-line primitive change.

- **`web/src/components/ui.tsx`** — `Select` (currently lines ~122–124).
  Destructure `className` and render `cx(INPUT_CLASS, className)`. `cx` is
  defined and exported in this file (line 12) and already used by `Input`,
  `Textarea`, and `Badge`. This is the whole fix; the token picker call site
  needs no change.
- **`web/src/pages/WorkersSettings.tsx`** — no code change. The existing
  `className="h-8 max-w-[11rem] text-xs"` on the token-picker `<Select>`
  (line ~427) now composes with the base styling instead of replacing it. Listed
  here only because it is the one call site whose rendering changes.
- **CLI** — no change. This is a web-only rendering concern; `api/cmd/uzi/` has
  no equivalent control (convention check per CLAUDE.md: the CLI is a second API
  consumer, but there is nothing to mirror for a CSS-class merge).
- **No API / DTO / state change.** Nothing server-side, no data shape, no
  behavior — appearance only.

## Milestones

- [x] **M1 — Fix the primitive + pin it.** Merge `className` in `Select` via
  `cx(INPUT_CLASS, className)`. Add a `web/src/components/ui.test.tsx` assertion
  (create it if absent) that a `<Select className="h-8 custom-x">` renders a
  `<select>` whose class list contains **both** a base-styling token from
  `INPUT_CLASS` (e.g. `border-edge` or `bg-raised`) **and** the caller's classes
  (`h-8`, `custom-x`) — a positive pin proving the two compose rather than one
  replacing the other. `npm test` + `npm run typecheck` green.
- [x] **M2 — Verify in-app + guard the regression.** Confirm in the running app
  (mock mode is enough) that the Workers-page token picker now matches the Size
  chooser's styling, and eyeball the row layout with several badges present so
  the now-`w-full`/`max-w-[11rem]` picker sits well next to them and the Delete
  button. `WorkersSettings.test.tsx` already renders a multi-token worker row
  (the "rebinds a worker to a named token by label" test, which finds the picker
  via `findByLabelText("Anthropic token for laptop")`), so extend it with an
  assertion that that `<select>` carries the base-field styling (e.g. `bg-raised`
  / `border-edge`), not just its sizing overrides — a future refactor of the
  `Select` primitive then can't silently re-strip it. `npm run build` green (runs
  `check-docs` + `tsc`).

## Risks & mitigations

- **Regression surface is effectively nil.** The token picker is the only
  `<Select>` in the codebase that passes a `className` (verified by grep across
  `web/src`), so the merge changes exactly one rendered control — the buggy one.
  Every other `<Select>` passes no `className`, so `cx(INPUT_CLASS, "")` is
  byte-for-byte the same class string they already get. No other call site can
  shift.
- **Conflicting utilities resolve by generated-CSS order, not by className
  order.** `cx` concatenates `INPUT_CLASS` first, the caller's classes second,
  but that string order is irrelevant: Tailwind has no per-utility precedence,
  and two utilities targeting the same property resolve by whichever rule the
  built stylesheet emits **last**. For this call site the overrides that matter
  don't actually conflict — `h-8` (height) and `max-w-[11rem]` (max-width) touch
  properties `INPUT_CLASS` doesn't set, so they simply apply. The one genuine
  conflict is `text-xs` vs `INPUT_CLASS`'s `text-sm`, and `text-xs` wins because
  the repo's Tailwind (v3.4) emits `.text-xs` after `.text-sm` (verified in the
  built stylesheet; same reason two `Textarea` callers already override `text-sm`
  with `text-xs` today). **This is generated-order luck, not a guarantee** — the
  reverse bites elsewhere: `AnthropicTokens.tsx`'s `<Input className="h-8 w-48">`
  loses its `w-48` to `INPUT_CLASS`'s `w-full`, because `.w-48` is emitted before
  `.w-full`. It doesn't bite the token picker (which wants no width override —
  see Solution), but any future styled `<Select>` that needs to override a
  width/size utility should reach for `tailwind-merge`-style resolution rather
  than assume concatenation order decides it. The M1 test pins that base and
  caller classes both survive the merge; it deliberately does not (and cannot)
  pin which conflicting utility wins.

## Success criteria

- The per-worker Anthropic token picker renders as a styled, compact field
  matching the Size chooser and the app's other inputs — rounded, `bg-raised`,
  `border-edge`, brand focus — with its `h-8 max-w-[11rem] text-xs` sizing intact.
- `Select` merges caller `className` via `cx(INPUT_CLASS, className)`, matching
  `Input`/`Textarea`; a test pins that base styling and caller classes compose.
- No behavioral change to token rebinding, and no other `<Select>` in the app
  renders differently. All web tests + typecheck + build green.
