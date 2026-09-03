---
title: Why a hosted run costs less than a local agent-team run
audience: contributor
---

# Why a hosted run costs less than a local agent-team run

A uzi run working a PRD end to end (plan, implement, review, MR) spends
noticeably fewer tokens than this repo's own dev-team workflow
(`.claude/agent-team.md`) working the same-sized task in a local Claude Code
session. The reflex explanation is "a cheaper model", and it's wrong: both
sides run on the same tier. The saving is structural, and it comes from four
places uzi's runtime differs from a local multi-agent session. This page
names them, then asks the harder question: is the cheaper run as good?

> **This page is about token spend, which is a different number from context
> fill.** Everything below tallies tokens *consumed* — what a run cost. The
> [lead context meter](./run-activity.md#lead-context-meter) in the Activity
> panel shows something else: how full the **lead's** live context window is
> *right now*, the fill that predicts the SDK's autocompaction. It's read
> once per lead turn from `query.getContextUsage()` and is lead-only — the
> SDK exposes a window for the main-loop session, not for subagents, so
> there's no context-fill number to add to a subagent's cost row.

## How a run's cost is counted

A run's lead makes one SDK `query()` call per turn — the planning turn, then
each implement/review iteration, each resumed on the previous turn's session
id. The Agent SDK reports cost **per `query()` call, not cumulatively over
the resumed session**, so every one of those turns is its own leg and its
`result` frame reports only that leg's tokens and cost. The run's stored
total (`run_usage`, and every rollup that reads it — the board, `uzi run
list`, `/api/usage`, `/api/admin/usage`) is the SUM of every leg, and the run
page's per-phase table shows each leg's figures exactly, not a running total
telescoped down to whichever leg happened to be largest. Historical runs
that predate this fold were re-folded once, automatically, from their still-
persisted message history on the first boot after the fix landed — no
action needed on a self-hosted instance. See [ADR-1079](../adr/1079-run-usage-per-leg-fold.md)
for the full design and the measured under-count this replaced.

## The model tier is not the difference

`override_subagent_model` (added by migration `00119_schedule_run_override_subagent_model.sql`,
PRD #305) defaults to `false` on both `run_schedules` and `runs`, meaning a
subagent's own pinned model wins unless a schedule opts in to overriding it.
Ten of the twelve builtin role templates under `api/internal/agenttmpl/builtins/`
pin `model: opus`; `documenter` and `researcher` pin `sonnet`. A local agent-team session
run against the same repo, on a subscription defaulted to Opus, is spending
the same tier for the same role. If a hosted run reads cheaper, it is not
because uzi quietly downgraded the model — it's because it asked the model to
do less redundant work.

## Where the savings come from

### 1. Orchestration is Go, not a model

A run's lifecycle (`queued → claimed → running → awaiting_approval → running
→ completed/failed`, see [ARCHITECTURE.md's Run lifecycle](../ARCHITECTURE.md#run-lifecycle))
is enforced by `api` Go code: the sweeper goroutine, the claim query, the
plan-gate state transition, and the message-persistence path all run outside
any model call and spend zero tokens. The lead itself is **one** resumed SDK
session per run (`agent/src/sdk-executor.ts`): a planning turn, then an
implement⇄review loop of resumed turns capped at `RUN_MAX_ITERATIONS`
(default 5), exiting when the lead calls `signal_done`.

A local agent-team run has no equivalent of the sweeper or the claim queue:
the *orchestrator* — a Claude Code session, not Go code — is what tracks
whose turn it is, relays every teammate's report, and decides what to spawn
next (`.claude/agent-team.md`, "Default flow for a typical task"). That
relaying is itself token spend, and it grows every turn: the orchestrator's
context is the running history of the whole task, not a fixed-size claim
record. A parked uzi run (awaiting approval, waiting on a rate limit) costs
nothing while it waits; an idle round-trip in a local session (a question to
the user, a wait for a teammate) still sits inside the one session whose
context keeps growing underneath it.

### 2. Every SDK session starts lean

Every SDK session uzi spawns — lead and subagent alike — runs with
`settingSources: []` (`agent/src/sdk-executor.ts`), so it never loads the
cloned repo's own `CLAUDE.md`, `.claude/rules/*`, `.claude/agents/*`, or
skills; the comment beside the option calls this out explicitly as
prompt-injection defense, not a cost optimization, but it has the cost effect
too. A subagent's entire role context is the body of its builtin template
under `api/internal/agenttmpl/builtins/` — measured today at roughly 2–13 KB
per role (`researcher.md` is the smallest at 2.3 KB, `tester.md` the largest
at 13.2 KB). The cloned repo's *own* `CLAUDE.md` still reaches the run, but
only the **lead** sees it, and only as a nonce-fenced `UNTRUSTED, ADVISORY`
block appended last, after every guardrail instruction (`agent/src/prompt.ts`,
PRD #246) — never as loaded instructions a model treats as authoritative.

A local Claude Code session auto-loads the project's root `CLAUDE.md` at
session start, by design — this repo's own is 49,756 bytes today — plus
whichever `.claude/rules/*.md` file a touched path pulls in (13–39 KB each)
and any skill in play. That happens once for the orchestrator and then again,
cold, for every subagent it spawns: `.claude/agent-team.md`'s own "Context
handoff" section is explicit that "every teammate cold-starts with no memory
of prior conversation," so a reviewer, an auditor, and a tester dispatched
for one milestone each pay that load independently rather than sharing it
with the lead the way a uzi subagent shares the run's context via the SDK's
own agent orchestration.

### 3. Subagents are in-process, not cold sessions

uzi's subagents are native SDK `AgentDefinition`s, mapped from the same
builtin templates and passed to a single `query({ options: { agents } })`
call (`agent/src/agents.ts`). Each one runs isolated and returns one result to
the lead; it doesn't fork a new top-level session, and it doesn't bloat the
lead's own context with its internal working.

A local agent-team wave is the opposite by construction: reviewer and auditor
"IN PARALLEL," each a fresh Claude Code session pinned to a commit SHA and
handed the coder's diff and report (`.claude/agent-team.md`, step 2). Each one
is a cold start that pays the `CLAUDE.md` + rules load described above again,
on top of whatever it needs to actually review.

### 4. Redundant local work with no uzi equivalent

Beyond the three structural points above, the local workflow this repo
documents does several things a hosted run has no reason to do:

- **Per-milestone gate re-runs across separate sessions.** The coder runs
  `task gate:api` / `gate:web` / … before reporting done
  (`.claude/agent-team.md`, step 1); if a validator wave sends a finding back,
  the coder re-runs the gate again after the fix, and each validator that
  re-checks a fix may re-read the diff from scratch.
- **Interactive design rounds.** A local session can go back and forth with a
  human on a design question before code exists. uzi's plan-approval gate is
  bounded — one plan, one approve/reject/revise cycle per gate, with a
  deadline (`agent/src/runner.ts`'s `gateDeadlines`) — not an open-ended
  conversation.
- **Cold re-reads.** A cold subagent that needs to understand a file
  re-reads it from disk; a uzi subagent sharing the lead's already-warmed
  worktree context by dispatch design does less of that.
- **Idle round-trips.** Waiting on a human, on another teammate, or on a
  clarifying question all happen inside the one session whose context keeps
  growing while it waits, as described above.

None of this is a flaw in the local workflow — it's tuned for maximum
independent scrutiny on work that's still risky enough to want it, which is
exactly the trade-off the next section is about.

## Is a cheaper run as good?

Cheaper is not automatically worse, and it is not automatically fine either.
The honest answer splits by what's being asked.

### Where the discipline carries over unchanged

The adversarial rigor this repo leans on is written into the builtin **role**
prompts, not into `CLAUDE.md` — so `settingSources: []` dropping `CLAUDE.md`
for every subagent doesn't drop the discipline with it:

- `tester.md` mandates mutation testing with positive controls: assert a
  mutation applied *textually*, then assert it changed *behavior*, because a
  mutation that applies cleanly and changes nothing proves nothing.
- `reviewer.md` treats a comment, a docstring, and a report sentence as
  assertions and reviews them as such — the vacuous-assertion catch.
- `auditor.md` carries the control/ANSI/bidi injection lens and the
  compound-predicate tenant-boundary check (a boundary check whose failure
  mode is silent once one half of a compound predicate is removed).
- `lead.md` mandates a single read-only validator wave per landed unit, over
  an immutable `<base>..<sha>`, rather than one end-of-run barrier.

A hosted run allocating the standard roster (reviewer, auditor, tester) is
running the same review lenses a local wave would, on the same model tier,
just without the redundant re-loads described above.

### Where it's genuinely weaker

- **Enforcement is prompt-level, not worker-enforced.** Once the lead calls
  `signal_done`, the worker pushes the branch and opens the MR directly
  (`agent/src/runner.ts`) — it does not re-run `task gate:*` itself. The
  lead's system prompt instructs it to run the repo's test suites
  (`agent/src/prompt.ts`), but nothing on the worker's side blocks a push if
  the lead skips that or the tests were flaky. The structural backstops are
  forge CI (which does run the real gate, independently) and human MR review
  — not a second, uzi-side gate run.
- **Fewer, less-independent passes, chosen by the lead.** A local wave's
  reviewer, auditor, and tester are dispatched by a human orchestrator who
  can escalate depth on a risky change. A hosted run's validation depth is
  whatever the lead template declares and whatever roster is allocated to
  the run; there's no human in the loop deciding to add a fourth pass.
- **No interactive design iteration.** One plan, one approval gate, and a
  bounded `ask_user` — not the back-and-forth a human reviewing a subjective
  design (a UX pass, a TUI redesign) would normally want. This is the axis a
  visually/subjectively judged change is most exposed on; it's not a
  correctness gap, it's a taste gap uzi has no mechanism to close.

### Where uzi is stronger

- **The run judge** ([docs/judge.md](./judge.md), `agent/src/judge-runner.ts`) is a
  genuinely independent retrospective pass: one structured-output model call,
  no tools at all (a deny-all tool hook plus `settingSources: []`, since the
  run's own trace is untrusted input), on the run owner's own Anthropic
  token — a check that can't be talked out of its verdict by the same
  conversation it's reviewing, because it never joins that conversation. Off
  by default, gated twice (admin, then per-user).
- **Forge CI is a structural gate an in-session claim can't fake.** A local
  session's coder self-reports a green gate; a hosted run's fix still has to
  pass the same CI pipeline a human would trust, independently of what the
  agent said about it.

### Net

For a default install (standard roster allocated, forge CI on),
correctness and security verification land at rough parity with a local
wave — same role prompts, same model tier, same review lenses, minus the
redundant reloads. The real gap is in the number of *independent,
human-anchored* checkpoints and in subjective design judgment, not in
intelligence or model tier.

## Practical guidance

To bring a cheap hosted run up to the rigor of a heavy local wave on a risky
change:

- Keep the **auditor** allocated and explicitly mandated on any diff that
  touches a trust boundary.
- Raise the declared validation depth in the milestone's own instructions for
  the security-relevant unit, rather than relying on the lead's default
  judgment.
- Enable the [judge](./judge.md) on high-value runs, so there's an
  independent retrospective pass even though nothing gated the push itself.

And conversely, a local run doesn't have to spend like one: the redundant
gate re-runs and idle round-trips described above are the places to trim
first, before reaching for a cheaper model.
