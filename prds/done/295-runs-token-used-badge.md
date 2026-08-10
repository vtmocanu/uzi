# PRD #295: Show which token a run used, on the Runs list

**GitLab Issue**: [vtmocanu/uzi#295](https://gitlab.example.com/vtmocanu/uzi/-/issues/295)
**Status**: Complete (shipped on `agent/issue-295`)
**Priority**: Medium
**Created**: 2026-08-10
**Depends on**: none
**Related**: `web/src/pages/RunsList.tsx`, `web/src/components/RunCredential.tsx`, `web/src/lib/runCredential.ts`, `web/src/lib/hasToken.ts`, `web/src/lib/api.ts`

> Every file:line citation below was derived at **`efa79d1c`** (post-v0.26.0). A line
> number without a SHA is not a citation, re-derive before acting.

## Problem

A user with more than one Anthropic token cannot tell, from the Runs list, which
credential each run's claim billed, or why that one. The information already exists per
run: `RunCredential` (`web/src/components/RunCredential.tsx`) renders "which account paid,
and why" on the run detail view (`RunView.tsx`), `AgentDetail.tsx` and `Agents.tsx`. But it
is **not** on the list, so answering "which account paid for this batch of runs?" means
opening each run one at a time.

For a single-token user there is nothing to say (every run billed the one token), which is
why this must be gated rather than shown to everyone.

### What already exists (traced at `efa79d1c`)

- **The data is already on the list rows.** `GET /api/runs` (`ListRunsForUser`) and
  `GET /api/admin/runs` (`ListActiveRunsAll`) both `SELECT sqlc.embed(r)`
  (`api/internal/store/queries/runtime.sql:398` and `:431` respectively), so the full `runs`
  row reaches `runToDTO` (`api/internal/handler/workers.go:305`), which maps
  `anthropic_secret_id` (`:366-369`), `anthropic_secret_label` (`:370`),
  `anthropic_select_reason` (`:376`) and `anthropic_headroom_pct` (`:377-382`).
  `RunListItemDTO` embeds `RunDTO` (`api/internal/handler/runs.go:68` personal, `:95`
  admin), so the four credential fields are present on every list row **today**. On the TS
  side `RunListItem extends Run` (`web/src/lib/api.ts:1240`) and `Run` carries all four
  (`:1157-1158`, `:1174-1175`), so the fields are already typed and populated — **no server,
  query, or DTO change is needed.**
- **The rendering logic is already shared.** `describeCredential`
  (`web/src/lib/runCredential.ts`) turns the four fields into `{mode, hint, tone, linked,
  deleted}` from the server's own record. `RunCredential` renders the full sentence-length
  chip from it. This PRD reuses that module unchanged; it only adds a **compact** rendering
  and drops it into the list rows behind a gate.

## User journey

1. A user connects two or more Anthropic tokens (e.g. `prod-console` + `personal`, or an
   auto-selection pool) and starts several runs.
2. They open **Runs**.
3. **Today**: each row shows title, repo/#iid, worker, time, duration, MR, tokens and cost,
   and a status pill — but nothing about *which* credential the run billed.
4. **After**: each row also carries a compact credential badge in the existing pill cluster
   — the token label, with a small tone dot when the selection state is worth a look
   (info/warning), and the selection mode + reason on hover. A calm `auto`/`pinned` pick is
   quiet (no dot, neutral). A fallback (empty pool, stale readings, undecryptable token)
   reads amber and links to Settings → Anthropic tokens.
5. A single-token user sees the list exactly as before.

## Scope

**In scope**
- A **compact** rendering of the run credential: token label + tone dot (non-neutral only)
  + `(deleted)` marker, with the selection mode and hint on hover (`title`). Reuses
  `describeCredential`; adds a `variant="compact"` path to `RunCredential` (default stays
  the current full chip so `RunView`/`AgentDetail`/`Agents` are untouched).
- Wiring the compact badge into the run rows on `RunsList.tsx` (both the personal list and
  the admin factory list), gated on the viewer holding **more than one** Anthropic token.
- A `anthropicTokenCount(secrets)` helper alongside the existing `hasAnthropicToken`
  (`web/src/lib/hasToken.ts`), so the ">1" gate is one tested predicate, not an inlined
  expression (same rationale that file already documents for `hasAnthropicToken`).
- Fetching the viewer's secrets in `RunsList` to compute the gate.
- Tests (vitest) for the gate, the compact rendering, tone-by-reason, the deleted marker,
  the link-only-when-non-neutral rule, and the admin-list behaviour.
- Light doc note where token selection is already documented.

**Out of scope**
- Any server-side, SQL, or DTO change — the list endpoints already carry the fields.
- Changing the **full** `RunCredential` chip on `RunView`/`AgentDetail`/`Agents`.
- Changing selection logic, headroom computation, or what `select_reason` values exist.
- Showing the badge to single-token users, or any per-token meter on the list row (that
  lives on Settings → Anthropic tokens and the run detail view).

## Technical approach

**Compact variant (`RunCredential.tsx` + `runCredential.ts`).** Add an optional
`variant?: "full" | "compact"` prop (default `"full"`). The compact path renders a `Badge`
(`web/src/components/ui.tsx`) whose children are: an optional tone `dot` (only when
`tone !== "neutral"`, matching `RunCredential`'s existing "link iff non-neutral" rule), the
`sanitizeLabel(label)` text, and a trailing `(deleted)` when `deleted`. The selection
`mode` and `hint` do **not** render inline; they go in the `title` (and the existing
`sr-only` hint span + `aria-describedby` are preserved for screen readers). Tone → `Badge`
tone maps exactly as the full chip does (`warning`→warning, `info`→info, else neutral). The
link rule is unchanged: linked iff `describeCredential(...).linked`, i.e. tone is
non-neutral — **except on the admin list** (see Decision 2). Returns `null` when there is
no label, identical to today (a run claimed before PRD #111 M1, or not yet claimed, says
nothing).

Reuse `describeCredential` verbatim — do not re-derive mode/tone/deleted in the component.
A compact chip that computed its own tone would eventually disagree with the full chip and
the settings page.

**Gate + wiring (`RunsList.tsx`).** `RunsList.load()` already `Promise.all`s
`api.listRuns()` (+ admin calls). Add `api.listSecrets()` (`web/src/lib/api.ts:2147`) to
that batch and store `anthropicTokenCount(secrets)`. `RunRow` gains two props,
`showCredential?: boolean` and `credentialLinkable?: boolean` (default `true`), threaded to
`<RunCredential run={run} variant="compact" linkable={credentialLinkable} />`, rendered
inside the existing right-side badge container (the `<div className="flex items-center
gap-2">` at `RunsList.tsx:118`), after the status pill. Wire the three states explicitly at
the call sites:

- **Personal list** (`RunsList.tsx:224` active, `:249` past): `showCredential={count > 1}`,
  `credentialLinkable` left default (`true`).
- **Admin factory list** (`RunsList.tsx:272-276`, rendered with `showOwner`):
  `showCredential` set **unconditionally** and `credentialLinkable={false}` (Decision 2 —
  the label is another user's and the link points at the admin's own settings).

Add `flex-wrap` to that `:118` badge container (it is `flex items-center gap-2` today, **not**
wrapping), so the added chip wraps within the cluster on narrow viewports instead of
overflowing. The badge already self-hides when a row has no label, so a mixed list (some
pre-#111 runs, some claimed) renders correctly with no per-row guard beyond the label check
`RunCredential` already does.

**Safety.** The label is user-authored and already flows through `sanitizeLabel`
(`RunCredential.tsx:49`) which handles Cf/bidi overrides; React escapes the rest. On the
admin factory list this is **cross-principal** — an admin reads labels other users chose —
which is the same trust posture as the worker names and owner emails already shown there;
`sanitizeLabel` covers it. No new sink.

## Decisions

1. **Compact = label + hover, per the request.** The mode/reason/headroom are shown on
   hover (`title`) and to screen readers, not inline, so the list row stays scannable. The
   full inline chip remains the run-detail treatment.
2. **Admin factory list shows the badge but does not link it.** The credential on an admin
   row belongs to *another* user, while `RunCredential`'s link points at the viewer's own
   `/settings` — a dead end for someone else's token. So on `showOwner` rows the compact
   badge renders (an admin auditing spend wants provenance) but is **non-linked** regardless
   of tone. The personal-list gate (viewer has >1 token) is applied on the personal list; on
   the admin list the badge shows whenever a label exists (an admin with one token still
   audits others' multi-token spend).

   **As shipped (refined in review):** the compact badge is **never** a link at all — on the
   personal list *or* the admin list. It is always embedded inside the run row's own
   `<Link to="/runs/:id">`, so an inner `<Link to="/settings">` would be a nested `<a>`
   (invalid HTML, a competing tab stop). A live-browser review caught this on the personal
   list. The planned `linkable` prop was therefore **removed**: compact returns a bare
   `<Badge>`, and only the **full** run-detail chip links (iff non-neutral, unchanged). This
   satisfies Decision 2 by construction (nothing on the list links to the wrong `/settings`)
   and makes success criterion 5 hold everywhere, not just on admin rows. The tone dot +
   hover `title` carry the "worth a look" signal on the list; the actionable `/settings` link
   stays on the run-detail full chip, which is not inside a row link.
3. **Gate is ">1 Anthropic token", computed from `listSecrets`.** Not from a run field and
   not from `hasAnthropicToken` (which is "≥1"). One new tested helper,
   `anthropicTokenCount`, keeps the threshold in a single place.

## Milestones

- [x] **M1 — Compact credential rendering.** Added `variant?: "full" | "compact"` (default
  `"full"`) to `RunCredential`; compact renders label + tone dot (non-neutral only) +
  `(deleted)` marker, with mode/hint (and the full label) in `title`. `describeCredential`
  reused unchanged. Only `RunView` renders `RunCredential`, and it is unaffected (default
  variant). Added `anthropicTokenCount(secrets)` to `web/src/lib/hasToken.ts`. **Note:** the
  planned `linkable` prop was removed in review — see Decision 2 (as shipped): the compact
  badge is inherently non-linked (it lives inside the row `<Link>`), and its label is clamped
  `max-w-[12rem] truncate` (no in-badge `sr-only`/`aria-describedby`, so the row link's
  accessible name stays concise).
- [x] **M2 — Wire into the Runs list behind the gate.** `RunsList.load()` fetches
  `api.listSecrets()` (best-effort `.catch`); personal active + past rows render the compact
  badge when `count > 1`; admin factory rows render it (non-linked, always). Badge sits in
  the row's right-side badge container, which gained `flex-wrap` so the added chip wraps
  rather than overflowing on narrow viewports.
- [x] **M3 — Tests.** vitest covering: gate hidden at 0 and 1 token, shown at ≥2; a row
  with no label renders no badge at any count; tone/`(deleted)` derived from `select_reason`
  + `anthropic_secret_id`; the compact badge is **never** a link (regression guard for the
  nested-anchor fix, per reason via `it.each`); the full variant still links for non-neutral;
  admin rows render the badge non-linked; `anthropicTokenCount` unit test.
- [x] **M4 — Docs + mock.** Noted the list badge in `docs/anthropic-token.md`. The mock
  already seeds the mock user with five Anthropic tokens and run rows carrying
  `anthropic_secret_label`/`select_reason` across neutral/info/warning/deleted states, so the
  mock-mode visual check works with no fixture change (verified in-browser).
- [x] **M5 — Gate green.** `task gate:web` passes (oxlint, knip, check-docs, typecheck,
  vitest 1897/1897); no new knip unused-export warnings; no server/Go change introduced.

## Success criteria

1. With two or more Anthropic tokens connected, each run row on **Runs** shows a compact
   badge naming the token its claim billed; hovering reveals the selection mode and reason.
   (Verifiable in mock mode with the M4 fixture, or against a real multi-token account.)
2. With zero or one token, no credential badge renders on any personal row; the rows are
   otherwise unchanged.
3. A run with no recorded credential label (pre-PRD #111 M1, or not yet claimed) shows no
   badge, at any token count.
4. Tone, `(deleted)` marker, and linking match the run-detail chip for the same
   `select_reason`/`anthropic_secret_id`, because both derive from `describeCredential`.
5. On the admin factory list the badge appears but never links to the admin's own settings.
6. `task gate:web` is green; **no** change under `api/`, `controller/`, or the SQL/DTO layer.

## Risks and mitigations

- **Risk: the four credential fields are not actually populated on list rows.** Believed
  populated (see "What already exists"), but the `RunListItemDTO`-vs-`RunDTO` split
  (`web/src/lib/api.ts:1231-1240`) is exactly the trap where a TS field is typed but not
  sent. Mitigation: M3 asserts against real DTO-shaped fixtures, and the M4 mock/visual
  check exercises the live `realApi` shape; if a field is missing, it surfaces there before
  release. (No code change is anticipated — this is a verification, not a work item.)
- **Risk: row crowding.** The badge container already carries autopilot, milestone, health,
  judge and status badges; a sixth chip could overflow on narrow screens. Note the
  `RunsList.tsx:118` container is `flex items-center gap-2` with **no** wrap today (only the
  outer `Link` and the meta line wrap). Mitigation: compact = label only; **add `flex-wrap`
  to that container** in M2; visual check at mobile widths.
- **Risk: cross-principal label exposure on the admin list.** Mitigation: `sanitizeLabel`
  already runs (same posture as worker names/owner emails there); Decision 2 additionally
  strips the misleading self-settings link on admin rows.
- **Risk: mock mode validates rendering, not data divergence** (per `.claude/rules/web.md`).
  Accepted: this feature *is* a rendering concern, and the credential fields are simple
  scalars, not the multi-model usage shape that caveat is about. A real multi-token account
  is the ultimate check for population.

## Validation strategy

- Unit: the M3 vitest cases in `RunsList.test.tsx` and `hasToken.test.ts` (+ a compact case
  in `RunCredential.test.tsx`).
- Visual: `VITE_UZI_MOCK=1` with the M4 fixture (mock user with two tokens; a couple of runs
  carrying credential fields across neutral/info/warning states) — mock mode validates
  **rendering**, which is this PRD's concern. Confirm the single-token state shows no badge
  and hover reveals the mode.
- Real: against a multi-token account on the live stack, confirm the label and hover match
  the run-detail chip.
- Gate: `task gate:web`.
