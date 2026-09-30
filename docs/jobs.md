---
title: Jobs
order: 114
audience: user
---

# Jobs

A **job** is a run with no repository: a prompt plus optional named text inputs, worked by one of your workers, that ends in a structured report and findings. An external product starts one through `/api/v1/jobs`; you can also start one with [`uzi job`](./cli.md#uzi-job-repo-less-jobs). The only job type today is `research`.

A job never touches a forge. It has no repo, issue or branch, opens no merge request, and appears in run lists as kind `job`. It runs on your worker with your Anthropic credential, and its status, messages and result show on the run page.

## Start a job

1. An admin registers the product and ticks the job types it may start (**Admin → Products**). A product with none ticked cannot create jobs.
2. You mint a product token with the `jobs:run` scope (and `jobs:read` to read back). See [Product tokens](./product-tokens.md).
3. The product calls `POST /api/v1/jobs` with `Authorization: Bearer uzp_…`:

```
{"type": "research", "prompt": "Summarize the attached notes.",
 "inputs": [{"name": "notes.md", "content": "..."}],
 "requested_by_label": "Acme dashboard"}
```

`title` is optional (derived from the first line of the prompt). Inputs are inline text: at most 20, 1 MiB in total, names of 1 to 100 characters (letters, digits, `.`, `_`, `-`, starting with a letter or digit, no `..`). `wall_seconds` sets the wall-clock limit; it is clamped to 8 hours. `egress_profile` is refused (`not_supported`): there is no egress control yet.

The response is 201 with the queued job. Poll `GET /api/v1/jobs/{id}`, then read `GET /api/v1/jobs/{id}/result`. The full contract, including every field, is `api/openapi/v1.yaml`.

Your own `uzc_` CLI token works on the same endpoints and may create any known job type.

## Endpoints and scopes

| Endpoint | Scope |
|---|---|
| `POST /api/v1/jobs` | `jobs:run` |
| `POST /api/v1/jobs/{id}/cancel` | `jobs:run` |
| `GET /api/v1/jobs`, `/{id}`, `/{id}/result`, `/{id}/messages?after=<seq>` | `jobs:read` |

A product token sees only jobs created for its own product. A `uzc_` token sees every job you own. Anyone else's job, another product's job and a malformed id all return the same 404.

## Statuses

`queued`, `running`, `waiting`, `completed`, `failed`, `cancelled`. The last three are final. `waiting` is only a defensive mapping for the internal park states; a job never parks, so you should not see it in practice. A job **never parks**: hitting a usage limit, the wall-clock limit, a recovery situation or a disabled credential fails the job instead of leaving it waiting, so read `failure_reason` on a `failed` job.

## Cancel

`POST /api/v1/jobs/{id}/cancel` (or `uzi job cancel`) cancels a queued, running or waiting job. A queued job ends at once; a running job is stopped by its worker, so the response may still read `running` for a moment. A finished job returns 409 `job_terminal`.

## Results and findings

`GET /api/v1/jobs/{id}/result` returns `result: null` until the job reports. The result has a short `status` token, a markdown `report_md`, and `findings`, each with `severity` (`info`, `warning`, `error`), `message_md`, and an optional `url` or `file` and `line`. A job that ends without reporting has no result.

## Limits and refusals

- **Active jobs:** at most **10** non-terminal jobs per user (429 `over_cap`). An admin can change it with the `job_max_active_per_user` setting.
- **Request rate:** `/api/v1` has its own per-user budget, 120 per minute by default. `POST /api/v1/jobs` also has its own separate per-user counter, sized by the same `RATE_LIMIT_MAX` and window as the sign-in limiter (10 per minute by default); it does not share a count with sign-in attempts. A 429 without a `reason` carries `Retry-After`. See [Configuration](./configuration.md).
- **Body size:** a create body over 4 MiB is a 413; the prompt is capped at 256 KiB.

| Status | `reason` | Meaning |
|---|---|---|
| 401 | none | Missing, revoked, expired or unknown token |
| 403 | `insufficient_scope` | The token lacks `jobs:run` or `jobs:read` |
| 403 | `job_type_not_allowed` | The product does not allow this job type |
| 404 | `not_found` | No such job for this caller |
| 409 | `job_terminal` | Cancel of a finished job |
| 413 | `payload_too_large` | The create body is over 4 MiB |
| 422 | `invalid_request`, `unknown_job_type`, `not_supported`, `no_model_credential` | Bad request, or you have no usable Anthropic credential |
| 429 | `over_cap` | Active-job cap reached |

## Trust model

- **Inputs and the prompt are untrusted.** The product supplies them, not you. The job runs with only `Read`, `Write`, `Glob`, `Grep` and the result-submitting tool, confined to a per-run workspace on the worker; it has no shell, network or forge access. Worker routes for publishing, memory, forge reads, merge-request threads and reviews refuse job runs (403 `not_for_job`).
- **`requested_by_label` is reported by the product.** It is shown as text the product supplied, never as a verified identity. Treat the report and findings as untrusted text too: render them inert.
- **Revoking cuts jobs off.** Revoking a product token, disabling or deleting the product, or deactivating you cancels that product's non-terminal jobs. Token expiry alone does not. Jobs made with a `uzc_` token are cancelled only when you are deactivated.

## Worker requirement and rollout

Only a non-Docker worker that advertises the `job_runner_v1` capability claims jobs, and, if you opted in to ephemeral workers, an ephemeral worker is provisioned for a queued job when needed. Without that opt-in the job waits `queued` for a capable worker of yours and counts toward the active-job cap. If a job's bound ephemeral worker registers without `job_runner_v1`, or never registers, the job fails (`no_job_capable_worker` or `ephemeral_worker_never_registered`) rather than waiting. **Deploy the api and the chart's worker image tag together:** an older worker image never claims jobs, so a job queued against a fleet that has not rolled will sit `queued` or fail as above.

Design rationale: the job run kind ADR, adr/1908-job-run-kind.md in the repo.
