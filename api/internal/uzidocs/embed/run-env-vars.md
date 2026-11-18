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
  `DYLD_`, `BASH_`, `XDG_`, `SSL_CERT_`, `PYTHON`, `PERL5`, `NIX_`, `DEVBOX_`.
- Any name ending in `_PROXY`.
- Process, shell, Docker, Go, Node, Java, curl and browser settings such as
  `PATH`, `HOME`, `TMPDIR`, `SHELL`, `DOCKER_HOST`, `DOCKER_CONFIG`, `GOFLAGS`,
  `NODE_OPTIONS`, `JAVA_TOOL_OPTIONS`, `NETRC`, `CURL_CA_BUNDLE`, `RUBYOPT`.
- Harness controls: `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `API_TIMEOUT_MS`, `MAX_THINKING_TOKENS`, `MAX_MCP_OUTPUT_TOKENS`.

Allowed: `DATABASE_URL`, cloud credentials such as `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_REGION` and `GOOGLE_APPLICATION_CREDENTIALS`,
`NPM_TOKEN`, `GRADLE_OPTS`, `DOCKER_AUTH_CONFIG`, `NO_COLOR`. The full list is
`fixtures/run-env/name-rules.json`.

## Edit, disable, delete

1. **Edit** without a new value changes only the description; entering a value replaces it.
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
