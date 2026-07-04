# PRD #14: Adopt the Multica-inspired UI Redesign (ember)

**GitLab Issue**: [vtmocanu/uzi#14](https://gitlab.example.com/vtmocanu/uzi/-/issues/14)
**Status**: Ready (2026-07-04: reviewed by 2 agents — adversarial fact-check + design review; all findings incorporated: completed-reskin milestone added, PRD #12 collision resolved via merge-order decision, git-history/nav-group/docs-list corrections, forge_type wiring specified)
**Priority**: High
**Created**: 2026-07-04
**Depends on**: no open PRD blocks this one, but **PRD #12 (Ready, not started) rewrites the same `web/src/pages/Board.tsx` this PRD re-skins** — see Design Decision 6 for the merge-order resolution.

## Problem

The current web UI grew feature-by-feature and it shows. Three independent UX prototypes (2026-07-04) converged on the same critique of `web/`:

1. **Flat topbar of unequal things** (`web/src/components/Layout.tsx`: flat `<nav>` at :40, `Boards…` `<select>` at :59-67, standalone Forge link at :75): settings, admin, and core operations interleaved; boards hidden behind a form control used as navigation; no active-route indication.
2. **Dead-end dashboard**: an account-metadata card plus "control lands here in later milestones"; the product's real funnel (token → forge → repo → worker → run) is encoded in `startRunGate` but invisible until a disabled button rejects you.
3. **No status color system**: the 3-tone Badge renders "running" and "completed" the same gray; worker "online" is neutral while offline is red.
4. **The run view undersells the hero moment**: scattered gray text chips for vitals, the plan-approval gate styled like the follow-up composer and scrollable out of view, the MR link hidden as chrome.
5. **Bare loading/empty/error states**: "Loading…", "Empty" strings; empty states are dead ends.

Three mock redesigns were built in parallel worktrees and evaluated live. The **multica-inspired "ember" design** was selected: dark-only, molten-orange accent on near-black steel, sidebar shell, token-driven styling. Full critique with file citations: `ux-review.md` at the prototype worktree root.

## Source material

- **Prototype**: worktree `.claude/worktrees/agent-a099da95ae7695658`, branch `worktree-agent-a099da95ae7695658`, commit `baaecdf` (parent `092794c`) — 32 files, +3492/−786 vs its parent. `main` has since gained only `CLAUDE.md` and two e2e-harness fixes (`05b3727`, `1c637c6`, merge `99bb913`) — no file overlap with the prototype's diff (`ux-review.md` + `web/**` only), so the rebase is conflict-free (verified, not assumed).
- The prototype's **non-mock build is wired to the real API client**: `web/src/lib/api.ts:13` gates on `import.meta.env.VITE_UZI_MOCK === "1"`; with the flag unset, `api = realApi` and `createRunSocket` returns a real `WebSocket`. Mock *behavior* is unreachable in real builds; mock *bytes* do ship in the bundle (not tree-shaken — verified by grepping `dist` for mock-only strings). 67/67 vitest green in both modes.
- **The prototype worktree and its container (`uzi-ux-multica`, :8081) stay alive as reference until this PRD completes**, then are cleaned up (final milestone), together with the other two prototypes' worktrees/containers (`uzi-ux-mission` :8084, `uzi-ux-minimal` :8083). Their identities can return later as `data-theme` overrides (out of scope).

## Design Decisions

1. **Structure from the prototype, verbatim where it survived review.** Sidebar `AppShell` (nav groups **Work / Factory / Configure / Admin / Help**, boards as first-class children under Work, footer user chip, mobile sheet), token file (`:root/[data-theme="ember"]` CSS variables in `web/src/index.css`, Tailwind reads variables only), primitive kit (`ui.tsx`: Button matrix, StatusPill, Skeleton, EmptyState, PageHeader, Spinner), dashboard overview (stat tiles + onboarding checklist derived from the same preconditions as `startRunGate`), run view (stage pill, prominent plan gate, terminal hero banner with MR link), runs list (past runs collapsed), board (per-column accent identity, content-first cards, gate reason as tooltip).
2. **Board nav entries get a forge icon** (user requirement, 2026-07-04): each board child under **Work → Boards** — today the only icon-less nav items (`AppShell.tsx:141-149`) — renders a GitLab logo (tanuki) icon; non-GitLab forge types (future Forgejo driver) fall back to a generic git icon. Both are inline SVGs in `web/src/components/icons.tsx` (no new dependency; `BranchIcon` exists, no GitLab/git mark yet). **Wiring**: `forge_type` is *not* on the `Repo` DTO the nav renders from (`api.ts:68-76` has only `connection_id`); `AppShell` additionally fetches `api.listConnections()` and maps `connection_id → forge_type` — a web-only join, keeping the "no backend changes" scope intact. Today the fallback never fires (`forge_types` is hardcoded to `["gitlab"]` server-side); it exists so a new driver picks it up automatically.
3. **Forge lives only under Settings** (user requirement, 2026-07-04): the standalone `Forge` nav item (`AppShell.tsx:160`, under Configure) is removed. Forge configuration is reachable exclusively via **Settings** — the prototype's `SettingsShell` already tabs Account & token / Forge / Workers (`SettingsShell.tsx:11-15`). The `/settings/forge` and `/settings/workers` routes keep working unchanged (`App.tsx:49,57`); only the nav entry goes. Discoverability holds: the dashboard onboarding checklist links to `/settings/forge` directly (`Dashboard.tsx:163`). The `Workers` nav shortcut under **Factory** stays (operational surface, not just config).
4. **Mock mode ships, inert by default.** `web/src/mocks/`, `Dockerfile.mock`, `nginx.mock.conf` land on main as an opt-in demo build (`VITE_UZI_MOCK=1`): the cheapest way to demo uzi with zero backend and to prototype future themes over the real component tree. Real builds contain **no reachable mock behavior** (the mock module is a runtime-dead branch; its bytes ship and that bundle-size cost is accepted). Neither mock file is referenced by the real `docker-compose.yml`. Verified per-release by the M4 dist-grep check.
5. **Dark-only for now.** The ember theme does not add a light mode. Tokens make a later light theme (or porting the minimal/mission identities) a `data-theme` block, not a refactor.
6. **Merge order vs PRD #12: this PRD lands first.** #12 ("Ready", not started) plans `latest_run` cards, 10s polling, an attention strip, and in-app issue links — all in `Board.tsx`, which this PRD re-skins in its *pre-#12* shape (still the `api.listRuns()` fan-in). Re-skinning first means #12 builds its features directly on the new design system (StatusPill, tokens, EmptyState) instead of the components it would otherwise have to restyle later; the reverse order would have #14 wholesale-rewriting freshly-landed #12 logic. #12's implementer rebases its Board plan onto the redesigned `Board.tsx` and should reuse the new primitives.

## Solution Overview

Port the prototype commit onto a PRD branch off current `main`, apply the two nav adjustments (Decisions 2–3), **complete the reskin on the pages the prototype skipped**, validate against the **real** stack (the prototype was only ever exercised in mock mode), refresh docs that describe the old navigation, and merge via MR after an agent review gate.

## Technical Design

### Port

- Rebase/cherry-pick `baaecdf` onto `main` (verified conflict-free: the intervening main commits touch only `CLAUDE.md`, `e2e/README.md`, `e2e/run-e2e.sh`).
- Keep `ux-review.md` out of `main` (prototype artifact) — its durable content is this PRD's Problem section; drop the file during the port.

### Nav adjustments

- `icons.tsx`: add `GitLabIcon` (tanuki, single-path inline SVG, `currentColor`) and `GitIcon` (dedicated git mark; `BranchIcon` stays for its current uses). Board children in `AppShell` render per Decision 2's connection-join wiring.
- `AppShell.tsx`: delete the `Forge` NavItem from Configure.
- Active-state and `exactOnly` semantics on `/settings` must still highlight correctly when landing on `/settings/forge` via the tabs.

### Complete the reskin (prototype gap)

The prototype re-skinned roughly half the surface. Still on the legacy slate/indigo palette: `web/src/pages/{Register,Docs,DocPage,AgentDetail,AgentNew}.tsx`, `web/src/components/{AgentTemplateEditor,Markdown,RouteGuards}.tsx` (~48 raw palette literals), and the Markdown prose `@apply` block inside `index.css` itself. `Register` is a logged-out first-impression page sitting next to the already-ember `Landing`/`Login` — the clash is user-visible, so this is in scope, not deferred. All of these convert to the token vocabulary; the prose `@apply` block converts to token-backed values so rendered Markdown (docs, agent prompts, plans) matches the theme.

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
- A toast system; MR-state tracking on cards and all other board–run lifecycle features (PRD #12, which lands after this per Decision 6).
- Any API/backend change: this PRD is `web/` + docs only (Decision 2's icon wiring is a web-side join for exactly this reason).

## Milestones

- [ ] **M1 — Port lands on a PRD branch**: prototype commit rebased onto current `main`, `ux-review.md` dropped, typecheck + 67/67 vitest + real (non-mock) `npm run build` green.
- [ ] **M2 — Nav adjustments**: GitLab/git icons on board nav entries (via the `listConnections` join), standalone Forge nav entry removed, `/settings/forge` reachable via Settings tabs with correct active states; page-level smoke tests added for shell nav + settings tabs.
- [ ] **M3 — Reskin completed**: `Register`, `Docs`, `DocPage`, `AgentDetail`, `AgentNew`, `AgentTemplateEditor`, `Markdown`, `RouteGuards`, and the `index.css` prose block converted to tokens; the scoped grep gate (Success Criterion 3) passes with an empty allowlist.
- [ ] **M4 — Real-stack validation**: golden path items 1–5 green against the live compose stack; any real-API seams the mock hid (including the 401/session-expiry path) fixed.
- [ ] **M5 — Docs refresh**: README + `docs/configuration.md` + `docs/gitlab-bot-setup.md` match the new navigation; `check-docs` green.
- [ ] **M6 — Review gate + merge**: agent review round (reviewer + fact-check, same bar as PRD #12) on the final diff; findings resolved; MR merged to `main`; issue #14 closed.
- [ ] **M7 — Cleanup**: all three prototype containers stopped/removed, prototype worktrees removed; `specs/ai.md` updated with the token/theming decisions; `specs/human.md` addition (multica design selected, board icons, forge-under-settings-only) drafted and **explicitly confirmed with the user before writing** (per CLAUDE.md rule).

## Success Criteria

1. The redesigned UI runs against the real backend with zero functional regression: golden path items 1–5 (the primary gate) plus the full vitest suite pass.
2. Boards are first-class sidebar entries with a forge icon; Forge appears nowhere in the nav except inside Settings.
3. Token discipline, scoped and enforceable: `grep -rE 'slate-|orange-|indigo-'` over `web/src/**` (excluding `web/src/index.css`, whose variable definitions and token-backed `@apply` block are the one sanctioned home) returns zero hits after M3. No allowlist.
4. Real builds execute no mock behavior (mock bytes may ship — Decision 4); `VITE_UZI_MOCK=1` builds still work as a zero-backend demo.
5. Docs describe the shipped navigation; `check-docs` passes; README fixed via MR review.
