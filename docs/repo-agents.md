---
title: Repo agents
order: 52
audience: user
---

# Repo agents

Some repos ship their own agent roster in a `.claude/agents/` directory: a
team that knows a codebase can define the exact `coder`, `reviewer`, and
specialist roles it wants. When a run clones such a repo, uzi detects that
roster and lets you run the repo's agents instead of your own
[agent templates](./agent-templates.md) — chosen at the plan gate, per run.

The `lead` orchestrator is always uzi's own builtin and is never replaceable;
repo agents and your templates only ever supply the **subagents** it delegates to.

## Choose the agents at the plan gate

When a run reaches the approval gate, the plan panel shows an **Agents for this
run** section with two cards:

1. **Repo agents** — the roster detected in the repo's `.claude/agents/`. This
   is the default when a repo ships one; the card lists the detected names.
2. **My agent templates** — your uzi templates. This is the default (and the
   repo card is inert) when the repo has no `.claude/agents/`.

Pick one source, then click any agent chip to exclude it from the run. At least
one subagent must remain. The choice locks in when you approve; the run view then
shows which roster ran. Autopilot runs and CI-fix runs (which have no human gate)
apply the default automatically — repo agents when detected, else your templates —
and record what they used.

Approving from a [Slack](./slack.md) DM offers the source choice too: when a repo
roster is detected the message shows two approve buttons — **Approve · repo agents**
and **Approve · my templates** — so you pick the source without leaving Slack.
Per-agent exclusions are a web-only refinement; use "Open in uzi" for those.

## What is loaded, and what is never

uzi parses the agent files itself; it never points Claude Code at the repo's
`.claude/` directory. Each file's `name`, `description`, `tools`, and `model`
frontmatter is honored — including a `tools:` entry naming one of the
[forge read tools](./forge-read-tools.md) (`mcp__forge__*`), which a repo
agent can grant itself the same way a template does. That surface is
read-only and scoped to the run's own project regardless of which agent
calls it. A repo agent's own hooks, settings, and slash commands
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
  sees it. Repo skills (`.claude/skills/`, opt-in per repo) already worked this
  way, for the same reason.

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

For how detection, validation, and the gate-boundary rebuild work, see
[ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates).
