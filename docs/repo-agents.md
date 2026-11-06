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

The lead comes from your own selected lead template, never the repo roster.
Repo agents only supply the **subagents** it delegates to; a repo agent named
`lead` is still a subagent.

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

## Set a default for your runs on a repo

Each repo you connect has a **Default agents for my runs on this repo** setting,
in the repo's **Trusted repo** settings on the Repos page:

- **uzi templates**: your runs on this repo use your own templates.
- **repo agents**: your runs on this repo use the repo's roster when one is
  detected, and fall back to your templates (with the usual "no repo agent
  roster" note) when it is not.
- **not set (today's behaviour)**: the defaults described above apply.

The setting is yours alone. It changes only the runs you create on your own
connection to that repo; a teammate connected to the same project keeps their
own setting. An admin can clear it or set **uzi templates** on any repo, but only
the repo's owner can choose **repo agents**.

uzi freezes the setting onto each issue run when the run is created, whether you
start it from the board, the CLI, chat, a schedule or autopilot. Changing the
setting later does not change runs already queued or running. Other run kinds
(CI fix, MR rework, self-improvement, prompt, task and job runs) are not affected.

A frozen default decides the implementation agents when nothing else does:

- an approval that carries no agent choice (for example `uzi run approve` with
  no `--agent-source`, or an external dispatcher using the API) implements with
  the default, and the run records it;
- an autopilot run implements with the default;
- an explicit choice at the plan gate (web, Slack, or `--agent-source`) still
  wins, and the run page then shows it as "changed at approval".

Planning is not yet affected: the plan turn still uses your own templates. A
run that carries a default is claimed only by a worker new enough to honour it;
while every online worker is older, the run stays queued and its health reason
names the missing worker version (`agent_source_request_v1`).

The run page's **Agents used** card shows the request and the outcome, for
example "Requested: my templates (repo default) · Implemented with: my
templates (as requested)". `uzi run get` shows the same facts as the
`AGENTS_REQUESTED` and `IMPLEMENTED_WITH` rows, and `uzi repo list` shows each
repo's setting (`agent_source_default` in `--json`).

## Repo agents on Codex runs

On a Codex run the agent selection applies the same way: implementation
delegates to the roster you chose at the plan gate (by default the detected
repo roster), honours your exclusions, and falls back to your own templates when
the selection is invalid. Planning keeps your own templates. The model rule
below applies to both harnesses; Codex also filters tools:

- **Markdown `model:`** maps by vocabulary: a supported Codex role id is a
  Codex pin; any other well-formed model id is a Claude pin, including custom
  Claude ids. A run silently ignores the other harness's pin, so that subagent
  follows the lead. For example, `gpt-6-sol` pins Codex only, while `opus` pins
  Claude only. Codex role pins include `gpt-6-luna`.
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
- **Optional model:** a top-level `model` string can pin a Codex subagent to a
  supported Codex role ID, including `gpt-6-luna`, using the same vocabulary as
  Markdown Codex pins. An absent `model` or exactly `model = ""` follows the
  resolved lead model. A whitespace-only, unsafe, unsupported, wrong-family,
  or non-string value skips the agent with a bounded invalid-file note that
  does not echo the model value. On Claude runs the pin is omitted, so the
  subagent follows the resolved lead model.
- **Model override and effort:** the schedule's "Use the run's resolved model
  for subagents too" toggle (`override_subagent_model`) overrides the TOML pin. Reasoning effort
  still comes from the existing run settings; `model_reasoning_effort` is ignored.
- **Default tools:** a TOML agent runs with the run's full default tool set,
  which can include `Bash`, subject to the guards below.
  `nickname_candidates`, `config_file`, `includes`, and other unrecognised keys
  are ignored.
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

### Which revision is read

uzi never reads the agent folders from the run's working tree. On a run's
first claim the worker fetches the repo and resolves the repository's
configured default branch from its remote-tracking ref after that fetch, never
from another branch. It reads `.claude/agents/` and `.codex/agents/` from git
objects at that commit and reports the commit together with the roster. No
agent turn starts until the server confirms it recorded that commit. A
`--require-base` run held because its kept work is not attested
(`base_attestation_missing`) has the commit recorded the same way before the
hold. The server records the commit once per run and refuses a later report
that names a different one.

Every later claim of the same run (a resume, a requeue, a restart on another
worker) reads the agent folders at the recorded commit. Edits a run makes to its
own agent files, and agent files already on the branch an `mr_rework` or
`ci_fix` run starts from, therefore never change the roster that run uses. The
run view shows the revision under **Agents used**.

Agent files stay writable: a run can still change them as part of its task. The
change reaches runs whose first claim comes after it is merged into the default
branch.

The read follows the same rules as the folder layout above: a symlink for
`.claude`, `.codex` or either `agents` folder means that folder is not read (it
still counts as present, so there is no fallback to the other folder), symlinks
and submodules inside the folder are skipped, and the same caps apply.

The agent does not start, and the run is held as **repo agents unreadable**
(`hold_reason` `repo_agent_source_unavailable`), when the worker:

- cannot resolve the configured default branch (the repository has none, the
  branch is missing from the fetch, or the repository has no commits yet);
- cannot get the server's confirmation that it recorded the commit (the report
  failed, was refused, or its answer did not name the commit);
- cannot read the agent folders at the commit, or the recorded commit is missing
  from its repository cache and fetching it fails.

When the worker resolved a commit, the hold records it under the same
record-once rule, so the claim after an owner resume reads the same commit even
if the default branch has moved since. Resume the run once the repository can
be fetched, or cancel it.

A run that reads repo agents is marked when it is created
(`repo_agent_source_required`). Only a worker that reports the
`repo_agent_source_v1` protocol capability claims a marked run or a run with a
recorded revision, its first claim included. Judge, job, cross-check,
task-review and chat runs do not read repo agents and are not marked. While no
such worker is online, a queued run's health reason names the capability.

When the API is older than the worker, nothing is recorded: each claim reads
the agent folders at the configured default branch's commit at that time, and
a failure to read them leaves the run on the owner's own agent templates.

## What is loaded, and what is never

uzi parses the agent files itself; it never points Claude Code at the repo's
`.claude/` directory. (This section describes `.claude/agents/*.md` files;
`.codex/agents/*.toml` files are covered under
[Which folder is read](#which-folder-is-read).) Each file's `name`, `description`, `tools`, and `model`
frontmatter is honored, with model pins filtered for the run's harness as
[described above](#repo-agents-on-codex-runs) — including a `tools:` entry naming one of the
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
  lead refuses a bare "approved" or "LGTM". It inspects the evidence a repo agent
  ties to the current revision (commit, commands and their results, file:line)
  and spot-checks it. It does not re-run a valid gate or redo an investigation
  only because a repo agent did it; it re-runs on missing or stale evidence,
  changed code, a contradiction or a concrete concern. A different repo agent can
  still review independently, but an agent checking its own work is not an
  independent review. The merge request records that the run used repo agents so
  the human reviewer knows.
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
