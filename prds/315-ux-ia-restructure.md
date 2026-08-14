# PRD #315: Web UX information-architecture restructure — Workers as a first-class page, tabbed Admin, split Settings, chosen sidebar meters

**GitLab Issue**: [vtmocanu/uzi#315](https://gitlab.example.com/vtmocanu/uzi/-/issues/315)
**Status**: Complete (2026-08-14) — retroactive PRD; the work shipped on branch `feature/ux-ia-restructure` (see the feature branch tip) and is documented here as implemented.
**Priority**: Medium
**Created**: 2026-08-14
**Depends on**: PRD #14 (multica UI redesign, done) for the grouped `AppShell` sidebar this restructures; PRD #23 (collapsible sidebar, done) for the collapsed-rail chrome the density trims respect; PRD #104 (named Anthropic tokens, done) for the multi-token sidebar meters this makes user-selectable; PRD #53 (rate-limit visibility, done) for the sidebar micro-meters themselves.

## Problem

The SPA's navigation and settings surfaces accreted faster than their information architecture. Five independent gaps, all observed on a real multi-token admin account at 1440×900 (a common laptop viewport):

1. **Workers was dual-homed and mis-placed.** The fleet lived both as a top-level sidebar entry *and* as a Settings tab (`/settings/workers`). Keeping the two nav entries from lighting simultaneously required an `excludeSubpath` active-state hack in `AppShell.tsx` (Settings stayed lit across `/settings/*` *except* `/settings/workers`, which the Factory entry owned). Worse, the red worker-incident badge — the one piece of nav that must always be visible — was buried inside a preferences area. Fleet status, upgrade rolls, and an attention badge are operations, not preferences, and since "we mostly test in k8s now" (Conventions) hosted workers are the primary runtime.
2. **Admin was five loose top-level entries.** Users, Rate limits, Tool allowlist, Blocked repos, and Instance settings were five sidebar rows wired together only by proximity. They crowded the sidebar for admins and never read as one area ("this instance's controls").
3. **The Account tab carried two unrelated jobs.** It had grown to hold both credentials (account, Anthropic tokens, vault, appearance) *and* run behavior (autopilot, usage-limit park, judge + its token binding, CI autofix, worker model) in one long scroll, with no discriminator between "who you are / what you hold" and "how your runs behave."
4. **The sidebar rate-limit rail did not scale to multiple tokens.** Since PRD #104 the rail rendered one meter pair *per readable token* — 8 meter rows on a four-token account, pushing the Factory nav group under the scroll fold at 1440×900. A round-2 automatic "most-constrained token" pick fixed the overflow but removed the user's control over *which* token they watch.
5. **Tab strips jumped, and the full admin nav did not fit a laptop.** Both `SettingsShell` and the new `AdminShell` rendered a per-tab description in the page header, so its varying line count moved the tab strip up and down on every switch. Separately, an admin's full nav (both groups, three board children, the bottom cluster) exceeded a 900px-tall viewport and scrolled.

## Solution Overview

A navigation/IA restructure of the web SPA plus one additive per-user backend field. No new service, no new trust boundary, and none of the four guardrail layers are touched — this is entirely the board/web surface (ARCHITECTURE.md's *first surface*) reorganizing itself, with the sidebar meters drawing on the existing third-surface (Agent runtime) and PRD #53 data.

- **Workers** becomes a first-class `/workers` page in the **Factory** nav group (beside Agents, Skills, Runs, Schedules, Judge); `/settings/workers` 301-equivalent redirects for bookmarks and stale deep links; the `excludeSubpath` hack is deleted.
- **Admin** consolidates into one 5-tab `AdminShell` (Users / Rate limits / Tool allowlist / Blocked repos / Instance), mirroring the existing `SettingsShell` pattern; the sidebar carries a single admin-only **Admin** entry; `/admin` redirects to the first tab.
- **Settings** splits into **Account & tokens** and **Run defaults** (now 5 tabs: Account & tokens / Run defaults / Forge / Access / Memory); run-behavior controls move verbatim to `RunDefaults.tsx`.
- **Sidebar meters** become the user's explicit choice: a per-token "Show in sidebar" checkbox in Settings, the default token always pinned, persisted as `users.sidebar_token_ids`.
- **Both shells** render a constant per-shell header (the per-tab description moves below the strip), so the tab strip is byte-stable across switches; `lg:`-scoped density trims let the full admin nav fit 1440×900 with boards still expanded.

### Inspiration check

The grouped `AppShell` shell and the unified layout-header pattern both trace to multica (`packages/views/layout/`), whose headers exist precisely to prevent the hand-placed-corner-link drift this PRD finishes removing. No prior-art project models a user-selectable subset of rate-limit meters; that piece is uzi-specific.

## Technical Design

### F1: Workers as a first-class page (`AppShell.tsx`, `App.tsx`, `WorkersSettings.tsx`)

`WorkersSettings` drops its `SettingsShell` wrapper for a plain `PageHeader` and renders at `/workers` (`ProtectedRoute`). `App.tsx` adds `/settings/workers` → `<Navigate to="/workers" replace />`. The sidebar's Workers `NavItem` moves from the Configure/Settings area into the Factory group and points at `/workers`, carrying the existing `workersAttention` badge. The `excludeSubpath` prop and its `if (active && excludeSubpath …) active = false` branch are removed from `NavItem` — dead once Workers is single-homed. Inbound links from Dashboard, Chat, and ChatComposer already target `/workers`.

### F2: Tabbed Admin (`AdminShell.tsx` new; `AdminUsers/AdminRateLimits/ToolAllowlist/AdminBlockedRepos/AdminSettings.tsx`)

`AdminShell` is a header + tab-bar wrapper, a direct sibling of `SettingsShell`: a constant `PageHeader` ("Admin — Configuration and controls for this uzi instance."), a `NavLink` tab strip with the issue-#204 overflow contract (`overflow-x-auto`, `shrink-0`/`whitespace-nowrap` tabs), then the per-page `description` below the strip, then children. Each of the five admin pages drops its own `PageHeader` and wraps its body in `AdminShell`. The sidebar's five admin rows collapse to a single `Admin` entry (admin-only), and `App.tsx` adds `/admin` → `/admin/users`. `AdminSettings` additionally gains an on-page section index (quiet pill links to `scroll-mt` anchors) built from the same render conditions as the cards, so a hidden card never leaves a dead jump link.

### F3: Split Settings (`RunDefaults.tsx` new; `Settings.tsx`, `SettingsShell.tsx`)

The run-behavior handlers (`toggleAutopilot`, `toggleWaitOnLimit`, `toggleJudge`, `setJudgeToken`, CI-autofix, worker model via `ModelSelect`) move verbatim from `Settings.tsx` into a new `RunDefaults.tsx` at `/settings/run-defaults`. `Settings.tsx` (Account & tokens) keeps the account card, the Anthropic token lifecycle, the vault, and appearance. `SettingsShell`'s `TABS` gains **Run defaults** and loses **Workers**; its header goes constant like `AdminShell`'s. Discriminator, stated in both files' header comments: Account is *who you are and what you hold*; Run defaults is *how your runs behave*.

### F4: User-selected sidebar meters (`sidebarTokens.ts` new; `AnthropicTokens.tsx`, `RateLimitMeters.tsx`)

`lib/sidebarTokens.ts` holds the single rule `isShownInSidebar(token, ids)` — default token always, else `ids.includes(id)` — plus a `uzi:sidebar-tokens-changed` window event (mirroring `lib/notifications.ts`) so a Settings save reaches the separate `SidebarRateLimits` mount immediately. `AnthropicTokens` renders a per-token "Show in sidebar" checkbox: checked+disabled for the default (pinned; hiding it would make the pinning unreadable), the explicit choice for the rest. `SidebarRateLimits` fetches `sidebar_token_ids` on mount and on the change event, filters readable tokens through `isShownInSidebar`, and renders a "+N more tokens in Settings" link for the hidden remainder. Trade recorded in code: the rail no longer self-escalates when an *unchecked* token runs hot — deliberate, the price of user control — while the app-wide `RateLimitAnnouncer` still announces tone crossings for every token via aria-live.

### F5: `sidebar_token_ids` persistence (migration `00123`, `users.sql`, `user_settings.go`, `api.ts`, `mockApi.ts`)

Migration `00123` adds `users.sidebar_token_ids uuid[]` (nullable, no default → no backfill; NULL and `{}` both read as default-only). `GetUserSettings` selects the column; `SetUserSidebarTokens` replaces the whole set. `userSettingsDTO` gains `sidebar_token_ids []string`. `PutMySettings` decodes it as `json.RawMessage` (absent = unchanged) and runs `validateSidebarTokenIds`: nil/empty clears to default-only; each entry must parse as a UUID (else 400); ids that are not one of the caller's own **non-default** `anthropic_token` secrets are silently dropped (the web sends ids it read moments ago; a token deleted in another tab must not fail the whole save); duplicates collapse; a 100-entry cap bounds the list before any per-id work. The store call failing is a 500, not a 400.

### F6: Constant-header tab stability + laptop-height fit (`SettingsShell.tsx`, `AdminShell.tsx`, `AppShell.tsx`)

Both shells render a byte-identical header across tabs; the per-tab sentence renders below the strip as ordinary content flow, so the strip never moves (byte-stable at the ~108px header height). `AppShell` gains `lg:`-scoped density trims — 4px per nav row plus trims on the nav container, group headers, divider, and footer — measured to fit an admin's full nav in a 597px nav at 1440×900 (content 574px, 23px slack). Boards stay expanded (a user requirement); each extra pinned token meter costs ~66px of footer and can push the nav back into scroll, which is the user's explicit `sidebar_token_ids` choice, not a layout regression.

## Milestones

All four shipped as the four review rounds on `feature/ux-ia-restructure`; each success criterion below was verified met at the reviewed tip.

- [x] **M1 — Core IA restructure** (`AppShell.tsx`, `App.tsx`, `WorkersSettings.tsx`, `AdminShell.tsx`, the five admin pages, `SettingsShell.tsx`). Single tabbed Admin, Workers → `/workers`, decluttered sidebar (drop the Configure/Help/Admin labeled groups for one unlabeled bottom cluster; remove `excludeSubpath`), redirects `/settings/workers`→`/workers` and `/admin`→`/admin/users`.
  - *Success (met):* both redirects resolve; a single admin-only Admin entry and Workers-under-Factory render; `excludeSubpath` is fully removed (dead code gone); no two nav items light simultaneously; every inbound `/settings/workers` link is redirect-covered (grep-clean). Guard: `AppShell.test.tsx`, admin-page tests.
- [x] **M2 — Split + wayfinding** (`RunDefaults.tsx`, `Settings.tsx`, `AdminSettings.tsx`, `RateLimitMeters.tsx`). Run-behavior controls move to the Run defaults tab; the Instance tab gains an on-page section index; the sidebar footer meter gets the round-2 automatic most-constrained pick.
  - *Success (met):* Account no longer renders run-behavior controls and RunDefaults renders all five; section-index anchors resolve only to rendered card ids (no dead jump on a hidden card). Guards: `RunDefaults.test.tsx`, `AdminSettings.test.tsx`.
- [x] **M3 — Chosen sidebar tokens, frontend** (`lib/sidebarTokens.ts`, `AnthropicTokens.tsx`, `RateLimitMeters.tsx`, `SettingsShell.tsx`, `AdminShell.tsx`). Per-token "Show in sidebar" checkbox (default pinned), single-rule `isShownInSidebar`, change-event refresh, "+N more" link; constant per-shell headers stop the tab-strip jump.
  - *Success (met):* the default token box is checked+disabled; toggling a non-default persists and updates the separate `SidebarRateLimits` mount via the change event; "+N more" appears when hidden > 0; both shell headers are byte-constant across tab switches. Guards: `AnthropicTokens.test.tsx`, `RateLimitMeters.test.tsx`, `SettingsShell.test.tsx`.
- [x] **M4 — Backend + density** (migration `00123`, `queries/users.sql`, `user_settings.go`, `api.ts`, `mockApi.ts`, `AppShell.tsx`). The `sidebar_token_ids` column, `GetUserSettings`/`SetUserSidebarTokens`, `validateSidebarTokenIds`, the DTO field; `lg:`-scoped density trims so the full admin nav fits 1440×900 with boards expanded.
  - *Success (met):* validate drops non-owned and default ids, dedupes, 100-caps, and 400s a non-UUID; a store fault is a 500; absent/older-server reads as `[]`; the admin nav fits 1440×900 on demo data without scroll. Guards: `user_settings_sidebar_test.go`, `user_sidebar_tokens_integration_test.go` (live-DB).

## Success Criteria

- Workers is reachable at `/workers` from the Factory group with its incident badge always visible; `/settings/workers` redirects; the `excludeSubpath` active-state hack no longer exists in the tree.
- Admin is one tabbed area (5 tabs) reached from a single sidebar entry; `/admin` lands on Users; non-admins are bounced by `AdminRoute` on every tab.
- Settings is Account & tokens / Run defaults / Forge / Access / Memory; run-behavior controls live only under Run defaults.
- A user picks which non-default token meters ride the sidebar; the default is always shown; the choice survives reload (persisted) and reaches the rail without a page refresh.
- Neither tab strip moves on a tab switch; an admin's full nav fits 1440×900 with boards expanded.
- Gate status at the reviewed tip: `task gate:api` green; `task gate:web` green except two pre-existing `AuthContext` failures (node `localStorage`, identical on `origin/main`); live-DB suite 360/360.

## Out of Scope

- Any change to services, trust boundaries, or the four guardrail layers — this is board/web IA only.
- A CLI surface for the sidebar preference: `api/cmd/uzi/` does not consume `/me/settings` (grep-verified) and a web-rail preference has no CLI meaning; the additive DTO field is back-compatible for any future CLI reader.
- FK-backed cleanup of stale `sidebar_token_ids`: the column is a soft reference list (a deleted token's id simply matches nothing at read), matching the theme(00041)/default_model(00031) precedent of scalar prefs on `users`.
- Re-deciding the sidebar rail's escalation behavior: dropping self-escalation on unchecked hot tokens is the accepted trade for explicit control; aria-live still covers every token.

## Risks

- **Frontend-before-backend ordering (M3 before M4).** M3 shipped the Settings checkbox and rail against `mockApi` before M4 landed the Go handler. Against a live pre-M4 server the web sent a `sidebar_token_ids` field the server ignored and read one it never returned. This was safe *only* because the design is additive and tolerant both directions (`PutMySettings` reads only named fields, so unknown JSON is ignored; the web reads `settings.sidebar_token_ids ?? []`). A clean forward-looking build would land the seam (DTO field + backend) first; recorded so the tolerance is not mistaken for luck.
- **Mock/real divergence.** `mockApi.updateMySettings` stores `sidebar_token_ids` as-given, while the real `validateSidebarTokenIds` drops non-owned/default ids and rejects bad UUIDs. Benign today because the UI can structurally only send owned non-default ids (the default checkbox is disabled; only the user's own tokens render), so no mock-mode test asserts a fiction the real UI can reach. Flagged under PRD #311 (mock/demo currency) as the case to pin if a differential test is added.
- **Goose numbering.** `00123` is a **draft** number, assigned at drafting time as the next free above the then-live head. It is renumbered to the next free number above the live `origin/main` head at the landing rebase (`origin/main` has drifted, so the final number is rechecked at landing). The boot runner is strict goose — a version below an applied head bricks upgraded instances.

## Decision Log

- 2026-08-14 (user): boards stay expanded — the laptop-height fit must come from density trims, never from collapsing the boards.
- 2026-08-14 (user, round 3): the sidebar rail must show an *explicit* user-chosen set of token meters, not an automatic pick.
- 2026-08-14 (AI): **Workers → first-class `/workers` page**, removed from Settings. The incident badge needs an always-visible home, fleet status/upgrades/attention are operations not preferences, and k8s hosted workers are the primary runtime. `/settings/workers` redirects for bookmarks; the `excludeSubpath` active-state hack (which existed only to keep the dual-homed entries from co-lighting) is deleted. Rationale lives here, not in an ADR — see the review verdict below.
- 2026-08-14 (AI): **Admin consolidated into one 5-tab `AdminShell`** mirroring `SettingsShell`, replacing five loose sidebar entries; single admin-only sidebar entry; `/admin` → `/admin/users`. Same accepted three-point coupling `SettingsShell` already carries (add page ⇒ route + `TABS` entry + wrap), no new architectural risk.
- 2026-08-14 (AI): **Settings split into Account & tokens / Run defaults.** Discriminator: identity/credentials vs run behavior. Run-behavior handlers moved verbatim (a move, not a re-implementation, so no drift risk).
- 2026-08-14 (AI): **Footer token picker** — a per-token "Show in sidebar" checkbox with the default pinned (checked+disabled). The slot has now been all three: every readable token (PRD #104, overflowed the nav), an automatic most-constrained pick (round 2), and this explicit set (round 3). Accepted trade: the rail no longer self-escalates on an unchecked hot token; the app-wide aria-live announcer still covers every token.
- 2026-08-14 (AI): **`sidebar_token_ids` persisted over `/me/settings`** (the theme/default_model seam), deliberately **not** the cookie-only secrets PATCH lanes. It is a UI *preference* — a list of references the metadata-only secrets GET already returns — not a credential; the cookie-only rule exists to stop a Bearer/CLI token from minting or replacing credentials, which this is not. Stored as a `uuid[]` on `users` (soft reference, write-validated, read-tolerant).
- 2026-08-14 (AI): **Constant per-shell header** with the per-tab description below the strip, so the tab strip is byte-stable across switches (round-3 "the tabs jump" feedback); both shells keep the identical treatment.
- 2026-08-14 (AI): **Laptop-height fit via `lg:`-scoped density trims**, boards left expanded per the user requirement; each extra pinned meter costs ~66px of footer and may reintroduce scroll — the user's explicit choice, not a regression.
- 2026-08-14 (AI, post-review fix round): DB fault on the settings write returns 500 (not 400); the checkbox and the rail both call `isShownInSidebar` so the "default always, extras by choice" rule has a single source; added two tests (the 100-entry cap boundary, the handler live-DB path); removed the two now-dead icon exports `GaugeIcon` and `UsersIcon` from `icons.tsx` (`PackageIcon` stayed — still used by `ToolAllowlist`).
- 2026-08-14 (AI, architecture review — the feature branch tip): **No ADR.** The Workers-as-first-class-page decision is a navigation/IA choice, not a cross-service or trust-boundary seam. Every entry in `adr/` is a backend/trust-boundary invariant; web-IA rationale in this repo consistently lives in web-UX PRD Decision Logs (PRD #23, #11, #120). The "seam other surfaces link into" is a redirect-preserved route path, not an interface other packages implement — if `/workers` moved again the fix is one more redirect, no silent breakage — and no invariant a future change would silently break. This Decision Log is the durable record; the inline `AppShell`/`SettingsShell`/`AdminShell` header comments carry the per-site rationale. ARCHITECTURE.md documents no web-IA/nav structure (by design — it covers services/boundaries/data flows), so it needs no edit for this change.
