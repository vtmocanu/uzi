---
title: Run environment variables
order: 116
audience: user
---

# Run environment variables

> **Temporary note (milestone 1).** Variables are stored and managed, but
> **not yet injected into runs**. Saving one has no effect on any run today.

Give a repo's runs named settings and credentials, such as a database URL or a
package index token. Each variable belongs to one repo and is either **secret**
or **plain**. Values are sealed through your per-user vault.

Set them under **Repos → Environment variables**. Writes are web-only. The CLI
can list a repo's variables read-only: `uzi repo env list <id>`
([CLI reference](./cli.md)).

## Secret or plain

- **Secret**: the value is never shown again, in the web UI, the CLI or the
  API. Once injection ships, secret values are redacted from run output and a
  publish that contains one is blocked.
- **Plain**: the value stays readable, including in `uzi repo env list`, and is
  **not** redacted. Use it only for non-sensitive settings such as URLs and
  regions.

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
  `DYLD_`, `BASH_`, `XDG_`, `SSL_CERT_`, `PYTHON`, `PERL5`, `NIX_`, `DEVBOX_`, `BUN_`.
  The `BUN_` prefix also refuses Bun application settings such as `BUN_INSTALL`
  and `BUN_CONFIG_REGISTRY` as repo variables: the bundled Claude CLI is a Bun
  executable whose runtime reads `BUN_*` controls.
- Any name ending in `_PROXY`.
- Process, shell, Docker, Go, Node, Java, curl and browser settings such as
  `PATH`, `HOME`, `TMPDIR`, `SHELL`, `DOCKER_HOST`, `DOCKER_CONFIG`, `GOFLAGS`,
  `NODE_OPTIONS`, `JAVA_TOOL_OPTIONS`, `NETRC`, `CURL_CA_BUNDLE`, `RUBYOPT`.
- Harness controls: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `API_TIMEOUT_MS`, `MAX_THINKING_TOKENS`, `MAX_MCP_OUTPUT_TOKENS`,
  `NODE_INSPECTOR_IPC`, `VSCODE_INSPECTOR_OPTIONS`, `API_FORCE_IDLE_TIMEOUT`,
  `ENABLE_TOOL_SEARCH`, `SLASH_COMMAND_TOOL_CHAR_BUDGET`, `AUTOMODE_DECISION_LOG`,
  `NODE_USE_SYSTEM_CA`, and `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL` (a
  provider selector whose leading underscore escapes the `CLAUDE_` prefix).
- Telemetry switches the Claude CLI reads outside the denied prefixes:
  `ENABLE_BETA_TRACING_DETAILED`, `ENABLE_ENHANCED_TELEMETRY_BETA`,
  `BETA_TRACING_ENDPOINT`. Other `OTEL_*` names are allowed for your own
  application for now: the CLI starts no third-party OpenTelemetry exporter
  unless `CLAUDE_CODE_ENABLE_TELEMETRY` (denied by the `CLAUDE_` prefix) or the
  beta tracing pair above is set, and the worker never sets them. One exception:
  `OTEL_LOG_RAW_API_BODIES=file:<dir>` makes the CLI write raw API request and
  response bodies to that directory. The `OTEL_LOG_` and `OTEL_EXPORTER_OTLP_`
  prefixes are planned to be denied, so do not rely on them.
- Managed directories the sandbox reads: `TEMP`, `TMP`, `SYSTEMDRIVE`. These
  are the uppercase spellings of Windows variables (the Windows environment
  lookup is case-insensitive); API names stay uppercase-only, so the mixed-case
  `SystemDrive`, `Temp` and `Tmp` are rejected by the name rule.

Allowed: `DATABASE_URL`, cloud credentials such as `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION` and `GOOGLE_APPLICATION_CREDENTIALS`,
`NPM_TOKEN`, `GRADLE_OPTS`, `DOCKER_AUTH_CONFIG`, `NO_COLOR`. The full list is
`fixtures/run-env/name-rules.json`.

A variable whose name became denied after it was saved can still be disabled or
deleted, but never enabled or edited.

## Entering a value

The value field is a single line by default; a secret is masked by a password
field. Turn on **Multiline** for values with line breaks, such as PEM keys or
JSON. Pasting a value that contains line breaks into the single-line field
switches to Multiline automatically and keeps the line breaks. A paste with only
a single trailing line break, such as a token copied from a terminal, is trimmed
and stays single-line. The form saves line endings as LF. A multiline secret is masked with CSS, which is not portable
across browsers.

## Worked examples

These describe how the variables will be used once injection ships. Today they
are only stored. All hosts are placeholders.

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

Login persistence across commands is not yet available, so log in in the same
command that uses the registry.

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
