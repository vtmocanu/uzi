# PRD #1648: Admin Health tab, triage-first layout and consistent badges

**Issue**: [#1648](https://github.com/vtmocanu/uzi/issues/1648) | **Priority**: Medium
**Status**: Complete (2026-09-25); direction approved by the maintainer on 2026-09-25 after a two-agent UX brainstorm (Claude + Codex) and a before/after screenshot review of the Card padding fix
**Evidence baseline**: `2a24f254` (2026-09-25); refresh file anchors on the implementation base
**Mock**: `prds/mockups/1648-admin-health-triage-mock.html` (open locally). Frame 1 is today's tab with measured defects outlined; frames 2 and 3 are the accepted target (incident, all clear); frame 4 is the badge and count-pill vocabulary; frame 5 is the Card padding fix. Fixture data only. The mock uses literal Dawn hex values; the product must use the theme tokens. Where the mock and the *Binding design decisions* disagree, the decisions win.

## Problem and outcome

Admin → Health (`web/src/pages/AdminHealth.tsx`, shipped by PRD #1484) reads differently from every other admin page, and its severity marks disagree with the rest of the app:

1. **Wrong type size.** The needs-attention list and every check row carry no `text-sm`, so titles and summaries render at the 16px body size; every sibling admin page renders at 14px.
2. **Double padding (a real bug, app-wide).** `Card` (`web/src/components/ui.tsx`) always adds `p-5`, and `cx` only joins class names (no tailwind-merge). The compiled CSS emits `.p-0` before `.p-5`, so `<Card className="p-0">` keeps its 20px padding. Health's group headers and rows then add their own `px-5`, so labels sit 40px in and dividers and row tints stop 20px short of the card edge. The same no-op exists at 9 other call sites (listed under *Verified current behavior*); their tables float inside an unintended 20px frame.
3. **Four badge anatomies on one page.** `SeverityPill`/`TallyPill` (full-round, 12px, semibold, font glyphs ● ▲ ◆ ? ○ whose size drifts with the typeface), `WorkerUpgradeBadge` (6px radius, 12px, regular), bare coloured status text in the fleet table, and the shared `Badge` everywhere else.
4. **Colour carries no information.** In the all-clear state 14 green "OK" pills compete for attention; in an incident the one row that matters is one of 14 pills.
5. **The Health count pip is off-vocabulary.** `HealthPip` (`web/src/components/healthSeverity.tsx`) is translucent, 11px, with an inner dot; every other sidebar count (Runs, Workers, Judge, Notifications) is a solid 10px min-width pill. Its aria-label says "(warning)" even when the worst attention check is Unknown.
6. **Smaller inconsistencies.** Hand-rolled buttons instead of `Button variant="secondary" size="sm"`; the fleet table uses uppercase `text-xs` headers where sibling admin tables use sentence-case `text-muted font-medium`; developer copy leaks into the UI ("Upgrade and Blocking read the roll health the admin list now carries"); the fleet column headed "Since" shows the last heartbeat.

**Outcome.** The Health tab becomes a triage page: what needs attention sits at the top, fully explained, worst first; a compact, expandable inventory lists every check; the fleet table has its own card. Every health surface (the tab, the Overview card, the danger banner, the tab and sidebar pips) speaks the app's shared `Badge` and nav count-pill vocabulary, with a stable SVG shape per severity. `Card` gains a real flush mode and all ten call sites use it.

This is a **web-only** change. No API, DTO, store, migration or CLI change: `GET /api/admin/health` and `GET /api/admin/workers` already return everything the new layout needs.

## Related work

| Item | Relationship |
|---|---|
| PRD #1484 (`prds/1484-admin-health-tab.md`) | Shipped the Health tab, the Overview card, the danger banner and the pips. This PRD reworks their presentation; the health document, its checks, severity rules, polling, snooze and notification behaviour are unchanged. |
| PRD #1167 | Light themes (Dawn, Hall, Shadow) and the Plex typeface; every new surface must read correctly on all five themes. |
| PRD #113 | The sidebar count-badge tones (`count`, `alert`); this PRD adds `warn`. |

## Scope

### In

- `Card` flush mode, applied at all ten call sites, with 20px outer table cells.
- A shared severity badge (Badge anatomy + SVG shape) replacing `SeverityPill` and `TallyPill`.
- The Health pip moved onto the nav count-pill anatomy (new `warn` tone) with a corrected accessible label, in the Admin tab strip and the sidebar.
- The Health tab restructure: header actions, attention card or all-clear line, All checks inventory with per-group disclosure, Fleet card.
- The Overview health card and the danger banner switched to the new severity marks.
- Tests, the admin-health doc and its mirror.

### Out

- Any change to the health document, the check registry, severities, verdict thresholds, polling cadence, snooze, the Danger notice, or `uzi admin health`.
- Conditional section reordering (for example moving Fleet up during a worker incident). The order is fixed (D1).
- Collapsing Warn items in the attention card (D3).
- Redesigning `WorkerUpgradeBadge` on the Workers page. Only the Health fleet table stops using it (D7); the Workers page keeps its current badge.
- Any other page's layout beyond the `Card` flush change and its outer-cell padding.
- Any creation, modification or validation write under `.github/workflows/**`. Any edit to frozen `specs/ai.md`.

## Verified current behavior

Resolved local-code facts at the evidence baseline; the offline worker need not look anything up online.

- `Card` (`web/src/components/ui.tsx`, `export function Card`) renders `cx("rounded-xl border border-edge bg-surface p-5", className)`. `cx` is `parts.filter(Boolean).join(" ")`. In the Tailwind v4 build `.p-0` is emitted before `.p-5`, so `p-5` wins whenever both are present (verified on the running mock server's compiled CSS, and visually: before/after screenshots of Users, Rate limits and Blocked repos show a 20px inset around each table that disappears when `p-0` is honoured, making each card 40px shorter).
- The ten `Card className="…p-0"` call sites: `pages/AdminBlockedRepos.tsx` (2: lines ~202, ~277), `pages/AdminHealth.tsx` (~205, `overflow-hidden p-0`), `pages/AdminRateLimits.tsx` (2: ~301, ~387), `pages/AdminUsers.tsx` (~120), `pages/Agents.tsx` (~140), `pages/ForgeSettings.tsx` (~367), `pages/Skills.tsx` (~302), `pages/ToolAllowlist.tsx` (~112). Nine wrap `<div className="overflow-x-auto"><table className="w-full text-left text-sm">` with `px-4 py-3` cells and a `thead` carrying `border-b border-edge text-muted`; `AdminBlockedRepos.tsx:~202` starts with a `border-b border-edge px-4 py-3` header block. Re-run `git grep -n 'Card className="[^"]*p-0' -- web/src` on the implementation base; the list is authoritative from that command, not from these line numbers.
- `web/src/components/healthSeverity.tsx` exports `Sev`, `SEV` (label, glyph, pill classes), `sevOf`, `SeverityPill`, `HealthPip`. `SeverityPill` is used by `AdminHealth.tsx` and `HealthOverviewCard.tsx`; `HealthPip` by `AdminShell.tsx` (tab strip) and `AppShell.tsx` (expanded sidebar Admin item; the collapsed rail draws its own severity dot and an sr-only count). `HealthDangerBanner.tsx` renders a literal `◆` glyph before the verdict.
- `Badge` (`ui.tsx`) is `rounded-md border px-1.5 py-0.5 text-[11px] font-medium` with tone classes `border-<tone>/40 bg-<tone>/10 text-<tone>` (`neutral` and `queue` use their own border/surface/fg tokens) and an optional CSS dot.
- The sidebar count pill (`AppShell.tsx`, `NavItem`, `badgeTone?: "count" | "alert"`) is `ml-auto min-w-[1.25rem] rounded-full px-1.5 py-0.5 text-center text-[10px] font-semibold leading-none`, `bg-brand text-on-brand` or `bg-danger text-on-brand`, capped at `99+`, with an `aria-label` of `${badge} ${badgeNoun}`. `text-on-brand`, not `text-white`, is deliberate (a comment there records the measured contrast).
- `AdminHealth.tsx` today: `VerdictCard` (glyph at `text-2xl`, `h2` `text-lg`, `TallyPill`s, an attention `<ul>` whose links call `jumpToCheck` to open and scroll a `<details id="c-<check id>">`, a footer with "Checked … ago, refreshes every 10 s", the check count and a hand-rolled Copy diagnostics button); one `GroupCard` per group in `GROUPS` order (`workers`, `queue`, `control`, `integrations`, `housekeeping`) with `CheckRow` `<details>` rows (danger and unknown rendered open; the `open` value is stable so a manual toggle survives the 10 s poll); `FleetTable` at the end of the workers group, polling `api.adminListWorkers` every 10 s and keeping last-good rows; `CommandLine` with a hand-rolled Copy button. `reducedMotion()` gates smooth scrolling.
- `web/src/lib/healthView.ts` exports `isAttention`, `attentionChecks` (danger, then unknown, then warn) and `healthVerdict(status, counts)`, shared by the page, `HealthOverviewCard` and `HealthDangerBanner`.
- Untrusted strings: worker names and blocking reasons are re-stripped with `stripUnsafeChars` before reaching a `title=` attribute (`FleetRow`, `BlockingCell`). Check summaries, evidence and commands are server-composed. Keep both properties.
- Tests pinned to the current UI: `web/src/pages/AdminHealth.test.tsx` (for example "all passing" appears 5 times; tab pip label `/checks need attention \(warning\)/`), `web/src/components/HealthOverviewCard.test.tsx`, `web/src/components/HealthDangerBanner.test.tsx`, `web/src/lib/useAdminHealth.test.tsx`, plus AppShell tests that read the Admin pip.
- Mock mode: `?mock=health-silent|health-degraded|health-incident` (`web/src/mocks/mockApi/health.ts`, fixtures in `web/src/mocks/data/health.ts`, which also exports `unknownDoc` and `noHostedWorkersDoc` for tests).
- Docs: `docs/admin-health.md` (the "Admin → Health" bullet describes the verdict card, the jump list, one card per group, and the fleet table "at the bottom of the workers group"; the pip sentence). After editing any `docs/*.md`, run `task docs:sync` (embedded mirror, `TestEmbeddedDocsMatchSource`).

## Binding design decisions

**D1. Page order is fixed.** Header line → attention card (or the all-clear line) → **All checks** card → **Fleet, all users** card. No state reorders sections.

**D2. Header line.** Under the admin tab strip, the existing per-tab description sentence on the left; on the right "Checked 16s ago" (live, as today) and **Copy diagnostics** as `Button variant="secondary" size="sm"` (same clipboard behaviour and "Copied" feedback). The "refreshes every 10 s" and "N checks" texts move: the refresh note sits on the right of the all-clear line; the count is carried by the inventory header (D5).

**D3. Attention card (any check is danger, unknown or warn).** One `Card flush`. Its header band:
- Tone follows `doc.status`: `danger` uses the `HealthDangerBanner` treatment (`border-danger/40`, `bg-danger/10` band, danger border on the card); `warn` (which includes unknown-only) uses the same shape in warn tone.
- Title: `healthVerdict(...).title`, unchanged wording ("uzi cannot run work: 3 blocking checks", "2 warnings, nothing is blocked").
- Sub line, derived **locally in the page** from the same counts as the list below it (do not change the shared `healthVerdict.sub`, which the Overview card reads): in danger, "Plus K more that need attention without blocking work. Worst first, each with what to do." when K = attention − danger > 0, else "Worst first, each with what to do."; in warn, "Work is still flowing. Worst first, each with what to do."
- Right side: one severity badge per non-zero attention severity with its count (for example "3 Danger", "1 Unknown", "1 Warn"). No OK tally here.
- The header band carries `role="alert"` for danger and `role="status"` otherwise, consistent with `HealthOverviewCard`.

Below the band, every attention check, worst first (`attentionChecks`), **all fully expanded** (danger, unknown and warn alike; not a `<details>`): severity badge; title (`font-medium`), summary (`text-muted`), and a faint "· <group title>"; `since` on the right; then "**What to do.**" + action, the evidence `<dl>`, the command with a `Button` Copy, and a faint footer with the check id and its Docs link. For a check whose id starts with `fleet.`, the footer adds "See the affected workers in Fleet ↓", an in-page link to the Fleet card. Each item has `id="c-<check id>"` so in-page links land on it. All text in this card is `text-sm` (14px); evidence and footer `text-xs`.

**D4. All-clear line (no attention checks).** A single-line `Card` instead of the attention card: a 18px ok-tinted check icon, "**All systems normal.** Nothing needs attention." and "refreshes every 10 s" on the right. `na` checks do not break the all-clear state (they are not attention, as today).

**D5. All checks card.** `Card flush` with a `SectionTitle`-style header "All checks" and, on the right, "P of T passing" (T = checks that apply, i.e. excluding `na`; append ", N not applicable" when N > 0). One row per group, in `GROUPS` order:
- A disclosure `<button type="button" aria-expanded aria-controls>` with a chevron (rotates 90° open; no rotation animation under reduced motion) and the group title. **Collapsed by default.** Open state lives in component state keyed by group id and survives the 10 s poll; it is not persisted.
- Next to the button (outside it, so the two interactions never nest), one chip per check: ok = small ok check icon + name in `text-muted`; na = dashed-circle shape + name in `text-faint`; attention = the severity shape + name in `text-fg font-medium`, rendered as an in-page link to its attention item (`#c-<id>`), reusing the existing scroll-into-view + reduced-motion logic. Each chip's shape has an accessible severity word (visually hidden text or `aria-label`), never colour alone.
- On the right: "all N passing" in `text-faint` when every applicable check passes; otherwise a severity badge (danger tone if any danger in the group, else warn) reading "K of N need attention". In both, N counts only the group's applicable checks (`na` excluded from the denominator); when the group has `na` checks append ", M not applicable" (for example "all 2 passing, 1 not applicable"). A group whose checks are all `na` reads "not applicable".
- Expanded: one line per check, in registry order: its shape icon, title, summary (`text-muted`), and the check id in faint mono on the right. This keeps the passing checks' own numbers (for example "Database reachable (2ms); schema at head") one click away.
- Rows stack (button, chips, status on separate lines) below the `sm` breakpoint; no horizontal page scroll at 390px.

**D6. Severity marks.** One shared component (name it in the Decision Log, for example `SeverityBadge`) replaces `SeverityPill` and `TallyPill`: the `Badge` anatomy and tones (`ok`, `warning`, `danger`, `neutral` for unknown and na) with an 8px inline SVG shape in `currentColor`: ok = filled circle, warn = filled triangle, danger = filled diamond, unknown = hollow circle, na = dashed hollow circle. The visible words stay "OK", "Warn", "Danger", "Unknown", "N/A" (tests assert on them). Export the shape as its own small component so the attention band, the non-OK inventory chips and the banner reuse it. OK deliberately has two marks: the filled circle inside the OK badge, and a quiet check mark (no fill, no border) on inventory chips and expanded inventory lines (D5), so passing checks do not compete with attention ones. No font glyph (● ▲ ◆ ? ○) remains in any health surface; `HealthDangerBanner`'s `◆` becomes the danger shape.

**D7. Fleet card.** Own `Card flush` after All checks, header "Fleet, all users" + "Worker status and upgrade blockers across every user". Table matches sibling admin tables: sentence-case `th` in `text-muted font-medium`, `px-4 py-3` cells (outer cells per D8), `divide-y`. Columns: Owner, Worker (mono name + faint "hosted/…" kind on the same cell, since the Kind column is dropped), Status (`Badge dot`: online = ok, offline = neutral, upgrade failed state keeps the danger tone via the Upgrade column), Version (mono), Upgrade (`Badge`: up to date = ok, outdated = warning, upgrading = info, upgrade failed = danger). `WorkerUpgradeBadge.tsx`'s `PRESENTATION` map is module-private today: extract it into an exported pure mapping (status → label + Badge tone) that both `WorkerUpgradeBadge` and the Health fleet read, so the words cannot drift; the Workers page keeps rendering its own badge unchanged. The Health badge's `title` is `stripUnsafeChars(worker.upgrade_detail)` when present, exactly as `WorkerUpgradeBadge` does, Blocking (as today, danger mono text, same `stripUnsafeChars` handling and `likelyCause` title), **Last seen** (renamed from "Since"; same `last_heartbeat_at` value). Loading shows skeleton rows instead of "Loading fleet…"; the empty text stays. Polling and last-good behaviour unchanged.

**D8. Card flush.** `Card` gains a boolean `flush` prop: when set it omits `p-5` and makes the outermost table cells 20px (`first-child` `pl-5`, `last-child` `pr-5` on `th`/`td` descendants, applied once by `Card`, not at each call site), so table text shares the 20px left edge of ordinary card content while dividers run edge to edge. All ten `className="…p-0"` call sites switch to `flush` and drop `p-0`; `AdminBlockedRepos.tsx`'s header block goes to `px-5`. After the change `git grep -n 'Card className="[^"]*p-0' -- web/src` returns nothing. A regression test pins it (M1).

**D9. Count pill.** `NavItem`'s `badgeTone` gains `"warn"` (`bg-warn text-on-brand`). Measured from the current tokens: about 6.3:1 on the light themes (white on `--warn` 154 74 5) and over 11:1 on Ember and Mission, so it clears WCAG AA everywhere; note that in the same comment style as the existing `alert` contrast note. `HealthPip` is replaced by that anatomy in both places it renders (the expanded sidebar Admin item and the Admin tab strip): tone `alert` when any check is danger, else `warn`; same `99+` cap. Accessible label: "N health checks need attention: X danger, Y unknown, Z warning", listing only non-zero severities, singular "check" for N = 1. The collapsed rail's severity dot and sr-only text stay, with the same corrected wording.

**D10. Other health surfaces.** `HealthOverviewCard` and `HealthDangerBanner` switch to D6's marks and `text-sm` wording; their structure, roles, snooze and links are unchanged.

**D11. Typography.** Every health surface uses the app scale: body text `text-sm`, meta `text-xs`, section titles via `SectionTitle`, no `text-lg`/`text-2xl` glyphs. Theme tokens only (`text-ok`, `bg-danger/10`, `border-edge`, …); no literal colours.

## Milestones

Each milestone ends with `task gate:web` green (run once to a log, per the root CLAUDE.md *Run economy*). Tests assert behaviour and accessible text, never colour classes.

- [x] **M1. Card flush, applied app-wide.** D8 implemented; all ten call sites migrated. Tests: a `Card` unit test that `flush` renders no `p-5` class and a default `Card` still does (fails on the unfixed `Card`, which has no working flush path; watch both directions per the bug-fix rule in the root CLAUDE.md); a source guard test (or a check in an existing test) that no `web/src` file passes `p-0` to `Card`. Existing tests for the ten pages stay green.
- [x] **M2. Shared severity marks and the count pill.** D6 and D9: the severity badge and shape components, `warn` nav tone, the pip in the tab strip and the sidebar, corrected accessible label. `SeverityPill`, `TallyPill` and `HealthPip` removed (knip must stay green). Tests: shape + word per severity; pip label wording for danger-only, unknown-only, mixed and N = 1; the collapsed rail sr-only text.
- [x] **M3. Health tab: header line, attention card, all-clear line.** D1–D4, D11. Tests: all-clear renders the quiet line and no attention card; degraded renders a warn band and both warn items expanded with their action; incident renders the danger band, the derived sub line with the right K, items worst first, the Fleet link only on `fleet.*` items; `unknownDoc` renders Unknown items expanded; Copy diagnostics still copies the document.
- [x] **M4. All checks inventory.** D5. Tests: groups collapsed by default; toggling sets `aria-expanded` and shows summaries; open state survives a re-render with a new document (the poll); an attention chip is a link to `#c-<id>` and activating it scrolls (reduced-motion respected); `noHostedWorkersDoc` shows N/A chips and "not applicable" in the header count; no nested interactive elements.
- [x] **M5. Fleet card.** D7. Tests: sentence-case headers, "Last seen", Badge status/upgrade words, blocking text and its stripped `title`, skeleton while loading, last-good rows kept on a failed poll (existing behaviour).
- [x] **M6. Overview card and danger banner.** D10. Existing tests updated to the new marks; no behaviour change.
- [x] **M7. Docs, copy sweep and tests hygiene.** Update `docs/admin-health.md` (the Admin → Health bullet, the pip sentence) to the new layout; `task docs:sync` and commit the mirror. Sweep retired strings ("all passing" as the group-header wording if changed, "Loading fleet…", "Upgrade and Blocking read…", "(warning)" pip wording, "Since" header) across the test tree and repoint every unpaired negative assertion (`.claude/rules/web.md`, *Copy changes disarm negative assertions*). Check whether `api/cmd/uzi/` needs a change (expected: none; say so in the PR).
- [x] **M8. Visual validation in mock mode.** `cd web && VITE_UZI_MOCK=1 npm run dev`; check `/admin/health?mock=health-incident`, `?mock=health-degraded`, `?mock=health-silent`, and each of the ten flush call sites' pages, on Dawn and Ember at 1440px, plus Health at 390px (no horizontal page scroll; inventory rows stack). Attach before/after screenshots of Health (incident and all clear) and of at least Users, Rate limits and Blocked repos to the PR. Any flush site that looks worse is a finding to fix in this PR, not a follow-up.
  - *Run status (2026-09-25, commit `9537dea8`):* the mock-mode browser pass ran and passed. It covered Health incident, degraded and silent on Dawn and Ember at 1440px; Health at 390px (page scrollWidth equals clientWidth, inventory rows stack); every flush call site at 20px text edges with edge-to-edge tables; and the Rate limits rowspan continuation cells aligned to within 0.00px in both tables. Before and after screenshots were captured locally but not attached to the PR, because the agent run cannot attach images. The mock incident has no Unknown check, so the Unknown badge was not seen rendered.
  - *Maintainer validation (2026-09-25, head `3866d07d`, PR comment):* local mock build; Health incident, degraded and silent on Dawn and Plex at 1440px, incident on Ember at 1440px; Health at a true 390px viewport (no page-level horizontal scroll, inventory rows stack, commands and the fleet table scroll inside their cards); every flush card site (Users, Rate limits, Blocked repos, Tool allowlist, Agents, Skills, Forge settings) with edge-to-edge rules, 20px outer text and aligned Rate limits continuation rows. No regressions found. Screenshots were not attached to the PR (see Decision Log).

## Success criteria

- No `Card` call site passes `p-0`; tables in flush cards have edge-to-edge rules and a 20px text edge.
- No health surface renders a font glyph for severity or a `SeverityPill`/`TallyPill`/`HealthPip`; the tab and sidebar pips match the nav count pill.
- In the incident mock, the first card on the page lists all five attention checks with their actions without any click; in the all-clear mock, the page shows one quiet line, a collapsed inventory, and the fleet.
- `task gate:web` green; `gate:repo` unaffected (no workflow, spec or migration change).

## Risks

| Risk | Mitigation |
|---|---|
| The flush change alters nine pages outside Health | Approved by the maintainer from real screenshots; M8 checks every site visually before merge. |
| Negative assertions silently rot on copy changes | M7's retired-string sweep. |
| Disclosure state lost on the 10 s poll | M4 test re-renders with a fresh document and asserts the group stays open. |
| A long attention list pushes the inventory and fleet down | Accepted (D3); revisit only if real incidents produce a much longer list. |

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-09-25 | Triage-first layout (mock frame 2/3) over an "align only" or "quiet green" variant | Maintainer choice after reviewing four boards; the page's job is to say what is wrong and what to do. |
| 2026-09-25 | Warn items stay expanded; Fleet keeps a fixed position | Codex review: warn actions belong in the triage queue; conditional reordering adds complexity and is wrong for non-worker incidents. |
| 2026-09-25 | Passing checks stay one click away in a per-group disclosure | Their summaries carry real measurements (DB latency, loop counts). |
| 2026-09-25 | Page sub line derived locally, not via shared `healthVerdict.sub`; N/A excluded from every passing denominator; upgrade presentation map extracted and shared | Codex PRD review: keeps Overview copy stable, stops inapplicable checks reading as passing, and removes a literal-impossible reuse. |
| 2026-09-25 | Fix `Card` padding app-wide with a `flush` prop, not tailwind-merge | Every call site meant `p-0`; the before/after screenshots were approved; adding a dependency for one class conflict is not warranted. Outer cells at 20px (both reviewers). |
| 2026-09-25 | Implementation names: `SeverityShape` and `SeverityBadge` (`web/src/components/healthSeverity.tsx`), `CountPill` (`web/src/components/ui.tsx`, tones `count`/`alert`/`warn`, shared by `NavItem` and both health pips), `healthPipLabel` (`web/src/lib/healthView.ts`), and `upgradePresentation` (`web/src/components/WorkerUpgradeBadge.tsx`) | Recorded at implementation. One component per concept, so the Health tab, the pips, the Overview card and the banner cannot drift apart. |
| 2026-09-25 | `flush` is applied through Tailwind arbitrary variants on the `Card` root (`[&_th:first-child]:pl-5`, `[&_th:last-child]:pr-5`, `[&_td:last-child]:pr-5`, `[&_td:first-child:not([data-flush-inner])]:pl-5`). A cell that is first in the DOM but not the leftmost column (the Rate limits rowspan continuation rows) opts out with `data-flush-inner`. `overflow-hidden` stays opt-in per call site. | Plan review: `td:first-child` is not always the leftmost visual column. Clipping by default could cut off menus on the other nine pages. Tailwind 4.3.3 compiles the nested-bracket variant, verified in the built CSS. |
| 2026-09-25 | The attention card's tinted border uses the important modifier (`border-danger/40!`) | Card's own `border-edge` is emitted later in the CSS and would otherwise win: the same class-ordering trap as `p-0`. |
| 2026-09-25 | The pip label reads "1 health check needs attention: …" for one check and "N health checks need attention: …" otherwise | Grammatical singular. The tests match both forms (`/health checks? needs? attention/`). |
| 2026-09-25 | M8 closed on the maintainer's own mock-mode visual pass of `3866d07d` instead of before/after screenshots attached to the PR | The maintainer checked every page M8 names in a local mock build (Health degraded and silent on Dawn only; incident also on Ember), found no regressions, and said M8 can be ticked; the agent run cannot attach images. Accepted residual: the Unknown badge was not seen rendered in any mock (no mock carries an Unknown check); it is covered by unit tests only. |
