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
- **Resource bounds (untrusted input)**: compressed download cap, total extracted bytes cap, per-file cap, file-count cap, compression-ratio cap; all enforced in the api before anything reaches the worker; streaming extraction with path normalisation (no absolute paths, no `..`, no links). Stored bytes count against the per-run limit chosen in the transport decision below and expire with the run.
- **Transport to the worker (OPEN, must be decided before dispatch)**: no repo-run input-file seam exists today; job files are limited to `kind = 'job'` (`api/internal/store/queries/job_files.sql`). Choose between extending job files to `prompt` runs and a new bounded per-run store, and name its per-run byte limit and retention, before this PRD goes to uzi.
- **Prompt**: a manifest (name, size, kept/skipped, reason) inside the untrusted fence.

## Testing decisions

- Extraction: table tests with crafted zips (traversal, symlink, bomb, oversized file, non-UTF-8).
- Drivers: fake HTTP servers per forge for list + download, including expired and unsupported.
- End to end: a `ci_watch` live-DB test where the fake forge serves an artifact and the created run carries the manifest.

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
