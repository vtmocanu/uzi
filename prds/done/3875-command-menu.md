# PRD #3875: Command menu for navigation and your decisions

**Status**: Done (delivered on branch agent/issue-3875, 2026-11-10). Child of epic #3873. Its blockers have landed: #3878 (the Overview header that hosts the Search button, and the decisions selector) and #3880 (which completes that selector with held-work decisions). Shortcut scope confirmed by the maintainer (Decision Log).

**Design brief**: `prds/3873-overview-redesign.md` (constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`: press Cmd K or Ctrl K, or click Search in the page header. The brief's mock contract applies: implement the mock, small consistency tweaks are fine, larger deviations go under `Mock deviations` in the PR body with a reason, and the PR carries screenshots.

## Problem

Reaching a page, a run or a pending decision takes several clicks, and there is no keyboard path at all.

## Outcome

- **Cmd K** (macOS) or **Ctrl K** opens a command menu on any signed-in page; a **Search** button in the Overview header opens it too (`/overview/next`, or `/dashboard` once #3877 has promoted the page).
- **Groups**: Needs you (the viewer's own pending decisions, from #3878's `overviewDecisions`), Actions (Start a chat; open a repo's board), Go to (the sidebar's destinations, admin-only ones for admins only).
- Typing filters every group by label; Enter runs the first match; arrow keys move; Escape closes; focus returns to where it was.
- Choosing an item navigates within the app; nothing is executed in place (no approve or answer from the menu).

Acceptance examples:

- On `/runs`, Cmd K opens the menu; typing "work" leaves Workers; Enter navigates to `/workers` and the menu closes.
- With one plan awaiting the viewer's approval, the Needs you group lists "Review plan for #N"; choosing it opens that run.
- A non-admin's menu never lists Admin destinations.

## Out of scope

Global search over runs, issues or docs (a separate feature, not planned here); executing actions from the menu; custom shortcuts.

## Modules and seams

- **`CommandMenu`**: a modal dialog (`role="dialog"`, labelled), a text input with a listbox of options, focus trapped while open. Mounted once by the app shell. Build it on the app's dialog primitive `Modal` (`web/src/components/Modal.tsx`: focus move, Tab trap, focus restore), not a new one.
- **Shortcut**: one `keydown` listener in the shell. Before adding it, check existing key handlers for conflicts (none bind K today: Board, Runs and Docs bind `/` to focus their filter and Escape, `QuestionPanel` binds Cmd/Ctrl Enter; keep all of them working); Cmd K inside a text field still opens the menu, and the menu never steals keys while closed.
- **Navigation list** (the prefactor, first in M1): the sidebar's destinations are inline `NavItem` markup in `AppShell` today. Extract them into one shared definition (label, route, icon, admin-only flag) that the sidebar renders and the menu lists, keeping every `AppShell` test green.
- **Data on open**: when the menu opens it fetches the viewer's runs summary, the custody holds aggregate and the repos once, then derives the Needs you items with `overviewDecisions(ownRuns, holds)` (`web/src/lib/overview.ts`), as `web/src/pages/OverviewNext.tsx` does. No subscription and no new poll, so nothing duplicates the shell's existing passive runs poll (the `listRuns({ passive: true })` poll in `web/src/lib/useFavicon.ts`) or the Overview's hook; a fetch failure shows the Go to and Actions groups and a one-line "Decisions unavailable".
- **Held work**: the menu renders whatever `overviewDecisions` returns; #3880 lands first, so held-work decisions are already in it.
- **Untrusted text and links**: repo paths, run titles, decision labels and tooltips come from forge and run data. Repo paths render through `maskRepoPath` (`web/src/lib/demoMask.ts`, demo mode) and `stripUnsafeChars` (`web/src/lib/safeText.ts`); run and decision labels and tooltips through `stripUnsafeChars`. Board links are built only from repo IDs in the loaded repos response, run links only from run IDs in the loaded runs response, never from label text.
- **Sidebar extraction keeps behaviour**: the shared navigation definition preserves the sidebar's dynamic count badges and their accessible nouns, the admin gate, and the collapsed rail with its repo children; every existing `AppShell` test stays green.

## Testing decisions

- Component tests: open and close by shortcut and by button, filtering, Enter, arrows, Escape, focus return, admin-only items hidden for a non-admin.
- A test that the shortcut does nothing while another modal dialog is open.
- Labels carrying bidi and control characters render stripped, in normal and in demo mode (repo paths masked); links target only loaded repo and run IDs.
- Screenshots of the open menu against the mock (desktop and 400 px).

## Run boundary

Web only: no API, migration or `.github/workflows/**` edit is expected. In-run: `task check:web`, the touched and directly affected Vitest files, focused browser cases for the shortcut, modal focus and the 400 px layout, `cd web && npm run build`, `task check:changelog-entry`, `task scan:secrets` and `task sast:semgrep` (the menu newly renders forge- and run-supplied text), and `task docs:sync` plus `task check-docs:web` if a doc changes. Full `test:web`, `test:web-browser` and `gate:repo` stay exact-head PR CI obligations.

## Milestones

### M1: the menu with navigation and actions

**Blocked by**: none inside this PRD (the PRD is blocked by #3878).

`CommandMenu`, the shortcut, the Search button in the preview header, the Go to and Actions groups.

Acceptance: keyboard and focus tests; admin gating; mock parity.

### M2: your decisions in the menu

**Blocked by**: M1.

The Needs you group from `overviewDecisions`.

Acceptance: the group lists exactly the viewer's own decisions shown on the preview, and hides when there are none.

## Delivered

Shipped on branch agent/issue-3875 in two milestones. Web only: no API code, migration or workflow edit (the only file under `api/` is the regenerated docs mirror). Tests: `web/src/components/CommandMenu.test.tsx` (component), `web/src/components/CommandMenu.browser.test.tsx` (real browser), `web/src/components/AppShell.test.tsx` (sidebar characterization), `web/src/pages/OverviewNext.test.tsx` (Search button). Screenshots against the mock (desktop and 400 px, Ember and Dawn) were captured in-run by the browser validator; they are attached to the PR when it is landed.

### Outcome

- **Cmd K / Ctrl K and Search button**: one shell listener opens the menu on every signed-in page, including from a text field; the Overview preview header has a Search button. Tested in `CommandMenu.test.tsx` (shortcut, platform, text field, closed-state keys) and `OverviewNext.test.tsx` ("OverviewNext Search button"). Not delivered: `/dashboard` promotion, which belongs to #3877.
- **Groups**: Needs you (own decisions from `overviewDecisions`, hidden when empty), Actions (Start a chat, one "Open board: <repo>" per repo), Go to (sidebar destinations, Admin for admins only). Tested in `CommandMenu.test.tsx` (decisions, repos, Admin cases).
- **Filtering and keys**: ranked filter, arrows with wrap, Enter runs the highlighted item (first by default), Escape closes and focus returns. Tested in `CommandMenu.test.tsx` and `CommandMenu.browser.test.tsx` (focus return, Tab trap).
- **Navigate only**: choosing an item navigates; no approve or answer. Tested by the click and Enter cases in `CommandMenu.test.tsx`.

### Acceptance examples

- "work" on `/runs` leaves Workers first, Enter goes to `/workers` and closes: `CommandMenu.test.tsx` ("typing 'work' ranks Workers first ...").
- A plan awaiting the viewer lists "Review plan for #N" and opens that run: `CommandMenu.test.tsx` ("lists a plan, a question, a park and a PR ...").
- A non-admin never sees Admin: `CommandMenu.test.tsx` ("a non-admin never sees Admin").

### Testing decisions

- Component tests for open/close, filtering, Enter, arrows, Escape, focus return and admin gating: `CommandMenu.test.tsx`, plus focused browser cases in `CommandMenu.browser.test.tsx`.
- Shortcut does nothing while another modal dialog is open: `CommandMenu.test.tsx` ("does nothing while another modal dialog is open").
- Bidi and control characters stripped, repo paths masked in demo mode, links built from loaded IDs only: `CommandMenu.test.tsx` (repo path, demo mode and "builds the target from the loaded run id only" cases).
- Screenshots (desktop and 400 px): captured in-run side by side with the mock in Ember and Dawn, attached to the PR at landing. The 400 px layout is also checked in `CommandMenu.browser.test.tsx` (panel fits, no horizontal overflow).

### Milestones

- **M1** (menu, shortcut, Search button, Go to and Actions, shared navigation definition): done. The sidebar renders the shared definition; a characterization test written before the extraction (`AppShell.test.tsx`, "AppShell sidebar destination sequence (characterization)") pins the label, href and group sequence for admin and non-admin, expanded and collapsed, and the existing badge, admin-gate and collapsed-rail cases stay green.
- **M2** (Needs you): done. Data is fetched once per open and derived with `overviewDecisions`; a parity case in `CommandMenu.test.tsx` ("matches overviewDecisions on the same fixture") checks order, ids and targets, and the group is omitted when empty. The one exception to "exactly the decisions shown" is in the Decision Log (holds fetch failure).

## Decision Log

- **Button on the Overview only, shortcut everywhere** (brief D19): the Overview header is the only place with room for the button; a shortcut that worked on one page would surprise. Confirmed by the maintainer (2026-11-10): the Search button stays in the Overview header only, as in the mock; the shortcut works on every signed-in page. No sidebar entry.
- **Navigate, never execute**: approvals and answers keep their own screens and checks.
- **Reuse the sidebar's route list and the decisions selector**, so the menu cannot drift from what the app shows.
- **Ranked filtering**: a query ranks by label prefix, then word start, then any substring; ties go Go to, Actions, Needs you. "work" must reach Workers first even when "Review held work" or a repo containing "work" also matches, so the acceptance example holds with real data.
- **Platform modifier**: Meta on Apple platforms, Ctrl elsewhere. This leaves macOS Ctrl K (delete to the end of the line) alone. The shortcut matches the typed letter, and falls back to the physical key only for non-Latin layouts.
- **Failure notes are per input**: "Boards unavailable" shows when the repos fetch fails. "Decisions unavailable" is tied only to the runs and holds fetches, the only inputs to `overviewDecisions`. If only the holds fetch fails, the menu shows "Decisions unavailable" while the preview still lists run decisions; this is a deliberate exception to "exactly the decisions shown on the preview", chosen over showing a partial list.
- **Mobile drawer focus handoff**: focus moves to "Open navigation" before the drawer closes, so Escape restores focus to a connected control.
- **PR items open the run page**, not the forge PR, so navigation stays within the app. The label uses the forge's MR reference ("Review PR !N").
- **The park hint is "Parked"**, not "Resume".
- **Mock deviations**: the keyboard hint is platform-aware; per-repo "Open board: <repo>" items replace "Start a run on an issue"; the sidebar's Admin destination replaces "Health checks"; the group order follows the mock but every decision is listed (no per-kind cap).
- **Deferred**: a per-kind cap or summary rows for long decision lists; matching hints and run titles in search; a lighter shadow than the mock; the 400 px keyboard-hint label; rejecting a bare `..` run or repo id client-side (server-minted ids; it resolves in-app only).
