---
title: Repo agents
order: 52
audience: user
---

# Repo agents

Some repos ship their own agent roster in a `.claude/agents/` directory (Markdown
files) or a `.codex/agents/` directory (TOML files): a
team that knows a codebase can define the exact `coder`, `reviewer`, and
specialist roles it wants. When a run clones such a repo, uzi detects that
roster and lets you run the repo's agents instead of your own
[agent templates](./agent-templates.md) — chosen at the plan gate, per run.

The `lead` orchestrator is always uzi's own builtin and is never replaceable;
repo agents and your templates only ever supply the **subagents** it delegates to.

## Choose the agents at the plan gate

When a run reaches the approval gate, the plan panel shows an **Agents for this
run** section with two cards:

1. **Repo agents** — the roster detected in the repo's `.claude/agents/` or
   `.codex/agents/` (the card shows which folder). This is the default when a repo
   ships one; the card lists the detected names.
2. **My agent templates** — your uzi templates. This is the default (and the
   repo card is inert) when the repo has no agent folder uzi reads.

Pick one source, then click any agent chip to exclude it from the run. At least
one subagent must remain. The choice locks in when you approve; the run view then
shows which roster ran. Autopilot runs and CI-fix runs (which have no human gate)
apply the default automatically — repo agents when detected, else your templates —
and record what they used.

Approving from a [Slack](./slack.md) DM offers the source choice too: when a repo
roster is detected the message shows two approve buttons — **Approve · repo agents**
and **Approve · my templates** — so you pick the source without leaving Slack.
Per-agent exclusions are a web-only refinement; use "Open in uzi" for those.

## Repo agents on Codex runs

On a Codex run the agent selection applies the same way: implementation
delegates to the roster you chose at the plan gate (by default the detected
repo roster), honours your exclusions, and falls back to your own templates when
the selection is invalid. Planning keeps your own templates. Two Codex-specific
rules apply to each repo agent's frontmatter:

- **`model:`** is honored only when it names a supported Codex model; otherwise
  the run's model is used.
- **`tools:`** entries Codex does not recognise are dropped, never widened
  into something broader, so a repo
  agent can only end up with less authority than it declared.

The [trust trade-off](#the-trust-trade-off--read-before-you-pick-repo-agents)
below applies unchanged: the lead is told repo-defined subagents' output is
unverified.

## Which folder is read

A worker reads one folder per run, never both: a roster is never merged across
folders. Each harness prefers its own folder, and falls back to the other only
when its own folder is absent.

| Run harness | Preferred folder | Falls back to |
|---|---|---|
| Claude | `.claude/agents/*.md` | `.codex/agents/*.toml` |
| Codex | `.codex/agents/*.toml` | `.claude/agents/*.md` |

If the preferred folder is present, it is final, even when it is empty, holds
only invalid files, or is rejected as unsafe (see below): uzi does not then look
at the other folder. The detected roster is shown with its source folder at the
plan gate, in the run view, and in the feed lines. When the API is older than
the worker and cannot record the folder, the plan-gate card and the run view
show `.claude/agents`; the feed lines still name the real folder.

### `.codex/agents/*.toml` files

Each TOML file becomes one subagent:

- **Required:** a string `name` (there is no filename fallback), a string
  `description`, and a non-empty string `developer_instructions`, which becomes
  the agent's prompt. The `name` and `description` rules match the Markdown
  agents.
- **No tools, no model:** a TOML agent declares neither, so it runs with the
  run's full default tool set, which can include `Bash`, and the run's model.
  `model`, `model_reasoning_effort`,
  `nickname_candidates`, `config_file`, `includes`, and any other key are ignored.
- **Skipped, not run:** an otherwise valid file that declares `features`, `skills`,
  `sandbox_mode`, or `tools` (with any value) is skipped with a "declares a Codex
  restriction uzi cannot honour yet" note, rather than running without the
  restriction its author asked for.
- **Invalid:** a file that does not parse, including duplicate keys or tables,
  is invalid and skipped. The same caps apply as for Markdown agents (16 files,
  64 KiB per file).
- A Codex agent's instructions run verbatim even on a Claude run (the fallback
  case).

TOML-sourced agents get the same guards as Markdown ones on both harnesses:
their output is treated as untrusted, they cannot spawn nested agents, and the
deferral tools stay denied.

Out of scope: the `[agents]` table in `.codex/config.toml`, and other tools'
agent folders, are not read.

### How uzi keeps the read inside the clone

Detection is hardened against a repo that tries to point it elsewhere. uzi
walks the folder's path components without following symlinks: a symlink
anywhere in them means the folder is not read, and it still counts as present,
so there is no fallback to the other folder. Each file is opened without
following symlinks, and before any byte is read the opened file's real path must
sit directly inside the agents folder; a file that fails this check is skipped.
The size check and read use that same open handle. The check relies on Linux
`/proc`, so on a platform without it nothing is read. As supporting context,
detection runs during preflight, after the clone or resume and before the
executor starts, in a clone owned by the runner.

## What is loaded, and what is never

uzi parses the agent files itself; it never points Claude Code at the repo's
`.claude/` directory. (This section describes `.claude/agents/*.md` files;
`.codex/agents/*.toml` files are covered under
[Which folder is read](#which-folder-is-read).) Each file's `name`, `description`, `tools`, and `model`
frontmatter is honored — including a `tools:` entry naming one of the
[forge read tools](./forge-read-tools.md) (`mcp__forge__*`), which a repo
agent can grant itself the same way a template does. That surface is
scoped to the run's own project regardless of which agent calls it. The
read tools inspect forge data; the reply and resolve tools can modify review
threads present in the run's MR review snapshot. A repo agent's own hooks, settings, and slash commands
are **never** loaded, and the primary-directive guardrails always apply: no repo
agent can push to `main`, rewrite history, spawn nested agents, or schedule
deferred work, whatever its file says.

## What skills the repo's agents get

[Skill](./skills.md) allocations attach to your agent templates, and a repo
roster has no templates, so there is nothing to scope against. Every repo
subagent therefore receives exactly the run's materialized skill union: every
delivered skill its owner allocated to any template in the run, minus drops for
oversize, name collision, and the per-run cap. That is the same set the run's
lead already receives, so a repo subagent gets no superset of what the run
already materializes.

- **Nothing is lost by choosing repo agents.** A run started with repo agents
  carries the same delivered skills as one started with your own templates.
- **Per-template scoping does not apply.** Allocating a skill to `coder` alone
  is your scoping surface on a template run; on a repo-agent run every subagent
  sees it. Repo skills (`.claude/skills/` or `.agents/skills/`, opt-in per repo)
  already worked this way, for the same reason.

## The trust trade-off — read before you pick repo agents

Repo agents are **the repository's code, not uzi's reviewed templates.** Choosing
them means every subagent for that run — including the reviewer and auditor — is
defined by whoever can write to that repo. Enable repo agents only for repos you
trust as much as your own.

Two consequences are worth stating plainly:

- **Internal review becomes repo-authored.** When a run uses repo agents, its
  `reviewer`/`auditor` are the repo's, so their "looks good" is unverified. The
  lead is told to treat their output as input to double-check, not as a sign-off,
  and the merge request records that the run used repo agents so the human
  reviewer knows.
- **A repo agent that can run commands can reach your token.** A repo `coder`
  that declares the `Bash` tool (most do) runs shell inside the worker, where
  your Anthropic token lives — so it could send that token out, or run up cost by
  requesting an expensive `model`. uzi does not sandbox this away today; the
  guardrail is your choice of which repos to trust. (Locking the agent container's
  network down is a planned follow-up; see
  [proc-hardening](./proc-hardening.md).)
- **A repo agent can read every skill body the run carries.** Skill bodies are
  never secrets by product policy (a skill's description and body must never
  carry a credential, see [Agent skills](./skills.md)), but an admin-authored
  playbook about your internal infrastructure is readable by a repo-authored
  subagent, which can write it into the worktree and push it to the run's
  branch. Keep out of skill bodies anything you would not want in a merge
  request on that repo.

Repo agents need no opt-in; they are offered at the plan gate whenever the
repo has a `.claude/agents/` or `.codex/agents/` folder. Two related per-repo opt-ins are separate:
the repo's own skills and its root `CLAUDE.md`, both off by default (see
[Agent skills](./skills.md#repo-skills-opt-in-default-off) and
[Repo instructions](./skills.md#repo-instructions-opt-in-default-off)).
Whichever agents a run uses, uzi appends its runtime rules (scratch
directory, safety rules) to every subagent; see
[Agent templates](./agent-templates.md#resetting-a-builtin-template).

For how detection, validation, and the gate-boundary rebuild work, see
[ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates).
