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
(an isolated-lane run). The SQL function `fn_run_env_lane` (migration `00543_run_env_receipts.sql`)
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
  for consistency with `AWS_ENDPOINT_URL`. Merging main (#4649, Codex 0.171.0) added the
  broader `AWS_ENDPOINT_URL_` prefix, so every per-service name, including `AWS_ENDPOINT_URL_S3`,
  is now denied; the narrower BEDROCK prefix and the STS and SSO names stay as redundant,
  documented entries. It also added the names `SPIFFE_ENDPOINT_SOCKET` (`provider_auth`),
  `SSH2_CLIENT` (`git_authority`) and `OTEL_RESOURCE_ATTRIBUTES` (`harness_control`). Names are checked on write only, so a stored
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
- **Values open at one vault epoch.** `OpenSealedAtEpoch` (`api/internal/secretopen/secretopen.go`)
  opens a value only at the owner's vault epoch read in the same SQL statement as its ciphertext. If
  the vault changed between the read and the open (the epoch moved, a stale seal), the lead-lane
  assembly re-reads once (`retryOnSealStale` in `api/internal/workersvc/run_env_claim.go`); a second
  stale seal requeues the claim, and nothing is delivered from a failed pass. The epoch the values
  were opened at is kept in the snapshot, and `checkRunEnv` compares it with the epoch read under the
  users-row lock at claim finish: a moved epoch is `errRunEnvStale` and the run returns to queued.
  A vault-state read that fails is a retryable error (`mapRunEnvOpenError`), not a park; a missing or
  invalid vault still parks the run as `run_env_history_unavailable`.
  The variable list and write responses (`api/internal/handler/run_env_vars.go`) open plain values at
  the epoch read with them (the write response at the epoch locked in its transaction), so a value
  is never shown that was opened at a different epoch than its row. The one exception is the no-op
  description save, which reads no epoch and opens with a lock-state check instead.
- `run_env_v1` (`api/internal/capability`) is required by `ClaimRun` for a lead-lane run whose
  repo has an enabled live variable or that holds a secret receipt, as a standalone clause in
  `api/internal/store/queries/runtime.sql`. The same condition is mirrored in fleet-aware placement
  (migration `00544_run_env_placement.sql`) and in run health, which reports "no online worker supports
  repo variables (run_env_v1); repo variables need a current two-uid worker (UZI_UID_SPLIT, or
  UZI_WORKER_UID_SPLIT on hosted workers)" while no capable worker is connected. A worker
  advertises `run_env_v1` only under the uid split, so a single-uid worker never counts as capable
  (see "Single-uid posture" below). The claim
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
- **One state read after the open.** After the versions are opened, authority and openness are
  checked again and the owner's vault epoch and unlock state are checked from one vault state read:
  the epoch must still equal the one the versions were opened at and, only when the vault is
  protected, that epoch's cached key must be present (`vault.UsableAtState`, no second read). A
  moved epoch or a missing key is 409 `vault_locked` with no version. A state read that fails
  stays a reader failure (500), not a locked answer; an invalid state or no vault is
  `run_env_history_unavailable`. A change committed after that check and before the response is
  written is not fenced.
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

The child's lead versions are opened at one vault epoch too (`openReceiptVersions`), and the child's
claim finish requeues the child if the owner's users-row epoch moved since that open, so a redact
set opened before a vault rewrap is never delivered after it.

## Redaction and the exact-value scan

Registered forms (`agent/src/run-env-forms.ts`) are, for each secret value of the claim's
`run_env` and `run_env_redact`, the whole-value encodings: raw, JSON-escaped, JSON with escaped
slashes, HTML-safe `\u00XX` (both hex cases), percent-encoded (both hex cases, `+` for a space),
and three standard and three URL-safe base64 alignments trimmed to the characters that do not
depend on neighbouring bytes. Each is also registered for the value without its trailing line
break and, for a multi-line value, with line endings normalized to LF. A multi-line value also
registers each of its lines for redaction (and for the preserved-patch line check) only, never as a
scan needle, because a public line would hit unrelated files. Every line of 8 or more characters is
registered (`lineForms` in `run-env-forms.ts`), with no condition on letter case, token, digits or
key name; the token rule, the leading-key strip and the URL scheme and host carve-out are removed.
The only lines excluded are two carve-outs, both accepted by the owner (PRD #4416 Decision Log,
2026-11-22) and both tightened to a closed definition:

- **PEM armor.** Only a line exactly equal to `-----BEGIN <LABEL>-----` or `-----END <LABEL>-----`
  for a label in the finite list `PEM_STANDARD_LABELS` (`agent/src/run-env-forms.ts`) is exempt:
  `PRIVATE KEY`, `RSA PRIVATE KEY`, `EC PRIVATE KEY`, `DSA PRIVATE KEY`, `ENCRYPTED PRIVATE KEY`,
  `OPENSSH PRIVATE KEY`, `PUBLIC KEY`, `RSA PUBLIC KEY`, `CERTIFICATE`, `TRUSTED CERTIFICATE`,
  `CERTIFICATE REQUEST`, `NEW CERTIFICATE REQUEST`, `X509 CRL`, `PKCS7`, `CMS`, `EC PARAMETERS`,
  `DH PARAMETERS`, `ATTRIBUTE CERTIFICATE`. A marker with an unknown label (including PGP armor), an
  indented marker, or any line that carries more than the fixed marker keeps normal line protection.
- **Whitespace-only lines.** Only a line of ASCII space (0x20) and tab (0x09) is exempt
  (`WHITESPACE_ONLY = /^[ \t]*$/`). A line of NBSP or other Unicode spaces (U+2000 to U+200A, U+3000
  and so on) at or above the 8-character floor is a normal line form. This closes the earlier
  residual that the exemption used JavaScript's `\s`.

Structural lines of a secret file, such as `apiVersion: v1` or
`"type": "service_account",`, are therefore masked wherever they appear in run output. The scan needles instead add
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

### Redaction work guard (owner decision B3, fixed)

The redactor used to cost text length times the longest literal for long periodic secret lines
(the owner's review measured about 2.15 s for one such string on one host, which motivated the guard). The matcher now has a
**step-counted** guard, not a wall-clock one: a deadline cannot interrupt synchronous matching, so
every literal comparison is charged against a counter (`RedactionWork`) before it runs, in
`agent/src/redact.ts`.

- **Budget and bound.** `REDACTION_WORK_BUDGET` is 2^26 charged units. The charge is one unit per
  text position whose first unit can start a literal, plus the label length of each trie edge compared in `LiteralIndex`, and
  `1 + secret.length` per candidate in `spansOfEach`. The charged work of one redacted string never
  exceeds the budget + 1 + the longest single literal, whatever the host's speed.
- **One budget per message payload.** For the run's message-payload redactors (`makeRedactor`,
  `SecretSet.payload`, which every `MessageBatcher` uses), `redactPayloadWithin` redacts every string
  and member name of a payload against one shared counter, so the bound holds per payload, not per
  string. Other structured walks (`redactStrings` with a text redactor, e.g. a PR summary or a review
  verdict) stay bounded per string. After the
  budget is spent, each further string that still needs a charged comparison is replaced by the
  marker; strings that need none pass through.
- **On overflow, fail closed.** The whole string becomes the fixed marker `REDACTION_WITHHELD`
  (`[withheld: redaction work budget exceeded]`), never the raw text and never a partly redacted
  one (`runWithinBudget` returns undefined and the caller withholds). The logger
  (`agent/src/log.ts`) writes a fixed valid JSON record with `ts`, `level` and a fixed `msg`
  (`log record withheld: redaction work budget exceeded`); the base fields and the record's own
  fields are dropped. The preserved-patch scan (`literalsPresent`, used only by `run-env-scan.ts`'s
  `patchScanVerdict`) reports every form as present on overflow, so the patch verdict withholds. A preserved patch whose text
  redaction overflows is not shipped (`runner.ts`, `scanGatedPatch`).
- **What stays visible to the api.** The run status, run id, claim generation and `fail_origin` do not
  pass through the redactor, so a state report still reaches the api; `failure_reason` is
  redacted and becomes the marker on overflow.
- **Conservative charging.** The charge is an upper bound of the native work, so very large or
  prefix-dense benign text can be withheld although no secret is in it. That fails closed and is
  accepted.
- **Not redaction.** The git object scan and the path scan (`literalMatcher` in
  `agent/src/secret-scan-guard.ts`, used by the object and path passes in `agent/src/git.ts`) are
  not subject to this budget. They are bounded by a deadline and byte and path caps and fail closed
  (`trusted: false`, so the publication is withheld). Periodic input can exhaust their deadline but
  cannot make them pass.

Tests: `agent/test/redaction-work-guard.test.ts` and `agent/test/redaction-work-guard-runner.test.ts`.

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
- **The preserved patch** (`scanGatedPatch` in `agent/src/runner.ts`, `patchScanVerdict`,
  `patchImages`): the raw patch is withheld whole, checked in this order, when the claim is over
  budget; when a whole-value form occurs in it (`raw_value`); when any registered line form occurs
  (`line_form`); when the run holds any multi-line secret (`multi_line_secret`), meaning any live
  secret or retained version whose value contains a CR or LF, of any length, so a value saved with a
  trailing line break counts; and, for a run whose secrets are all single-line, when a git-quoted
  header path decodes to a secret form (`header_path`), when it cannot be parsed (`unparseable_patch`:
  binary, combined diff, a line-count mismatch, or a stray line such as a non-header line between a
  file header and its first hunk), or when a whole-value form occurs in a rebuilt per-hunk pre-image
  or post-image (`hunk_image_value`; context plus removed or added lines, also with line breaks
  normalized to LF). The multi-line rule closes the gaps of a secret split across files or by elided
  context between hunks, which no check on fragments below the line-form floor can close; the work
  stays recoverable through the run's custody (a held ref via `uzi run held`, or a recovery capture via `uzi run export`). With
  every secret single-line, `hunk_image_value` is a backstop only: a single-line form holds no line
  break, so a single-line hit is already `raw_value`. The image and parse checks apply when the run
  has any secret variable, retired versions included. Otherwise the patch is redacted and
  gitleaks-scanned as before.

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

## Global CLI config input

The M2 audit classifies one more file input to the Claude CLI: its global config. The pinned CLI
(2.1.325, SDK 0.3.325; first measured at 2.1.319) applies an `env` block from it (for example
`OTEL_LOG_RAW_API_BODIES`, `NODE_OPTIONS`, `ANTHROPIC_BASE_URL`) regardless of
`settingSources: []`, with the provider credential already in its environment. By default that
config is `<HOME>/.claude.json` (or `<HOME>/.claude/.config.json`, which the CLI can prefer), and
the agent can write both.

**The `OTEL_LOG_` rule does not cover this path.** The `OTEL_LOG_` prefix in "Name rules, with
evidence" filters run variables only. A run-written env block in a config file reaches the CLI
without passing the name rules.

**Where it lives and who writes it (Claude lead lane).**

- `HOME` is `agent-home/<runId>` (`sdkHomeRoot` in `agent/src/main.ts`; the worker creates it with
  mkdir in `agent/src/sdk-executor.ts`; `agent/src/sdk-env.ts` sets `HOME`).
- The CLI runs as the runner uid (`spawnDetached` in `agent/src/sdk-spawn.ts`, through
  `runnerSpawn`), and so does the agent's Bash tool and everything it runs (repo scripts,
  package-manager hooks).
- Every Claude CLI start now runs with `CLAUDE_CONFIG_DIR=<HOME>/.uzi-claude-config`
  (`claudeConfigDir` in `agent/src/claude-config-dir.ts`), a worker-owned directory the runner
  cannot write. The mechanism and its limits are in the header of
  `agent/src/claude-global-config.ts`, which is the authoritative description.

**Trust policy.** The CLI reads its configuration only from the worker-prepared directory. Before
every Claude CLI start in every lane, the worker:

1. builds or reuses `<HOME>/.uzi-claude-config` holding exactly a fixed `.claude.json` (mode 0444,
   the bytes `CLAUDE_GLOBAL_CONFIG_BYTES`, no `env` key) and two relative symlinks,
   `projects -> ../.claude/projects` and `tasks -> ../.claude/tasks`. The directory is mode 0755
   under the uid split (the runner cannot write it) and 0555 with one uid;
2. renames anything unexpected at that path aside as `.uzi-claude-config.stale-<hex>` (a file, a
   symlink, another owner or mode, an extra entry, a different link target). It never opens the
   path through a symlink and never deletes anything in it;
3. creates the `HOME/.claude/projects` and `HOME/.claude/tasks` targets as the runner uid with a
   fixed `/bin/mkdir -p -m 700` argv, so they are runner-owned and private, as when the CLI made
   them. The worker never writes or renames anything inside `.claude`. When that mkdir exits
   non-zero, a second runner-uid helper (`/bin/stat -c '%u|%g|%a|%F|%n'`) looks for a positive
   obstruction: a matched non-directory, or a `.claude` whose effective mode bits for the runner
   (owner bits if it is owned by the runner uid, else group bits if its gid is one of the runner's
   groups 10002 or 10004, else other bits) lack write or search. A foreign owner or a missing read
   whose bits still grant write and search is not an obstruction: that start is refused
   transiently and `.claude` stays. The evidence is mode bits, so ACL entries are not seen: an ACL
   granting access to a foreign-owned `.claude` (set by the directory's owner or a privileged
   process) could still read as an obstruction, but only after the mkdir has already failed for
   another reason, since the stat probe runs only after a failed mkdir; and an ACL
   mask can hide a deny, which fails closed as a refusal. Only on a positive obstruction is `.claude` renamed aside once and retried (transcripts stay in the aside
   copy, so a resume of that session finds none); a second obstruction refuses the start. Any
   other failure (spawn error, signal, timeout, an exit status other than 0 and 1, or exit 1
   with a lookup failure, a capacity failure, no stat output or a killed helper) refuses that
   start only and leaves `.claude`, and the shared chat root's transcripts, in place;
4. refuses the start, with no spawn, when it cannot do the above. When `HOME` is set,
   `spawnDetached` also refuses unless `env.CLAUDE_CONFIG_DIR` equals that exact path; a start that
   carries a provider credential but no `HOME` is refused (the CLI would fall back to the runner's
   passwd home).
   `CLAUDE_CONFIG_DIR` is a protected env key (`agent/src/sdk-env.ts`), so a run variable cannot
   set it.

With `CLAUDE_CONFIG_DIR` set, an env block planted in `HOME/.claude.json`, `HOME/.claude/.config.json`
or `HOME/.claude/settings.json` was not applied in the probes below (with controls), and neither was
an unsigned one in `HOME/.claude/remote-settings.json` (no positive control). The earlier
reset of `HOME/.claude.json` and removal of `HOME/.claude/.config.json` are therefore gone, and no
runner-uid removal under `HOME/.claude` remains.

`spawnDetached` is the one seam: lead, chat, isolated, advice and model-pass, job and cross-check
all start the CLI through it. The other lanes have no Bash tool. Chat and cross-check have no
write tool either (the chat tools in `agent/src/chat-executor.ts`; `CHECKER_SURFACE` in
`agent/src/claude-cross-check.ts`); the isolated and job lanes have Write and Edit
(`ISOLATED_TOOLS` in `agent/src/isolated-executor.ts`, reused by `agent/src/job-runner.ts`), held
to the run workspace by that lane's path guard (`buildPathGuardHook`), which excludes their HOME.

**Boundary.** Under the uid split HOME must be a real directory owned by the worker. The worker
adds the sticky bit to it, so the group-writable runner can create entries in HOME but cannot
rename, replace or unlink the worker-owned config directory, and the directory grants the runner
no write. Whether HOME's own entry can be swapped depends on the data-root layout, which this
module relies on and does not create (`agent/templates/entrypoint.sh`):

- Per-run HOMEs, and the cross-check and model-pass HOMEs (`mkdtemp` in the same root,
  `homeRoot: sdkHomeRoot`), live in `agent-home`, a sticky 3775 root (setgid, group-write, sticky),
  so a runner-group member that does not own an entry cannot rename or replace it.
- Job HOMEs live under the `jobs` root, in per-run trees that are 2750 under the split (`agent/src/job-workspace.ts`).
- The data root on a hosted volume is 2775 with group 10001, restored by `align_pvc_root` when the
  epoch lock is not in use. It is group-writable and not sticky, and the chat HOME is `agent-home`
  itself, so the worker does not refuse that parent: refusing it would stop every chat start.
- `guardHome` (`agent/src/claude-global-config.ts`) checks HOME itself (a real directory, not a
  symlink, its own real path, owned by the worker) and then HOME's parent and every ancestor up to
  `/`, each by `lstat` (no symlinks): a real directory, owned by uid 0 or the worker, not
  world-writable unless sticky, and not group-writable with a runner, runner-cmd or codex-session
  gid (10002, 10003, 10004) unless sticky, since the runner could then rename the entry below.
  Any other layout refuses the start. The hosted 2775 data root (group 10001) is not in that set.

**Single-uid posture.** Claude run-variable delivery requires the protected two-uid worker
(`UZI_UID_SPLIT=1`, set by the root-started entrypoint; on k8s the opt-in is
`UZI_WORKER_UID_SPLIT`, and compose workers are uid-split by default). A single-uid worker does not advertise
`run_env_v1` (`agent/src/worker.ts`, `uidSplitActive()`), so the api does not place a run that
carries variables (or secret receipts) on it; such a run waits for a two-uid worker. If one is
claimed anyway, the worker refuses it before starting anything (`assertRunEnvDeliverable`,
`REASON_RUN_ENV_NEEDS_SPLIT`, in `agent/src/sdk-env.ts` and the executor's setup phase). Ordinary
single-uid launches without variables still start, with the config directory prepared as 0555 and
owned by the same uid. That gives no protection against the owner's own uid and is not an
integrity boundary.

**Single-uid limit.** With one uid (a k8s single-uid start, or the unit tests) the CLI and the
agent are the same uid as the worker, so the directory gives no protection: the agent can alter it
between the worker's last check and the CLI's read. The checks then only repair what an earlier
turn left, and no ownership or ancestor check is made. This is the PRD's accepted shared-uid
residual, and it is why variables are not delivered there. Kernel enforcement of the sticky and
ownership boundary cannot be shown with one uid and is not unit-tested; the two-uid e2e below is
the evidence for the split.

**Codex.** There is no agent-reachable writer. Per-launch HOME and CODEX_HOME trees are
runner-owned 0700 (`agent/src/codex/launcher.ts`), `config.toml` is regenerated by the worker at
every launch from the launch spec and never read back, commands run as `COMMAND_UID` 10003 (`agent/src/runner-uid.ts`), the screener
denies `codex-data/`, and Codex runs only under the uid split (`agent/src/codex/codex-capability.ts`).
Pinned by `agent/test/codex-launcher.test.ts` "(c) creates the fresh trees 0700 as the runner uid
via the injected step".

**Spill Read allowance (ADR-2332).** The CLI names its transcript through `CLAUDE_CONFIG_DIR`, so
`sdkSpillRoots` in `agent/src/guardrails.ts` accepts the transcript path under
`<HOME>/.uzi-claude-config/projects` as well as `<HOME>/.claude/projects`. The real root stays
`realpath(HOME)/.claude/projects`, so a retargeted `projects` link resolves outside it and gets no
allowance. See [ADR-2332](2332-sdk-spill-read-allowance.md).

**Tests.**

- `agent/test/claude-global-config.test.ts`: the exact directory and private targets, reuse of a
  good directory (same inode, no stale copy), rebuild of a tampered one-uid 0555 directory (extra
  entry, retargeted link, other bytes, other mode), symlinked `.claude` and `projects` moved aside
  and never followed, a regular file at `.claude/projects` or `.claude/tasks` recovered, the
  runner mkdir failing with a positive obstruction once (rename aside and retry) and twice
  (refusal), a mkdir failure with no positive obstruction (refusal, `.claude` kept), the
  preparation deadline and the bounded helper, the `spawnDetached`
  refusals (mismatched `CLAUDE_CONFIG_DIR`, credential without `HOME`), the HOME and ancestor
  cases (a non-directory, a foreign owner, world-writable non-sticky or runner-gid group-writable
  non-sticky refused at any level up to `/`; hosted 2775 and sticky accepted), per-step hostile-process scenarios, concurrent prepares, and `rmHomeTree`
  removing the HOME with the directory and its aside copies.
- `agent/test/sdk-executor-global-config.test.ts`, through the real executor and spawn path: the CLI
  process is pointed at the prepared directory and sees the fixed bytes (its fake CLI reads only that
  directory). It does not show that a planted env block never reaches the CLI; the claim that planted
  old-location files are ignored rests on the "Planted config ignored" CLI probe below.
- `agent/test/sdk-env.test.ts`, `agent/test/sdk-spawn.test.ts` and `agent/test/guardrails.test.ts`:
  the `CLAUDE_CONFIG_DIR` env value and protected key, the spawn seam, and the config-dir
  transcript path in the spill allowance.
- The red/green mutation records of the earlier reset design no longer apply: that design is
  replaced by the directory above.

**Probes of the pinned CLI 2.1.325** (fake provider key, loopback servers only; no secret and no
real account involved). Command shape, one per run:
`env -i PATH=/usr/bin:/bin HOME=<home> ANTHROPIC_API_KEY=<fake> ANTHROPIC_BASE_URL=http://127.0.0.1:<port> DISABLE_TELEMETRY=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 CLAUDE_CONFIG_DIR=<home>/.uzi-claude-config timeout <T> <claude> -p hi </dev/null`,
against a fake Messages API (a second loopback server served as the "planted" target to detect
any redirect). Every probe used `-p` mode; SDK mode was not probed. The tool probes add `--permission-mode bypassPermissions --allowedTools <Tool>` and a
fake server that answers with a `tool_use` and then ends the turn. Results:

- **Layout.** Only `.claude.json` (0444, `{"hasCompletedOnboarding":true,"numStartups":1}`) and
  `projects` present: stderr is empty. With both `.claude.json` and `.config.json` present the CLI
  prefers `.config.json` and prints a one-line warning on every start, even for identical bytes;
  hence only one file.
- **Saves.** In a 0555 directory the CLI starts, reaches the API, and its saves fail silently: no
  temp files, no new entries. In a writable 0755 directory it atomic-renames over the 0444 file
  (the file is replaced with the CLI's content, mode kept) and creates `backups/`, `plugins/`,
  `sessions/`, `.last-cleanup`. A writable directory therefore gives no protection, which is why
  one uid gives none.
- **Planted config ignored.** A `HOME/.claude.json`, `HOME/.claude/.config.json`,
  `HOME/.claude/.claude.json` and `HOME/.claude/settings.json`, each with an `env` block pointing
  `ANTHROPIC_BASE_URL` at the second server, had no effect with `CLAUDE_CONFIG_DIR` set (all
  requests reached the real base, none the planted one; directory 0755 and 0555). The controls
  without `CLAUDE_CONFIG_DIR` applied them (the planted server received the requests) for three of
  the four planted files; there is no control for `HOME/.claude/.claude.json`, so that file's
  being ignored is shown only by the absence of an effect.
- **R2, a write during a running CLI.** The detector is `OTEL_LOG_RAW_API_BODIES=file:<dir>`, which
  makes the CLI write a request file per API call into `<dir>`. A control with the `env` block
  present at startup wrote request files (so the detector is sensitive and the block is applied at
  startup). An `env` block written into the config by atomic rename five seconds into a 40 second
  run (both files, `.claude.json` only, `.config.json` only) wrote no request file and redirected
  nothing. This shows the config is not re-read during a running process, with a fake API
  returning 500 and the CLI retrying (no completed turn), in `-p` mode only, not SDK mode.
- **R4, an unsigned remote settings file.** `{"env":{"ANTHROPIC_BASE_URL":"<planted>"}}` in
  `remote-settings.json`: an unsigned env block in it was not applied (fake-key, no-account
  session; the probe has no positive control, so it does not show the CLI never reads the file) in
  the config directory, in `HOME/.claude`, in both, and with `CLAUDE_CONFIG_DIR` pointed at
  `HOME/.claude`. An empty read-only file and `{}` also
  start cleanly. **Scope: this was a no-account session with a fake key only.** A real OAuth
  subscription login was not tested; see "Acceptance decision" below.
- **Tools.** With a 0555 directory holding `.claude.json` and `projects`: Bash, Read, Write and
  Edit work. TaskCreate fails with ENOENT without a `tasks` directory (the CLI cannot create it)
  and works with `tasks -> ../.claude/tasks`. `TodoWrite` is unavailable in `-p` mode in both
  modes. Not linked: `session-env` (its scripts run ahead of Bash commands), `shell-snapshots`
  (sourced by Bash), `plugins`, `hooks`, `skills`, `commands`, `agents`, `rules` (configuration and
  code). Interactive and SDK-mode `TodoWrite` were not probed.
- **Transcripts and resume.** The CLI writes `projects/<slug-of-cwd>/<session>.jsonl` through the
  link into `HOME/.claude/projects`, with the directory at 0755 and at 0555. `--resume <session-id>`
  and `-c` (same cwd) appended to the same file and were probed with the directory at 0755 only
  (the 0555 tool probe covered tools, not resume); a made-up session id exits 1 (so resume does
  find real sessions).

**Two-uid evidence (e2e).** `e2e/home-uid-split/fixture.test.ts`, run with
`task test:home-uid-split` against a freshly built worker image of the final tree of this change
(re-run at the final tree; see the PR verification list). The case "the
runner cannot change, move or replace the prepared Claude config directory, and a re-prepare reuses
it (#4416)" shows that the runner cannot write, replace, rename or chmod the config
directory or its file and that runner-cmd cannot write or `mv` it (runner-cmd chmod was not
checked), and that neither can rename HOME; that a re-prepare keeps the same inode; and that
`rmHomeTree` reclaims a HOME holding the directory. The case "a runner-planted file at
.claude/projects does not refuse the start: .claude is moved aside and rebuilt (#4416)" shows the
recovery described under R3.

**Review items R1, R2, R3, R4.**

- **R1, a live runner-uid writer: fixed under the uid split.** A live runner-uid process can no
  longer rewrite the config or swap it between the worker's check and the CLI's read, because the
  directory and its file are worker-owned and the runner cannot write or move them (e2e above). A
  live process can still plant files elsewhere under HOME: configuration files there were not
  applied in the probes, but the CLI does read the runner-writable `HOME/.claude/projects`
  (transcripts, per-project memory) and `HOME/.claude/tasks` through the links, as before. With one
  uid the limit above applies.
- **R2, a write during a running CLI: probed, `-p` mode only.** In the probe the config was not
  re-read during a running process (fake API returning 500, CLI retrying, no completed turn; SDK
  mode not probed), and the runner cannot write the file anyway under the split.
- **R3, a planted entry at a path the worker needs: recovered, with several fail-closed cases;
  follow-up documented.** A file, symlink or wrong-mode entry at `<HOME>/.uzi-claude-config`, or a
  non-directory `.claude`, `projects` or `tasks` is renamed aside and rebuilt, so a planted entry no
  longer refuses every later start of a HOME, the shared chat HOME included. What still fails
  closed (the start is refused, nothing is spawned):
  - the rename aside itself failing (for example a read-only data volume);
  - a second positive obstruction after `.claude` was moved aside once;
  - a mkdir failure with no positive obstruction (exit 1 with a lookup failure, a capacity failure,
    no stat output, a killed helper, or a foreign owner or missing read whose effective bits still
    grant write and search; a spawn error, signal, timeout, or a status other than 0 and 1): that
    start only, `.claude` kept;
  - a non-zero mkdir of `.claude` itself;
  - the preparation passing its 10 s deadline (checked before each helper spawn);
  - a HOME that fails the boundary checks (see Boundary: not worker-owned under the split, or an
    ancestor that is not a real directory, is owned by neither root nor the worker, is
    world-writable and not sticky, or is group-writable by a runner group and not sticky);
  - the directory changing past the bounded number of attempts ("kept changing"), and the same
    bound on the `.claude` targets;
  - a HOME that is not a canonical absolute path or not a real directory, and a
    `CLAUDE_CONFIG_DIR` that is not exactly `<HOME>/.uzi-claude-config`.

  A non-zero mkdir exit alone moves nothing, because exit 1 is not proof of a planted entry (mkdir
  also exits 1 on ENOSPC, EDQUOT or EIO, and `setpriv` exits 1 when it cannot resolve the runner
  user). `.claude` is moved aside only on a positive obstruction reported by the runner stat probe
  (a matched non-directory, or effective mode bits for the runner, by owner, runner group or other,
  that lack write or search; a foreign owner whose bits still grant both is not one, and ACLs are
  not seen); a lookup failure, a
  capacity failure, no output or a killed helper refuses that start and keeps `.claude` in place.
  There is no exit-1 residual.

  Asides (`.claude.stale-*`, `.uzi-claude-config.stale-*`) are not
  reclaimed automatically in a HOME that is never torn down, such as the shared chat root; each
  start makes at most a bounded number, and across starts they accumulate until removed. The run, or the
  chat, then fails with the refusal "Claude CLI config directory could not be prepared by the
  worker" or "Claude CLI config directory refused: <reason>". Operator recovery by class: make the
  data volume writable; check HOME and its parent modes against the entrypoint layout
  (`agent/templates/entrypoint.sh`); for a transient failure (load, spawn) start again. When space matters, remove
  the aside copies, `agent-home/.uzi-claude-config.stale-*` and `agent-home/.claude.stale-*` (and
  the same names under a per-run `agent-home/<runId>`); a HOME that is reclaimed takes its own asides
  with it (`rmHomeTree`). The owner marked this documented, not further automated.
- **R4, other agent-writable CLI inputs: probed in a no-account session only.** An unsigned env
  block in `remote-settings.json` was not applied (fake-key, no-account session; no positive
  control), including with the file in `HOME/.claude` while `CLAUDE_CONFIG_DIR` is set. Managed settings
  (`/etc/claude-code`) are root-owned and absent from the image.

**Acceptance decision (verification moved after the release).** The maintainer accepts landing on
the current direct-CLI evidence within its stated limits (the probes above: pinned CLI, `-p` mode,
fake key, loopback servers, no real account; the two-uid e2e exercises the worker's own steps, not
a CLI save). Verification through the pinned SDK launch path moves to a post-release acceptance
check: after the release rolls, a real Claude run confirms authentication, completion and resume.
It is a compatibility check only. It does not establish that agent-writable settings are ignored
and does not resolve the remaining R2 and R4 uncertainty (a real OAuth subscription session
against an unsigned stored remote-settings file, and a write during a running CLI in SDK mode,
stay untested). R3 stays a follow-up that the maintainer files.

**Known limit: the preparation is synchronous and bounded, not unbounded.** Every error in the
preparation, a full disk included, is reported as this refusal, not as a disk park. The bound is:

- each runner helper runs behind a constant worker-uid Node intermediary (`process.execPath -e`,
  argv only, no shell) that `spawnSync` kills with SIGKILL after 4 s (the worker uid can always
  deliver it). The intermediary exits 127 on any failure before or during the helper's spawn (including its own exceptions) and 128+n when the helper
  dies on signal n; the output limit stays 64 KiB. Dropping the shell closes CodeQL
  `js/shell-command-injection-from-environment` alert 229, pending the PR's CodeQL re-scan. Node's
  cold start per helper is unmeasured;
- the runner helper itself runs under `timeout -s KILL 2`;
- the whole preparation has a 10 s deadline, checked before each helper spawn.

The preparation is synchronous, so the worker's event loop is blocked for its duration: the worst
case is about 14 s per preparation (the 10 s deadline is checked before a spawn, so a last helper
can still run up to 4 s past it). The worker heartbeats every 15 s and the server marks a worker
stale after 45 s (`WORKER_HEARTBEAT_INTERVAL`, `WORKER_HEARTBEAT_STALE`), so one preparation
cannot reach the stale threshold, but several back-to-back worst-case preparations on one worker
could. A helper blocked in uninterruptible I/O (or a synchronous fs call on a hung volume) is not
bounded. An async spawn was not adopted: the enforced bound keeps the worker-uid kill and the
output file handling simple, at the price of the blocked loop above (PRD Decision Log).

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

Owner decisions of 2026-11-22 on the exceptions that earlier sat as pending (PRD #4416 Decision Log).
The split-across-files and elided-context patch gaps are closed by the `multi_line_secret` rule above.

- **Accepted, tightened: the PEM armor and whitespace-only carve-outs** (B1, B2), as defined under
  "Redaction and the exact-value scan". The whitespace-only carve-out uses ASCII space and tab only,
  which closes the earlier residual that it used JavaScript's `\s` (NBSP and other Unicode spaces).
- **Fixed: the redaction cost for periodic secret lines** (B3), by the step-counted work guard
  described under "Redaction work guard". It is no longer a residual; the radix-trie index (PRD
  Decision Log) and the guard together bound the cost.
- **Accepted as documented exact-match limitations: short and re-encoded fragments** (B4). A
  fragment of a secret line shorter than 8 characters, or a line whose own characters change on the
  way out (re-indented to less indentation, folded, or in an unregistered encoding), is not masked in run
  output. A line copied with extra indentation is still masked, since redaction matches it as a
  substring. The blob scan still blocks a committed file that holds the whole value.
- **Accepted with explicit grandfathering: a replayed terminal report from an older worker** (B5).
  Only terminal reports already journaled before this change, including by pre-release builds, may
  replay with the preserved patch they carry. An upgraded worker never mints a new multi-line-secret
  patch: `patchScanVerdict` (`agent/src/run-env-scan.ts`) withholds it with reason
  `multi_line_secret`. The immutable-journal and retry contract is unchanged: the terminal journal
  `terminal-<claim_generation>.json` under `<dataDir>/outbox/<runId>/` (`agent/src/outbox.ts`) is
  MAC-authenticated, the first durable writer for a generation wins, and the body is replayed
  verbatim by `listPendingTerminals` then `resolvePendingTerminal` (`agent/src/terminal-resolve.ts`).
  `sweepRetention` in `agent/src/outbox.ts` age-removes a run only when it has no pending manifest
  records, terminal journals, finalize outcomes or publication outcomes. A run with a pending
  terminal, finalize or publication outcome is never aged out, so `WORKER_OUTBOX_RETENTION`
  (`docs/configuration.md`) is not an upper bound on the set of affected journals.

  Identification procedure for affected pre-release deployment journals (steps to follow; none has
  been run by this change):

  1. Audit every retained source that may predate the release, whatever its age: worker data
     volumes and any backups or snapshots of them. List the terminal journals under
     `<dataDir>/outbox/<runId>/`.
  2. A journal is affected when its `body.preserved_patch` is non-empty, its run's repo held a
     multi-line secret run variable, and it was written by a worker build from before this change.
     A journal may be excluded only by matching it against that run's exact delivered secret
     history, retained versions included; the current repo configuration alone cannot exclude one.
  3. Classify as unknown, never unaffected: a run whose delivered history is incomplete; a journal
     whose MAC cannot be verified (the worker's key is missing or unreadable, or the file is
     truncated or malformed); and a journal with an unknown writer. The terminal journal
     (`TerminalJournalData` in `agent/src/outbox.ts`) records no writer build identity, so the
     writer is known only from deployment records.
  4. Keep every affected or unknown journal retained until an explicit owner resolution is
     recorded for it, without copying the report's contents. Manual journal deletion is not a
     retirement mechanism. A running worker, old or upgraded, replays pending journals itself and
     can delete one on several api answers, some of which deliver nothing (`agent/src/terminal-resolve.ts`);
     no mechanism lets an owner hold a journal against replay. Retention until the owner
     resolution therefore means no worker runs on a source that may hold one until that resolution
     is recorded. Every run on the source with no remaining journal is a candidate for an earlier
     deletion: it is resolved only when the api-side delivered report for that claim generation is
     itself shown unaffected (no multi-line-secret preserved patch), and is otherwise unknown.

  The maintainers run this check through a private authenticated inventory with exact correlation
  against delivered history, and record metadata-only results with owner dispositions.

  Deployment check: pending - maintainers supply the evidence; no fleet check has been performed by
  this change.

## Not shipped in M2

M3 (a per-run Docker client configuration directory, so a registry login lasts for the run) and
M4 (user-level variables) are not implemented; the user doc says logins do not persist across
commands on Codex. The compose E2E scenarios are authored and run at landing; they are
not evidence here.
