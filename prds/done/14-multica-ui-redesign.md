# PRD #14: Adopt the Multica-inspired UI Redesign (ember)

**GitLab Issue**: [vtmocanu/uzi#14](https://gitlab.example.com/vtmocanu/uzi/-/issues/14)
**Status**: Complete (2026-07-05) — merged to main via MR !14 (`2efd83b`), issue #14 closed, specs synced, prototypes cleaned up. Implementation by agent team: every milestone reviewed (reviewer 5 passes, all APPROVE) + audited (2 passes, clean) + browser-validated (web-ux, 8 flows, no blockers) + tested (M4 all green incl. real-GitLab scratch leg + 401-interceptor fix at 62f102d). Post-review additions, user-approved: unified run status colors across surfaces (runBadge info/ok tones), auth-page redirect guard, live-pill gating, themed focus-visible ring. M4 also surfaced a compose-isolation footgun (shell env overrides --env-file) — recorded in CLAUDE.md. 2026-07-05 earlier: PRD #12 landed on `main` before this PRD started, inverting Decision 6's planned merge order — Decision 6 and the Port plan rewritten accordingly (see below).
**Priority**: High
**Created**: 2026-07-04
**Depends on**: PRD #12 (done, merged `9d35080`) — its board/run-lifecycle UI is now the base this PRD re-skins; see Design Decision 6.

## Problem

The current web UI grew feature-by-feature and it shows. Three independent UX prototypes (2026-07-04) converged on the same critique of `web/`:

1. **Flat topbar of unequal things** (`web/src/components/Layout.tsx`: flat `<nav>` at :40, `Boards…` `<select>` at :59-67, standalone Forge link at :75): settings, admin, and core operations interleaved; boards hidden behind a form control used as navigation; no active-route indication.
2. **Dead-end dashboard**: an account-metadata card plus "control lands here in later milestones"; the product's real funnel (token → forge → repo → worker → run) is encoded in `startRunGate` but invisible until a disabled button rejects you.
3. **No status color system**: the 3-tone Badge renders "running" and "completed" the same gray; worker "online" is neutral while offline is red.
4. **The run view undersells the hero moment**: scattered gray text chips for vitals, the plan-approval gate styled like the follow-up composer and scrollable out of view, the MR link hidden as chrome.
5. **Bare loading/empty/error states**: "Loading…", "Empty" strings; empty states are dead ends.

Three mock redesigns were built in parallel worktrees and evaluated live. The **multica-inspired "ember" design** was selected: dark-only, molten-orange accent on near-black steel, sidebar shell, token-driven styling. Full critique with file citations: `ux-review.md` at the prototype worktree root.

## Source material

- **Prototype**: worktree `.claude/worktrees/agent-a099da95ae7695658`, branch `worktree-agent-a099da95ae7695658`, commit `baaecdf` (parent `092794c`) — 32 files, +3492/−786 vs its parent.
- **The port is no longer a clean rebase** (2026-07-05): `main` has since gained PRD #12's full implementation (merge `9d35080`), which rewrote `Board.tsx` (+290 lines: `latest_run` badges via the new `runBadge.ts`, 10s visibility-gated polling, attention strip, auto-move toasts, in-app issue links), added `IssueView.tsx`, `runBadge.ts`, `forgeUrls.ts`, and touched `App.tsx`, `api.ts`, `RunView.tsx`, `RunsList.tsx` — five of which the prototype also rewrites. The port is therefore a **design-over-logic merge**: #12's behavior is authoritative, the prototype's design system is applied on top.
- The prototype's **non-mock build is wired to the real API client**: `web/src/lib/api.ts:13` gates on `import.meta.env.VITE_UZI_MOCK === "1"`; with the flag unset, `api = realApi` and `createRunSocket` returns a real `WebSocket`. Mock *behavior* is unreachable in real builds; mock *bytes* do ship in the bundle (not tree-shaken — verified by grepping `dist` for mock-only strings). 67/67 vitest green in both modes.
- **The prototype worktree and its container (`uzi-ux-multica`, :8081) stay alive as reference until this PRD completes**, then are cleaned up (final milestone), together with the other two prototypes' worktrees/containers (`uzi-ux-mission` :8084, `uzi-ux-minimal` :8083). Their identities can return later as `data-theme` overrides (out of scope).

## Design Decisions

1. **Structure from the prototype, verbatim where it survived review.** Sidebar `AppShell` (nav groups **Work / Factory / Configure / Admin / Help**, boards as first-class children under Work, footer user chip, mobile sheet), token file (`:root/[data-theme="ember"]` CSS variables in `web/src/index.css`, Tailwind reads variables only), primitive kit (`ui.tsx`: Button matrix, StatusPill, Skeleton, EmptyState, PageHeader, Spinner), dashboard overview (stat tiles + onboarding checklist derived from the same preconditions as `startRunGate`), run view (stage pill, prominent plan gate, terminal hero banner with MR link), runs list (past runs collapsed), board (per-column accent identity, content-first cards, gate reason as tooltip).
2. **Board nav entries get a forge icon** (user requirement, 2026-07-04): each board child under **Work → Boards** — today the only icon-less nav items (`AppShell.tsx:141-149`) — renders a GitLab logo (tanuki) icon; non-GitLab forge types (future Forgejo driver) fall back to a generic git icon. Both are inline SVGs in `web/src/components/icons.tsx` (no new dependency; `BranchIcon` exists, no GitLab/git mark yet). **Wiring**: `forge_type` is *not* on the `Repo` DTO the nav renders from (`api.ts:68-76` has only `connection_id`); `AppShell` additionally fetches `api.listConnections()` and maps `connection_id → forge_type` — a web-only join, keeping the "no backend changes" scope intact. Today the fallback never fires (`forge_types` is hardcoded to `["gitlab"]` server-side); it exists so a new driver picks it up automatically.
3. **Forge lives only under Settings** (user requirement, 2026-07-04): the standalone `Forge` nav item (`AppShell.tsx:160`, under Configure) is removed. Forge configuration is reachable exclusively via **Settings** — the prototype's `SettingsShell` already tabs Account & token / Forge / Workers (`SettingsShell.tsx:11-15`). The `/settings/forge` and `/settings/workers` routes keep working unchanged (`App.tsx:49,57`); only the nav entry goes. Discoverability holds: the dashboard onboarding checklist links to `/settings/forge` directly (`Dashboard.tsx:163`). The `Workers` nav shortcut under **Factory** stays (operational surface, not just config).
4. **Mock mode ships, inert by default.** `web/src/mocks/`, `Dockerfile.mock`, `nginx.mock.conf` land on main as an opt-in demo build (`VITE_UZI_MOCK=1`): the cheapest way to demo uzi with zero backend and to prototype future themes over the real component tree. Real builds contain **no reachable mock behavior** (the mock module is a runtime-dead branch; its bytes ship and that bundle-size cost is accepted). Neither mock file is referenced by the real `docker-compose.yml`. Verified per-release by the M4 dist-grep check.
5. **Dark-only for now.** The ember theme does not add a light mode. Tokens make a later light theme (or porting the minimal/mission identities) a `data-theme` block, not a refactor.
6. **Merge order vs PRD #12 — inverted by events (2026-07-05).** The PRD originally scheduled #14 first; in fact **#12 landed on `main` first** (merge `9d35080`, done and archived). The resolution flips accordingly: **#12's behavior is authoritative and must survive the reskin intact** — `latest_run` badges (`runBadge.ts` tone mapping), 10s visibility-gated board polling, the attention strip, auto-move toasts with a11y, manual-drag semantics, the in-app `IssueView`, and the card title as in-app `<Link draggable={false}>` with the GitLab glyph. The prototype's `Board.tsx`/`RunView.tsx`/`RunsList.tsx` are **not** applied wholesale; their design vocabulary (tokens, StatusPill, EmptyState, column accent identity, collapsed past runs) is re-applied over #12's logic. `runBadge.ts` tone names map onto the token-backed StatusPill palette. Conflict-resolution rule for the port: when prototype design and #12 behavior disagree, behavior wins, then restyle.

## Solution Overview

Port the prototype commit onto a PRD branch off current `main`, apply the two nav adjustments (Decisions 2–3), **complete the reskin on the pages the prototype skipped**, validate against the **real** stack (the prototype was only ever exercised in mock mode), refresh docs that describe the old navigation, and merge via MR after an agent review gate.

## Technical Design

### Port

- Cherry-pick `baaecdf` onto the PRD branch; expected conflicts in `Board.tsx`, `RunView.tsx`, `RunsList.tsx`, `App.tsx`, `api.ts` (PRD #12 overlap). Resolve per Decision 6: #12 behavior wins, prototype design is re-applied over it. Files without #12 overlap (AppShell, tokens, ui.tsx, icons, Dashboard, Settings*, mocks, Dockerfile.mock) apply cleanly.
- The full vitest suite on `main` (grown past the prototype's 67 by #12's `runBadge`/board/forgeUrls tests) must stay green — those tests pin exactly the #12 behavior the merge must preserve. The mock layer (`web/src/mocks/`) must be extended to cover #12's new surfaces (`latest_run` on board payloads, `IssueView` data) so the demo build still exercises every page.
- Keep `ux-review.md` out of `main` (prototype artifact) — its durable content is this PRD's Problem section; drop the file during the port.

### Nav adjustments

- `icons.tsx`: add `GitLabIcon` (tanuki, single-path inline SVG, `currentColor`) and `GitIcon` (dedicated git mark; `BranchIcon` stays for its current uses). Board children in `AppShell` render per Decision 2's connection-join wiring.
- `AppShell.tsx`: delete the `Forge` NavItem from Configure.
- Active-state and `exactOnly` semantics on `/settings` must still highlight correctly when landing on `/settings/forge` via the tabs.

### Complete the reskin (prototype gap)

The prototype re-skinned roughly half the surface. Still on the legacy slate/indigo palette: `web/src/pages/{Register,Docs,DocPage,AgentDetail,AgentNew}.tsx`, `web/src/components/{AgentTemplateEditor,Markdown,RouteGuards}.tsx` (~48 raw palette literals), **`web/src/pages/IssueView.tsx` (new in PRD #12, 16 legacy palette refs)**, and the Markdown prose `@apply` block inside `index.css` itself. `Register` is a logged-out first-impression page sitting next to the already-ember `Landing`/`Login` — the clash is user-visible, so this is in scope, not deferred. All of these convert to the token vocabulary; the prose `@apply` block converts to token-backed values so rendered Markdown (docs, agent prompts, plans) matches the theme.

### Real-stack validation (the risk center)

The mock hid every real-API seam: latency, error shapes, the WS reconnect/seq-replay path, CSRF/session refresh, and the `?after=<seq>` REST replay. The **manual golden path below is the primary regression gate for this PRD** — the 67 existing vitest cases cover the run-message renderer and docs bundling, not the redesigned shell/board/dashboard/settings surfaces. Against the live compose stack of this checkout (real `.env`, real forge, real worker):

1. register/login → connect forge → enable repo → board DnD (forge-first move + snap-back on failure) → start run → live stream → plan gate approve → implement⇄review → MR link;
2. reconnect-mid-run: kill the tab, reopen, verify gapless replay;
3. API-down: stop the `api` container, verify pages degrade to error states, not white-screens;
4. session expiry mid-session: expire/delete the auth cookie while inside the shell, verify a graceful path back to `/login` rather than a wall of per-page 401 errors (there is no global 401 interceptor today — `AuthContext` only handles the initial `me()`; if this fails ugly, add the interceptor as part of this milestone);
5. mock-absence proof: build the real (flag-unset) bundle and grep `dist` for a known mock-only string (e.g. `andrei@uzi.local`) — must not execute; document that bytes may still appear (Decision 4) but verify no mock code path is reachable (login against the real API, watch the network tab).

New page-level smoke tests accompany the port where cheap (AppShell nav renders groups/boards, SettingsShell tab switching, Dashboard checklist gating) — but the golden path, not the suite, is the acceptance bar.

### Docs refresh

Stale navigation language lives in `README.md:26` ("**Forge** (top nav)"), `docs/configuration.md:37` ("The Forge page (top nav, not Settings)…"), and `docs/gitlab-bot-setup.md:30` ("open **Forge** from the top nav"). `docs/getting-started.md` and `docs/board.md` already describe the sidebar and need no change (verified). `web/scripts/check-docs.mjs` gates `docs/*.md` frontmatter/links only — the README fix is guarded by MR review, not tooling.

## Out of Scope

- Light theme; porting the mission-control or minimal identities as additional `data-theme`s (future PRD — tokens make it cheap).
- Touch/keyboard drag-and-drop for the board (native HTML5 DnD kept; noted as follow-up in the prototype review).
- MR-state tracking on cards (deferred by PRD #12 too). PRD #12's toast pattern stays as-is (restyled, not rearchitected).
- Any API/backend change: this PRD is `web/` + docs only (Decision 2's icon wiring is a web-side join for exactly this reason).

## Milestones

- [x] **M1 — Port lands on a PRD branch** (3aac410, dc2980a, 47a8fa2): design-over-logic merge, #12 behavior preserved (reviewer APPROVE, all 11 pre-flags cleared; auditor clean), mocks extended, 107/107 → gates green.
- [x] **M2 — Nav adjustments** (2f33d9c, c4aafc8): tanuki/git icons via `listConnections` join, Forge nav removed, active states pinned by new smoke tests (reviewer APPROVE, auditor clean).
- [x] **M3 — Reskin completed** (8e9847f, + f1eb50d NITs; polish f9237b6/08e31d1/eabfbec/ec96b75 user-approved): grep gate zero hits, no allowlist; web-ux browser validation of all 8 flows, no blockers.
- [x] **M4 — Real-stack validation** (fix 62f102d): golden path 1–5 PASS headless (isolated stacks), e2e harness PASS, real-GitLab leg PASS on `vtmocanu/uzi-e2e-scratch` (real project untouched), 401 interceptor added + validated + reviewed.
- [x] **M5 — Docs refresh** (3c80657): README/configuration/gitlab-bot-setup match the shipped nav (incl. Repos→Boards rename); check-docs green.
- [x] **M6 — Review gate + merge**: final coverage sweep (reviewer: MERGEABLE, all code commits covered) + fact-check (all CONFIRMED); MR !14 merged to `main` (`2efd83b`); issue #14 closed.
- [x] **M7 — Cleanup**: prototype/preview containers and images removed; all four worktrees removed (prototype branches kept locally as archives for the future themes PRD); `specs/ai.md` #76-84 added; `specs/human.md` Feature #14 added (user pre-approved).

## Success Criteria

1. The redesigned UI runs against the real backend with zero functional regression: golden path items 1–5 (the primary gate) plus the full vitest suite pass.
2. Boards are first-class sidebar entries with a forge icon; Forge appears nowhere in the nav except inside Settings.
3. Token discipline, scoped and enforceable: `grep -rE 'slate-|orange-|indigo-'` over `web/src/**` (excluding `web/src/index.css`, whose variable definitions and token-backed `@apply` block are the one sanctioned home) returns zero hits after M3. No allowlist.
4. Real builds execute no mock behavior (mock bytes may ship — Decision 4); `VITE_UZI_MOCK=1` builds still work as a zero-backend demo.
5. Docs describe the shipped navigation; `check-docs` passes; README fixed via MR review.
