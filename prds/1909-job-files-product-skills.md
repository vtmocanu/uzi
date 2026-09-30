# PRD #1909: Job files and product skill sets

**Issue**: #1909
**Status**: Draft
**Priority**: Medium
**Created**: 2026-09-29
**Depends on**: PRD #1908 (repo-less jobs and `/api/v1/jobs`), PRD #1906 (the fetch service and its source log), PRD #1907 (product registration and product tokens)
**Related**: PRD #1910 (interactive product consent; not required here), PRD #1296 / ADR-1296 (recovery archives, the storage precedent reused here), PRD #602 / ADR-602 (agent-source sync, the sealed clone credential and approve-before-apply precedent reused here), PRD #16 (skills)

## Problem

PRD #1908 gives external products a way to start a job as a user and read back a text report and structured findings. Two things are still missing for real use:

1. **Files.** A product often needs to hand the job a document (for example a PDF or a Markdown export of a document section) and get files back: the report, the findings as JSON, and the official documents the job downloaded through the fetch service (PRD #1906). Today a job accepts text only and returns text only.
2. **Product skill sets.** A product carries domain playbooks (how to analyse a vendor's datasheets, what a good comparison looks like). Today skills come from four sources (user, global, builtin, repo; `docs/skills.md`), none of which is "this product's skills, for this product's jobs only". An admin could paste them in as global skills, but then they would reach every run on the instance and drift from the product's own source of truth.

This PRD adds bounded, encrypted, short-lived job file storage and a product-scoped skill set synced from the product's own git repo. Product policy (which job types and site lists a product may use) is not here: PRD #1908 and PRD #1906 own it.

## Current state (verified on `main`, 2026-09-29)

- **No object store.** Nothing in `api/`, the Helm chart or `docker-compose.yml` talks to S3 or another object store. The only S3 reference in `deploy/chart/values.yaml` is the optional CNPG Postgres backup target (~1474-1546), not an application store.
- **Binary data already lives in Postgres, encrypted and chunked.** Recovery archives (PRD #1296) store sealed ~1 MiB chunks as `bytea` (`api/internal/store/migrations/00223_recovery_archive.sql`, table `recovery_capture_chunks`, ~119-126), with a metadata row carrying `byte_size`, `checksum`, `chunk_count`, `expires_at` (`recovery_captures`, ~66-112) and a partial index for the expiry sweep (~116).
- **The recovery archive limits are the size precedent**: max bundle 64 MiB, ready payload 1 GiB per owner, **instance 4 GiB**, retention 7 days (`api/internal/config/config.go` ~719-724, defaults ~1242-1247; enforced in `api/internal/recovery/service.go` ~112-125, expiry set ~324). The retention sweep `ExpireReadyCaptures` flips expired rows and deletes their chunks in one statement (`api/internal/workersvc/service.go` ~623-627, query `api/internal/store/queries/recovery.sql` ~145).
- **Default Postgres volume is small**: the chart's CNPG storage defaults to `5Gi` data plus `5Gi` WAL (`deploy/chart/values.yaml` ~1511-1517). The recovery archive instance quota alone (4 GiB) already takes most of that data volume, so any new in-database file store cannot simply add its own quota on top.
- **Sealing with bound context exists**: `secretbox.Box.SealWithAAD` / `OpenWithAAD` (`api/internal/secretbox/secretbox.go` ~66, ~80), keyed by `UZI_SECRET_KEY`.
- **Skills schema**: `skills.scope` is `CHECK (scope IN ('builtin','global','user'))` (`api/internal/store/migrations/00040_skills.sql` ~13); skills reach a run only through `agent_skill_allocations` to an agent template (~29-39). Claim-time assembly resolves name collisions by precedence user > global > builtin and caps the union (`api/internal/workersvc/claim_skills.go` ~25-72). Caps are `SKILL_MAX_BYTES` (64 KiB body) and `SKILLS_MAX_PER_RUN` (32) (`docs/skills.md`, "Limits and drops"), also sent to the worker on the claim (`agent/src/protocol.ts` ~798-803).
- **A git-sync precedent with an SSRF allowlist, a sealed private-repo credential and approve-before-apply exists**: agent-source sync clones with a bounded go-git path (`api/internal/agentsource/git.go`, `FetchRoleFiles` ~120, 60 s `cloneTimeout` ~37, a credentialed redirect must stay on the starting origin ~218, the token is scrubbed from error text ~455), gated by `AGENT_SOURCE_ALLOWED_BASE_URLS` (`docs/configuration.md` ~158). A private repo is read with an admin-set **sealed clone token** (`settings.KeyAgentSourceCredential`, `api/internal/settings/keys.go` ~170-176, capped in `settings_agent_source.go` ~137), sent as BasicAuth with username `oauth2` (`git.go` ~39-42); an empty token means a public clone. Every change is staged for admin approval (`api/internal/store/migrations/00160_agent_source_staged.sql`; ADR-602, "Approve-before-apply is the primary control").
- **Repo skills are the untrusted-skill precedent**: only `name`, `description` and body are read; any other frontmatter key (for example `allowed-tools`) is stripped (`docs/skills.md`, "Repo skills").

## Decisions

**D1. Size and retention first, then storage.** The limits below are the requirement; the storage choice follows from them.

| Limit | Default | Env |
|---|---|---|
| One input file | 25 MiB | `UZI_JOB_INPUT_FILE_MAX_BYTES` |
| Inputs per job | 10 files, 50 MiB total | `UZI_JOB_INPUTS_MAX_FILES`, `UZI_JOB_INPUTS_MAX_BYTES` |
| One output file | 25 MiB | `UZI_JOB_OUTPUT_FILE_MAX_BYTES` |
| Outputs per job | 50 files, 100 MiB total | `UZI_JOB_OUTPUTS_MAX_FILES`, `UZI_JOB_OUTPUTS_MAX_BYTES` |
| Retained job files per owner | 256 MiB | `UZI_JOB_FILES_PER_OWNER_BYTES` |
| Retained job files, instance | 1 GiB | `UZI_JOB_FILES_INSTANCE_BYTES` |
| Shared stored-file budget (job files plus recovery archives) | 4 GiB | `UZI_STORED_FILES_BUDGET_BYTES` |
| Retention after the job ends | 7 days | `UZI_JOB_FILES_RETENTION` |
| Uploaded but never attached to a job | 1 hour | `UZI_JOB_UPLOAD_TTL` |

Uzi is a **transfer point, not an archive**: the product downloads what it needs and keeps its own copy. Seven days matches the recovery archive retention and covers a product that polls late or retries.

**D2. One shared stored-file budget, so the two file stores cannot jointly outgrow the database.** A job file is admitted only if both hold after the write: job-file bytes stay within `UZI_JOB_FILES_INSTANCE_BYTES`, **and** job-file bytes plus current recovery-archive bytes stay within `UZI_STORED_FILES_BUDGET_BYTES`. The 4 GiB default equals the existing recovery instance quota, so turning job files on never lets stored files exceed what the default 5Gi data volume was already sized to hold. **The budget is a hard combined limit, and recovery wins** (maintainer decision, 2026-09-29): recovery-archive admission also counts job-file bytes against `UZI_STORED_FILES_BUDGET_BYTES` (a deliberate change to existing recovery admission). When a recovery capture would not fit, admission first reclaims job files (expired ones, then the oldest `available` ones, never files attached to a non-terminal job) until it fits, in the same transaction; only if that is still not enough is the capture refused, exactly as today. Job files never reclaim recovery archives. The budget is fixed, not scaled to the volume size (the api cannot reliably know it); the docs page states the sizing rule: an operator who raises a quota sizes the database volume for the budget plus ordinary data.

**D3. Store job files in Postgres, chunked and sealed, reusing the recovery-archive shape.** A metadata table (`job_files`) plus a chunk table (`job_file_chunks`, ~1 MiB sealed `bytea` chunks, `ON DELETE CASCADE` from the file). Each chunk is sealed with `SealWithAAD`, the AAD binding file id, chunk index and owner, so a chunk cannot be replayed into another file. Reason: the caps in D1 fit the existing database; uzi has no object store, and adding one means a new dependency, credentials and a new failure mode in both compose and the chart for a feature whose files are short-lived. **Revisit trigger**: if an operator needs a job-file quota above ~10 GiB, move chunks behind a storage interface with an S3 backend; the metadata table and API do not change.

**D4. Quota admission is atomic and reserves before writing.** Admission runs in one transaction that takes `pg_advisory_xact_lock` on the owner and on one fixed instance key (in that order, always), sums the retained bytes (`job_files` rows in `reserved|attached|available` state, plus `recovery_captures` bytes for the shared budget), and inserts the new row in state `reserved` with its declared size before any chunk is written. Two concurrent uploads therefore cannot both pass a check that only one fits. **Both stores admit under the same lock.** Recovery-capture admission (today a lock-free pre-check inside its streaming transaction, `api/internal/recovery/service.go` ~264-285) is restructured to the same shape: a short transaction takes the same owner key and then the same fixed shared-budget key (identical order in both paths, so no deadlock), sums job-file plus recovery bytes including reservations, reclaims job files if needed (D2), and records the capture as reserved at its declared size; the chunk stream then runs outside the lock, so a long upload never blocks the other store. A live-DB test runs concurrent job-file and recovery uploads that each fit alone but not together and asserts the combined total never exceeds the budget. A reservation whose upload fails or times out is released by the same sweep that expires files. An upload whose written bytes exceed its declared size is rejected (`http.MaxBytesReader` on the declared size), so the reservation is an upper bound. Quotas are enforced at admission, never by failing a finished job: an input over quota is refused with `413` and a stated reason; an output over quota is refused per file, the job still completes, and its result lists the refused file with the reason. The instance and shared-budget breach scope to the affected owner, as recovery archives do (`api/internal/config/config.go` ~721 comment).

**D5. Upload, then reference.** `POST /api/v1/files` (multipart, one file) returns a file id in state `unattached`; `POST /api/v1/jobs` (PRD #1908) accepts `input_file_ids`. An unattached file expires after `UZI_JOB_UPLOAD_TTL`. This keeps job create a small JSON request, lets a large upload retry on its own, and gives one place to enforce the per-file cap. A file can be attached to one job only, and only by its owner.

**D6. Files are untrusted data, both ways, and are named by content.**
- Inputs: accepted types are an allowlist (PDF, plain text, Markdown, CSV, JSON, DOCX, XLSX, PNG, JPEG), checked by magic bytes as well as the declared type. On the worker they land read-only under the job's `inputs/` directory and are presented to the agent as data, never instructions (the same framing rule as other untrusted content).
- Outputs: the same allowlist plus HTML (a downloaded page). Every download is served with `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and `Content-Type: application/octet-stream`, never rendered inline, so a hostile file cannot run in a product's or uzi's origin.
- **Storage and on-disk names come from the content, never from the source.** A file is stored and written on the worker as `<sha256>.<ext>`, where the extension comes from the detected type. A name from the URL, a `Content-Disposition` header or the uploader is kept as sanitised, bounded, path-free display metadata only, and is never used as a path. This matches the fetch tool, which saves downloads under their sha256 (PRD #1906).

**D7. The source log belongs to the fetch service (PRD #1906) and is authoritative; this PRD surfaces it.** The fetch service, not the worker, records each fetch (URL, final URL after redirects, verdict, content SHA-256, byte size, time) in `run_fetches`, because a worker-written log would be forgeable. The fetch service keeps **no** bytes: it returns them to the worker, which saves them in the run workspace (PRD #1906). The worker uploads the downloads it keeps as outputs. `GET /api/v1/jobs/{id}/result` gains `sources`, read from `run_fetches` for that run only, and an output file whose SHA-256 equals a fetched body in that same run carries `source_url` from that row. Any origin the agent claims for a file is ignored: a `source_url` exists only when the hashes match.

**D8. Worker transfer uses the existing worker trust boundary.** The claim carries the input manifest (id, display name, size, SHA-256). The worker downloads inputs and uploads outputs through new endpoints under `/api/worker/runs/{run}/files`, authenticated by the worker's join token and allowed only for a run that worker currently holds, bodies wrapped in `http.MaxBytesReader` (the recovery upload pattern, `api/internal/handler/recovery.go` ~92). The worker verifies each input's SHA-256 before use.

**D9. Product skill sets are a new skill scope, synced from the product's repo, applied only to that product's jobs.**
- A registered product (PRD #1907) gains optional `skills_repo_url`, `skills_ref` and a **sealed, write-only clone token**. The URL must match `UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS` (https-only; empty means the feature is off), a separate allowlist from `AGENT_SOURCE_ALLOWED_BASE_URLS` so enabling one does not widen the other. The URL cannot carry userinfo.
- **Private repo authentication reuses the agent-source credential pattern, per product.** The registering admin pastes a read-only clone token (for example a forge deploy token or a fine-grained read-only PAT on the skills repo). It is sealed with `secretbox`, never returned by any endpoint (the field reads back as set or not set), capped in length like `maxAgentSourceCredentialLen`, sent as BasicAuth with username `oauth2` (`git.go` ~39-42), scrubbed from error text, and kept on the starting origin across redirects (`git.go` ~218). An empty token means a public repo. Rejected alternatives: reusing a user's or admin's forge connection PAT (broad write scope over unrelated repos, and the product's skills would silently depend on one person's account); SSH deploy keys (uzi's clone path is HTTPS-only). Rotating `UZI_SECRET_KEY` invalidates the token like every other sealed secret.
- The api fetches with the agent-source bounded clone path (`api/internal/agentsource/git.go`), reads `skills/*/SKILL.md` (and `.claude/skills/*/SKILL.md`), and keeps only `name`, `description` and body, stripping every other frontmatter key, exactly as repo skills do.
- Every change is **staged** and needs an **admin approval** before it reaches a job (ADR-602's primary control). Approval records the approving admin and the commit SHA.
- Approved skills are stored as `skills` rows with a new scope `product` and a `product_id`. They are delivered **only** to jobs started through that product, and a product job gets **only** its product's skills: no user, global, builtin or repo skills, so what shapes a product's output is exactly what the product ships. The existing caps apply (`SKILL_MAX_BYTES`, `SKILLS_MAX_PER_RUN`).
- Product skill bodies are visible to uzi admins; that is accepted.

**D10. Mirrors.** The new skill scope touches the DB check constraint, `scopeRank` (`claim_skills.go` ~28) and the web/CLI skill lists. The new file endpoints need CLI parity (`uzi job files`, `uzi job file get`) and the OpenAPI document for `/api/v1` that PRD #1907 establishes.

## Out of scope

- Product policy: which job types (PRD #1908) and site lists (PRD #1906) a product may use.
- An object-store backend (parked behind the D3 revisit trigger).
- Files larger than the D1 caps, resumable uploads, and streaming partial outputs while a job runs.
- Per-user product skill overlays, and mixing product skills with user or global skills.
- Showing job files in uzi's web UI beyond an admin quota view; products present files to their own users.
- Anything specific to a code-review job type (repo diffs, PR comments).
- The source log itself (PRD #1906).

## Milestones

Gate for every Go milestone: `task gate:api` (plus `task gate:web` / `task gate:agent` where named). Live-DB tests follow `.claude/rules/go.md`. No milestone creates or edits anything under `.github/workflows/`. Clone-token test fixtures are assembled at runtime, never a complete token literal (`check:token-literals`).

- [ ] **M1: File store and atomic quotas.** Migration (draft number) adding `job_files` (id, owner, run nullable until attached, direction, content-derived storage name, display name, detected type, byte size, SHA-256, state `reserved|unattached|attached|available|expired`, `expires_at`) and `job_file_chunks` (sealed chunks, cascade). A `jobfiles` service: D4 reserve-then-write admission under advisory locks, chunked write with per-chunk `SealWithAAD`, streaming read, D1 caps and the D2 shared budget from config, and a periodic sweep that releases stale reservations, expires files and deletes their chunks while keeping the metadata row, modelled on `ExpireReadyCaptures`. Live-DB tests: cap edges (exactly at and one byte over), owner scoping, the shared budget counting recovery bytes, recovery admission counting job-file bytes and reclaiming expired then oldest available job files (never ones attached to a live job) before refusing, the existing recovery-quota tests still passing, AAD mismatch rejection, expiry, and a **concurrency test** where N parallel reservations that each fit alone but not together admit exactly as many as fit. Gate: `task gate:api`.
- [ ] **M2: Input upload and attach.** `POST /api/v1/files` (magic-byte type check, display-name sanitising, unattached TTL) and `input_file_ids` on job create (ownership check, single attachment, per-job caps). Product tokens (PRD #1907) and user tokens both work, scoped to the caller's own files. Handler tests include a type spoof (PDF extension, other magic bytes), a path-like display name, and a cross-owner file id. Gate: `task gate:api`.
- [ ] **M3: Worker input delivery.** The claim carries the input manifest; the worker downloads via the worker files endpoint, verifies SHA-256, writes files read-only under the job's `inputs/` by content name, and the job prompt names them as untrusted data. A hash mismatch fails the job with a stated cause instead of running on altered input. Gate: `task gate:api`, `task gate:agent`.
- [ ] **M4: Output upload.** The worker uploads output files (report, findings JSON, downloaded documents it chose to keep) before reporting the job terminal; per-file and per-job caps; an over-cap or over-quota file is refused and listed, the job still completes (D4). Gate: `task gate:api`, `task gate:agent`.
- [x] **M5: Result files and sources.** `GET /api/v1/jobs/{id}/files` and `GET /api/v1/files/{id}` (attachment, nosniff, octet-stream), and `sources` plus per-file `source_url` in the result (D7), read from PRD #1906's `run_fetches`. OpenAPI updated. Tests assert the headers, that `source_url` appears only on a hash match within the same run, and that a file never matches a source from another job. Gate: `task gate:api`.
  - Landed: the file-by-id route and the job listing follow one visibility rule. A file with no run yet (an unattached input) is visible only to the product it was uploaded through (a `uzc_` caller only its own product-less uploads); a file of a run (an attached input or an output, which the worker stores with no product) is visible exactly as its job is (`GetJobForCaller`). `source_url` is set only on an output, from the earliest `allowed` fetch of the same run whose non-empty sha256 equals the file's (the final URL when one was recorded, else the requested URL). `sources` lists the run's first 500 fetches, allowed and refused. Also adds `refused_files` to the result and listing.
- [ ] **M6: Product skill sets.** Migration adding scope `product` and `product_id` to `skills`; product fields `skills_repo_url` / `skills_ref` and the sealed clone token (write-only API, length cap); `UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS`; sync via the agent-source clone path; stage, admin approve, apply; claim-time delivery of only the product's skills to that product's jobs. Tests: frontmatter stripping, allowlist refusal, userinfo in the URL refused, the token never appearing in any response or error text, an unapproved change never reaching a claim, a product job receiving no user/global skills, and a non-product run never receiving product skills. Gate: `task gate:api`, `task gate:web` (admin approve view).
- [ ] **M7: CLI, docs, config.** `uzi job files` / `uzi job file get`, admin product-skills commands, `docs/configuration.md` rows for every new env var, a docs page section on job files, the D2 sizing rule and product skill sets, then `task docs:sync`. Env vars wired into the chart and `.env.example`. Gate: `task gate:api`, `task check-docs:web`.
- [ ] **M8: Hosted k8s acceptance (maintainer-owned, required for completion).** On a hosted k8s deployment: a product token starts a job with a PDF input; the job fetches an official document through the fetch service; the result lists it with a matching `source_url`; a private product skills repo syncs with its clone token and reaches only that product's jobs after approval; files expire after the retention window; stored-file bytes stay within the shared budget on the default volume.

## Dependency plan

C2 is phase 3: it starts after PRD #1908 (phase 2) and PRD #1906's fetch service and source log have merged. It runs in parallel with PRD #1906's job integration and PRD #1910.

| Milestone | Needs |
|---|---|
| M1 | PRD #1908 merged (job run kind) |
| M2 | M1; PRD #1907 merged (already required by PRD #1908) |
| M3, M4 | M1, M2 |
| M5 | M4; PRD #1906's `run_fetches` merged |
| M6 | PRD #1907 merged (products exist); independent of M1-M5 |
| M7 | M2-M6 |
| M8 | all, deployed |

M6 can run in parallel with M1-M5.

## Success criteria

- A product can send a PDF into a job and receive the report, the findings JSON and the fetched official PDFs back, each within the D1 caps.
- Every downloaded official file in a result carries a `source_url` that comes from the fetch service's own record, matched by hash in the same run, never from the agent.
- Concurrent uploads never push retained bytes past a quota or the shared budget.
- No job file outlives its retention window; the sweep reclaims chunk storage and stale reservations.
- A product job runs with exactly its approved product skills, a private skills repo is read with a token no endpoint ever returns, and no other run ever receives those skills.
- An over-cap or over-quota file never fails a completed job and never goes unreported.

## Risks

- **Database growth.** Files share the database volume with everything else (default 5Gi). Mitigation: the D2 shared budget caps job files plus recovery archives together at the existing recovery quota, the 7-day retention reclaims space, and the D3 revisit trigger names the move to object storage. Deleted chunk space is reused after vacuum rather than returned to the filesystem; the quota, not disk size, is the control.
- **Starvation between the two stores.** Recovery archives can fill the shared budget and leave no room for job files. Mitigation: job files refuse with a clear quota reason (never a failed job); the docs sizing rule tells operators to raise the volume and budget together.
- **Hostile files.** A PDF or HTML page can carry prompt injection or content meant to run in a browser. Mitigation: D6 (data framing, attachment-only serving, type allowlist, content-derived names), and the research worker has no general internet access (PRD #1906).
- **Skill supply chain.** A product's skills repo is a prompt source for its jobs. Mitigation: allowlisted URL, stripped frontmatter, stage and admin approval per change (ADR-602), read-only clone token.
- **Two new scopes of mirrors.** The `product` skill scope must reach every place that lists or ranks scopes (D10); a missed mirror shows as a product skill absent from a list or mis-ranked. Mitigation: a vocabulary test like the existing run-kind ones.

## Open questions

None. Resolved 2026-09-29 by the maintainer (see D2 and the Decision Log): the shared budget is a hard combined limit with recovery winning, and it stays a fixed default rather than scaling with the volume size.

## Decision Log

- 2026-09-29: D1-D9 recorded at drafting. Storage in Postgres chosen over object storage because the stated caps fit the existing database and uzi has no object store (D3); product jobs get only their product's skills (D9).
- 2026-09-29 (review round): Scope narrowed to files and product skill sets; product policy stays with PRD #1908 (job types) and PRD #1906 (site lists). Quota admission made atomic (reserve under advisory locks before writing, D4) because separate check-then-write lets concurrent uploads overshoot. Instance default cut to 1 GiB per store and a shared 4 GiB stored-file budget added (D2), because the existing 4 GiB recovery quota plus a 2 GiB job quota could exceed the chart's default 5Gi data volume. Private skills repos authenticate with a per-product sealed, write-only, read-only clone token, reusing the agent-source credential pattern (D9). Files are named by sha256 on disk, never by URL or `Content-Disposition` (D6). The fetch service keeps no bytes; `source_url` requires a hash match against the same run's `run_fetches` (D7). Hosted k8s acceptance (M8) is part of completion.
- 2026-09-29 (maintainer decisions, round 2): the shared stored-file budget is a hard combined limit and recovery wins: recovery admission now counts job files and reclaims expired, then oldest available, job files before refusing a capture (an intentional change to existing recovery admission, approved by the maintainer); the budget stays a fixed default with a documented volume-sizing rule rather than auto-scaling (buddy and author agreed).
- 2026-09-29 (final review): both admission paths (job files and recovery captures) take the same owner and shared-budget advisory locks in the same order, recovery admission becomes reserve-then-stream so the lock is not held during upload, and a cross-store concurrency test pins the hard limit (buddy finding: recovery's pre-check ran without the shared lock).
