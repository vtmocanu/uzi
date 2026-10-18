# PRD #2866: CI artifacts as repair evidence

**Status**: M1 dispatched source built; final validation and acceptance incomplete. #2865 merged via #2983; transport seam decided in D3. M2 is deferred and remains open. This PRD stays active until acceptance is established.
**Issue**: #2866
**Umbrella**: #2864 (child 2 of 2)
**Priority**: Low

## Problem

Before M1, a default-branch repair run (#2865) saw only bounded job log tails. Harness-style CI (multi-phase end-to-end suites) writes its real evidence (per-phase logs, server logs, state dumps) into an uploaded artifact, and the console tail often shows only the last assertion. The run then guesses or reports that it cannot diagnose.

## Outcome

An owner can name artifact names (or globs) on a `ci_watch` schedule. When a repair run starts for a red execution, the api fetches those artifacts for that exact execution, bounds and sanitises them, and hands them to the run as read-only input files; the prompt lists them and says what was omitted.

Acceptance examples:

1. Schedule watches `nightly-e2e` with artifact `e2e-rundir-*`. The red execution uploaded `e2e-rundir-gitlab` (zip, 40 MiB). The run receives the extracted text files up to the byte cap, plus a bounded manifest recording refusals and known omissions. Uninspected omissions have unknown counts; the manifest need not name every skipped file.
2. Artifact expired or absent: the run starts anyway with logs only and the manifest records the missing/expired refusal.
3. A zip containing `../escape`, a symlink, or a 2 GiB compressed bomb: nothing is written outside the run's input directory; extraction stops at the cap; the manifest records the refusal.

## Out of scope

- Artifacts for MR-branch `ci_fix` runs (could reuse this later).
- Binary artifacts (images, core dumps): only UTF-8 text files are passed; inspected refusals are recorded within the manifest bounds, not as an exhaustive inventory.

## Modules and seams

- **Forge interface**: new `ListRunArtifacts(ctx, projectID, runID)` and `DownloadArtifact(ctx, projectID, artifactID, maxBytes)` on all three drivers plus `forgetest.BaseFake` (an interface change costs four sites). M1 implements GitHub: Actions artifacts API (zip). GitLab and Forgejo currently return `ErrForgeVersionUnsupported` and remain logs-only. M2 plans GitLab job artifacts archives and Forgejo Actions artifacts where the server version supports them.
- **Resource bounds (untrusted input)**: compressed download cap, total extracted bytes cap, per-file cap, file-count cap, compression-ratio cap; all enforced in the api before anything reaches the worker; streaming extraction with path normalisation (no absolute paths, no `..`, no links). Stored bytes count against the per-run limits in the transport decision below and expire after the run ends under job-files retention.
- **Transport to the worker (decided, D3)**: reuse the job-files store (PRD #1909: sealed chunks in Postgres, the stored-files quota lock and shared budget, the retention sweeper, the fenced worker download), not a new per-run store.
  - **Eligibility**: only runs created by the ci_watch repair path (`kind = 'prompt'` with `ci_config_guard`) may carry repair inputs. Widen only the internal repair-input writer, the fenced input download, the claim manifest and the lifecycle (settle/expire) queries. The `/api/v1` upload and attach routes and the worker output-upload routes stay job-only: a guarded prompt run gains no job-output or product-file authority.
  - **Admission** (transaction-aware): fetch and extract outside any database lock. Admission and chunk writes then run on the repair run's creation transaction, taking the existing stored-files quota lock (`store.LockStoredFiles`) after the schedule row lock; they never call `JobFiles.Reserve`, which opens its own transaction. A quota or budget refusal leaves no file rows, chunks or reservations, and the run is still created (logs-only, manifest reason "skipped: storage budget"), with run creation and episode accounting atomic either way.
  - **Limits**: per run at most `UZI_CI_ARTIFACT_MAX_FILES` (default 50) files, `UZI_CI_ARTIFACT_MAX_BYTES` (default 16 MiB) of extracted text, 4 MiB per file, all also counted against the existing per-owner, instance and shared stored-files budgets.
  - **Retention**: as job files: `UZI_JOB_FILES_RETENTION` after the run ends, by the existing sweeper.
  - **Names**: the artifact's relative path is untrusted manifest metadata only. The stored name is safe and content-addressed (`display_name` forbids separators), and identical content is deduplicated so repeated digests cannot collide when files are materialised exclusively.
  - **Rollout**: a new capability `ci_artifacts_v1` gates claiming a repair run with attached artifacts, in addition to the existing `ci_config_guard_v1` requirement; a run without artifact files retains the guard requirement.
  - **Worker**: materialise in a private worker-owned 0700 directory with 0400 files under the run directory, outside the clone, reusing the verified size and sha256 writer. Both harnesses use read-only mediated evidence tools, including across resumes; cleanup with the run directory. Artifact URLs and collection tokens stay in the api.
- **Prompt**: a bounded manifest (name, size, kept/skipped, reason) inside the untrusted fence; counts remain unknown for uninspected omissions. Scrub content before hashing/storage. Bind provenance to the observed execution; GitHub run-scoped archives are admitted only for a positively identified first attempt, so unknown attempts and reruns stay logs-only. Missing, expired, unsupported and budget-refused evidence permits repair with log tails; partial evidence is recorded when files remain admitted.

## Testing decisions

- Extraction: table tests with crafted zips (traversal, symlink, bomb, oversized file, non-UTF-8).
- Drivers: fake HTTP servers per forge for list + download, including expired and unsupported.
- End to end: a `ci_watch` live-DB test where the fake forge serves an artifact and the created run carries the manifest.
- Admission: rollback leaves no rows or chunks; concurrent admissions respect the quota lock; a one-connection pool completes repair creation with artifacts (no second connection while the transaction holds the lock); a budget refusal still creates the run logs-only.
- Guards: a guarded prompt run cannot use the `/api/v1` upload/attach or worker output-upload routes; a worker without `ci_artifacts_v1` does not claim a run with attached artifacts.

## Milestones

### M1: GitHub artifacts reach the repair run
Blocked by: #2865 merged.
Owner artifact selector on `ci_watch` (CLI, web), GitHub driver, bounded extraction, manifest in the prompt, docs.

### M2: GitLab and Forgejo artifacts
Blocked by: M1.

- [ ] GitLab artifact provider.
- [ ] Forgejo artifact provider.
- [ ] M2 validation and acceptance.

### Dispatched progress and evidence

These checks distinguish source present at `2aff5c0035a96c2feebbc3a9a9db6e7f932a995c`
from final acceptance. Test outcomes below are dispatch-reported evidence,
not new receipts from the documentation pass. Provider fixtures are scripted;
none establishes live-provider acceptance.

- [x] M1 backend/worker source present: selectors, GitHub collection, bounded
  extraction and scrub-before-hash, sealed job-file admission, manifest,
  guarded/capability-fenced delivery and mediated read-only evidence tools.
  CLI behavior is covered in `api/cmd/uzi/schedule_artifacts_test.go`;
  selector bounds live in `api/internal/ciartifacts/artifacts.go`.
- [x] Harness unit `34a6f8b5` adds real CodexExecutor fresh/resume coverage.
  Dispatch reports the lead's discard-result mutation caused two runtime
  failures, restoration gave two passes, and the focused suite passed 70 tests.
  This scripted harness evidence does not substitute for worker-UID/runtime
  acceptance.
- [x] UI source present at `2aff5c00`: one name/glob per line, up to 20,
  default/user parity, preservation/clearing, validation and detail display.
  Dispatch reports 188 focused tests and two Chromium tests passed, subject
  to the dependency provisioning failure below.
- [x] Owner selection and GitHub-only documentation written and mirrored;
  documentation checks are recorded separately below.
- [ ] Required final component gates on the final integrated head. The earlier
  web gate **FAILED** at dependency check (`log7NvY8U`, `EXIT=201`):
  installed Playwright and playwright-core 1.63 versus locked 1.64,
  KaTeX 0.18.2 versus 0.18.10, and Commander 8.3 versus 15.
  The same four mismatches were confirmed on the base `34a6f8b5` export
  (`logKa6z8f`); this was a provisioning failure, not a product regression
  or a passing gate. On 2026-10-18 the owner authorized the narrow
  project-dependency exception: `npm ci --ignore-scripts` in `web/` at
  committed `266d4ef3f8f475098c842d327a21ed52ff09e70f` completed with
  `EXIT=0` (`gate-log.AJCXVQ`). Tracked manifests and lockfiles remained
  byte-identical. The merge did not change the agent lockfile, so `agent/`
  was not refreshed. Every web check must rerun in the refreshed environment;
  no earlier mismatched-environment result discharges acceptance.
- [ ] Four real-database composition cases: **NOT RUN**:
  `TestCIArtifactsRepairE2E/claude/fresh`,
  `TestCIArtifactsRepairE2E/claude/resume`,
  `TestCIArtifactsRepairE2E/codex/fresh`, and
  `TestCIArtifactsRepairE2E/codex/resume` in
  `api/internal/handler/ci_artifacts_repair_e2e_test.go`. These use real
  adapters, HTTP and PostgreSQL with scripted provider transports.
- [ ] LiveDB admission, rollback, quota, lifecycle and download-fence checks:
  **NOT RUN** in this continuation.
- [ ] Worker-UID/runtime acceptance: **NOT RUN**. The lead's worker-upgrade/MTU
  prerequisite attestation establishes prerequisites only, not a UID pass.
- [ ] Maintainer-only packaged proof by the landing lead before merge:
  **NOT RUN**.
- [ ] M1 delivered/accepted as a whole. Source progress and focused passes
  above do not close the outstanding acceptance work.

Dispatch provenance: original `217a` work preserved; integration merge
`cda42eca`; `40f51034` renumbered the migration to 342. Independent reviewer,
tester and auditor reported clean on both units. Earlier API and agent
component gates passed at `40f51034`, not final HEAD.

Documentation-pass checks: `task docs:sync`, `task check-docs:web`, and the
supported fragment validator passed; embedded copies match the three changed
docs. These checks do not establish component or runtime acceptance.

The existing #2865 decisions remain: default-branch repair only, human merge,
no CI-config publication (even after approval), and auto-approve initially OFF.
No infrastructure, workflow, custody or provider expansion is part of this
documentation unit.

## Decision Log

| # | Decision | Reason | Rejected alternative |
|---|---|---|---|
| D1 | Api fetches and sanitises with its own forge credentials; the worker never gets an artifact URL or token | Bounds are enforced once, server-side, before any byte reaches a worker | Worker downloads with its own PAT |
| D2 | Text files only | Smallest safe evidence set; binaries rarely help an LLM diagnosis | Passing the raw archive |
| D3 | Reuse the job-files store for repair inputs, admitted on the run-creation transaction, eligible only for ci_watch repair runs, gated by `ci_artifacts_v1`; per-run caps 50 files / 16 MiB / 4 MiB per file; job-files retention (landing session + buddy, 2026-10-14) | The sealed storage, quota lock, shared budget, sweeper and fenced download already exist; the admission must share the run's transaction so a refusal never splits run creation from its files | A new bounded per-run store (rebuilds all of that, second retention path); inline in the run description (256 KiB cap) |
