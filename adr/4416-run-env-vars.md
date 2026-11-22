# ADR-4416: Run environment variables reach lead-lane runs, redacted and scanned

**Status**: Implemented (PRD #4416 M2: runs receive the variables); M3 (registry logins that last for the run) and M4 (user-level variables) pending; exact-head CI, the compose E2E scenarios and live-fleet acceptance pending
**Date**: 2026-11-21
**Related**: PRD #4416 (`prds/4416-repo-env-vars.md`); [ADR-319](0319-env-read-diagnostic-allowlist.md); [ADR-285](0285-worker-egress-tier-trust-model.md); [ADR-91](0091-runner-cross-run-persistence-residual.md); [ADR-3507](3507-optional-credential-vault.md); user doc `docs/run-env-vars.md`

## Decision

A repo's variables (M1 stores and manages them) are delivered on the claim of a
**lead-lane** run and set in the environment of every command that run's agent
executes. Each variable is plain or secret. Secret values are redacted from run output and
every publish is searched for them, failing closed. Delivery is checked
against a snapshot at claim finish, recorded as receipts, and gated on a new worker
capability, `run_env_v1`.

## Lead lane only

Variables are delivered to the runs `leadcfg.LeadLane` selects: the kinds `issue`, `ci_fix`,
`prompt`, `self_improve`, `mr_rework` and `task`, with no review target and no egress profile
(an isolated-lane run). The SQL function `fn_run_env_lane` (migration `00504_run_env_receipts.sql`)
mirrors `leadcfg.LeadLane`, `workersvc.RunEnvDelivered` calls it, and a LiveDB test pins the two
to agree for every run kind. The worker applies the same predicate again
(`agent/src/run-env-lane.ts`) before it hands variables to an executor. Chat, judge, job,
task-review and cross-check runs never receive `run_env`. A worker-side test
(`agent/test/run-env-inject.test.ts`) pins that the Codex app-server launcher and the chat,
isolated, advice, cross-check and job-runner builders take no run variables.

The PRD named the lane by kind list. The function makes the egress-profile exclusion
explicit, because an isolated-lane run has no shell the owner's variables should reach.

## Amended invariants

This amends two recorded invariants.

- **The Codex command environment is no longer credential-free.** `buildCommandEnv`
  (`agent/src/codex/codex-executor.ts`) takes an optional `runEnv`. User variables are folded
  in after the tool profile and before every worker-set key, so they have the lowest
  precedence: a key already present, a protected key (`COMMAND_ENV_PROTECTED_KEYS`, the git
  config carriers) or a denied name is dropped with a run event naming the variable, never its
  value. They go only into the per-command environment. The Codex app-server environment, which
  is worker-owned, never receives them.
- **The SDK environment no longer holds exactly one secret.** ADR-319 relied on `buildSdkEnv`
  handing the Claude subprocess a replacement environment whose only secret was
  `CLAUDE_CODE_OAUTH_TOKEN`, with `PATH` and `TMPDIR` as the allowed diagnostic reads. User
  variables are now in that environment (`buildSdkEnv`, same lowest-precedence fold). The
  allowlist `{PATH, TMPDIR}` is unchanged and still the only names readable through
  `printenv`/`env`; bare enumeration and non-allowlisted reads stay denied. This feature does
  not rely on the guardrail as a boundary: the agent chooses and authors its commands, so it can
  read every value given to them. The user doc says so.

## Name rules, with evidence

`fixtures/run-env/name-rules.json` is the single list of denied names, prefixes and the `_PROXY`
suffix, pinned by table tests on the api (`api/internal/runenv`) and the worker
(`agent/src/run-env-names.ts`). It records the harness versions it was derived from (the
pinned Claude Agent SDK and Codex), and a test fails when a pin changes without a fixture
update. **Every denied entry carries an `invariant`** (one of `worker_credentials`,
`provider_auth`, `harness_control`, `git_authority`, `managed_dirs`, `network_wiring`,
`process_loader`, `shell_semantics`) and a note naming the reason. The review that produced it
read the environment settings the pinned CLI and Node runtime consume. Examples of what it
caught beyond the PRD list:

- `BUN_` prefix (`harness_control`): Bun runtime controls such as `BUN_CONFIG_VERBOSE_FETCH`,
  which prints request headers, `BUN_BE_BUN`, `BUN_OPTIONS` and `BUN_INSPECT`.
- Node runtime controls (`NODE_OPTIONS`, `NODE_PATH` and the loader and startup controls such as
  `NODE_REPL_EXTERNAL_MODULE`, `NODE_PRESERVE_SYMLINKS`, `NODE_COMPILE_CACHE`, `OPENSSL_CONF`;
  `process_loader`), Node debug and telemetry switches (`NODE_DEBUG`, `NODE_V8_COVERAGE`;
  `harness_control`) and Node TLS and proxy behaviour (`NODE_EXTRA_CA_CERTS`,
  `NODE_TLS_REJECT_UNAUTHORIZED`, `NODE_USE_ENV_PROXY`, `NODE_USE_SYSTEM_CA`; `network_wiring`).
- Further names the Node runtime the harness runs on reads: `NODE`, `NODE_NO_WARNINGS`,
  `NODE_SKIP_PLATFORM_CHECK`, `NODE_PENDING_PIPE_INSTANCES` and `UV_USE_IO_URING` (`NODE` and
  `UV_USE_IO_URING` are `process_loader`, `NODE_NO_WARNINGS` is `harness_control`).
- `SSLKEYLOGFILE` (`network_wiring`): it would log the TLS keys of the worker's provider traffic.
- Trust-root overrides of other tools (`AWS_CA_BUNDLE`, `PIP_CERT`, `DENO_CERT` and similar;
  `network_wiring`), credentials the Claude CLI reads without a provider selector
  (`AWS_BEARER_TOKEN_BEDROCK`, `CLOUDSDK_AUTH_ACCESS_TOKEN`; `provider_auth`), and the CLI's
  internal prefixes (`CCR_`, `_CLAUDE_`, `ANT_OTEL_`, `OTEL_LOG_`; `harness_control`).
- `LC_` (`shell_semantics`, like `LANG`) and `GCM_` (`git_authority`).
- Telemetry and metadata endpoints the harness reads: the prefixes `OTEL_EXPORTER_OTLP_`,
  `GCE_METADATA_` and `AWS_ENDPOINT_URL_BEDROCK` and the name `AWS_EC2_METADATA_SERVICE_ENDPOINT`
  (all `network_wiring`). The `AWS_ENDPOINT_URL_BEDROCK` prefix has no trailing underscore on
  purpose, so it also covers `AWS_ENDPOINT_URL_BEDROCK_AGENT*`. `OTEL_EXPORTER_OTLP_*` and
  `GCE_METADATA_*` are denied as network wiring because the harness reads them.
- `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_STS`, `AWS_ENDPOINT_URL_SSO` and
  `AWS_ENDPOINT_URL_SSO_OIDC` (`network_wiring`), by exact name: the global AWS endpoint override
  also routes Bedrock traffic. The STS, SSO and SSO OIDC names override the endpoints of the AWS
  credential exchange that the Bedrock harness's AWS SDK credential chain can use; they are denied
  for consistency with `AWS_ENDPOINT_URL`. The other per-service names
  (for example `AWS_ENDPOINT_URL_S3`) stay allowed. Names are checked on write only, so a stored
  variable whose name a later rule denies is dropped at injection by a worker that has the rule,
  with a run event naming the rule.

Names that a harness also reads but that are application configuration (`AWS_REGION`, cloud
credentials, `CLOUD_ML_REGION`) stay allowed and are recorded in the fixture's `overlaps` with the
reason. The PRD's invariant holds: user variables cannot change the provider or the
authentication the worker selects. The rules guard the worker, not the build.

Rollout: the added names and prefixes ship with run-variable delivery, which no released worker
has, so no deployed worker injects a variable under them. Enabling a variable keeps the full name
rule. Disabling or deleting one checks only the name's shape, so a stored variable whose name a
later rule denies can still be removed.

## Claim, snapshot, gate

- `assembleClaim` reads the effective injection set (enabled, live versions) and the redact-only
  secret versions as lists of `(var_id, rev)` and opens each value for the run's owner.
  `finishRunClaimTx` then takes the owner's users row `FOR KEY SHARE NOWAIT`, after the run lock,
  recomputes both lists and compares them (`checkRunEnv`). Every user-facing variable writer, the
  account reset and the user delete lock the same row `FOR UPDATE`, so the recompute sees every
  committed write and the next one waits for the receipts. Two deliberate exceptions (listed in
  `queries/run_env_receipts.sql`): the retired-version sweeper takes the row `FOR SHARE NOWAIT` and
  deletes only versions no held run's receipt references, and a repo delete cascades its variables
  together with the repo's runs and their receipts. A busy lock rolls back into the finisher's bounded
  retry; the claim never waits on it.
- A changed list, or a non-empty set on a worker without `run_env_v1`, returns the run to queued
  with nothing delivered. A change committed after the finish reaches the run at its next claim.
  A locked vault requeues as it does for other secrets.
- `run_env_v1` (`api/internal/capability`) is required by `ClaimRun` for a lead-lane run whose
  repo has an enabled live variable or that holds a secret receipt, as a standalone clause in
  `api/internal/store/queries/runtime.sql`. The same condition is mirrored in fleet-aware placement
  (migration `00505_run_env_placement.sql`) and in run health, which reports "no online worker supports
  repo variables (run_env_v1); upgrade a worker" while no capable worker is connected. The claim
  response is `Cache-Control: no-store`, and the snapshot is unexported so no outbox, journal or
  hibernation record can serialise it.
- **A receipted version that is missing, swapped or cannot be opened parks the run** with hold
  reason `run_env_history_unavailable` instead of delivering partial protection. The run neither
  resumes nor publishes until the owner acts: Resume retries once the history is restored,
  otherwise Cancel (terminal).

## Receipts, retention, identity

- **Receipts** (`run_env_receipts`) record one row per delivered version of a run: `run_id`,
  `var_id`, `rev`, scope, name, the secret flag and the first and last claim generation. They carry
  no value and no hash, and have no foreign key to the variable table, so a deleted or purged version
  stays visible as missing. **Plain versions get receipts too**: the run page needs the names a
  claim injected. Retention, the retire and pending-delete decisions and the finish gate read the
  secret receipts only (a partial index), so a plain receipt never keeps a version.
- **Retention.** A retired, disabled-and-replaced or pending-deletion secret version stays sealed
  while a **held** run holds a secret receipt for it. `fn_run_env_run_held` defines held: the run
  is not terminal, or has been terminal for less than **24 hours** (the grace window), or holds
  recovery custody in any form (an open custody hold, a live capture, a held-work publication, a
  terminal preservation, salvage, a prerequisite closure, a publication-resume enrollment, a
  checkpoint retention or an unaccounted checkpoint push). The sweeper pass `run_env_retired_sweep`
  and the history-full answer read that one definition. The pass locks only the owner's users row
  (`FOR SHARE NOWAIT`, a busy owner is skipped), deletes in bounded batches, then deletes variable
  identities no row and no receipt references.
- **`var_id` identity guard.** `run_env_var_ids` binds each `var_id` to one
  `(user, repo, scope, name)` and is never reused while its identity row exists. The identity is deleted by the
  sweeper once no row and no receipt references it (`DeleteRunEnvSweepIdentities`), after which the
  id may be issued again; var_ids are random UUIDs. A trigger
  on `run_env_vars` creates the identity on first insert and refuses, with `check_violation`, any
  insert under another owner, repo, scope or name and any update of those columns. A receipt that
  names a `var_id` can therefore never be satisfied by a row of another variable. The migration refuses
  loudly when existing rows already map one `var_id` to more than one identity.
- **AAD version 2.** `run_env_vars.aad_version` names the sealing context. Version 2
  (`runenv.AADKindV2`) binds the owner, scope, repo, secret flag, name, `var_id` and `rev`, every
  field length-prefixed, so a ciphertext cannot be moved to another version of the same variable,
  to another variable or to another owner. Every new seal writes 2; `runenv.AADFor` selects the
  context by the row's version.
- **Account reset and repo delete.** The account reset deletes the owner's variable rows but
  leaves receipts, so runs that held versions still show what they were given. Deleting a repo
  cascades to its variable rows and, with its runs, their receipts.
- **Stale descriptions are accepted.** The finish compares `(var_id, rev)`, and a
  description-only edit changes neither, so a claim in flight can carry the previous description.
  That is not a mismatch and does not requeue.

## Maintenance redact route

`GET /api/worker/runs/{id}/run-env-redact` (worker bearer, `no-store`) returns every secret
version the run's governing run was delivered, so a worker can mask and scan after the claim that
delivered them: a restart, a held or terminal publication, a recovery upload. It answers
`{protected:false}` for a run with no governing run or no secret receipt, 409 `vault_locked`, or 409
`run_env_history_unavailable`.

- **Authorization**: the run belongs to the worker's user and the worker is its last holder
  (`runs.worker_id`), or took an open custody hold on it.
- **Bound while a publication can still need it.** Plaintext is served only while the requested
  run is not terminal, or while a publication is in flight for it: an open custody hold, a capture
  preparing or uploading, a held publication prepared, invoked or of unknown outcome, or a
  terminal preservation bound or publishing (`RunEnvRedactOpen` and
  `fn_run_env_publication_pending`). A cross-check child is served only while it is not terminal
  and its `cross_checks` row is open before its deadline, or while a publication is in flight for it.
  Otherwise the route answers 409 `run_env_redact_closed`, and the worker withholds the publish. The
  route's own 404 carries the code `run_env_run_not_found`.
- **No generation bound.** A worker that last held the run may read the full receipted history
  whatever the current claim generation. This is accepted: the route is read-only, same user
  only, and the same worker was delivered those values by the claim that wrote the receipts.

## Cross-check children

A `cross_check` child never receives `run_env` and writes no receipts, but its checker reads the
lead's code and output, so it must mask what the lead was given. Its claim carries
`run_env_redact` with the **lead's** secret versions only, opened for the run's owner. A worker
without `run_env_v1`, or a lead version that cannot be opened, ends the child as
`checker_unavailable` (a non-pass that forces a human gate) rather than parking it. The redact
route answers the child with its lead's set, for as long as the route is open for the child
(see the bound above).

## Redaction and the exact-value scan

Registered forms (`agent/src/run-env-forms.ts`) are, for each secret value of the claim's
`run_env` and `run_env_redact`, the whole-value encodings: raw, JSON-escaped, JSON with escaped
slashes, HTML-safe `\u00XX` (both hex cases), percent-encoded (both hex cases, `+` for a space),
and three standard and three URL-safe base64 alignments trimmed to the characters that do not
depend on neighbouring bytes. Each is also registered for the value without its trailing line
break and, for a multi-line value, with line endings normalized to LF. A multi-line value also
registers each of its lines for redaction (and for the preserved-patch line check) only, never as a
scan needle, because a public line would hit unrelated files. A line that is not PEM armor is
registered only when its value part holds a token (`lineForms` in `run-env-forms.ts`). The value part
is the line without a leading `key:` or `key=` (optionally after `- ` and quotes; an `=` that only
pads the end and a `:` followed by `//` are not key separators), with the scheme and host[:port] of
each URL removed (userinfo, path and query stay). Tokens are maximal runs of `A-Z a-z 0-9 + = _ -`
(`/` separates). A token qualifies with at least 8 characters and 5 distinct ones, and is all digits,
or has both letter cases, or has a letter and at least 2 digits. The scan needles instead add
diff-shaped forms of the value, its lines joined by `\n+`, `\n-` and `\n ` (LF and CRLF). Plain
values are never registered. The forms join the run's existing redactor, so every sink it already
covers masks them; the plan, the plan summary, the intent summary and
the planning diff also go through it. The lead's prompt lists only the variables the executor actually
injected: a name dropped as protected or already set by the tool profile is not listed. That set is
computed once per executor run, after tool provisioning and before any prompt, with the rules the env
build applies (`runEnvInjectedSet`, `agent/src/run-env-inject.ts`), and the env build folds exactly that set.

A claim's forms are computed once (`runEnvClaimForms`) and capped at `RUN_ENV_FORM_CHARS_MAX`
(40,000,000 UTF-16 units of distinct forms). Over the cap only the raw values (and their trimmed and
LF variants) are registered for redaction, with no scan needles or line forms: the claim
fails before the clone and any output with a failure reason starting `run_env_forms_over_budget` (no
value), and nothing is published. A lane with no live flight (held, terminal, bundle publication)
withholds with the reason `forms_over_budget`. A cross-check child over the cap ends as
`checker_unavailable`. To keep a claim under the cap, a variable save is refused at write time with
409 `env_forms_budget` when it would raise the repo's counted secret value bytes above
`MaxClaimSecretBytes` (116,959 = 40,000,000 / 342 form units per byte, `api/internal/runenv/form_budget.go`,
`fixtures/run-env/form-budget.json`). Counted: every secret row of the repo and the user scope in any
state (`GetRunEnvFormBudgetUsage`); plain values do not count. The unit count includes the logger's
JSON-escaped copy of every redaction form; 342 is the control-character worst case. A save is refused
only when it raises the count and leaves it above the cap, so a repo already above it can shrink but not
grow; Disable, Enable and Delete are never refused for budget reasons. The per-byte bound is analytic, pinned
by `agent/test/run-env-forms-budget.test.ts` for the form counts it assumes, not proved for every value.

Every publish is searched for the registered forms by `scanRunEnvRange`
(`agent/src/run-env-scan.ts`): every object reachable from the candidate tip and not from the
exclusion set, messages, paths and contents. The verdict is `clean`, `hit` (the finding names the
variable, never a value) or `withhold` (deadline, git error, unresolved range, byte cap, or needles
that could not be resolved). There is no fail-open outcome, unlike the gitleaks path, which defers
to forge push protection that cannot know these values. A clean verdict is bound to the exact tip
and exclusion set that is then published (`scanCoversCandidate`).

Needle resolution (`resolveScanNeedles`): a publish with a live claim uses its needles. With no
live flight the worker asks the redact route. A 409 withholds, and so does a 404 that carries any
code, including `run_env_run_not_found`. Only a 404 with no code (an api that lacks the route)
reaches the marker check, and it resolves to no needles only when no durable per-run marker exists. The worker writes the marker (`agent/src/run-env-seen.ts`: temp
file, fsync, rename, fsync of the directory) before any variable reaches an executor, for every
claim carrying `run_env` or `run_env_redact`, and a failed write stops the run from starting.
Markers are never removed. The marker proves only that this worker never received variables for the
run: work adopted from another worker, or claimed by a build from before the marker existed (not
released), has none.

Sites:

- **Checkpoint publish**, at the single `publishCheckpoint` call, so tick, iteration, milestone,
  park, shutdown, pause and overlay checkpoints all pass it. A run holding secret variables also
  defers every in-permit publish, overlay or not.
- **Held and terminal packs and recovery bundle uploads**, through the recovery coordinator, which
  takes its needles from the live claim or, with no flight, from the redact route; an unresolvable
  answer withholds and the publish is retried later.
- **Finalize, on every forge**, before the branch push. A hit, or a scan that cannot prove the
  range clean, fails the run with `fail_origin = push_secret_blocked`, no push and no preserved
  patch.
- **The preserved patch** (`scanGatedPatch`, `patchScanVerdict`, `patchImages`): the raw patch is
  withheld whole when the claim is over budget; when a whole-value form occurs in it; when one occurs
  in a rebuilt per-hunk pre-image or post-image (context plus removed or added lines, also with line
  breaks normalized to LF); when any registered line form occurs; or when it cannot be parsed
  (binary, combined diff, a line-count mismatch, or a stray line such as a non-header line between a file
  header and its first hunk). The image, line-form and parse checks apply when the run has any
  secret variable, retired versions included, so a run with a secret variable loses its preserved patch when the diff touches a binary file. Otherwise it is
  redacted and gitleaks-scanned as before.

## Where the PRD differed from the code

Recorded so the PRD's plan is not read as the shipped mechanism.

- The PRD listed a salvage-ref publish as a separate worker path. The salvage ref is an API-side
  copy of the last published checkpoint, so scanning the checkpoint covers it; there is no
  worker-side salvage publish to scan.
- Before M2 the park, shutdown, pause and overlay checkpoints were not gitleaks-scanned.
  They now pass the exact-value scan for secret variables through the single `publishCheckpoint`
  call. gitleaks behaviour is unchanged.
- Recovery bundle uploads were unscanned and are now scanned.
- The finalize gitleaks scan was GitHub-only and fail-open. The exact-value scan runs on every
  forge and fails closed. gitleaks stays as it was.
- The PRD said a finalize hit would be held by the secret-remediation hold. A finalize hit instead
  fails the run `push_secret_blocked`, the existing typed finalize outcome; checkpoint hits still
  latch into the remediation state. The recovered work stays reachable through the run's custody and `uzi run export`.
- The PRD named the lane by kind; the code uses the lane function above.
- The PRD said receipts record secret versions. They record plain versions too.
- The PRD's AAD named the user id, scope, repo, secret flag and name; the M1 code's version-1 context binds scope, repo, secret flag and name, with the owner bound by the vault seam (`OpenSealed` takes the user id). Version 2 adds owner, `var_id` and `rev` to the context itself.

## Residuals

Accepted, in addition to the PRD's list (agent-readable values, shared uid and daemon on a worker,
exact-match redaction with an 8-character floor, values reaching the model provider):

- **AAD v1 rows.** Rows sealed before M2 keep `aad_version = 1`, whose context lacks `var_id` and
  `rev`: a database writer can swap two such ciphertexts of the same owner that share repo, name, scope and
  secret flag (in practice, two versions of the same variable).
  A database writer can also downgrade `aad_version` on a row. Both close when the version is
  rewritten (a new save seals under version 2). Whoever can write the table can also write
  other vault material; this is a defence against a swap, not against a hostile operator.
- **Redact route without a generation bound**, as above.
- **Patch gaps.** The patch check misses a secret whose lines hold no qualifying token (single-case
  letters, letters with one digit, digits with fewer than 5 distinct, formatted codes such as
  `8462-9137`, non-ASCII text, a secret that is the line's leading key) and that is split across files or by elided context between hunks, and a value that appears only re-encoded
  (each line indented in a YAML block, or an encoding other than those registered). The blob scan
  still blocks a committed file.
- **Path gap in two sinks.** The planning diff sent to a cross-check
  and the PR description diff context are redacted, not path-scanned: a secret with non-ASCII
  characters used as a path name reaches them C-quoted (octal-escaped) and unredacted.
- **Marker coverage.** The absent-route fallback relies on the per-run marker, which exists only
  for runs this worker received variables for, as described above.
- **Migration Down refuses** rather than losing data: it raises when any version-2 row or any receipt
  exists, because removing them would leave values unopenable or unmasked.
- **Sweeper and cross-check children** (deferred). Retention reads the receipts of the run that
  holds them. It does not consider a cross-check child's dependence on its lead's versions, so a
  version could be swept while a child still needs it. The child then fails closed (the claim
  cannot open the version and ends `checker_unavailable`); unlikely, and not fixed here.
- **Placement and cross-check children** (deferred). Placement does not steer a cross-check child
  to a `run_env_v1` worker. During a rolling deploy a child claimed by an old worker ends
  `checker_unavailable`, which forces a human gate.
- Neither of the last two is a leak: both withhold or force a human decision.

## Not shipped in M2

M3 (a per-run Docker client configuration directory, so a registry login lasts for the run) and
M4 (user-level variables) are not implemented; the user doc says logins do not persist across
commands on Codex. The compose E2E scenarios are authored and run at landing; they are
not evidence here.
