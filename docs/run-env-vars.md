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
- **Worker support**: delivery needs a worker that reports `run_env_v1`. A run
  whose repo has variables is never claimed by an older worker, and run health
  says "no online worker supports repo variables (run_env_v1); upgrade a worker"
  while none is online. Cross-check children get no variables, but one claimed
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
value with LF line endings. Each line of a multi-line value is redacted on its
own when it is at least 8 characters, has a letter or digit and at least 2
distinct non-space characters, and is not PEM header or footer armor or a public
line (`Proc-Type` lines and the service-account JSON members `type`, `auth_uri`,
`token_uri`, `auth_provider_x509_cert_url` and `universe_domain`). Redaction is
exact-match with an 8-character floor, so it is a net, not a boundary: another
encoding of the value is not caught.

A run's secret forms are computed once per claim and capped at 40,000,000
characters (`RUN_ENV_FORM_CHARS_MAX`). A claim over the cap fails before any
work, with a failure reason starting `run_env_forms_over_budget` (never a
value), and publishes nothing. A publish with no live flight (held, terminal and
bundle publication) is withheld with the reason `forms_over_budget`. A
cross-check child whose claim is over the cap ends as checker unavailable
instead. A store filled to its limits with multi-line values can pass the cap.
To get under it, remove or shorten secret variables, multi-line ones first. A
run keeps counting the older versions it was given; a new run counts only the
current values, and a cross-check child counts every version its lead was given.

Before work leaves the worker, every commit in the outgoing range is searched
for those forms, not only the final tree. For a multi-line secret it also
searches for the value as it reads inside a diff, with its lines joined by
`+`, `-` or space prefixes. That covers checkpoint pushes
(including parked, paused and shutdown checkpoints), the final push, held and
terminal preservation packs, recovery archives and the preserved patch.

- A hit blocks the publish and names the **variable**, never the value. At the
  final push the run fails as `push_secret_blocked`; a checkpoint hit is held by
  the existing secret-remediation hold. A secret that is a common word can
  trigger this; pick a longer value.
- The scan **fails closed**. If it cannot prove a range clean (a timeout, an
  unreadable range, a scanner error, or the run's secret history cannot be
  looked up), a checkpoint, pack or recovery upload is withheld and retried
  later, and the work stays on the worker under its recovery hold. At the final
  push the run fails as `push_secret_blocked` instead.
- A plain value found in tracked files holds nothing.
- The preserved patch is withheld, not redacted, when the claim is over the form
  cap; when a whole-value form occurs in the raw patch or in a rebuilt per-hunk
  pre-image or post-image (context plus removed or added lines, also with line
  breaks normalized to LF); when any registered line form occurs in it; or, for
  a run with secret variables, when the patch cannot be parsed (binary, combined
  diff, a line-count mismatch, or a stray line such as a non-header line between a file
  header and its first hunk). The image, line-form and parse checks apply when
  the run has any secret variable, retired versions included, so such a run
  loses its preserved patch when the diff touches a binary file. Otherwise the
  patch is redacted and scanned as before.
- Gaps that remain: a secret whose lines are all short, public, or without a
  letter or digit, or with only one distinct non-space character, and that is
  split across files or by elided context between hunks, and a value that
  appears only re-encoded, such as each line indented in a YAML block or in an
  encoding other than those registered. A committed file that holds the whole
  secret is still blocked.
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
  `AWS_ENDPOINT_URL_BEDROCK_AGENT*`).
- Any name ending in `_PROXY`.
- Process, shell, Docker, Go, Node, Java, curl and browser settings such as
  `PATH`, `HOME`, `TMPDIR`, `SHELL`, `DOCKER_HOST`, `DOCKER_CONFIG`, `GOFLAGS`,
  `NODE`, `NODE_OPTIONS`, `UV_USE_IO_URING`, `JAVA_TOOL_OPTIONS`, `NETRC`,
  `CURL_CA_BUNDLE`, `RUBYOPT`.
- Network and cloud endpoint settings the harness reads, such as
  `AWS_EC2_METADATA_SERVICE_ENDPOINT`, `AWS_ENDPOINT_URL` (the global AWS
  endpoint override also routes Bedrock traffic), and any `OTEL_EXPORTER_OTLP_*`,
  `GCE_METADATA_*` or `AWS_ENDPOINT_URL_BEDROCK*` name. The other non-Bedrock
  per-service names, such as `AWS_ENDPOINT_URL_STS`, stay allowed.
- Harness controls: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `API_TIMEOUT_MS`, `MAX_THINKING_TOKENS`, `MAX_MCP_OUTPUT_TOKENS`.

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
