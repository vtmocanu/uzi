# PRD #4416: Per-repo secrets and variables for runs

**Status**: Draft
**Priority**: Normal

## Problem

A run executes repo code on the worker: builds, unit tests, integration and end-to-end tests, code generators, migrations. That code reads configuration and secrets from the environment, the way it does in the repo's own CI pipeline, where CI variables supply them: API keys for third-party services and their sandboxes, database and broker URLs, licence keys, tokens for private package indexes, artifact-repository and container-registry credentials, cloud credentials, feature flags, and plain settings such as a region or a base URL.

uzi gives a run none of them. Stored secrets are limited to three kinds (`anthropic_token`, `openai_api_key`, `codex_auth`, `api/internal/store/migrations/00198_codex_secret_kinds.sql`), the Claude and Codex command environments are built from scratch (`agent/src/sdk-env.ts`, `buildCommandEnv` in `agent/src/codex/codex-executor.ts`), and a user has no place to put a value a repo needs.

A run that lacks a value skips the tests that need it, swaps the dependency for a stand-in and tests something CI does not test, or guesses. One real run of a JVM service did all three:

- The build reads its private artifact-repository URL, user and password from the environment. None was set. The agent looked for the URL in the repo, found only an outdated hostname in a code comment, probed it, got a DNS failure and concluded the repository was unreachable. The correct URL existed only as a CI variable.
- The integration and end-to-end suites start containers from a private registry. The registry was reachable, and every private pull was refused for missing credentials.
- The run fell back to public dependencies and reported both suites as blocked, so the change it produced was never tested the way CI tests it.

Two things follow. A missing non-secret value (a URL) blocks a run as surely as a missing password, and the agent will guess when it has none. And nothing short of the values themselves fixes it: the pipeline file tells the agent which names are needed, not their values.

## Outcome

A user defines named variables on one of their repos. Every run on that repo that executes shell commands gets them as environment variables, the way a CI job gets CI variables. Each variable is either a secret or a plain value. uzi does not interpret them: what a variable is for is between the user and the repo's code.

Acceptance examples:

1. A user opens a repo's settings and adds `PAYMENTS_SANDBOX_URL` as a plain value and `PAYMENTS_SANDBOX_KEY` as a secret, each with a description. The list shows every name, description and update time. Plain values are shown and can be read back. Secret values are never shown again, in the web UI, the CLI or any API response.
2. A new run on that repo runs the integration tests. The test code reads both variables from the environment and calls the sandbox, with no step by the agent. This holds on the Claude harness and on the Codex harness, in every command of the run, whichever agent issues it.
3. The lead's prompt lists each variable's name and description, and the value of each plain variable, so the agent knows what is set and does not guess. Secret values never appear in a prompt.
4. Any tool that takes its settings from the environment works unchanged. A package manager installs from a private index with a token variable. A build tool resolves private dependencies from `ARTIFACT_REPO_URL`, `ARTIFACT_REPO_USER` and `ARTIFACT_REPO_PASSWORD`. A database migration runs against `DATABASE_URL`.
5. A tool that logs in once and keeps the result on disk is driven by the agent or a repo script. For a container registry: `echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin` runs once, and later commands in the same run, including an image build with a private base image and a test suite that starts containers through a library reading the Docker client configuration, pull private images without logging in again. This holds on both harnesses. A run resumed on a worker that no longer has the login is told to log in again.
6. A command prints a secret's value. The run message, the worker log and any memory, review reply or finding the agent writes show the redaction marker instead. A plain value such as a hostname is not redacted, so output stays readable.
7. The agent writes a secret's value into a tracked file. The next checkpoint or the final push is held by the existing secret-remediation hold, the hold names the variable, and nothing containing the value reaches the forge. A plain value that already appears in tracked files (a hostname in a build file) holds nothing.
8. A user tries to save `ANTHROPIC_BASE_URL`, `NODE_OPTIONS`, `GITHUB_TOKEN` or `PATH`. The save is refused with a field error naming the rule.
9. A user marks a 5-character value as secret. The save is refused: a secret must be at least 8 characters so it can be masked. The same value saves as a plain value. A plain value that is a URL carrying a user and password is refused.
10. The user's vault is locked. Their runs on that repo wait exactly as they do today and continue after unlock; none fails because of a variable.
11. `uzi repo env list <repo>` prints names, kinds, descriptions, update times and plain values. The CLI has no command that writes a variable.
12. A worker that does not implement the feature never claims a run whose repo has variables. Run health names the reason while no capable worker is connected.
13. The run page lists the names injected into that run. The repo row shows how many variables the repo has.
14. A user edits a variable's description without re-entering its value, replaces a value, and deletes a variable. A run already claimed keeps the set it was given; the next claim of any run gets the current set.

## Out of scope

- **Hiding secret values from the agent.** The agent chooses the commands and edits the code those commands run, so it can read any value given to them. The UI and docs say so. Keeping a credential out of the run entirely needs a credential-holding proxy (the direction of `prds/50-llm-egress-proxy.md` and the fetcher of `adr/1906-isolated-fetch-lane.md`); that is a separate PRD.
- **Typed credentials and tool setup.** uzi delivers variables and knows nothing about what they are for. It does not log in to anything or write a tool's configuration file. The agent does that, guided by the variable descriptions, the repo's pipeline file or repo instructions, or the repo's own scripts do. Typed entries the worker acts on (a registry login, a package-index configuration) are a possible follow-up.
- **Network reachability.** A variable holding a URL does not make its host reachable from the worker.
- **Making container test libraries work against the worker's Docker daemon.** The daemon is a separate sidecar; a library that expects mapped ports on localhost may need its own setting (for example a host override). That is repo configuration, and a variable can carry it.
- Per-run variables. Planned as a follow-up; the wire field and worker merge here are written so a second source can be added.
- Variables shared between users. Repos, secrets and runs are per user today and stay so.
- CLI or product-API writes. Secret writes are cookie-only today (`api/internal/handler/routes_me.go`) and this keeps that rule.
- File-type variables.
- Variables in worker-spawned checks and the dependency install (`buildCheckEnv` in `agent/src/sdk-env.ts`), and in lanes without a shell (judge, task review, summary, cross-check, chat, jobs, isolated research).
- Forge credentials as variables (`GH_TOKEN`, `GITLAB_TOKEN` and similar). They would bypass the guardrail that only the worker holds a forge credential.

## Design: what changes and what stays

What changes:

- The agent command environment carries user-supplied values, some of them secrets. This amends two recorded invariants, by an ADR in M2: the Codex command environment being credential-free, and the SDK environment holding exactly one secret (`adr/0319-env-read-diagnostic-allowlist.md`).
- A second vault-sealed table exists beside `user_secrets`.
- The Docker client configuration directory becomes per run on both harnesses (M3). It is the one tool the worker has to help, because the worker owns `DOCKER_CONFIG` and a user variable cannot set it.

What stays:

- No reveal endpoint for secrets, no admin read path to another user's values.
- Worker-set environment keys always win. A user variable can never replace or remove one.
- A locked vault delays work and never fails it.
- Guardrails are unchanged. `env`, `printenv` and `/proc` reads stay denied; this feature does not rely on them as a boundary.

Residuals, accepted and to be recorded in the ADR:

- The agent can read secret values and docker-tier egress is open (`adr/0285-worker-egress-tier-trust-model.md`). Users should store read-only, narrowly scoped credentials.
- Concurrent runs on one worker share a runner uid and one DinD daemon (`adr/0091-runner-cross-run-persistence-residual.md`, `docs/proc-hardening.md`). A run on one repo can reach another repo's variables and pulled images. A worker serves one user, so nothing crosses users. The Anthropic token has the same exposure today.
- Redaction is exact-match with an 8-character floor (`agent/src/redact.ts`) and does not survive arbitrary encodings. It is a net, not a boundary.
- Login state a tool writes to disk holds the credential for as long as the directory lives. On Claude that is the run home, until the run ends. On Codex the command home is recreated per command, so only the run's Docker configuration directory keeps such state, including while the run is parked.
- Values passed to an image build as build arguments stay in that image's history in the shared DinD daemon. That is how such builds behave everywhere; the user doc says so.

## Modules and seams

### Store and vault (`api`)

- New table `run_env_vars` (additive goose migration): `id`, `user_id` (`ON DELETE CASCADE`), `repo_id` (nullable, `ON DELETE CASCADE`), `name`, `secret` (bool), `description`, `ciphertext`, `sealed_with`, `master_aad`, `rev`, `created_at`, `updated_at`. A row with a repo belongs to that repo; a row without one is the user-level scope that M4 uses. M1 to M3 write and read repo rows only. Two partial unique indexes: `(user_id, repo_id, name)` where `repo_id` is set, and `(user_id, name)` where it is not.
- Every value, plain or secret, is sealed through the existing vault seam, so there is one storage path. The `secret` flag decides what may be read back, redacted and scanned. The AAD binds a domain tag, the user id, a scope tag (repo or user) with the repo id when there is one, the secret flag and the length-prefixed name. A ciphertext cannot be moved between names, repos or scopes, and a secret cannot be relabelled plain without its value being supplied again.
- Edits: a save without a value changes only the description. A save with a value replaces it and bumps `rev`. Secret to plain requires a value in the same save. Plain to secret reseals the stored value and needs an unlocked vault. There is no rename; delete and create.
- The table joins every explicit vault enumeration, or its rows become undecryptable: the material totals, the lock and reseal batches used when the vault is enabled or disabled (`api/internal/store/queries/vault_material.sql`, `api/internal/vault/lifecycle.go`), the budgets of `adr/3507-optional-credential-vault.md`, and the account-reset delete (`api/internal/store/queries/account_reset_links.sql`). Writes take the same users-row fence as other secret writes.
- Limits: name `^[A-Z_][A-Z0-9_]{0,127}$`; value at most 64 KiB; a secret at least 8 characters; a plain value must not be a URL with userinfo; at most 50 variables and 256 KiB per scope.

### Name rules (shared fixture)

- The rules protect the worker's own invariants and guardrails: its credentials, its Docker endpoint, its git authority, the provider configuration and the directories it manages. They do not protect the build from its owner. The user who sets a variable owns the repo and the run, and can already change what the build does by editing it.
- One JSON fixture under `fixtures/` lists denied names and prefixes and is pinned by tests on the api and the worker, like `fixtures/claims/claim-response-limit.json`. It contains at least the union of the two protected-key sets (`PROTECTED_ENV_KEYS` in `agent/src/sdk-env.ts`, `COMMAND_ENV_PROTECTED_KEYS` in `agent/src/codex/codex-executor.ts`) and every key either builder sets after its fold.
- Denied names: `HOME`, `PATH`, `TMPDIR`, `LANG`, `SHELL`, `ENV`, `IFS`, `PS4`, `PROMPT_COMMAND`, `SHELLOPTS`, `BASHOPTS`; `DOCKER_HOST`, `DOCKER_CONFIG`, `DOCKER_CONTEXT`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH`, `BUILDKIT_HOST`; `GOMAXPROCS`, `GOFLAGS`, `GOTMPDIR`, `GOMODCACHE`, `GOCACHE`, `GOPROXY`, `GOSUMDB`, `GOENV`, `GOTOOLCHAIN`; `NPM_CONFIG_CACHE`; `NODE_OPTIONS`, `NODE_PATH`, `NODE_EXTRA_CA_CERTS`, `NODE_TLS_REJECT_UNAUTHORIZED`; `JAVA_TOOL_OPTIONS`, `_JAVA_OPTIONS`, `JDK_JAVA_OPTIONS`; `CURL_HOME`, `NETRC`, `WGETRC`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE`, `RUBYOPT`; the browser discovery keys and run-attempt markers; and any name ending in `_PROXY`.
- Denied prefixes: `UZI_`, `CLAUDE_`, `ANTHROPIC_`, `CODEX_`, `OPENAI_`, `MCP_`; `GH_`, `GITHUB_`, `GITLAB_`, `GLAB_`; `GIT_`, `SSH_`; `LD_`, `DYLD_`, `BASH_`, `XDG_`, `SSL_CERT_`, `PYTHON`, `PERL5`, `NIX_`, `DEVBOX_`.
- Everything else is allowed: application names of any kind (`DATABASE_URL`, `STRIPE_API_KEY`, `SENTRY_DSN`), cloud credentials (`AWS_*`, `AZURE_*`, `GOOGLE_*`), package-index tokens (`NPM_TOKEN`, `PIP_*`, `NPM_CONFIG_*` other than the cache), and build-tool configuration such as `GRADLE_OPTS`, `MAVEN_OPTS`, `ORG_GRADLE_PROJECT_*`, `COMPOSE_*`, `TESTCONTAINERS_*` and `DOCKER_AUTH_CONFIG`.

### API (`api/internal/handler`)

- `GET /api/repos/{id}/env`: list with metadata and plain values, plus the variable count on the repo DTO. Accepts the session cookie and the CLI token. This is the first list a CLI token can read values from; they are values the user declared not secret.
- `PUT /api/repos/{id}/env/{name}` (value optional on an existing variable) and `DELETE /api/repos/{id}/env/{name}`: cookie and CSRF only, owner only, behind the same repo preflight as the tool profile (`api/internal/handler/tool_profiles.go`). A locked vault answers 409 `vault_locked`, as secret saves do; the list then shows names and metadata with plain values withheld.

### Claim (`api/internal/workersvc`)

- `ClaimSecrets` gains `run_env` (`omitempty`): a list of name, value and the secret flag. It is filled in `assembleClaim` by its own query, for run-lane claims only, in the transaction that finishes the claim, so the set a worker receives is the set recorded. The set is a claim-time snapshot: a change made while a run is claimed reaches it at its next claim.
- The run records injected names and revs (no values, no hashes).
- An open failure caused by a locked vault maps to the existing requeue path, never to a failed run.
- New worker protocol capability `run_env_v1`. `ClaimRun` requires it when the run's repo has variables, using the existing `EXISTS` gate pattern in `api/internal/store/queries/runtime.sql`. The same condition is mirrored in fleet-aware placement and in run health, so an unplaceable run says why.
- The claim response sets `Cache-Control: no-store`.

### Worker (`agent`)

- Variables are merged at the lowest precedence in `buildSdkEnv` and `buildCommandEnv`: a key already present is kept, then worker-set keys are applied as today. The worker re-checks every name against the fixture and drops a violation with a run event.
- Variables are set on every command, so a tool that reads the environment needs nothing else on either harness. A tool that logs in and stores the result under the home directory keeps it on Claude, where the run home lasts for the run, and loses it on Codex, where the command home is recreated per command; there the login is repeated or the tool is pointed at variables. The user doc says so.
- Docker client configuration (M3). The Docker CLI cannot take a login from the environment and the worker owns `DOCKER_CONFIG`, so the worker gives each run one directory named `.docker` and points `DOCKER_CONFIG` at it. A login made by one command is then read by the Docker CLI, image builds and container test libraries in later commands.
  - Claude: the run home is already per run and kept across commands (`agent/src/main.ts`); the directory is `.docker` under it.
  - Codex: the command home is recreated for every command (`commandEffectSpec` in `agent/src/codex/codex-executor.ts`), and the run's shared cache is optional and rotated, so neither can hold it. The command sandbox gets a dedicated per-run grant for this directory, independent of the cache, and `DOCKER_CONFIG` is set per command in `commandEffectSpec`. It applies with and without the uid split.
  - Lifecycle follows the run home: kept while the run is parked, removed when the run ends. The name `.docker` keeps `config.json` inside the existing credential denylist for terminal snapshots (`agent/src/git.ts`).
  - M3 starts by proving on Codex, on a uid-split worker, that a login in one command is visible to an image pull and an image build in the next. If the sandbox cannot be granted the directory, M3 stops and the PRD is revised; M2 does not depend on it.
- Redaction. One helper registers, with every redactor the run owns (the run-message set and logger set in `agent/src/runner.ts`, the Codex stream scrub), each secret value, its base64 form, and the base64 form of `<other value>:<secret>` for every other variable of the repo, plain or secret, whose value is at most 128 characters. That is the HTTP Basic form, which is what an `Authorization` header, a package-manager configuration and a registry login carry. Tool inputs that leave the worker without passing those sets today are redacted too: saved memories, review-thread replies, findings and CI evidence notes. Plain values alone are never registered.
- On a re-claim after a secret is replaced, the previous value stays registered for redaction for the life of the run's working directory.
- Exact-value scan: before a checkpoint publish, a salvage-ref publish and the final push, the outgoing range is searched for the same forms the redactor registers. A hit uses the existing secret-remediation hold and names the variable, so a user who chose a common word as a secret sees why. gitleaks stays as it is; it cannot know these values.
- Prompt. The environment-facts block of `agent/src/prompt.ts` (Claude and Codex call sites) and the append every subagent gets list names, descriptions and plain values, fenced and sanitised like other user-supplied text, and state that the listed variables are already set in the command environment. From M3, a resumed run is also told that a registry login may not have survived and to log in again if a pull is refused.
- A test pins that no outbox, recovery journal or hibernation record serialises `run_env`.

### Web and CLI

- `web/src/pages/Repos.tsx`: an "Environment variables" panel beside the tool profile, with the count on the repo row. Name, value, a secret toggle (on by default), description, update time, edit, delete with confirmation. A secret's value field is write-only. A fixed note says the agent can read every value, secret or not, and that uzi only sets the variables: any login or tool configuration is done by the agent or the repo's scripts. Mock in `web/src/mocks/mockApi/forge.ts`.
- Run page: injected names.
- `uzi repo env list <repo>` in `api/cmd/uzi/repo.go` with the client in `api/internal/uzicli`.
- User doc page under `docs/`: the rules, secret versus plain, the agent-readable note, scoped-credential advice, which tools keep a login across commands on each harness, and three worked examples: an API key and base URL for integration tests against a third-party sandbox; a token for a private package index; and a private artifact repository plus a private container registry (which variables to define, how to describe them so the agent logs in, build arguments and image history, and the setting a container test library may need to reach the worker's Docker daemon).

## Testing decisions

- Store: both partial unique indexes, limits, the edit rules, cascade on repo and user delete.
- Vault: enable, disable and account reset with variables present; every row opens afterwards or is gone. A ciphertext swapped between two names, two repos, or a secret and a plain row, fails to open.
- Name rules: the shared fixture drives table tests on both sides; a name allowed by one side and denied by the other fails the build.
- Claim: gate on and off, an old worker never claims a gated run, locked vault requeues, names and revs recorded, a mid-run change reaching only the next claim, `no-store` header present.
- Worker: precedence (a user `PATH` smuggled past the api is dropped); redaction of a secret, of its base64 form and of a written registry login entry through each sink listed above, and no redaction of a plain value; redaction of a replaced secret's previous value after a re-claim; exact-value scan holding a checkpoint, a salvage publish and a final push for a secret, and holding nothing for a plain value present in the tree.
- E2E for M2, compose, both harnesses, dummy values: a run's test command reads a plain URL variable and a secret, calls a local HTTP stub that requires the secret, and passes; the secret printed by a command is redacted in the run messages.
- E2E for M3, compose, both harnesses, with a local registry stub that requires authentication: a run logs in with a secret, and later commands pull an image and build one from a private base image through the Docker client configuration.
- Fixtures that look like credentials are assembled at runtime and `task scan:secrets` runs in every milestone gate.

## Milestones

### M1: Variables can be stored and managed

Migration, vault integration (seal, enumerations, reset), secret flag, edit rules, name rules fixture, API, web panel and repo-row count, CLI list, user doc. Nothing is injected yet and the panel says so. Gate: `task gate:api`, `task gate:web`, `task scan:secrets`.

### M2: Runs receive the variables

The claim field and capability gate with its placement and health mirrors, worker merge on both harnesses, redaction helper and tool-input redaction, exact-value scan on the three publish paths, prompt blocks for the lead and subagents, injected names on the run page, the ADR, E2E. After this, everything that reads the environment works: test code, package managers, build tools, cloud CLIs. Gate: `task gate:api`, `task gate:agent`, `task gate:web`, `task scan:secrets`, compose E2E, and a hosted k8s run of the E2E scenario.

### M3: Container registry logins last for the run

The Codex login-persistence proof first; it gates the rest. Then the run's Docker configuration directory on both harnesses, the resume hint, the registry example in the user doc, E2E. Gate: `task gate:agent`, `task scan:secrets`, compose E2E, and a hosted k8s run of the E2E scenario.

### M4: Variables for all of a user's repos

A user-level set applied to every repo the user owns, with a repo variable of the same name winning. Services of one product usually share the same values, and without this a user re-enters them per repo. The table, its indexes and the AAD already carry the user-level scope from M1; this adds the routes and settings panel, the second source in the claim query and worker merge, and widens the claim gate from "the repo has variables" to "the repo or its owner has variables".

## Landing obligations

- No file under `.github/workflows/` is touched by implementation or validation.
- Migration numbers are assigned at merge time.
- A changelog fragment per PR.
- The ADR for the amended invariants and accepted residuals lands with M2.

## Decision Log

- **Plain variables, agent-readable.** Reviewed alternatives: command-only injection, a worker-run fixed command, running the tests in the forge's CI, and short-lived tokens. None hides a value from an agent that authors the commands and the code. Only a credential-holding proxy does, at a much larger cost, and only for pull credentials. Chosen: ship plain variables with an honest label; treat the proxy as its own PRD.
- **A secret flag per variable.** Treating every value as a secret would redact registry hosts out of every log line and hold every push from a repo whose build files name that host. It would also hide the URL the agent most needs to see.
- **One sealed storage path for both kinds.** Simpler than two tables or a plaintext column; the cost is that plain values are unavailable while the vault is locked, when runs do not start anyway.
- **Generic variables, not typed credentials.** A store of registry or artifact-repository logins would cover one class of need and leave API keys, database URLs and every other value a repo reads without a home. Named variables cover all of them the way CI variables do, and the typed cases reduce to variables plus a command.
- **uzi does not log in or configure tools.** A worker-written login or configuration file is deferred until variables prove insufficient.
- **A Docker configuration directory per run, as its own milestone.** It is the only tool-specific piece. Without it a login on Codex does not survive to the next command, so container-based test suites cannot authenticate there at all, and no variable can fix that because the worker owns `DOCKER_CONFIG`. It is split from M2 so generic injection does not wait on the Codex sandbox proof. It is kept while a run is parked: clearing it would need a call on every park path, and the credential it holds is already readable by the run.
- **Plain values readable by a CLI token.** The secrets list is readable by a CLI token because it never carries a value. This list carries values the user declared not secret; a URL with embedded credentials is refused as plain to keep that true.
- **The name rules guard the worker, not the build.** Build-tool options that can load code stay allowed, because the user who sets them owns the repo and can do the same by editing the build.
- **No audit table.** No other secret has one, and nothing would read it.
- **Per user per repo, then per user.** Matches repos, the tool profile and the vault. The user-level scope is in the schema from M1 so M4 needs no migration of existing rows. Sharing between users would need an authorization model uzi does not have.
- **Web-only writes.** Keeps the existing cookie-only rule for secret writes.
- **Lowest-precedence merge plus a denylist.** The existing provisioning fold is a three-key allowlist and cannot carry user names; precedence protects every worker-set key structurally, the denylist covers names the worker never sets.
- **Hard claim gate.** A soft gate would let an old worker run the job without the variables and fail in a confusing way.
- **Cross-repo exposure on a shared worker accepted.** Same exposure as the Anthropic token today; recorded as a residual rather than requiring uid-split or ephemeral workers.
