# PRD #1976: Site-list jobs: per-product site-list allowance and profile-bound jobs on the isolated lane

**Status**: Planned. Blocked by PRD #1909 (dispatch only after its PR merges). Split out of PRD #1906 (its M8), which closed with this milestone open.

## Problem

PRD #1906 shipped site lists, the fetcher and the isolated no-internet worker lane, and PRD #1908 shipped repo-less jobs, but the two are not connected: job create refuses `egress_profile` with 422 `not_supported`, so no job can do official-sources research. The refusal stays until a product token can be limited to the site lists its product is allowed (PRD #1906 Decision 13); lifting it without that check would let any product token use any list.

## Outcome

A job can name a site list, and runs on the isolated lane with only that list's hosts reachable through the fetcher, when its caller may use that list.

Acceptance examples:
1. An admin allows product P to use list `vendor-docs`. A P product token creates a job with `egress_profile: vendor-docs`; the job is claimed only by a lane worker, fetches an allowed page, and its source log shows the fetch.
2. The same token names list `other-docs`, which P is not allowed: job create returns 403 and no run row is created.
3. A user token (not a product token) names any existing list: accepted, as before for user-owned runs.
4. The admin removes P's allowance for `vendor-docs`: the next P job naming it gets 403; jobs already created are not changed.

## Out of scope

- Live k8s verification. It is tracked as PRD #1906's M9 checklist (maintainer-owned, optional, not a milestone here).
- New site-list features (editing hosts, wildcards): PRD #1906 owns them and they are done.
- Job files and product skill sets: PRD #1909.
- Any change to lane placement rules (PRD #1906 Decision 9) or to the Claude-only rule for profile-bound runs (Decision 10).

## Modules and seams

- **Job create (`workersvc` `CreateJobRun`, `/api/v1/jobs` `V1JobCreateRequest`)**: gains the `egress_profile` decision. Interface: a caller identity (user token or product token with its product) plus a list name in; either a created profile-bound job or a typed refusal (403 not allowed, 404 unknown list, 422 for the existing non-list refusals). Invariant: the 422 `not_supported` on `egress_profile` is removed in the same change that adds the allowance check; no build accepts `egress_profile` without it. Tested through the `/api/v1/jobs` endpoint and the service call, live-DB.
- **Allowance store (new table, product x profile)**: admin writes are cookie-only (never a product or personal token), reads are admin-only. Tested through the admin API.
- **Lane claim**: unchanged clause (Decision 9); a profile-bound job must satisfy it and the job claim clause together, including PRD #1909's `job_files_v1` protocol arm. Tested by claim tests: a lane worker claims a profile-bound job; a non-lane worker never does.
- **Lane worker route allowlist (`api/internal/handler/worker_lane_routes.go` `laneWorkerAllowlist`)**: gains PRD #1909's worker file routes (`GET /api/worker/runs/{id}/files/{fileID}`, `POST /api/worker/runs/{id}/files`) so a profile-bound job can use job files. Verify the exact routes against #1909's merged code before dispatch.

## Testing decisions

- Live-DB tests at the job-create seam for every row of the outcome examples, plus: unknown list 404, disabled product 403, a product token whose product has no allowance rows is refused (fail closed), and the allowance check runs for every product-token create (a mutation run removing it must redden a test).
- Claim tests for profile-bound jobs, following the existing lane-placement tests from PRD #1906 M5.
- Route-table test that the two file routes are on the lane allowlist and every other non-lifecycle worker route still refuses a lane worker.
- Prior art: PRD #1906 M3/M5 live-DB tests, PRD #1908 M2/M3 job tests.

## Milestones

- [ ] **M1: A product job can use an allowed site list, end to end.** Allowance table and its cookie-only admin API; job create accepts `egress_profile` with the allowance check (user token: any list; product token: its product's allowed lists; fail closed), removing the 422 in the same change; a profile-bound job is placed on the lane and passes the job claim clause including `job_files_v1`; PRD #1909's file routes join `laneWorkerAllowlist`. Tests per the Testing decisions. Blocked by: PRD #1909 merged. Gate: `task gate:api`, `task gate:agent`.
- [ ] **M2: Admins manage allowances in the web and CLI.** Admin page section to add and remove a product's allowed lists (`task gate:web`), read-only CLI listing, docs and the #1906 operator page updated. Blocked by: M1. Gate: `task gate:web`, `task gate:api`, `task check-docs:web`.

## Decision Log

- 2026-09-30: Split from PRD #1906 M8 into its own PRD, because #1906 closed with M8 open and nothing tracked it. Dispatch waits for PRD #1909, which changes the same seams (job create, the job claim clause, a migration) and adds the file routes this PRD must allow on the lane. Reviewed with a Codex buddy and the session that dispatched #1909.
- 2026-09-30: The 422 removal and the allowance check ship in one milestone (M1), per PRD #1906 Decision 13, so no build accepts a site list from a product token unchecked.
