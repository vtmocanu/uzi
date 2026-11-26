---
title: Run environment variables
order: 116
audience: user
---

# Run environment variables

Give a repo's runs named settings and credentials, such as a database URL or a
package index token. Each variable belongs to one repo and is either **secret**
or **plain**. Values are sealed through your per-user vault.

Set them under **Repos → Environment variables**. Writes are web-only. The CLI
can list a repo's variables read-only: `uzi repo env list <id>`
([CLI reference](./cli.md)).

## Which runs get them

A run receives its repo's enabled variables as environment variables in every
command its agent runs, on both the Claude and the Codex harness, the way a CI
job receives CI variables. Tools that read the environment work unchanged.

- **Run kinds**: runs that have a lead agent working on the repo (issue, prompt,
  CI-fix, MR-rework, self-improve and task runs). Review runs, chat, judge, job
  and cross-check runs, and runs bound to an egress profile, get none.
- **Claim time**: a run gets the set that is current when it is claimed. A
  variable you edit, disable or delete afterwards reaches the run at its next
  claim (a resume re-claims); a run keeps the set it was given until then. A
  description-only edit does not interrupt a claim in progress, so a run can
  carry the previous description.
- **Precedence**: user variables have the lowest precedence. A name the worker
  sets itself is never replaced or removed by a variable, and the worker drops
  a variable whose name breaks the rules below, with a run event naming it.
- **Worker support**: delivery needs a worker that reports `run_env_v1`, and a
  worker reports it only when the worker and the agent run as separate users
  (the protected two-uid worker: compose workers by default, k8s with
  `UZI_WORKER_UID_SPLIT`; see [Configuration](configuration.md)). A run whose
  repo has variables is never claimed by an older or single-user worker, and
  run health says "no online worker supports repo variables (run_env_v1); repo
  variables need a current two-uid worker (UZI_UID_SPLIT, or UZI_WORKER_UID_SPLIT
  on hosted workers)" while none is online. Such a run waits for a two-user
  worker; if a single-user worker is somehow handed one, it refuses the run
  before starting anything. Cross-check children get no variables, but one claimed
  by a worker without `run_env_v1` ends as checker unavailable, which asks for a
  human decision; upgrade the worker to avoid it.

## Secret or plain

- **Secret**: the value is never shown again, in the web UI, the CLI or the
  API. It is redacted from run output, and every publish of the run's work is
  searched for it (see below).
- **Plain**: the value stays readable, including in `uzi repo env list`, and is
  **not** redacted or scanned. Use it only for non-sensitive settings such as
  URLs and regions.

## What the agent is told

The lead's prompt lists each injected variable's name, scope and description,
and the value of each plain variable. A variable the worker did not inject,
because its name is protected or the tool profile already sets it, is not
listed. A secret's value is never in a prompt. The
block states that the variables are already set in every command, so the agent
does not have to guess or ask. Descriptions and plain values are treated as
untrusted text: control and invisible characters are stripped or escaped.
Describe a variable so the agent knows what to do with it, for example which
login command uses it.

## Redaction and the publish scan

For each secret, uzi redacts the value from run messages, the worker log, the
plan and its summary, and the other output the run produces. It also redacts
these forms of the value: JSON-escaped (with and without escaped slashes),
HTML-safe `\u00XX`, percent-encoded, and base64 or base64url text that embeds
it (such as the entry a registry login stores, or an HTTP Basic header). Each
form is also registered without a trailing line break, and for a multi-line
value with LF line endings. Every line of a multi-line value that is at least 8
characters long is redacted on its own, with no condition on letter case,
digits or key name. Two kinds of line are left out, both accepted carve-outs:

- **PEM armor.** Only a line exactly equal to `-----BEGIN <LABEL>-----` or
  `-----END <LABEL>-----` for one of these labels: `PRIVATE KEY`,
  `RSA PRIVATE KEY`, `EC PRIVATE KEY`, `DSA PRIVATE KEY`,
  `ENCRYPTED PRIVATE KEY`, `OPENSSH PRIVATE KEY`, `PUBLIC KEY`,
  `RSA PUBLIC KEY`, `CERTIFICATE`, `TRUSTED CERTIFICATE`,
  `CERTIFICATE REQUEST`, `NEW CERTIFICATE REQUEST`, `X509 CRL`, `PKCS7`, `CMS`,
  `EC PARAMETERS`, `DH PARAMETERS`, `ATTRIBUTE CERTIFICATE`. A marker with any
  other label (PGP armor, for example), an indented marker, or a line that
  carries anything besides the marker is redacted like any other line.
- **Whitespace-only lines.** Only lines made of ASCII spaces and tabs. A line of
  NBSP or another Unicode space (U+2000 to U+200A, U+3000 and so on) that is 8
  or more characters long is redacted like any other line.

So structural lines of a secret file, such as `apiVersion: v1` or
`"type": "service_account",`, are masked wherever they appear in run output.
Redaction is exact-match with an 8-character floor, so it is a net, not a boundary: another
encoding of the value is not caught, and a fragment of a line shorter than 8
characters is not masked (accepted limitations; see the end of
[Redaction and the publish scan](#redaction-and-the-publish-scan)).

Redaction work is bounded by a step count, not a clock. The matcher charges each
literal comparison against a budget of 2^26 units per redacted string (one
shared budget per payload), so a string costs at most the budget plus one plus
the longest literal. When a string passes it, the whole string is replaced by
`[withheld: redaction work budget exceeded]`, never shown raw or half
redacted, and the rest fails closed:

- Payload strings that still need a comparison are withheld the same way; the
  worker log writes one fixed JSON record (timestamp, level and a fixed message,
  with no other fields) in place of the record.
- The preserved-patch secret check treats every form as present, so its verdict
  withholds, and the preserved patch is not shipped.
- The run's status, id, claim generation and failure origin never pass through
  the redactor and still reach uzi; the failure reason becomes the marker.
- The charge is a conservative bound, so very large or prefix-dense text with no
  secret in it can also be withheld.
- The git object and path scans are not redaction. They are capped by a
  deadline and by bytes, and fail closed (the publish is withheld), so input
  with a repeating pattern can exhaust their deadline but cannot make them pass.

A run's secret forms are computed once per claim and capped at 40,000,000
characters (`RUN_ENV_FORM_CHARS_MAX`). A claim over the cap fails before any
work, with a failure reason starting `run_env_forms_over_budget` (never a
value), and publishes nothing. A publish with no live flight (held, terminal and
bundle publication) is withheld with the reason `forms_over_budget`. A
cross-check child whose claim is over the cap ends as checker unavailable
instead. To stay under it, uzi refuses a save at write time: see
[Secret size budget](#secret-size-budget). A run keeps counting the older
versions it was given; a new run counts only the current values, and a
cross-check child counts every version its lead was given.

### Secret size budget

A save is refused with 409 `env_forms_budget` when it would raise the repo's
counted secret value bytes above 116,959 bytes (40,000,000 form characters
divided by a conservative bound of 342 form characters per value byte). The
count includes the copy of each redaction form that the worker's logger
indexes JSON-escaped, and the bound is the worst case, a control character.

- Counted: every secret row of the repo and of the user scope, in any state
  (live, disabled, pending deletion or retired). Plain values do not count.
- A save is refused only when it would raise the count and leave it above the
  budget, so a store already over the budget can shrink or stay the same size,
  but not grow.
  **Disable**, **Enable** and **Delete** are never refused for budget reasons.
- Disabling a secret frees no budget. Deleting a secret that a run still holds
  frees it once the held version is released and the sweeper removes it.
- The bound is analytic: `fixtures/run-env/form-budget.json` carries the numbers
  and an agent test pins the form counts it assumes, but it is not proved for
  every value. The claim-time cap above stays as the backstop.

Before work leaves the worker, every commit in the outgoing range is searched
for those forms, not only the final tree. For a multi-line secret it also
searches for the value as it reads inside a diff, with its lines joined by
`+`, `-` or space prefixes. That covers checkpoint pushes
(including parked, paused and shutdown checkpoints), the final push, held and
terminal preservation packs, recovery archives and the preserved patch.

- The search covers paths too. Every commit scan above except the preserved
  patch (and the provider-key scan) matches the forms against the full path of
  every tree entry an outgoing commit introduces, including empty trees and submodule entries. Paths are read as raw
  NUL-separated names, so git path quoting does not hide a value, and a merge is
  read through its combined diff. A finding on a path shows `[path withheld]`
  in place of the file name.
- A hit blocks the publish and names the **variable**, never the value. At the
  final push the run fails as `push_secret_blocked`; a checkpoint hit is held by
  the existing secret-remediation hold. A secret that is a common word can
  trigger this; pick a longer value.
- The scan **fails closed**. If it cannot prove a range clean (a timeout, an
  unreadable range, a scanner error, or the run's secret history cannot be
  looked up), a checkpoint, pack or recovery upload is withheld and retried
  later, and the work stays on the worker under its recovery hold. At the final
  push the run fails as `push_secret_blocked` instead. Path enumeration is
  bounded at 1,000,000 paths, 256 MiB of path bytes and the scan deadline; past
  any of them the publish is withheld with the reason `path_scan_incomplete`.
- A plain value found in tracked files holds nothing.
- The preserved patch is withheld, not redacted, in this order: when the claim
  is over the form cap; when a whole-value form occurs in the raw patch (reason
  `raw_value`); when any registered line form occurs in it (`line_form`); and
  when the run holds any multi-line secret (`multi_line_secret`). That means any
  live secret or retained version whose value contains a line break, of any
  length; a secret saved with a trailing line break counts. For a run whose
  secrets are all single-line it is also withheld when a git-quoted header path
  decodes to a secret form (`header_path`), when the patch cannot be parsed
  (`unparseable_patch`: binary, combined diff, a line-count mismatch, or a stray
  line such as a non-header line between a file header and its first hunk), or
  when a whole-value form occurs in a rebuilt per-hunk pre-image or post-image
  (`hunk_image_value`, a backstop only: a single-line value holds no line
  break, so a hit is already `raw_value`). The image and parse checks apply when
  the run has any secret variable, retired versions included, so such a run
  loses its preserved patch when the diff touches a binary file. Otherwise the
  patch is redacted and scanned as before.
- A run with a multi-line secret therefore has no preserved patch. Its work is
  not lost: it stays in the run's custody, as a held ref (`uzi run held`, then
  `git fetch`) or a recovery capture (`uzi run export`).
- Accepted limitations: a fragment of a secret line shorter than 8
  characters, or a line whose own characters change on the way out (for example
  re-indented to less indentation, folded, or in an encoding other than those
  registered), is not masked in run output. A committed file that holds the
  whole secret is still blocked.
- A terminal report that a worker built before this change had already
  journaled with a patch can still replay with that patch after an upgrade. An
  upgraded worker never creates a new patch for a run holding a multi-line
  secret.
  Such a journal is kept while its outcome is pending, whatever its age.
  Maintainers audit retained pre-release journals before rollout and resolve
  each one explicitly rather than deleting it.
- Known gap: the planning diff sent to a cross-check and the diff context in a
  pull request description are only redacted, not path-scanned. A secret with
  non-ASCII characters used as a path name reaches them octal-escaped
  (C-quoted) and unredacted.
- The list of changed files sent with a plan turn, the file paths in capability
  inference evidence, and a proposal's repo path and id pass the run's redactor
  (a quoted path is decoded first). Park and recovery reports carry only fixed
  values (cause and reason codes, counts, SHAs, booleans, rate-limit type and
  reset time), never run variables. Judge and task-review runs never receive
  variables.
- A worker that must look up a finished run's secrets is served them only while
  the run is active or a publication of its work is still in flight. A cross-check
  child is served only while its check is open and before its deadline, or while
  a publication of its work is in flight. Otherwise
  the publish is withheld.

## Rotating and deleting while runs are active

Rotating a secret keeps its older value sealed so runs that were given it can
still mask it. The old value is kept until every such run has ended, its
retained work has been released, and 24 hours have passed. Until then:

- a write that would exceed the retired-value limit is refused with
  `env_history_full`, and the panel lists the runs holding versions;
- a save that would raise the repo's secret value bytes above the form budget is
  refused with `env_forms_budget` (see [Secret size budget](#secret-size-budget));
- **Disable** and **Delete** always work. A deleted secret that runs still need
  shows **Pending deletion**.

If a secret version a run was given is missing or cannot be read, the run is
held with the reason **variable history unavailable** (`run_env_history_unavailable`)
and does not resume or publish. **Resume** retries once the history is restored;
otherwise **Cancel** the run.

The run page lists **Variables injected** for the run's current attempt: each
name, its scope and a secret badge, never a value.

## Warning: the agent can read every value

The agent can read every variable, secret or not. Store read-only, narrowly
scoped credentials. uzi does not log in or configure tools for you; the agent
or your repo scripts do.

Every Claude CLI start runs with its global config taken from a directory the
worker prepares under the run's home (`.uzi-claude-config`) and the agent cannot
write when the worker and the agent run as separate users. An `env` block the
agent writes to the usual config files in the home is not read, so it does not
apply at the next start. Variables are delivered to Claude runs only on that
protected two-user worker. A single-user worker (some small installs) does not
report `run_env_v1`, so a run that carries variables is not placed on it and
waits for a two-user worker. Ordinary launches without variables still start
there, with the directory prepared read-only and owned by the same user; that
gives no protection against the agent's own user and is not an integrity
boundary.

If something unexpected sits at that path, the worker moves it aside
(`.uzi-claude-config.stale-*`, `.claude.stale-*`) and rebuilds. It fails the run
instead of starting the CLI when it cannot do that or cannot trust the layout:
the rename fails (for example a read-only data volume), the home directory or
one of its parent directories fails the worker's ownership and mode checks (a
symlink, an owner other than root or the worker, world-writable without the
sticky bit, or group-writable by a runner group without it), preparing the
transcript directories fails (the worker moves `.claude` aside only when it
proves an obstruction, and only once: a non-directory planted at `.claude` or at
a target, or a `.claude` the runner cannot write and search by its mode bits;
any other failure, a full disk or a foreign-owned `.claude` whose bits still
let the runner write and search included, refuses that start and keeps
`.claude` and its transcripts), the preparation passes its 10 second limit, or the directory keeps
changing while the worker prepares it. Every preparation error is reported as
this refusal, not as a disk park. The preparation blocks the worker for up to
about 14 seconds in the worst case; a helper stuck in uninterruptible I/O is not
bounded. ADR-4416 has the details and the limits.

## Name and value rules

1. A name matches `^[A-Z_][A-Z0-9_]{0,127}$`.
2. A value is UTF-8 without NUL, at most 64 KiB. A secret is at least 8
   characters; a plain value may be empty.
3. A plain value containing a URL with a user or password is refused. Make it
   secret.
4. A description is at most 1024 bytes.

A repo holds at most **50 variables and 256 KiB** of values. Disabled variables
and variables pending deletion count toward both.

## Names uzi refuses

Some names would change how the worker or the agent harness runs, so they are
denied:

- Prefixes: `UZI_`, `CLAUDE_`, `ANTHROPIC_`, `CODEX_`, `OPENAI_`, `MCP_`,
  `DISABLE_`, `GH_`, `GITHUB_`, `GITLAB_`, `GLAB_`, `GIT_`, `SSH_`, `LD_`,
  `DYLD_`, `BASH_`, `XDG_`, `SSL_CERT_`, `PYTHON`, `PERL5`, `NIX_`, `DEVBOX_`,
  `BUN_`, `CCR_`, `SELF_HOSTED_RUNNER_`, `BUGHUNTER_`, `_CLAUDE_`, `ANT_OTEL_`,
  `ANT_CLAUDE_`, `OTEL_LOG_`, `LC_`, `GCM_`, `OTEL_EXPORTER_OTLP_`,
  `GCE_METADATA_`, `AWS_ENDPOINT_URL_BEDROCK` (which also covers
  `AWS_ENDPOINT_URL_BEDROCK_AGENT*`), `AWS_ENDPOINT_URL_` (every per-service
  AWS endpoint override, such as `AWS_ENDPOINT_URL_S3`).
  The `BUN_` prefix also refuses Bun application settings such as `BUN_INSTALL`
  and `BUN_CONFIG_REGISTRY` as repo variables: the bundled Claude CLI is a Bun
  executable whose runtime reads `BUN_*` controls.
- Any name ending in `_PROXY`.
- Process, shell, Docker, Go, Node, Java, curl and browser settings such as
  `PATH`, `HOME`, `TMPDIR`, `SHELL`, `DOCKER_HOST`, `DOCKER_CONFIG`, `GOFLAGS`,
  `NODE`, `NODE_OPTIONS`, `UV_USE_IO_URING`, `JAVA_TOOL_OPTIONS`, `NETRC`,
  `CURL_CA_BUNDLE`, `RUBYOPT`.
- Network and cloud endpoint settings the harness reads, such as
  `AWS_EC2_METADATA_SERVICE_ENDPOINT`, `AWS_ENDPOINT_URL` (the global AWS
  endpoint override also routes Bedrock traffic), `AWS_ENDPOINT_URL_STS`,
  `AWS_ENDPOINT_URL_SSO` and `AWS_ENDPOINT_URL_SSO_OIDC` (they override the
  endpoints of the AWS credential exchange, STS, IAM Identity Center and SSO
  OIDC, that the Bedrock harness's AWS SDK credential chain can use; denied for
  consistency with `AWS_ENDPOINT_URL`), and any `OTEL_EXPORTER_OTLP_*`, `GCE_METADATA_*` or
  `AWS_ENDPOINT_URL_*` name, including `AWS_ENDPOINT_URL_S3`. Other AWS
  settings such as `AWS_REGION` stay allowed. Codex 0.171.0 adds the names
  `SPIFFE_ENDPOINT_SOCKET`, `SSH2_CLIENT` and `OTEL_RESOURCE_ATTRIBUTES` to
  the denied set.
- Harness controls: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `API_TIMEOUT_MS`, `MAX_THINKING_TOKENS`, `MAX_MCP_OUTPUT_TOKENS`,
  `NODE_INSPECTOR_IPC`, `VSCODE_INSPECTOR_OPTIONS`, `API_FORCE_IDLE_TIMEOUT`,
  `ENABLE_TOOL_SEARCH`, `SLASH_COMMAND_TOOL_CHAR_BUDGET`, `AUTOMODE_DECISION_LOG`,
  `NODE_USE_SYSTEM_CA`, and `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL` (a
  provider selector whose leading underscore escapes the `CLAUDE_` prefix).
- Telemetry switches the Claude CLI reads outside the denied prefixes:
  `ENABLE_BETA_TRACING_DETAILED`, `ENABLE_ENHANCED_TELEMETRY_BETA`,
  `BETA_TRACING_ENDPOINT`. `OTEL_*` names outside the denied `OTEL_LOG_` and
  `OTEL_EXPORTER_OTLP_` prefixes and `OTEL_RESOURCE_ATTRIBUTES` are allowed for your own application: the CLI
  starts no third-party OpenTelemetry exporter unless
  `CLAUDE_CODE_ENABLE_TELEMETRY` (denied by the `CLAUDE_` prefix) or the beta
  tracing pair above is set, and the worker never sets them. `OTEL_LOG_` is
  denied because `OTEL_LOG_RAW_API_BODIES=file:<dir>` makes the CLI write raw
  API request and response bodies to that directory; `OTEL_EXPORTER_OTLP_` is
  denied as exporter endpoint and header wiring.
- Managed directories the sandbox reads: `TEMP`, `TMP`, `SYSTEMDRIVE`. These
  are the uppercase spellings of Windows variables (the Windows environment
  lookup is case-insensitive); API names stay uppercase-only, so the mixed-case
  `SystemDrive`, `Temp` and `Tmp` are rejected by the name rule.

Allowed: `DATABASE_URL`, cloud credentials such as `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION` and `GOOGLE_APPLICATION_CREDENTIALS`,
`NPM_TOKEN`, `GRADLE_OPTS`, `DOCKER_AUTH_CONFIG`, `NO_COLOR`. The bullets above
are examples; the full list is `fixtures/run-env/name-rules.json`.

Saving or enabling a variable checks the full list. **Disable** and **Delete**
check only the name's shape, so a stored variable whose name a later rule denies
can still be removed. Names are checked on write only, so a stored variable whose
name a later rule denies is dropped at injection by a worker that has the rule,
with a run event naming the rule.

## Entering a value

The value field is a single line by default; a secret is masked by a password
field. Turn on **Multiline** for values with line breaks, such as PEM keys or
JSON. Pasting a value that contains line breaks into the single-line field
switches to Multiline automatically and keeps the line breaks. A paste with only
a single trailing line break, such as a token copied from a terminal, is trimmed
and stays single-line. The form saves line endings as LF. A multiline secret is masked with CSS, which is not portable
across browsers.

## Worked examples

These show how a repo's variables are used. All hosts are placeholders.

### An API

| Name | Kind | Value | Why |
|---|---|---|---|
| `EXAMPLE_API_URL` | plain | `https://api.example.com` | Not sensitive; plain values are readable in the UI and CLI. |
| `EXAMPLE_API_KEY` | secret | the key | A credential; never shown again and redacted from output. |

### A private Maven repository

| Name | Kind | Value | Why |
|---|---|---|---|
| `MAVEN_REPO_URL` | plain | `https://maven.example.com/releases` | Not sensitive. Keep credentials out of it: rule 3 refuses a plain URL with a user or password. |
| `MAVEN_REPO_USER` | plain | the user name | Usually not sensitive. Make it secret if the name itself is. |
| `MAVEN_REPO_PASSWORD` | secret | the password | A credential; at least 8 characters. |

A build can reference them, for example a `settings.xml` server entry:

```xml
<server>
  <id>example</id>
  <username>${env.MAVEN_REPO_USER}</username>
  <password>${env.MAVEN_REPO_PASSWORD}</password>
</server>
```

and a matching repository declaration. Declare it in a `settings.xml` profile
that is listed under `<activeProfiles>`:

```xml
<profiles>
  <profile>
    <id>example</id>
    <repositories>
      <repository>
        <id>example</id>
        <url>${env.MAVEN_REPO_URL}</url>
      </repository>
    </repositories>
  </profile>
</profiles>
<activeProfiles>
  <activeProfile>example</activeProfile>
</activeProfiles>
```

The repository `id` must match the server `id`, or Maven sends no credentials.

Maven 3.10.0 and later, and Maven 4.0.0 and later, bind a server's credentials
to the origins (scheme, host and port) of the same-id repositories declared in an
active `settings.xml` profile, so a same-id repository at another origin gets
none. A repository declared only in a `pom.xml` is not known in advance. When the
server has no known origin, the default `origin` scope still sends its
credentials, with a warning; `maven.repository.credentialScope=strict` refuses
that fallback. `maven.repository.credentialScope=id` restores matching by `id`
alone, with no origin binding, and a repository can set it in its own
`.mvn/maven.config`; check the effective value before relying on origin binding. Maven 3.9.x and earlier match
credentials by `id` alone, so a same-id repository in any dependency or parent
POM receives them; use 3.10.0 or later.

### A container registry

| Name | Kind | Value | Why |
|---|---|---|---|
| `REGISTRY_HOST` | plain | `registry.example.com` | Not sensitive; keeping it plain leaves it readable in logs. |
| `REGISTRY_USER` | plain | the user name | Usually not sensitive. |
| `REGISTRY_PASSWORD` | secret | the password or token | A credential. |

The agent or your repo scripts log in, for example:

```sh
printf '%s' "$REGISTRY_PASSWORD" | docker login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin &&
  docker pull "$REGISTRY_HOST/team/image:tag"
```

A login does not yet persist across commands, so log in in the same command
that uses the registry.

## Edit, disable, delete

1. **Edit** without a new value changes only the description; entering a value replaces it.
   Switching a secret to plain is the exception: it replaces the secret with
   whatever the field holds, even an empty value.
2. **Replace the value** to rotate it. A secret keeps its secret flag unless
   you change it.
3. **Disable** stops injection without deleting the variable; enable it again
   later. A disabled name cannot be re-created.
4. **Delete** removes it. It shows **Pending deletion** while a run still needs
   the old value for masking; the name can be re-created at once, subject to
   the 50-variable and 256 KiB limit, which pending rows still count against.

Neither Disable nor Delete revokes the credential. Revoke it at the provider.

## Locked vault

While your vault is locked, values cannot be saved and plain values are hidden.
Runs wait as they do today. Unlock with `uzi vault unlock`.
