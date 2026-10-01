---
title: Product tokens
order: 96
audience: user
---

# Product tokens

A **product token** (`uzp_…`) lets an external product call uzi **as you**. You mint it in Settings for a product an admin has registered, and paste it into that product. Work the product starts, [jobs](./jobs.md), runs on your own worker and model credential.

## What it can and cannot reach

- **Only `/api/v1`**, the stable external API. Everywhere else, including every `/api/*` route the web app and the `uzi` CLI use, `/api/ws` and every admin route, a product token is treated exactly like an unknown token. This is structural: product tokens live in their own table that the internal routes never read.
- **`GET /api/v1/whoami`**, which returns your user (id, display name), the product, and the token's scopes, **the jobs endpoints** (`/api/v1/jobs`), and **the file endpoints** (`POST /api/v1/files`, `GET /api/v1/jobs/{id}/files`, `GET /api/v1/files/{id}`), gated by the scopes below. See [Jobs](./jobs.md) and [Job files](./jobs.md#job-files).
- **Never admin.** `/api/v1` drops admin authority for every caller, so a token an admin minted still acts as a plain user.
- **`/api/v1` also accepts your own `uzc_` CLI token** (user scope), for scripts. `uza_` tokens and browser cookies are refused there.

The `uzi` CLI cannot use a product token: it needs a CLI token (`uzc_` or `uza_`).

## Mint a token

1. Open **Settings → Access → Product tokens**.
2. Pick a product, name the token, choose scopes and an expiry, then **Create product token**.
3. Copy the token now. It is shown once and only its hash is stored.

Minting is a browser action only (no CLI or Bearer mint), so a stolen token cannot mint replacements.

**Scopes:** `jobs:run` (start and cancel jobs) and `jobs:read` (read status and results). Pick at least one; a token can never do more than its scopes. A token with `jobs:run` can still only start the **job types its product allows** (below), and sees only its own product's jobs.

**Files follow the same scopes.** `jobs:run` uploads input files, `jobs:read` lists and downloads a job's files. An upload made with a product token is scoped to that product: only a token of the same product can attach it to a job, and until it is attached only that product can see it. After that a file is visible exactly as its job is, so a product token reads only the files of its own product's jobs. Another product's file, or a file of your own `uzc_` jobs, is a 404. Files expire (7 days after the job ends by default), so download what you need.

**Expiry:** 30 days, 90 days (default), 1 year, or never. The server sets the timestamp. Choose "never" only for unattended products you trust, and watch its last-used entry.

**Cap:** at most **10 active** tokens (not revoked, not expired) per product for you. An eleventh mint is refused with a 409; revoke one first.

## Revoke a token

A token stops working on its **next request** when any of these happens:

| Action | Who |
|---|---|
| Revoke one token in **Settings → Access** | you |
| **Revoke all** in **Settings → Access** (revokes your CLI tokens **and** product tokens in one step) | you |
| Revoke one product token on **Admin → Products** | an admin |
| The product is disabled or deleted | an admin |
| Your account is deactivated | an admin |
| The token expires | automatic |

**Revoking cuts off jobs.** Revoking a product token, disabling or deleting its product, or deactivating your account also cancels that product's non-terminal jobs. A token that merely expires does not cancel them. See [Jobs](./jobs.md#trust-model).

**A password change and logging out do NOT revoke product tokens**, exactly as for CLI tokens. If a token may have leaked, revoke it or use Revoke all.

Each row shows the token prefix, product, scopes, expiry, **last used** and **last IP** (written at most once a minute). There is no per-request audit log, so treat an unfamiliar last IP as a reason to revoke. Lists show active tokens first and say when they were cut off (200 rows for you, 1000 for admins).

## Register products (admins)

Product registration is an admin-only browser action under **Admin → Products**.

- **Register:** a name (unique among live products, case-insensitive, 200 bytes) and an optional one-line description (1000 bytes). Both are shown to every user, so control characters and invisible formatting are rejected.
- **Disable / enable:** disabling refuses every token of the product on its next request; enabling restores them.
- **Delete:** soft. All its tokens stop working, but their rows, last-used data and revoked state stay listed for the audit trail. A deleted product cannot be edited or re-enabled, and its name can be registered again. The confirm says how many tokens the delete stops (0 if it was already disabled).
- **Allowed job types:** each product card has a checkbox per job type (today `research`). A product with none ticked cannot create jobs: a create is refused 403 `job_type_not_allowed`. `uzi admin products` shows them in a `JOB_TYPES` column.
- **Revoke one token:** each product card lists its tokens with owner, prefix, last used and IP; an admin can revoke a single compromised one. This does not change the rule that admins cannot revoke a user's personal CLI tokens.
- **Allowed site lists:** each product card also lists the site lists its tokens may name on job create; see [Site lists for jobs](#site-lists-for-jobs).
- **Read-only from the CLI:** `uzi admin products` (needs a `uza_` token). See [CLI](./cli.md#managing-tokens).

## Site lists for jobs

A job can name a [site list](./egress-profiles.md) in `egress_profile` so it can read official web sources ([Jobs](./jobs.md#site-lists)). A product token may name **only a list an admin has allowed for its product**: any other existing list is refused with 403 `egress_profile_not_allowed`, an unknown name is 404 `unknown_egress_profile`. Your own `uzc_` token may name any list. A product with no allowed lists cannot create a bound job.

Admins allow a list on **Admin → Products**: each product card has a **Site lists** section where allowed lists are added and removed (a browser-session action; the equivalent `PUT`/`DELETE /api/admin/products/{id}/egress-profiles/{name}` refuse a Bearer token). Removing an allowance affects only jobs created afterwards. Deleting a site list removes its allowances. The CLI is read-only: `uzi admin products egress-profiles <product>` (needs a `uza_` token) lists a product's allowed lists.

## Compatibility promise

`/api/v1` is described by the checked-in OpenAPI 3.1 document `api/openapi/v1.yaml`, and a test keeps it identical to the router. Changes are **additive only**: new paths, new optional request fields, new response fields. Removing or renaming a path, field or enum value, or making an optional request field required, is breaking. A breaking change keeps the old shape working for **at least two minor releases and at least 90 days, whichever is later**, announced in the [changelog](./changelog.md), or ships under a new `/api/v2`. The internal `/api/*` routes are not covered: products must not call them.

`/api/v1` shares one per-user request budget (`V1_RATE_LIMIT_MAX` requests per `V1_RATE_LIMIT_WINDOW`, 120 per minute by default); creating a job also has a separate per-user counter sized by `RATE_LIMIT_MAX` and its window (10 per minute). Over either you get a 429 with `Retry-After`.
