---
title: Jobs
order: 114
audience: user
---

# Jobs

A **job** is a run with no repository: a prompt plus optional named text inputs, worked by one of your workers, that ends in a structured report and findings. An external product starts one through `/api/v1/jobs`; you can also start one with [`uzi job`](./cli.md#uzi-job-repo-less-jobs). The only job type today is `research`.

A job never touches a forge. It has no repo, issue or branch, opens no merge request, and appears in run lists as kind `job`. It runs on your worker with your Claude source (your Anthropic credential, or your [model provider](#jobs-on-a-model-provider)), and its status, messages and result show on the run page.

## Start a job

1. An admin registers the product and ticks the job types it may start (**Admin → Products**). A product with none ticked cannot create jobs.
2. You mint a product token with the `jobs:run` scope (and `jobs:read` to read back). See [Product tokens](./product-tokens.md).
3. The product calls `POST /api/v1/jobs` with `Authorization: Bearer uzp_…`:

```
{"type": "research", "prompt": "Summarize the attached notes.",
 "inputs": [{"name": "notes.md", "content": "..."}],
 "requested_by_label": "Acme dashboard"}
```

`title` is optional (derived from the first line of the prompt). Inputs are inline text (for a document, upload a file instead: see [Job files](#job-files)): at most 20, 1 MiB in total, names of 1 to 100 characters (letters, digits, `.`, `_`, `-`, starting with a letter or digit, no `..`). `wall_seconds` sets a positive wall-clock limit; it is clamped to `RUN_WALL_CEILING` (24 hours by default). Omitted values default to `min(max(12h, RUN_TIMEOUT), max(RUN_WALL_CEILING, RUN_TIMEOUT))`, frozen at creation. With the default base timeout this is 12 hours. As a compatibility exception, an omitted budget preserves a larger base timeout even when it exceeds the derived 72h ceiling; explicit caller budgets still use the ceiling. A caller may request less than 12 hours. Jobs cannot be extended; they fail at the limit. `egress_profile` names a [site list](./egress-profiles.md) (see [Site lists](#site-lists)); leave it out for an ordinary job.

The response is 201 with the queued job. Poll `GET /api/v1/jobs/{id}`, then read `GET /api/v1/jobs/{id}/result`. The full contract, including every field, is `api/openapi/v1.yaml`.

Your own `uzc_` CLI token works on the same endpoints and may create any known job type.

## Endpoints and scopes

| Endpoint | Scope |
|---|---|
| `POST /api/v1/jobs` | `jobs:run` |
| `POST /api/v1/jobs/{id}/cancel` | `jobs:run` |
| `POST /api/v1/files` | `jobs:run` |
| `GET /api/v1/jobs`, `/{id}`, `/{id}/result`, `/{id}/messages?after=<seq>`, `/{id}/files` | `jobs:read` |
| `GET /api/v1/files/{id}` | `jobs:read` |

A product token sees only jobs created for its own product. A `uzc_` token sees every job you own. Anyone else's job, another product's job and a malformed id all return the same 404.

## Statuses

`queued`, `running`, `waiting`, `completed`, `failed`, `cancelled`. The last three are final. `waiting` is only a defensive mapping for the internal park states; a job never parks, so you should not see it in practice. A job **never parks**: hitting a usage limit, the wall-clock limit, a recovery situation or a disabled credential fails the job instead of leaving it waiting, so read `failure_reason` on a `failed` job.

## Cancel

`POST /api/v1/jobs/{id}/cancel` (or `uzi job cancel`) cancels a queued, running or waiting job. A queued job ends at once; a running job is stopped by its worker, so the response may still read `running` for a moment. A finished job returns 409 `job_terminal`.

## Results and findings

`GET /api/v1/jobs/{id}/result` returns `result: null` until the job reports. The result has a short `status` token, a markdown `report_md`, and `findings`, each with `severity` (`info`, `warning`, `error`), `message_md`, and an optional `url` or `file` and `line`. A job that ends without reporting has no result. The result also lists the job's `sources`, `files` and `refused_files` (see [Job files](#job-files)).

## Job files

A job can take files in and hand files back. uzi is a **transfer point, not an archive**: the product downloads what it needs and keeps its own copy, because every file expires (below). Files are stored encrypted in uzi's database, in small sealed chunks.

### Upload an input file

Upload first, then reference the file from the job create.

1. `POST /api/v1/files` (needs `jobs:run`) with `multipart/form-data` and the file in a part named `file`. Two headers matter: `X-Uzi-File-Size` (**required**, the file's exact size in bytes; uzi reserves exactly that much, then reads the file's bytes, and refuses an upload whose bytes differ; any multipart parts before `file` are read and discarded first, so put `file` first) and `X-Uzi-File-Sha256` (optional, 64 lowercase hex characters, verified against the bytes). The response is 201 with the file in state `unattached`.
2. Create the job with `"input_file_ids": ["<file id>", ...]` in the `POST /api/v1/jobs` body, next to (or instead of) the inline `inputs`.

```
curl -H "Authorization: Bearer uzp_..." \
     -H "X-Uzi-File-Size: $(wc -c < datasheet.pdf)" \
     -F "file=@datasheet.pdf" https://uzi.example/api/v1/files
```

- **Allowed types**, decided from the file's bytes and required to agree with the type the part's `Content-Type` and the file name's extension declare: PDF, PNG, JPEG, DOCX, XLSX and UTF-8 text (plain text, Markdown, CSV and JSON; JSON must parse). Anything else is refused. HTML is not accepted as an input.
- **Only the uploader can attach it, to one job.** An upload made with a product token attaches only for a token of that same product; one made with a `uzc_` token only for a `uzc_` caller. An unknown id, another user's or product's file, an already attached or expired file, and an id listed twice all read the same: 422 `file_unavailable`.
- **An unattached upload expires** after `UZI_JOB_UPLOAD_TTL` (1 hour by default), so a file you never use costs nothing for long.
- **The stored name comes from the content**, `<sha256>.<ext>`, never from the uploader. The uploader's file name is kept only as a sanitised display name (last path component, no control or invisible characters). Treat it as untrusted text.

### What the job sees

The worker downloads each attached file from uzi, checks its SHA-256 against the manifest the claim carried, and writes it read-only under the job's `inputs/` directory as `<sha256>.<ext>`. The prompt presents file contents as **untrusted data**, never instructions. A hash mismatch, a timeout or a refused file **fails the job with a stated cause** instead of running on altered or missing input.

### Output files

Jobs return:

- **`report.md` and `findings.json`**, which uzi stores from the scrubbed result it received. The worker cannot upload or replace them.
- **The files the job chose to keep**, uploaded by the worker before it reports the result, from its `outputs/` and `sources/` directories.

While a generated report or findings file waits for storage, its `refused_files` entry reads `generation_pending`. Waiting capacity overflow reads `generation_failed`; waiting work stopped by API shutdown reads `generation_shutdown`.

Output types are the input allowlist plus HTML (a downloaded page). An output that is over a per-file or per-job cap, over a storage quota, or not an allowed type is **refused and listed, and the job still completes**: a refused file never fails a finished job and is never silently missing. Quotas are checked when a file is admitted, never by failing the job afterwards.

Worker-produced files must be regular files under `outputs/` or `sources/`, with no symlink component or extra hard link. An unsafe path is refused as `worker_unreadable`.

### Read the files

| Endpoint | Scope | Returns |
|---|---|---|
| `GET /api/v1/jobs/{id}/files` | `jobs:read` | `{"files": [...], "refused_files": [...]}` for that job |
| `GET /api/v1/files/{id}` | `jobs:read` | the file's bytes |

The result (`GET /api/v1/jobs/{id}/result`) carries the same `files` and `refused_files`, plus `sources`. Each file has `id`, `display_name`, `content_type` (detected from the bytes), `byte_size`, `sha256`, `direction` (`input` or `output`), `state` (`attached` while the job runs, then `available`, then `expired`), `expires_at` (null while attached) and `source_url`. Each refused file has `display_name`, `byte_size` and a `reason`. `display_name` and `source_url` are untrusted text; render them inert.

**A download is never rendered.** Every `GET /api/v1/files/{id}` answers with `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and `Content-Type: application/octet-stream`, whatever the file's type, so a hostile file cannot run in your origin or uzi's. The `filename` in the header is the content-derived storage name, not the uploader's. Read the real type from `content_type` in the metadata.

**Visibility follows the job.** A file that belongs to a job is visible exactly as the job is (your own jobs; a product token sees only its product's). A file not yet attached to any job is visible only to the product that uploaded it. Anything else is 404, the same as an unknown id. An expired file is 410 `file_expired`.

### Sources and `source_url`

`sources` in the result is the job's **source log**: every fetch the fetch service recorded for that run, allowed or refused, oldest first (at most the first 500), each with the URL, final URL after redirects, verdict, reason, HTTP status, content type, size, SHA-256 and time. The fetch service writes it, not the worker, so the job cannot forge it.

An output file carries `source_url` **only when its SHA-256 equals the SHA-256 of an allowed fetch recorded in the same run**; the URL is taken from that record (the final URL when one was recorded). Any origin the job claims for a file is ignored, and a file never matches a fetch from another job. Otherwise `source_url` is null, always for an input.

A job that names a site list runs on the no-internet worker lane that uses the fetch service ([Isolated research lane](./isolated-research-lane.md)), so its `sources` and `source_url` are filled. A job without a site list never fetches, so its `sources` is empty and no output has a `source_url`.

### Limits

Defaults; an operator can change each one ([Configuration](./configuration.md#job-files-and-product-skill-sets-prd-1909)).

| Limit | Default |
|---|---|
| One input file | 25 MiB |
| Input files per job | 10 files, 50 MiB in total |
| One output file | 25 MiB |
| Output files per job | 50 files, 100 MiB in total |
| Retained job-file bytes per owner | 256 MiB |
| Retained job-file bytes, whole instance | 1 GiB |
| Job files plus recovery archives, combined | 4 GiB (see below) |
| Retention after the job ends | 7 days |
| Uploaded but never attached | 1 hour |

The input limits are also clamped to fixed worker ceilings (256 MiB per file, 64 files, 1 GiB per job), so raising one past them has no effect.

**Sizing rule.** Job files live in Postgres next to everything else, and they share one **stored-file budget** with [recovery archives](./run-recovery.md#shared-stored-file-budget): job-file bytes plus recovery-archive bytes must stay within `UZI_STORED_FILES_BUDGET_BYTES` (4 GiB by default, the recovery-archive quota the database volume was already sized for: 8Gi in the chart's default `database.mode: simple` install, 5Gi for the CNPG cluster, which is off by default). The budget is fixed, not scaled to the volume, so if you raise a quota or the budget, raise the database volume to hold the budget plus your ordinary data.

### Retention and expiry

Files of a running job stay attached. When the job ends, each file becomes `available` and expires `UZI_JOB_FILES_RETENTION` later (7 days by default). When a file expires its bytes are deleted and its row stays as an honest record, so a late download is a clear 410 rather than a 404. If storage runs short, recovery admission can expire `available` job files early (see the recovery page), but never one attached to a running job.

### Upload and download refusals

| Status | `reason` | Meaning |
|---|---|---|
| 400 | `invalid_request` | The body ended or broke before the file was read |
| 408 | `request_timeout` | The body was not received within the upload deadline; a stalled upload is cut off |
| 413 | `file_too_large` | The declared size is over the per-file cap |
| 413 | `payload_too_large` | The request body is over the per-file cap plus multipart overhead |
| 413 | `too_many_files`, `job_bytes_exceeded` | On job create: more `input_file_ids` than the per-job file cap, or attached files over the per-job byte cap |
| 415 | `unsupported_file_type` | The bytes are not an allowed type, or disagree with the declared type or extension |
| 422 | `invalid_request` | Missing or malformed `X-Uzi-File-Size` or `X-Uzi-File-Sha256`, not multipart, or no `file` part |
| 422 | `empty_file`, `size_mismatch`, `sha256_mismatch` | A zero-byte file, or the streamed size or digest differs from the declared one |
| 422 | `file_unavailable` | On job create: an `input_file_ids` entry cannot be attached |
| 503 | `uploads_busy` | Too many uploads are streaming; retry after the `Retry-After` seconds |
| 503 | `files_unavailable` | This deployment has no job-file store |
| 507 | `storage_quota_exceeded` | Your quota, the instance quota or the shared budget is full; retry after files expire |
| 410 | `file_expired` | On download: the file's retention ended |

## Limits and refusals

- **Active jobs:** at most **10** non-terminal jobs per user (429 `over_cap`). An admin can change it with the `job_max_active_per_user` setting.
- **Request rate:** `/api/v1` has its own per-user budget, 120 per minute by default. `POST /api/v1/jobs` also has its own separate per-user counter, sized by the same `RATE_LIMIT_MAX` and window as the sign-in limiter (10 per minute by default); it does not share a count with sign-in attempts. A 429 without a `reason` carries `Retry-After`. See [Configuration](./configuration.md).
- **Body size:** a create body over 4 MiB is a 413; the prompt is capped at 256 KiB.

A store failure while checking the token answers 503 `auth_unavailable` with no `Retry-After` header; before [#1992](https://github.com/vtmocanu/uzi/issues/1992) it answered 401, so a 401 now always means the credential itself was refused. Every `/api/v1` operation can return it.

| Status | `reason` | Meaning |
|---|---|---|
| 401 | none | Missing, revoked, expired or unknown token |
| 403 | `insufficient_scope` | The token lacks `jobs:run` or `jobs:read` |
| 403 | `job_type_not_allowed` | The product does not allow this job type |
| 404 | `not_found` | No such job for this caller |
| 409 | `job_terminal` | Cancel of a finished job |
| 409 | `provider_source_unusable` | Your Claude source is a model provider that cannot serve the job: its key is missing or disabled, the provider is retired or gone, or its base URL is no longer on the operator allowlist. No job is created and it never falls back to your Anthropic token |
| 409 | `provider_model_unmapped` | Your Claude source is a model provider whose model map does not map the job's model (see [Jobs on a model provider](#jobs-on-a-model-provider)) |
| 409 | `provider_lane_unsupported` | The job names a site list in `egress_profile` while your Claude source is a model provider; site-list jobs do not support a provider yet. A Codex source does not affect jobs |
| 413 | `payload_too_large` | The create body is over 4 MiB |
| 403 | `egress_profile_not_allowed` | A product token named a site list its product is not allowed to use |
| 404 | `unknown_egress_profile` | `egress_profile` names no existing site list |
| 422 | `invalid_request`, `unknown_job_type`, `no_model_credential` | Bad request (an explicit empty `egress_profile` included), or you have no usable Anthropic credential |
| 429 | `over_cap` | Active-job cap reached |
| 503 | `auth_unavailable` | Authentication temporarily unavailable (the token could not be checked); retry with backoff |
| 503 | `isolated_lane_unavailable` | A non-empty `egress_profile` was supplied while this instance has no isolated research lane enabled; checked after body decoding and empty-profile validation (422), before the site list's allowance (403) and existence (404) and the remaining job validation |

## Site lists

A job can read official web sources from an admin-approved [site list](./egress-profiles.md) (an egress profile). Name one in `egress_profile` on create:

```json
{"type": "research", "prompt": "Summarize the datasheet at the vendor site.",
 "egress_profile": "vendor-x-docs"}
```

- **Who may name what.** Your own `uzc_` token may name any existing list. A product (`uzp_`) token may name only a list an admin has allowed for its product ([Product tokens](./product-tokens.md#site-lists-for-jobs)); any other existing list is 403 `egress_profile_not_allowed`, an unknown name is 404 `unknown_egress_profile`, and an explicit empty string is 422 `invalid_request`. The check runs when the job is created.
- **Removing an allowance affects only jobs created afterwards.** A job already created keeps its list.
- **Where it runs.** A job with a site list is placed on the [isolated research lane](./isolated-research-lane.md): a worker with no internet that reads the web only through the fetch service, limited to the list's hosts, with every attempt in the result's `sources`. Inputs, uploaded files, output files and the result work as for any job.
- **The fetch credential is redacted** from messages, failure reasons, logs and the posted result. **Output-file contents are not covered by that redaction**: the job writes them, so treat them as untrusted and do not assume an echoed credential can never reach one.
- **Once a lane worker is provisioned for a bound job, a worker that cannot serve it fails the job rather than leaving it waiting.** If that lane worker never registers, the job fails (`ephemeral_worker_never_registered`); a lane worker that registers without the `isolated_job_v1` capability fails it too (`no_job_capable_worker`). If no lane worker is provisioned at all (ephemeral worker provisioning is turned off on the instance, or you are at your ephemeral worker limit), the bound job stays `queued`, like a bound research run, and counts toward your active jobs; cancel it if it should not wait. If the instance has no isolated research lane enabled, naming a site list is refused at create with 503 `isolated_lane_unavailable` and no job is created (this check comes first when `egress_profile` names a list, ahead of the allowance 403 and existence 404, so on such an instance a disallowed or unknown list gets 503; it runs after the empty-string 422); a job bound before the lane was turned off with no lane worker provisioned for it stays `queued`, and its queued reason says an admin must enable the lane, while one whose lane worker was already provisioned is not recalled (that worker still fails it at the deadline if it never registers, and a registered lane worker may still run it). A job without a site list is unaffected. See [Isolated research lane](./isolated-research-lane.md#jobs-on-the-lane).

## Jobs on a model provider

When your Claude source is a [custom model provider](./providers.md#use-a-provider-as-your-claude-source) and the job names no site list, the job runs on that provider instead of your Anthropic token.

- **Frozen at creation.** The provider, its revision and your key are bound to the job when it is created, with no Anthropic credential in the run. A later change of your source does not move a queued job. A job created with a site list is refused (409 `provider_lane_unsupported`); site-list jobs on a provider are not shipped yet.
- **One model, checked up front.** The job uses the run's own model when it names one, else your Claude default model, else `sonnet`; an empty value or `inherit` at any level counts as unset. That model must be in the provider's model map, or the create is refused with 409 `provider_model_unmapped`. If you change your default to an unmapped model while the job is queued, the job fails at claim.
- **A job never parks.** If the key, its revision or the base URL allowlist entry is revoked before the job starts, or the gateway answers 429 or overloaded, the job fails (`failed`, with a fixed reason); it does not wait or fall back to a vendor credential. A model-settings read error at claim is retried; one that is not transient fails the job ("the job's model settings could not be read").
- **Worker requirement.** Only a non-Docker worker advertising `claude_provider_job_v1` (and `claude_provider_v1`) claims a provider job, never one on the isolated lane. Until one is online the job stays `queued` and its health reads "no worker that supports provider jobs (claude_provider_job_v1) is online". A provider job bound to an ephemeral worker lacking those capabilities fails as `no_job_capable_worker`.
- **Cost reads as unknown.** The model names are the gateway's, so the vendor price table does not apply; token counts are still recorded.
- **The key is scrubbed from what the job publishes.** The bound key's forms are removed from messages, result fields and diagnostics, and a result `status` that contains a key form is rejected. An output file whose path contains a key form, or whose file name contains an upper- or mixed-case spelling of an all-lowercase key, is withheld. Its whole path is redacted first and the display name is then derived and capped, so it is reported under that scrubbed name, or as `withheld-output-<n>` when the scrubbed path still carries a case variant of the key. Every output file is also copied to a private capture and scanned for the key's bytes; a hit withholds that file as a `worker_unreadable` refusal while the job still completes with its other files. The scanner matches these forms of the key: raw, JSON-escaped, JSON with `\/`, `\u` escapes of `<`, `>` and `&` (`\u003c` and so on, both hex cases) and of U+2028 and U+2029 (`\u2028`, `\u2029`), and percent-encoded (both hex cases, and `+` for a space). Any other encoding or transformation, such as base64, is not detected. This protects against accidental publication only. The job's agent can read its own environment, so it is not a defence against a malicious or prompt-injected job, which can see the key ([ADR-4320 D5](../adr/4320-custom-model-providers.md)).
- **Inputs go to your gateway.** The prompt, input files and everything the job reads are sent to your provider's gateway, and through any redirect it issues (see the redirect warning in [Model providers](./providers.md)). This holds for a job created with a product (`uzp_`) token too: it routes the product's inputs to the owner's gateway.

## Trust model

- **Inputs and the prompt are untrusted.** The product supplies them, not you. The job runs with only `Read`, `Write`, `Glob`, `Grep` and the result-submitting tool (plus `Skill` when its product has approved skills), confined to a per-run workspace on the worker; it has no shell, network or forge access. A job with a site list runs on the isolated lane with `Read`, `Write`, `Edit`, `Glob`, `Grep`, a fetch tool limited to that list and the result-submitting tool (plus `Skill` when its product has approved skills); it still has no shell or forge access. Worker routes for publishing, memory, forge reads, merge-request threads and reviews refuse job runs (403 `not_for_job`).
- **`requested_by_label` is reported by the product.** It is shown as text the product supplied, never as a verified identity. Treat the report and findings as untrusted text too: render them inert.
- **Revoking cuts jobs off.** Revoking a product token, disabling or deleting the product, or deactivating you cancels that product's non-terminal jobs. Token expiry alone does not. Jobs made with a `uzc_` token are cancelled only when you are deactivated.

## Worker requirement and rollout

Only a non-Docker worker that advertises the `job_runner_v1` capability claims jobs, and a job created since job files shipped also needs the `job_files_v1` capability. If you opted in to ephemeral workers, an ephemeral worker is provisioned for a queued job when needed. Without that opt-in the job waits `queued` for a capable worker of yours and counts toward the active-job cap. If a job's bound ephemeral worker registers without `job_runner_v1` (or, for a job needing files, `job_files_v1`), or never registers, the job fails (`no_job_capable_worker` or `ephemeral_worker_never_registered`) rather than waiting. **Deploy the api and the chart's worker image tag together:** an older worker image never claims jobs, so a job queued against a fleet that has not rolled will sit `queued` or fail as above. Each new job is stamped as needing `job_files_v1`, and the claim refuses it for any worker that does not advertise it, so an api rolled ahead of the workers never hands a job that needs files to a worker that cannot deliver them; it waits `queued` (health reason: `no online worker supports jobs (job_runner_v1, job_files_v1); update or provision a non-Docker worker`) until a current worker is online. A job created before the upgrade carries no such stamp and any `job_runner_v1` worker may still claim it. A provider-bound job (see [Jobs on a model provider](#jobs-on-a-model-provider)) needs a non-Docker worker advertising `claude_provider_job_v1` as well as `claude_provider_v1`; roll the api and the worker image together, or such a job waits `queued`. A job with a site list needs an isolated-lane worker that advertises `isolated_job_v1`; see [Isolated research lane](./isolated-research-lane.md#jobs-on-the-lane).

Design rationale: the job run kind ADR, adr/1908-job-run-kind.md in the repo, and the job-file storage budget ADR, adr/1909-stored-file-budget.md.
