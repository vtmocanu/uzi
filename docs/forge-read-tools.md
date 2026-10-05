---
title: Forge read tools
order: 53
audience: user
---

# Forge read tools

A run's `forge` server exposes six read tools for its own project's forge
(GitLab, Forgejo, or GitHub) — issues (title, description, and comments),
merge requests, pipelines, and label history — to check a claim against live
state instead of trusting the repo's own restatement of it. It also exposes
two scoped MR-thread write tools for replying to and resolving review threads
addressed in an `mr_rework` run. These tools run inside a run, separate from
[chat](./chat.md)'s `propose_issue`.

## The six read tools

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

## The two scoped write tools

These use the same `forge` server and `mcp__forge__<tool>` naming:

| Tool | Action |
|---|---|
| `reply_mr_thread(reply_id, body)` | Reply in a review thread addressed this cycle, using a nonempty `reply_id` from THIS run's review snapshot. |
| `resolve_mr_thread(resolve_id)` | Resolve a review thread addressed this cycle, using a nonempty `resolve_id` from THIS run's review snapshot. |

These writes are for `mr_rework` runs only and target the source MR. The
API derives the project and `mr_iid` from the run record, and the corresponding reply or resolve id
must match an anchor in THIS run's review snapshot. An otherwise-valid call
on a run with no `mr_iid` returns 422. An absent or empty snapshot fails
closed; an unmatched id returns 403. Corrupt snapshot JSON refuses the write
with 502. For 403/422, the model receives the fixed, nonfatal refusal
"that thread is not part of this run's review snapshot, so no reply or
resolve was performed".

## Which agents get them

The lead gets the forge server, including both write tools. Among subagents,
only the `fact-checker` builtin gets the six **read** tools by default: it is
the run's dedicated adversarial verifier (uzi adds the six read entries to
its tools list; the upstream role file does not name them). A subagent,
including a repo-authored agent, reaches the forge only through an explicit
`mcp__forge__*` entry in its [agent template](./agent-templates.md)'s `tools:`
list naming a recognized forge tool. A template with no `tools:` list (such
as `coder`) inherits the standard tools but not the forge ones, and a
validator with its own allowlist (`reviewer`, `auditor`) gets none unless
you add one.

## Credential-free, own project only

The agent never holds a forge token, base URL, or numeric project id. Each
call goes through the worker (join-token authenticated) to the uzi API,
which derives the run's own project **server-side from the run record**
and accesses it with the Go forge driver — never a tool parameter, so a
subagent cannot access another run's forge, and none of these tools accepts
a project or repo as a parameter. A failed lookup returns fixed,
coordinate-free text ("could not read from the forge", "that run or item
was not found") rather than the raw error, which can embed a forge host
or project id. The two MR-thread writes use this same credential-free path,
with the additional source-MR and review-snapshot scope above.

## Budget and truncation

A single per-session budget (40 read calls, shared across the six read tools,
the lead, and every subagent in the run) bounds how much one run can enumerate.
The two write tools do not consume this budget. It lives in the
agent process and resets if a run is resumed (a fresh executor), so it is
per-session rather than strictly per-run. Once exhausted, a further read call
returns a plain refusal rather than an error, so a runaway loop doesn't fail
the run. List results and long descriptions
carry explicit truncation markers rather than silently dropping rows.

## Untrusted evidence

Issue titles, descriptions, labels, and MR/pipeline state are
attacker-influenceable — anyone who can open an issue or MR controls
them. Every successful read payload is wrapped in a nonce-fenced "untrusted
evidence" envelope before it reaches the model, the same framing
[chat](./chat.md) uses for run logs and issue text, so a prompt-injection
attempt inside a forge payload reads as data, not instructions. Issue
**comments** join that same nonce-fenced untrusted-evidence class — each
comment is independently attacker-authored, so `get_issue`'s comment list
is filtered (uzi's own bot comments and forge system notes dropped) and
capped the same way as its description before it ever reaches the model.
Write successes return fixed text ("replied in the review thread" or
"resolved the review thread"), rather than wrapped forge evidence.

## Status

This capability has shipped in the `fact-checker`'s tool allowlist. The
acceptance run confirming a real `fact-checker` cites forge state
end-to-end, on the worker's own image, is still pending.
