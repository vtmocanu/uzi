# PRD #3877: Promote the new Overview and retire the old one

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3878, #3879, #3880, #3881 and **the maintainer's trial verdict** (brief open decision 1). #3875 and #3876 are not blockers. Not dispatchable until the verdict is recorded on #3873.

**Design brief**: `prds/3873-overview-redesign.md`. **Design mock**: `prds/mockups/3873-overview-redesign-mock.html`; the promoted page must still match it (brief mock contract).

## Problem

Two Overviews must not live on side by side: every dashboard change would be built twice or the two would drift (the biggest risk the brief names).

## Outcome

When the verdict is **promote**:

- `/dashboard` renders the new Overview; `/overview/next` redirects to `/dashboard`.
- The old `Dashboard` page, its tests and every component only it used are deleted; the "Try the new Overview" and "Back to the current Overview" links go.
- Docs and in-app copy that describe the old Overview are updated: check `docs/admin-health.md`, `docs/run-recovery.md`, `docs/hosted-workers.md`, `docs/worker-setup.md`, `docs/run-cost.md` and `docs/run-activity.md`, which mention it, then run `task docs:sync`.

When the verdict is **drop**: the preview route, its components and its tests are deleted instead, and the old Overview stays.

When the verdict is **keep both** (brief option C): this PRD is replaced by a per-user layout preference PRD; do not implement it as written.

**Optional children** (#3875, #3876), whichever state they are in:

- **Promote**: their shared pieces stay (`overviewDecisions`, the shared navigation list, `ByUserTable`). The Search button moves with the page to `/dashboard`. Edit any still-open child PRD so it targets `/dashboard` instead of `/overview/next`.
- **Drop**: keep every shared piece another consumer still uses (the command menu uses `overviewDecisions` and the navigation list); move #3875's Search button out or drop it, and move #3876's column to the current Overview's per-user table or cut it; then edit or close any still-open child PRD so nothing dispatchable targets removed components.

Acceptance examples:

- After promotion, opening `/overview/next` lands on `/dashboard`, and `/dashboard` shows Needs your decision and the issue-work line.
- `task deadcode:web` (knip) passes with no unused export left from the old page.
- No test asserts on copy that only the old page rendered.

## Out of scope

New Overview features; the per-user Tokens, Out and Cost share columns (dropped by maintainer call D13; if they are wanted elsewhere, file it separately).

## Modules and seams

- The route table in `web/src/App.tsx` and the shell's banner exception (#3878 skips the shell stack on `/overview/next`; after promotion it applies to `/dashboard`).
- Deletion is proven by knip (`task deadcode:web`), not by reading.

## Testing decisions

- A route test for the redirect.
- After deleting the old page, re-point every negative assertion on retired copy (`.claude/rules/web.md`, "Copy changes disarm negative assertions"), keeping deliberate did-the-old-copy-come-back pairs.
- One screenshot of `/dashboard` against the mock.

## Milestones

### M1: promote and retire

**Blocked by**: #3878, #3879, #3880, #3881 and the recorded verdict.

The route swap, the redirect, deletions, docs, the moved banner exception, tests.

Acceptance: as above; mock parity on `/dashboard`.

## Decision Log

- **Delete, do not keep two layouts**, unless the verdict says otherwise (brief option B over C).
- **Gate on a recorded verdict**, not on the code being ready: the trial exists to decide.
