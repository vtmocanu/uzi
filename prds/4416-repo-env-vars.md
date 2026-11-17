# PRD #4416: Per-repo environment variables for runs

**Status**: Draft
**Priority**: Normal

## Problem

A run executes repo code on the worker: builds, unit tests, end-to-end tests. Some of that code needs credentials the repo's own CI pipeline gets from CI variables, for example to pull images from a private container registry or packages from a private artifact repository. uzi has no way to give a run such values. Stored secrets are limited to three kinds (`anthropic_token`, `openai_api_key`, `codex_auth`, `api/internal/store/migrations/00198_codex_secret_kinds.sql`), the Claude and Codex command environments are built from scratch (`agent/src/sdk-env.ts`, `buildCommandEnv` in `agent/src/codex/codex-executor.ts`), and no registry credential exists anywhere. A run whose tests need a private pull fails with an authorization error and cannot recover.

## Outcome

A user defines named variables on one of their repos. Every run on that repo that executes shell commands gets them as environment variables, the way a CI job gets CI variables.

Acceptance examples:

1. A user opens a repo's settings, adds `REGISTRY_USER` and `REGISTRY_PASSWORD` with a description each, and saves. The list shows the two names, descriptions and update times. No value is ever shown again, in the web UI, the CLI or any API response.
2. A new run on that repo runs `echo "$REGISTRY_PASSWORD" | docker login <registry> -u "$REGISTRY_USER" --password-stdin` and then pulls a private image. Both succeed on the Claude harness and on the Codex harness.
3. The lead's prompt names the available variables and their descriptions, never their values. The run page lists the names that were injected.
4. A command prints a variable's value. The run message, the worker log and any memory, review reply or finding the agent writes show the redaction marker instead.
5. The agent writes a value into a tracked file. The next checkpoint or the final push is held by the existing secret-remediation hold and nothing containing the value reaches the forge.
6. A user tries to save `ANTHROPIC_BASE_URL`, `NODE_OPTIONS`, `GITHUB_TOKEN` or `PATH`. The save is refused with a field error naming the rule.
7. A user saves a 5-character value. The save is accepted with a visible warning that values under 8 characters cannot be masked.
8. The user's vault is locked. Their runs on that repo wait in `queued` exactly as they do today and are claimed after unlock; none fails.
9. `uzi repo env list <repo>` prints names, descriptions and update times. The CLI has no command that writes a variable.
10. A worker that does not implement the feature never claims a run whose repo has variables. Run health names the reason while no capable worker is connected.

## Out of scope

- **Hiding values from the agent.** The agent chooses the commands and edits the code those commands run, so it can read any value given to them. This feature is labelled as such in the UI and docs. Keeping a credential out of the run entirely needs a credential-holding proxy (the direction of `prds/50-llm-egress-proxy.md` and the fetcher of `adr/1906-isolated-fetch-lane.md`); that is a separate PRD.
- Per-run variables. Planned as a follow-up; the wire field and worker merge here are written so a second source can be added.
- Variables shared between users. Repos, secrets and runs are per user today and stay so.
- CLI or product-API writes. Secret writes are cookie-only today (`api/internal/handler/routes_me.go`) and this keeps that rule.
- A typed registry login written by the worker, and file-type variables. Follow-ups if plain variables prove insufficient.
- Variables in worker-spawned checks and the dependency install (`buildCheckEnv` in `agent/src/sdk-env.ts`), and in lanes without a shell (judge, task review, summary, cross-check, chat, jobs, isolated research).
- Forge credentials as variables (`GH_TOKEN`, `GITLAB_TOKEN` and similar). They would bypass the guardrail that only the worker holds a forge credential.

## Design: what changes and what stays

What changes:

- The agent command environment gains user-supplied secrets. Today the Codex command environment is documented as credential-free and `adr/0319-env-read-diagnostic-allowlist.md` reasons from the SDK environment holding one secret. Both statements are amended by an ADR in M2.
- A second vault-sealed table exists beside `user_secrets`.

What stays:

- No reveal endpoint, no admin read path to another user's values.
- Worker-set environment keys always win. A user variable can never replace or remove one.
- A locked vault delays work and never fails it.
- Guardrails are unchanged. `env`, `printenv` and `/proc` reads stay denied; this feature does not rely on them as a boundary.

Residuals, accepted and to be recorded in the ADR:

- The agent can read the values and docker-tier egress is open (`adr/0285-worker-egress-tier-trust-model.md`). Users should store read-only, narrowly scoped credentials.
- Concurrent runs on one worker share a runner uid and one DinD daemon (`adr/0091-runner-cross-run-persistence-residual.md`, `docs/proc-hardening.md`). A run on one repo can reach another repo's variables and pulled images. A worker serves one user, so nothing crosses users. The Anthropic token has the same exposure today.
- Redaction is exact-match with an 8-character floor (`agent/src/redact.ts`) and does not survive arbitrary encodings. It is a net, not a boundary.

## Modules and seams

### Store and vault (`api`)

- New table `repo_env_vars` (additive goose migration): `id`, `user_id` and `repo_id` (both `ON DELETE CASCADE`), `name`, `description`, `ciphertext`, `sealed_with`, `master_aad`, `rev`, `created_at`, `updated_at`; unique on `(user_id, repo_id, name)`.
- Values are sealed through the existing vault seam with a new AAD: a domain tag, the user id, the repo id and the length-prefixed name. The existing `user || 0x00 || kind` AAD is not reused, so a ciphertext cannot be moved between names or repos.
- The table joins every explicit vault enumeration, or its rows become undecryptable: the material totals, the lock and reseal batches used when the vault is enabled or disabled (`api/internal/store/queries/vault_material.sql`, `api/internal/vault/lifecycle.go`), the budgets of `adr/3507-optional-credential-vault.md`, and the account-reset delete (`api/internal/store/queries/account_reset_links.sql`). Writes take the same users-row fence as other secret writes.
- Limits: name `^[A-Z_][A-Z0-9_]{0,127}$`; value at most 64 KiB; at most 50 variables and 256 KiB per repo.
- New table `repo_env_var_events`: actor, action (`set`, `delete`), repo, name, time. Never a value. Actor is `SET NULL` on user delete.

### Name rules (shared fixture)

- One JSON fixture under `fixtures/` lists denied names and prefixes and is pinned by tests on the api and the worker, like `fixtures/claims/claim-response-limit.json`.
- Denied: every key the worker sets (the `PROTECTED_ENV_KEYS` set, `HOME`, `PATH`, `TMPDIR`, `LANG`, `DOCKER_HOST`, `DOCKER_CONFIG`, `DOCKER_CONTEXT`, the Go keys the worker manages, browser discovery keys, run-attempt markers); provider and product prefixes (`UZI_`, `CLAUDE_`, `ANTHROPIC_`, `CODEX_`, `OPENAI_`, `MCP_`); forge credentials (`GH_`, `GITHUB_`, `GITLAB_`, `GLAB_`); loader, shell and trust hooks (`LD_`, `DYLD_`, `NODE_OPTIONS`, `NODE_PATH`, `NODE_EXTRA_CA_CERTS`, `NODE_TLS_REJECT_UNAUTHORIZED`, `BASH_`, `ENV`, `SHELL`, `SHELLOPTS`, `BASHOPTS`, `PROMPT_COMMAND`, `PS4`, `IFS`, `GIT_`, `SSH_`, `XDG_`, `CURL_HOME`, `NETRC`, `WGETRC`, `SSL_CERT_`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `JDK_JAVA_OPTIONS`, `PYTHON`, `RUBYOPT`, `PERL5`, `GOFLAGS`, `GOPROXY`, `GOSUMDB`, `GOENV`, `GOTOOLCHAIN`, `NIX_`, `DEVBOX_`); and any name ending in `_PROXY`.
- Tool configuration that a private artifact repository legitimately needs stays allowed: `MAVEN_OPTS`, `GRADLE_`, `PIP_`, `NPM_CONFIG_`, `COMPOSE_`, `BUILDKIT_`. The ADR records that choice.

### API (`api/internal/handler`)

- `GET /api/repos/{id}/env`: metadata list. Accepts the session cookie and the CLI token, like the secrets list.
- `PUT /api/repos/{id}/env/{name}` and `DELETE /api/repos/{id}/env/{name}`: cookie and CSRF only, owner only, behind the same repo preflight as the tool profile (`api/internal/handler/tool_profiles.go`). A locked vault answers 409 `vault_locked`, as secret saves do.

### Claim (`api/internal/workersvc`)

- `ClaimSecrets` gains `run_env` (`omitempty`): a list of name and value. It is filled in `assembleClaim` by its own query, for run-lane claims only, in the transaction that finishes the claim, so the set a worker receives is the set recorded.
- The run records injected names and revs (no values, no hashes).
- An open failure caused by a locked vault maps to the existing requeue path, never to a failed run.
- New worker protocol capability `run_env_v1`. `ClaimRun` requires it when the run's repo has variables, using the existing `EXISTS` gate pattern in `api/internal/store/queries/runtime.sql`. The same condition is mirrored in fleet-aware placement and in run health, so an unplaceable run says why.
- The claim response sets `Cache-Control: no-store`.

### Worker (`agent`)

- User variables are merged at the lowest precedence in `buildSdkEnv` and `buildCommandEnv`: a key already present is kept, then worker-set keys are applied as today. The worker re-checks every name against the fixture and drops a violation with a run event.
- One helper registers each value and its base64 form with every redactor the run owns: the run-message set and logger set in `agent/src/runner.ts` and the Codex stream scrub. Tool inputs that leave the worker without passing those sets today are redacted too: saved memories, review-thread replies, findings and CI evidence notes.
- On a re-claim after a rotation, the previous value stays registered for redaction for the life of the run's working directory.
- Exact-value scan: before a checkpoint publish, a salvage-ref publish and the final push, the outgoing range is searched for each value and its base64 form. A hit uses the existing secret-remediation hold. gitleaks stays as it is; it cannot know these values.
- The environment-facts block of `agent/src/prompt.ts` (Claude and Codex call sites) lists names and descriptions, fenced and sanitised like other user-supplied text.
- A test pins that no outbox, recovery journal or hibernation record serialises `run_env`.

### Web and CLI

- `web/src/pages/Repos.tsx`: an "Environment variables" panel beside the tool profile. Write-only value field, description, update time, delete with confirmation, the short-value warning, and a fixed note that the agent can read these values. Mock in `web/src/mocks/mockApi/forge.ts`.
- Run page: injected names.
- `uzi repo env list <repo>` in `api/cmd/uzi/repo.go` with the client in `api/internal/uzicli`.
- User doc page under `docs/` covering the rules, the agent-readable note and the scoped-credential advice.

## Testing decisions

- Store: uniqueness, limits, cascade on repo and user delete, events never carrying values.
- Vault: enable, disable and account reset with variables present; every row opens afterwards or is gone. A swapped ciphertext between two names fails to open.
- Name rules: the shared fixture drives table tests on both sides; a name allowed by one side and denied by the other fails the build.
- Claim: gate on and off, an old worker never claims a gated run, locked vault requeues, names and revs recorded, `no-store` header present.
- Worker: precedence (a user `PATH` smuggled past the api is dropped), redaction through each sink listed above, exact-value scan holding a checkpoint, a salvage publish and a final push.
- E2E: a compose run with a variable set reads it in a command on both harnesses, using a dummy value and a local registry stub; no real credential.
- Fixtures that look like credentials are assembled at runtime and `task scan:secrets` runs in every milestone gate.

## Milestones

### M1: Variables can be stored and managed

Migration, vault integration (seal, enumerations, reset), name rules fixture, API, web panel, CLI list, events, user doc. Nothing is injected yet and the panel says so. Gate: `task gate:api`, `task gate:web`, `task scan:secrets`.

### M2: Runs receive the variables

Claim field and capability gate with its placement and health mirrors, worker merge on both harnesses, redaction helper and tool-input redaction, exact-value scan on the three publish paths, prompt block, injected names on the run page, the ADR, E2E. Gate: `task gate:api`, `task gate:agent`, `task gate:web`, `task scan:secrets`, compose E2E, and a hosted k8s run.

## Landing obligations

- No file under `.github/workflows/` is touched by implementation or validation.
- Migration numbers are assigned at merge time.
- A changelog fragment per PR.
- The ADR for the amended invariants and accepted residuals lands with M2.

## Decision Log

- **Plain variables, agent-readable.** Reviewed alternatives: command-only injection, a worker-run fixed command, running the tests in the forge's CI, and short-lived tokens. None hides a value from an agent that authors the commands and the code. Only a credential-holding proxy does, at a much larger cost, and only for pull credentials. Chosen: ship plain variables with an honest label; treat the proxy as its own PRD.
- **Per user per repo.** Matches repos, the tool profile and the vault. Sharing would need an authorization model uzi does not have.
- **Web-only writes.** Keeps the existing cookie-only rule for secret writes.
- **Lowest-precedence merge plus a denylist.** The existing provisioning fold is a three-key allowlist and cannot carry user names; precedence protects every worker-set key structurally, the denylist covers names the worker never sets.
- **Hard claim gate.** A soft gate would let an old worker run the job without the variables and fail in a confusing way.
- **Cross-repo exposure on a shared worker accepted.** Same exposure as the Anthropic token today; recorded as a residual rather than requiring uid-split or ephemeral workers.
