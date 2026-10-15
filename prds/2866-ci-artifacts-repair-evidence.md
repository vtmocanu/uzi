# PRD #2866: CI artifacts as repair evidence

**Status**: Draft, blocked by #2865
**Issue**: #2866
**Umbrella**: #2864 (child 2 of 2)
**Priority**: Low

## Problem

A default-branch repair run (#2865) sees only bounded job log tails. Harness-style CI (multi-phase end-to-end suites) writes its real evidence (per-phase logs, server logs, state dumps) into an uploaded artifact, and the console tail often shows only the last assertion. The run then guesses or reports that it cannot diagnose.

## Outcome

An owner can name artifact names (or globs) on a `ci_watch` schedule. When a repair run starts for a red execution, the api fetches those artifacts for that exact execution, bounds and sanitises them, and hands them to the run as read-only input files; the prompt lists them and says what was omitted.

Acceptance examples:

1. Schedule watches `nightly-e2e` with artifact `e2e-rundir-*`. The red execution uploaded `e2e-rundir-gitlab` (zip, 40 MiB). The run receives the extracted text files up to the byte cap, plus a manifest naming every skipped file and why.
2. Artifact expired or absent: the run starts anyway with logs only and the prompt states "artifact `<name>` unavailable: expired".
3. A zip containing `../escape`, a symlink, or a 2 GiB compressed bomb: nothing is written outside the run's input directory; extraction stops at the cap; the manifest records the refusal.

## Out of scope

- Artifacts for MR-branch `ci_fix` runs (could reuse this later).
- Binary artifacts (images, core dumps): only UTF-8 text files are passed; others are listed by name and size.

## Modules and seams

- **Forge interface**: new `ListRunArtifacts(ctx, projectID, runID)` and `DownloadArtifact(ctx, projectID, artifactID, maxBytes)` on all three drivers plus `forgetest.BaseFake` (an interface change costs four sites). GitHub: Actions artifacts API (zip). GitLab: job artifacts archive per job. Forgejo: actions artifacts API where the server version supports it, else `ErrForgeVersionUnsupported` (logs-only).
- **Resource bounds (untrusted input)**: compressed download cap, total extracted bytes cap, per-file cap, file-count cap, compression-ratio cap; all enforced in the api before anything reaches the worker; streaming extraction with path normalisation (no absolute paths, no `..`, no links). Stored bytes count against the per-run limits in the transport decision below and expire with the run.
- **Transport to the worker (decided, D3)**: reuse the job-files store (PRD #1909: sealed chunks in Postgres, the stored-files quota lock and shared budget, the retention sweeper, the fenced worker download), not a new per-run store.
  - **Eligibility**: only runs created by the ci_watch repair path (`kind = 'prompt'` with `ci_config_guard`) may carry repair inputs. Widen only the internal repair-input writer, the fenced input download, the claim manifest and the lifecycle (settle/expire) queries. The `/api/v1` upload and attach routes and the worker output-upload routes stay job-only: a guarded prompt run gains no job-output or product-file authority.
  - **Admission** (transaction-aware): fetch and extract outside any database lock. Admission and chunk writes then run on the repair run's creation transaction, taking the existing stored-files quota lock (`store.LockStoredFiles`) after the schedule row lock; they never call `JobFiles.Reserve`, which opens its own transaction. A quota or budget refusal leaves no file rows, chunks or reservations, and the run is still created (logs-only, manifest reason "skipped: storage budget"), with run creation and episode accounting atomic either way.
  - **Limits**: per run at most `UZI_CI_ARTIFACT_MAX_FILES` (default 50) files, `UZI_CI_ARTIFACT_MAX_BYTES` (default 16 MiB) of extracted text, 4 MiB per file, all also counted against the existing per-owner, instance and shared stored-files budgets.
  - **Retention**: as job files: `UZI_JOB_FILES_RETENTION` after the run ends, by the existing sweeper.
  - **Names**: the artifact's relative path is untrusted manifest metadata only. The stored name is safe and content-addressed (`display_name` forbids separators), and identical content is deduplicated so repeated digests cannot collide when files are materialised exclusively.
  - **Rollout**: a new capability `ci_artifacts_v1` gates claiming a repair run with attached artifacts; a run without artifacts is unaffected.
  - **Worker**: materialise read-only under the run directory, outside the clone (never in the repo diff, so no CI-config guard or publication interaction), reusing the verified size and sha256 writer and its directory protections, for both harnesses and across resumes; cleanup with the run directory.
- **Prompt**: a manifest (name, size, kept/skipped, reason) inside the untrusted fence.

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

## Decision Log

| # | Decision | Reason | Rejected alternative |
|---|---|---|---|
| D1 | Api fetches and sanitises with its own forge credentials; the worker never gets an artifact URL or token | Bounds are enforced once, server-side, before any byte reaches a worker | Worker downloads with its own PAT |
| D2 | Text files only | Smallest safe evidence set; binaries rarely help an LLM diagnosis | Passing the raw archive |
| D3 | Reuse the job-files store for repair inputs, admitted on the run-creation transaction, eligible only for ci_watch repair runs, gated by `ci_artifacts_v1`; per-run caps 50 files / 16 MiB / 4 MiB per file; job-files retention (landing session + buddy, 2026-10-14) | The sealed storage, quota lock, shared budget, sweeper and fenced download already exist; the admission must share the run's transaction so a refusal never splits run creation from its files | A new bounded per-run store (rebuilds all of that, second retention path); inline in the run description (256 KiB cap) |
