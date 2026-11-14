# PRD #3877: Promote the new Overview and retire the old one

**Status**: Verdict recorded 2026-11-14 on #3873: **promote, with a soak**. M1 is ready; M2 **must** merge before the next stable release. Child of epic #3873. #3875 and #3876 are not blockers.

**Design brief**: `prds/3873-overview-redesign.md`. **Design mock**: `prds/mockups/3873-overview-redesign-mock.html`; the promoted page must still match it (brief mock contract).

## Problem

Two Overviews must not live on side by side indefinitely: every dashboard change would be built twice or the two would drift (the biggest risk the brief names). A short, explicitly bounded soak is the exception: the old page stays reachable but frozen until the new one is the only one, and that window closes before the next stable release.

## Outcome

The maintainer's verdict (2026-11-14, recorded on #3873) is **promote, in two steps**: make the new Overview the default now, keep the old one one click away while release candidates soak it, and retire the old one before the next stable release.

**Frozen old page**: from M1 until M2 the old page gets no new features and no copy changes, except M1's navigation-link change below and fixes needed for correctness.

**M1 (default)**:

- `/dashboard` renders the new Overview (`OverviewNext`). The sidebar's Overview link and every post-sign-in redirect therefore land on it.
- `/overview/next` redirects to `/dashboard`, **preserving the query string and hash** (so `?mock=overview-next` bookmarks keep their scenario).
- The old `Dashboard` page moves to `/overview/classic`, unchanged except its header link: "Try the new Overview" becomes "Go to the new Overview", linking to `/dashboard`.
- The new page's "Back to the current Overview" link becomes "Back to the old Overview", linking to `/overview/classic`.
- Header and banner order needs no shell change: it follows the route's element, because `OverviewNext` portals its header through `InShellHeader`. Do not add a pathname check to `AppShell`. Update comments that name `/overview/next` (`AppShell.tsx`, `lib/overview.ts`, the `overviewNext` mock data) to the new routes.
- Every in-app link to `/overview/next` points at `/dashboard`.
- Docs that describe the old Overview are updated to the new one: check `docs/admin-health.md`, `docs/run-recovery.md`, `docs/run-cost.md`, `docs/run-activity.md`, `docs/hosted-workers.md`, `docs/worker-setup.md` and `docs/dev-conventions.md`, then run `task docs:sync`.
- The E2E harness: `git grep -F` the old page's copy and `/overview/next` across `e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`; update stale expectations; run `task test:e2e-lib`, and `task test:forge-fake` when the sweep finds an affected fake route.
- A changelog fragment in `changelog.d/`.

**M2 (retire, must merge before the next stable)**: blocked by M1, #4334 and #4335, which replace the old page's Repos and Agent templates counts.

- `/overview/classic` redirects to `/dashboard`, preserving the query string and hash.
- The old `Dashboard` page, its tests and every component only it used are deleted; the "Back to the old Overview" link goes.
- Any doc still naming the old page is updated (`task docs:sync`).
- A changelog fragment.

**Optional children** (#3875, #3876), whichever state they are in:

- Their shared pieces stay (`overviewDecisions`, the shared navigation list, `ByUserTable`). The Search button moves with the page to `/dashboard`. Edit any still-open child PRD so it targets `/dashboard` instead of `/overview/next`.

Acceptance examples:

- M1: opening `/overview/next?mock=overview-next#x` lands on `/dashboard?mock=overview-next#x`, which shows Needs your decision and the issue-work line; "Back to the old Overview" opens `/overview/classic`, which shows the old tiles; its "Go to the new Overview" returns to `/dashboard`.
- M2: opening `/overview/classic` or `/overview/next` (with query and hash) lands on `/dashboard` with them preserved; `task deadcode:web` (knip) passes with no unused export left from the old page; no test asserts on copy that only the old page rendered.

## Out of scope

New Overview features; the per-user Tokens, Out and Cost share columns (dropped by maintainer call D13; if they are wanted elsewhere, file it separately).

## Modules and seams

- The route table in `web/src/App.tsx` and the shell's banner exception (the banner order follows the route element through `InShellHeader`; no pathname check).
- Deletion is proven by knip (`task deadcode:web`), not by reading.

## Testing decisions

- Route tests: M1 for `/dashboard`, `/overview/classic` and the `/overview/next` redirect with query and hash preserved; M2 for the `/overview/classic` redirect the same way.
- Focused browser coverage of the header and banner order on `/dashboard` and `/overview/classic` (extend `web/src/components/AppShell.overviewBanners.test.tsx` and `web/src/pages/OverviewNext.browser.test.tsx`).
- In-run/local checks per milestone: `task check:web`, the touched Vitest and browser files, `cd web && npm run build`, `task docs:sync` + `task check-docs:web`, `task check:changelog-entry`, and the E2E commands above. Full `test:web`, `test:web-browser` and `gate:repo` stay pending exact-head PR CI.
- M1 changes link copy and M2 deletes the old page: after each, re-point every negative assertion on retired copy (`.claude/rules/web.md`, "Copy changes disarm negative assertions"), keeping deliberate did-the-old-copy-come-back pairs.
- One screenshot of `/dashboard` against the mock.

## Milestones

### M1: the new Overview is the default

**Blocked by**: none (verdict recorded).

The route swap, the redirect and the move, the link copy, comment updates, docs, the E2E harness sweep, the changelog fragment, tests.

Acceptance: M1 above; mock parity on `/dashboard`.

### M2: retire the old Overview

**Blocked by**: M1, #4334, #4335. **Must merge before the next stable release.**

The `/overview/classic` redirect, deletions (proven by knip), docs, the changelog fragment, tests.

Acceptance: M2 above.

## Decision Log

- **Delete, do not keep two layouts**, unless the verdict says otherwise (brief option B over C).
- **Gate on a recorded verdict**, not on the code being ready: the trial exists to decide.
- **Promote with a soak** (maintainer, 2026-11-14): default first, so release candidates on the hosted instance are the trial with the old page one click away; retire before the next stable so the two pages never ship side by side in a stable. The old page is frozen during the soak, which bounds the double-build risk this PRD exists to avoid.
