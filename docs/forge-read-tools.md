---
title: Forge read tools
order: 53
audience: user
---

# Forge read tools

A run's subagents can read its own project's forge (GitLab, Forgejo, or
GitHub) — issues (title, description, and now comments), merge requests,
pipelines, and label history — to check a claim against live state instead
of trusting the repo's own restatement of it. This is separate from
[chat](./chat.md)'s `propose_issue`: it is **read-only**, and it runs
inside a run, not a chat conversation.

## The six tools

Exposed as in-process MCP tools under the server name `forge`
(`mcp__forge__<tool>`):

| Tool | Answers |
|---|---|
| `get_issue(iid)` | One issue's title, state, labels, author, description (capped at 32 KiB; `description_truncated` flags a cut), and its human comments (bot- and system-note-filtered, oldest-first, capped at 200 items / 32 KiB total; `comments_truncated` flags a cut). |
| `list_issues(state?, labels?, updated_after?)` | Filtered issue summaries, no descriptions, capped at 50 rows with `truncated`. |
| `list_issue_label_events(iid)` | Who added/removed which label and when, on one issue. |
| `get_merge_request(iid)` | An MR's state. |
| `get_pipeline_jobs(pipeline_id)` | A pipeline's jobs — name, stage, status. |
| `latest_pipeline(ref \| mr_iid)` | The latest pipeline for a branch ref OR a merge request (exactly one), or `null` if none has run. |

## Which agents get them

Only the `fact-checker` builtin lists these six tools in its `tools:`
allowlist today — it is the run's dedicated adversarial verifier, and the
tools exist to give it something to verify a claim against. An
[agent template](./agent-templates.md) with no `tools:` list (the default
`lead` and `coder`) inherits every tool, forge tools included; a template
with its own explicit allowlist gets forge access only if you add an
`mcp__forge__*` entry to it. A read-only validator template with its own
allowlist (`reviewer`, `auditor`) does not get forge access unless you
name one.

## Credential-free, read-only, own project only

The agent never holds a forge token, base URL, or numeric project id. Each
call goes through the worker (join-token authenticated) to the uzi API,
which derives the run's own project **server-side from the run record**
and reads it with the Go forge driver — never a tool parameter, so a
subagent cannot read another run's forge, and none of these tools accepts
a project or repo as a parameter. A failed lookup returns fixed,
coordinate-free text ("could not read from the forge", "that run or item
was not found") rather than the raw error, which can embed a forge host
or project id. There is no write path here: the chat lane's
`propose_issue` remains the only way a run's agent creates or edits
anything on the forge.

## Budget and truncation

A single per-session budget (40 calls, shared across all six tools and every
subagent in the run) bounds how much one run can enumerate. It lives in the
agent process and resets if a run is resumed (a fresh executor), so it is
per-session rather than strictly per-run. Once exhausted, a further call
returns a plain refusal rather than an error, so a runaway loop doesn't fail
the run. List results and long descriptions
carry explicit truncation markers rather than silently dropping rows.

## Untrusted evidence

Issue titles, descriptions, labels, and MR/pipeline state are
attacker-influenceable — anyone who can open an issue or MR controls
them. Every successful payload is wrapped in a nonce-fenced "untrusted
evidence" envelope before it reaches the model, the same framing
[chat](./chat.md) uses for run logs and issue text, so a prompt-injection
attempt inside a forge payload reads as data, not instructions. Issue
**comments** join that same nonce-fenced untrusted-evidence class — each
comment is independently attacker-authored, so `get_issue`'s comment list
is filtered (uzi's own bot comments and forge system notes dropped) and
capped the same way as its description before it ever reaches the model.

## Status

This capability has shipped in the `fact-checker`'s tool allowlist. The
acceptance run confirming a real `fact-checker` cites forge state
end-to-end, on the worker's own image, is still pending.
