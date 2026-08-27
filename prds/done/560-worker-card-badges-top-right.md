# PRD #560: Workers settings card — badges top-right, controls on their own row (Option B)

**GitHub Issue**: [vtmocanu/uzi#560](https://github.com/vtmocanu/uzi/issues/560)
**Status**: Complete
**Priority**: Low
**Created**: 2026-08-22
**Scope**: web-only. No API, DB, CLI, forge-driver, or `.github/workflows/**` change.
**Anchors**: all line numbers below are against `main` at `8b2d60a30`. Re-derive
them at implementation time (`grep -n` the quoted class strings) rather than
trusting the numbers — the file changes often.

## Problem

On Settings → Workers, each worker card renders its header as a single
`flex flex-wrap items-center justify-between` row with exactly two children:
a left text column (name + upgrade badge, then a meta line, then the effective-token
line) and **one** right-side group that holds *all* the status badges **plus** the
token `<Select>` **plus** the Delete button together
(`web/src/pages/WorkersSettings.tsx:431` and `:546`).

That right group is wide (up to five badges + an 11rem select + a button). At the
Settings panel width it cannot sit beside the 3-line left column, so `flex-wrap`
drops the **whole** group onto its own line. With one flex item left on that line,
`justify-between` left-aligns it. The result is a full-width, left-aligned 4th row
of badges-and-controls under the token line — a wasted line, and a scan order that
buries the worker's live state (online / run-load) at the bottom of the card
instead of beside its name.

Observed on a real card (v0.53.0): title `base.l-da4a` + `up to date`, meta line,
`auto-selects from your token pool`, then a 4th row `hosted · docker · docker ·
online · 1/2 runs · [Auto-select from the pool ▾] · Delete`, then the CPU/Memory
gauges.

## Solution Overview

Restructure only the card **header region** (the `:431` block) into the layout
validated with the user as **Option B**:

1. **Row 1 — identity | badges.** A `flex items-start justify-between` row: the
   worker name + `WorkerUpgradeBadge` on the left, and the status-badge cluster
   top-right beside it.
2. **Meta line — full width**, directly under Row 1 (unchanged content).
3. **Row 2 — token | controls.** A `flex items-end justify-between` row: the
   effective-token sentence on the left, and the token `<Select>` + Delete button
   right-aligned.

The CPU/Memory gauges and the confirm-in-place delete block are **unchanged**
siblings below the header region.

Net effect: the badges no longer claim a dedicated line (one line shorter on the
common case), and the card reads top-down — **identity → state → controls** — which
is the conventional card pattern.

### Why the meta line must be full-width (the load-bearing design fact)

The tempting minimal fix — just change `items-center` → `items-start` on the
existing `:431` row and split the controls off — **does not work**, and this was
confirmed in the browser mock. Flexbox sizes a flex child by its widest line. If
the name and the long meta line stay in the *same* left column, that column's
width is the meta line's width (~380px: `template base · 8 GiB limit ·
v0.53.0+gbed5c0d · up <1m · last seen 22/08/2026, 10:56:11`), and the badge
cluster placed to its right still exceeds the panel width and wraps below — square
one.

Option B breaks that by making the meta line a **full-width sibling** rather than a
member of the left column. Row 1 then only has to fit `name + badges`, which it
does at Settings width. This is the reason the restructure is a header-shape change
and not a one-class alignment tweak.

## Target structure (before → after)

**Before** (`WorkersSettings.tsx:431`–`:649`, abbreviated):

```tsx
<div className="flex flex-wrap items-center justify-between gap-2">
  <div>
    <span className="font-medium text-fg">{stripUnsafeChars(w.name)}</span>
    <span className="ml-2 align-middle"><WorkerUpgradeBadge worker={w} /></span>
    <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
      {/* template / mem limit / version / up / last seen */}
    </div>
    <div className="mt-1 text-xs text-muted">
      {/* effective-token sentence: auto / auto-empty / pinned / no-token / default */}
    </div>
  </div>
  <div className="flex items-center gap-1.5">
    {/* hosted + docker; template drift; capabilities.map; status badge;
        WorkerCordonBadge; WorkerRunBadge; token <Select> (tokens.length>1); Delete */}
  </div>
</div>
```

**After** (Option B — same children, regrouped into three blocks):

```tsx
<div>
  {/* Row 1 — identity | badges. min-w-0 + break-words lets a long name shrink/
      wrap instead of shoving the badge cluster (names are length-capped at ingest
      since #169, but add the character-level guard anyway). The badge cluster is
      flex-wrap (the original :546 group was NOT) so it rewraps under the name at
      narrow widths rather than overflowing. */}
  <div className="flex items-start justify-between gap-3">
    <div className="min-w-0 break-words">
      <span className="font-medium text-fg">{stripUnsafeChars(w.name)}</span>
      <span className="ml-2 align-middle"><WorkerUpgradeBadge worker={w} /></span>
    </div>
    <div className="flex flex-wrap items-center justify-end gap-1.5">
      {/* hosted + docker; template drift; capabilities.map; status badge;
          WorkerCordonBadge; WorkerRunBadge  — the READ-ONLY badges only.
          Do NOT move the delete-asymmetry / token-picker rationale comments
          (currently :592–:603) here — they document the controls, see below. */}
    </div>
  </div>

  {/* Meta line — full width */}
  <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-faint">
    {/* template / mem limit / version / up / last seen — unchanged */}
  </div>

  {/* Row 2 — effective token | controls */}
  <div className="mt-1 flex items-end justify-between gap-3">
    <div className="min-w-0 text-xs text-muted">
      {/* effective-token sentence — unchanged */}
    </div>
    <div className="flex items-center gap-1.5">
      {/* token <Select> (tokens.length>1 && !confirming); Delete (!confirming) */}
    </div>
  </div>
</div>
```

The three badge helpers move verbatim: `WorkerCordonBadge` and `WorkerRunBadge`
(`:590`–`:591`) and the hosted/docker/drift/capabilities/status badges
(`:552`–`:589`) go into the Row 1 badge cluster; the `<Select>` (`:604`–`:638`) and
Delete `<Button>` (`:639`–`:648`) go into the Row 2 controls cluster. No badge or
control markup changes — only its parent.

**Route the two control-rationale comments with the controls, not the badges.**
Between `WorkerRunBadge` (`:591`) and the `<Select>` (`:604`) sit two explanatory
blocks — the delete-asymmetry rationale (`:592`–`:598`) and the token-picker
rationale (`:599`–`:603`). They document the `<Select>`/Delete, so they travel to
**Row 2** with the controls. A naive "read-only badges = `:552`–`:591`, controls =
`:604`–`:648`" cut leaves them orphaned above unrelated badges in Row 1 — don't.

**Honest note on "only the parent changed":** the new wrappers add load-bearing
classes the originals lacked — `min-w-0 break-words` on the identity block,
`justify-end` and (critically) `flex-wrap` on the badge cluster (the `:546` group
was `flex items-center gap-1.5`, no wrap). That added `flex-wrap` is what delivers
"no horizontal overflow" at narrow widths, so it is intentional, not incidental.

## Design Decisions

1. **Split the one right group into two clusters, not just re-align it.** Read-only
   status badges belong beside identity (Row 1); interactive controls belong with
   the thing they act on (Row 2, next to the token sentence). Keeping them one
   group is what forces the wrap in the first place.

2. **Meta line becomes a full-width sibling** (see "Why the meta line must be
   full-width" above). This is the mechanism that keeps badges on Row 1 at Settings
   width regardless of how long the meta line grows.

3. **Preserve every existing behavior and its comment.** In particular, all of the
   invariants the current code documents at length must survive the move,
   unchanged:
   - **`stripUnsafeChars(w.name)`** stays on the name and on the two `aria-label`s
     that reference it — the RLO/Cc+Cf strip is load-bearing for pre-`#169` rows.
   - **Token `<Select>` gating** stays `tokens.length > 1 && confirmingDelete !== w.id`;
     its `value` still derives from `anthropic_bind_mode` first, then label.
   - **Delete** stays `confirmingDelete !== w.id`, hosted → arm confirm, external →
     immediate `remove`; the confirm-in-place block below the header is untouched,
     and the `deleteButtonId(w.id)` focus-return after a dismissed confirm still
     lands on the Delete button (it moved rows but keeps its id).
   - **Hosted-only badges** (`w.kind === "hosted"` → `hosted` + conditional
     `docker`), **template-drift** badge, **`capabilities.map`** chips, the
     **status** badge (`tone ok/neutral`, `dot`), `WorkerCordonBadge`,
     `WorkerRunBadge` all render exactly as before, just relocated.
   - **Effective-token three-state sentence** (auto-empty warn / auto / pinned /
     no-token warn / default) is moved verbatim into Row 2's left cell.

4. **When Delete is armed (`confirmingDelete === w.id`)** both the Select and Delete
   are already hidden by their existing guards, so Row 2's controls cell renders
   empty and the token sentence sits alone on that row; the confirm-in-place block
   renders below as today. An empty flex cell is harmless — no conditional wrapper
   needed.

5. **No new component, no shared-primitive change.** `Badge`, `Button`, `Select`
   (`web/src/components/ui.tsx`) are untouched; this is pure JSX regrouping inside
   one `.map` in one file.

6. **No docs, CLI, or API surface.** Card layout is not described in any
   `docs/*.md` page, the CLI (`api/cmd/uzi/`) renders workers as text not as this
   card, and no route/DTO changes — so PRD #57's docs-link convention and the
   "new functionality ⇒ check `api/cmd/uzi/`" convention both resolve to "no change
   here". Stated explicitly so the worker does not go looking.

## Milestones

- [x] **M1 — Header restructured to Option B.** The `:431` header block is split
      into Row 1 (identity | badges), a full-width meta line, and Row 2 (token |
      controls), per the target structure above. `task typecheck:web` clean.

- [x] **M2 — Behavior + a11y parity preserved** *(verification gate, not a separate
      deliverable — no independent artifact; it confirms M1 broke nothing).* All
      badges (hosted/docker/drift/capabilities/status/cordon/run), the token
      `<Select>` gating and value derivation, the Delete arm/confirm/focus-return
      flow, and the effective-token three-state sentence behave exactly as before.
      `stripUnsafeChars` and the two name `aria-label`s (the Select's, `:606`, which
      moves to Row 2; and the confirm block's, `:686`, untouched) are intact.

- [x] **M3 — Tests updated and green (jsdom-testable claims only).** `WorkersSettings.test.tsx`
      asserts the new structure as **DOM order/containment**, not visual adjacency —
      jsdom has no layout engine, so "badges beside the name" is expressed as the
      badge cluster preceding the controls / status→cordon→run order preserved, in
      the style of the existing `status.compareDocumentPosition(pill)` test
      (`WorkersSettings.test.tsx:328`). Assert controls still present and the
      delete-confirm flow still works (the existing focus-return tests are
      role+name-based: `getByRole("button", { name: "Delete" })` — they pass
      unchanged as long as Delete keeps its name and id). Run the
      **retired-string / negative-assertion sweep** required by `.claude/rules/web.md`
      — but **expect it to be a no-op here**: Option B retires no copy and no
      test-asserted class string, only DOM grouping. One trap: a source-side
      `git grep -F 'flex flex-wrap items-center justify-between gap-2'` still returns
      a **live** hit on the untouched confirm-in-place block (`:697`), which is NOT
      evidence about the header — don't treat it as a leftover to "fix".
      `task gate:web` passes (deps-check + lint + deadcode + check-docs + typecheck + test).

- [x] **M4 — Wrap safety asserted in code; visual confirmation is human/browser-gated.**
      *(Worker-verifiable half done: a jsdom test asserts `flex-wrap` on the badge
      cluster and `min-w-0 break-words` on the identity wrapper. The pixel/overflow
      visual confirmation at real widths remains the human/browser post-merge check
      the PRD scopes out of the worker's work.)*
      *(The offline sweep worker has no rendering engine, so it cannot observe pixel
      layout or measure overflow — that half is post-merge and human/browser.)*
      What the **worker** does and can verify: confirm the badge cluster wrapper
      carries `flex-wrap` and the identity wrapper carries `min-w-0 break-words`
      (the classes that make rewrap-not-overflow structurally true), via a jsdom
      test asserting those classes on the rendered nodes. What a **human** confirms
      post-merge in a `VITE_UZI_MOCK=1 npm run dev` build (a scenario listing
      workers): at Settings panel width the badges sit on Row 1 beside the name (no
      dedicated badge row), and at narrow widths badges + controls rewrap with no
      horizontal page overflow — record the widths checked. Do **not** use
      `vite preview` / non-mock `vite dev` (they proxy to the live stack).

## Success Criteria

1. At Settings panel width, a worker card shows its status badges on the **first
   row** beside the worker name, and no longer renders a separate full-width
   left-aligned badges-and-controls row. The common-case card is one line shorter.
2. Every worker card behavior listed in Design Decision 3 is equivalent in effect.
   The DOM grouping changes and the wrappers gain a few load-bearing classes
   (`min-w-0 break-words`, `justify-end`, `flex-wrap` on the badge cluster); no
   badge/control/behavior logic changes.
3. `task gate:web` is green, including the negative-assertion sweep for any
   retired class/copy.
4. No horizontal overflow at the narrow end; badges/controls rewrap under the name.

## Risks & Mitigations

- **A negative test assertion silently goes vacuous** when a class/copy string is
  retired (the `.claude/rules/web.md` trap: a `queryByText(/…/).toBeNull()` that
  can no longer render passes forever). *Mitigation*: M3's explicit
  `git grep -F`-based sweep of retired strings across the test tree, repointing
  each per the paired/unpaired rule.
- **Focus-return regression** on delete-confirm dismissal — the Delete button moved
  from Row-of-everything to Row 2. *Mitigation*: it keeps its `deleteButtonId(w.id)`
  id, and M2 verifies `confirmRef`/focus-return still resolves. Covered by the
  existing confirm-flow test.
- **Vertical rhythm drift** — meta/token spacing changes from intra-column margins
  to sibling rows. *Mitigation*: keep the existing `mt-0.5` (meta) and `mt-1`
  (token row) so spacing matches today; verify in the mock build.
- **Long name shoves the badge cluster** on Row 1 (a `justify-between` row with no
  wrap on the row itself). *Mitigation*: `min-w-0 break-words` on the identity
  wrapper lets the name shrink/wrap; names are also length-capped at ingest (#169),
  so the risk is bounded.
- **Over-scope creep** into `Badge`/`Button`/`Select` or into the CPU/Memory gauge
  block. *Mitigation*: PRD scope is the `:431` header region only; those files are
  out of scope.

## Non-goals

- No change to `Badge` / `Button` / `Select` primitives or their tokens.
- No change to the CPU/Memory `WorkerStatGauges` block or the confirm-in-place
  delete block (they stay as sibling rows below the header).
- No admin fleet list (`RunsList.tsx`) change — this PRD is the user's own
  Settings → Workers card only.
- No docs, CLI, API, DB, or forge-driver change.
- No `.github/workflows/**` change (the worker PAT lacks `workflow` scope; a
  workflow-file touch would reject the whole push — see `.claude/rules/prds.md`).

## Validation strategy

- **Gate**: `task gate:web` (see `.claude/rules/web.md`). Single-file test run
  during iteration: `cd web && npx vitest run src/pages/WorkersSettings.test.tsx`.
- **Visual/responsive**: `cd web && VITE_UZI_MOCK=1 npm run dev`, open Settings →
  Workers under a mock scenario that lists workers; check Row-1 badges at Settings
  width and rewrap + no horizontal overflow at narrow widths. Do **not** use
  `vite preview`/non-mock `vite dev` (they proxy to the live stack — `.claude/rules/web.md`).
- **Negative-assertion sweep**: `git grep -F` (tracked-content aware) each retired
  class/copy string across `web/src/**/*.test.tsx`; repoint per the paired/unpaired
  discriminator in `.claude/rules/web.md`.

## Offline-worker notes (sweep handoff)

This PRD is fully internet-independent: every fact is from this repo's own source,
read locally. No milestone needs the open web, an external API, or `WebFetch`/
`WebSearch`, so it is safe for a restricted-egress uzi sweep worker. Prior-art
check (bottega / dot-agent-deck, per convention) is not load-bearing for
a self-contained flex regrouping and is skippable offline.

**Internet-independent ≠ browser-independent.** M1–M3 and M4's class assertions are
fully doable by a headless CLI worker (`task gate:web` is jsdom/vitest, no browser).
Only the *visual* half of M4 — observing pixel layout and overflow at real widths —
needs a rendering engine, and that is explicitly marked human/browser-gated
post-merge, not a worker task. The worker can complete and self-verify the code
change without it.

## Decision Log

- **2026-08-22**: Option B chosen by the user after comparing three header layouts
  (A current, B badges-top-right + controls-row, C compact inline) in a browser
  mock built from the real ember-theme card. B won on "fewer lines + conventional
  identity → state → controls scan order". The mock also disproved the
  minimal `items-start`-only tweak (Design Decision 2 / "Why the meta line must be
  full-width").
