---
title: Agent memory trust
audience: design
---

# Agent memory trust

Cross-run agent memory (PRD #90) is a durable note a run's **lead** persists
via the `save_memory` tool, scoped to one `(user, repo)` pair. A later run on
the same repo, for the same user, gets it injected into its planning prompt as
context — never as an instruction. It exists so a hard-won operational
learning (a build flag, a setup quirk, a non-obvious gotcha) survives past the
run that found it, without needing the file-write path (which the worktree
guard denies outside the run's worktree, and which the per-run home directory
doesn't survive anyway).

It is **untrusted, advisory data, always**: `buildMemoryContext`/`memoryFrame`
(`agent/src/prompt.ts`) wraps every injected entry in a fenced block the lead
is told to treat as background it *may* weigh, never as commands or an
authority that overrides the task. That blanket frame alone was not enough —
see the incident below — which is why PRD #266 added per-entry provenance on
top of it.

## The incident this discipline closes

On 2026-08-09, a run asserted in its plan, without testing it, that the
repo's `coder` subagent "has no Edit/Write tools." The claim was false — a
subagent with no `tools:` override inherits every tool, Edit/Write included,
on the implement turn (only the *plan* turn strips write tools, PRD #203) —
but the run saved it to memory anyway. Two later, unrelated runs retrieved it
within 90 minutes, cited it as "the repo convention I noted," and serialized
implementation onto the lead's own thread instead of delegating to a fully
write-capable `coder`. Nothing forced the run to have tested the claim before
persisting it, and nothing on the reader side singled out that specific entry
as unverified. PRD #266
([../prds/266-agent-memory-provenance.md](../prds/266-agent-memory-provenance.md))
closes both gaps.

## Provenance: `basis` and `evidence`

`save_memory` takes a writer-declared `basis`, `"observed"` or
`"inferred"`, alongside the existing `title`/`body`:

- **`observed`** — the claim is backed by something nameable: a tool result,
  a command's output, a `file:line`. The pointer goes in the optional
  `evidence` field (a short string, capped like the other fields — a
  `file:line`, a command, a tool name, not prose).
- **`inferred`** — anything else, including an omitted `basis`, which
  defaults to `inferred` rather than being trusted by default.

This is a **writer-declared field, not an automatic classifier**: uzi cannot
verify an arbitrary natural-language claim, so it asks the lead to state
which kind of claim it is and carries that statement to the reader. A
dishonest or careless `observed` label is possible — this is a discipline
nudge, not a proof — but combined with the roster fix below, the specific
class of failure that produced the incident is closed regardless.

On read, the per-entry `basis` (and `evidence`, if present) travels the full
pipeline — store → `AgentMemoryDTO` → both mappers (`workerMemoryToDTO` for
the worker feed, `/me/memory` for the human one) → `agent/src/protocol.ts`'s
`MemoryEntry` → the prompt builder — and `buildMemoryContext` marks each
entry individually: an `inferred` entry (including a legacy row saved before
this field existed, which reads as unknown-basis and fails safe to
`inferred`) gets its own "re-verify against live code before acting on it"
caveat, distinct from the blanket advisory frame around the whole block. An
`observed` entry is marked as such and shows its `evidence` inline, so the
reading run can check what backs it. The point of doing this per-entry rather
than only in the blanket frame: the incident happened *under* the blanket
frame, so a confidently-worded false claim needs its own, specific flag.

## Where a human sees it

- **`/settings/memory`** (`web/src/components/Memory.tsx`) is the primary
  human surface: each entry renders a basis badge ("observed" vs "inferred")
  and, when present, its evidence pointer, so you can audit which stored
  claims were actually verified before trusting or deleting one.
- **`uzi memory list`** has a `BASIS` column in its table output;
  `uzi memory list --json` (and `/api/me/memory`) carry `basis` and
  `evidence` as fields on each entry.

`uzi memory rm <id>` is the purge control — the CLI has no `save`, because
writing memory is the lead's in-run action, not a human one; the CLI's job is
visibility and removal of an entry you've decided not to trust.

## The discipline

- **Prefer `observed` with a nameable evidence pointer.** If you can point at
  the tool result, command output, or `file:line` that backs a claim, say so
  and cite it — that's what lets a later run trust it without re-deriving it.
- **Treat anything you didn't verify as `inferred`.** That's the honest
  default, and it's also what an omitted `basis` becomes.
- **Do not persist claims about the run's own roster, tools, or runtime
  configuration.** Which subagent can Edit/Write, which is read-only, what a
  role inherits — that class of fact is knowable *live*, from the per-turn
  roster (which now states each subagent's write capability, derived from its
  implement-turn tool set — PRD #266 M1), and it decays as the product
  changes. A remembered copy can go stale and mislead a later run exactly the
  way the incident's copy did; `save_memory` appends an advisory nudge when a
  body's phrasing looks like this class of claim (`CONFIG_CLAIM_RE`,
  `agent/src/memory-tools.ts`), but the nudge is heuristic and will have
  false positives and negatives — the discipline is the backstop, not the
  regex.
- **The config-claim nudge, like the pre-existing volatile-snapshot nudge for
  fast-decaying tallies (a test-pass count, an "N of M" ratio), is advisory
  only.** Neither ever rejects a save; the entry is stored either way, with a
  note appended to the tool's response.
- **A `save_memory` failure — a cap hit, a rejected entry, a network error —
  never fails the run** (PRD #90). The tool reports it as a clear,
  non-fatal message and the run continues.

See [../prds/266-agent-memory-provenance.md](../prds/266-agent-memory-provenance.md)
for the full design record, including the incident's file:line citations and
the milestone-by-milestone build.
