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
frontmatter is honored. A repo agent's own hooks, settings, and slash commands
are **never** loaded, and the primary-directive guardrails always apply: no repo
agent can push to `main`, rewrite history, spawn nested agents, or schedule
deferred work, whatever its file says.

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

For how detection, validation, and the gate-boundary rebuild work, see
[ARCHITECTURE.md](../ARCHITECTURE.md#agent-templates).
