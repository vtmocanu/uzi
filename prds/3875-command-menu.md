# PRD #3875: Command menu for navigation and your decisions

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3878 (the Overview header that hosts the Search button, and the decisions selector) and #3880 (which completes that selector with held-work decisions). **Confirm before dispatch**: whether the shortcut works on every page (recommended, brief D19) or only on the Overview.

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

- **`CommandMenu`**: a modal dialog (`role="dialog"`, labelled), a text input with a listbox of options, focus trapped while open. Mounted once by the app shell. Reuse the app's existing dialog primitive if `web/src/components/` has one; otherwise a native `<dialog>`.
- **Shortcut**: one `keydown` listener in the shell. Before adding it, check existing key handlers (the chat composer, dialogs, the TUI-like run views) for conflicts; Cmd K inside a text field still opens the menu, and the menu never steals keys while closed.
- **Navigation list** (the prefactor, first in M1): the sidebar's destinations are inline `NavItem` markup in `AppShell` today. Extract them into one shared definition (label, route, icon, admin-only flag) that the sidebar renders and the menu lists, keeping every `AppShell` test green.
- **Data on open**: when the menu opens it fetches the viewer's runs summary, the custody holds aggregate and the repos once, then derives the Needs you items with `overviewDecisions`. No subscription and no new poll, so nothing duplicates the shell's existing passive runs poll (`web/src/lib/useFavicon.ts:96`) or the Overview's hook; a fetch failure shows the Go to and Actions groups and a one-line "Decisions unavailable".
- **Held work**: the menu renders whatever `overviewDecisions` returns; #3880 lands first, so held-work decisions are already in it.

## Testing decisions

- Component tests: open and close by shortcut and by button, filtering, Enter, arrows, Escape, focus return, admin-only items hidden for a non-admin.
- A test that the shortcut does nothing while another modal dialog is open.
- Screenshots of the open menu against the mock (desktop and 400 px).

## Milestones

### M1: the menu with navigation and actions

**Blocked by**: none inside this PRD (the PRD is blocked by #3878).

`CommandMenu`, the shortcut, the Search button in the preview header, the Go to and Actions groups.

Acceptance: keyboard and focus tests; admin gating; mock parity.

### M2: your decisions in the menu

**Blocked by**: M1.

The Needs you group from `overviewDecisions`.

Acceptance: the group lists exactly the viewer's own decisions shown on the preview, and hides when there are none.

## Decision Log

- **Button on the Overview only, shortcut everywhere** (brief D19): the Overview header is the only place with room for the button; a shortcut that worked on one page would surprise. The maintainer leaned towards Overview-only; confirm before dispatch.
- **Navigate, never execute**: approvals and answers keep their own screens and checks.
- **Reuse the sidebar's route list and the decisions selector**, so the menu cannot drift from what the app shows.
