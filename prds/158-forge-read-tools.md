# PRD #158: Forge read tools — let a run check a claim against the forge instead of against the repo's copy of it

**GitLab Issue**: [#158](https://gitlab.example.com/vtmocanu/uzi/-/issues/158)
**Status**: Draft (created 2026-07-27)
**Priority**: Medium

## Problem

A run can read its clone. It cannot read its forge. Issue state, MR state, pipeline
results and label history all live outside the working tree, and there is no path from
inside a run to any of them.

The judge raised this twice, independently, as `install_worker_tool: glab`:

- Run `1dfc65b4` — a `fact-checker` tried `glab ci list` to corroborate deploy state and
  got "No such file or directory."
- Run `b64b98f3` — a `fact-checker` verifying a load-bearing baseline number against
  GitLab issue #128 found no `glab` binary, so external corroboration was impossible. It
  **fell back to the in-repo PRD copies of that number.**

### Why this is worse than a missing tool

The second case is the one that matters, and its cost is not the wasted tool call.

The claim under test came from the repo. The only source available to check it against
was **also** the repo. That is not verification, it is restatement — and it returns a
confident PASS. There is no error, no degraded-confidence marker, nothing in the report
that distinguishes it from a real verification. A reader of that review cannot tell.

This is the exact failure class the agent-team workflow doc already names ("a
verification that cannot fail is not evidence"; "the check can be as blind as the
claim"), and the class `#157` recorded honestly for the prompt fence rather than
claiming closed. Every other gap in the backlog fails loudly. This one manufactures false
confidence in the mechanism the factory uses to catch its own mistakes, which is why it
is worth more than its twice-in-21-runs frequency suggests.

### Why installing `glab` is not the fix

The forge PAT is absent from the agent environment **by construction**, in two
independent places:

- `agent/src/git.ts` — the PAT-handling directive: the token reaches authenticated git
  operations through env-scoped config only, never argv, never on-disk config, never a
  log line.
- `agent/src/sdk-env.ts` — `buildCheckEnv` builds a *scrubbed replacement* env rather
  than spreading `process.env`, so the join token and API URL are "ABSENT BY
  CONSTRUCTION" for anything agent-authored.

Against a private GitLab, an installed `glab` is an unauthenticated binary: ~50 MiB of
image to make a tool visible that cannot answer a single question. MR !130 added five
tools to the worker toolchain and deliberately excluded `glab` for this reason. The
obvious follow-up — "give it a token" — reopens a posture two PRDs closed on purpose.

### What already exists

Most of this is built, which is the main argument for doing it now rather than later.

**The reads, for both backends:**

- `api/internal/forge/gitlab.go` — `ListIssues` (:246), `GetIssue` (:277),
  `ListIssueLabelEvents` (:347), `GetMergeRequest` (:376), `ListPipelineJobs` (:428)
- `api/internal/forge/forgejo.go` — the same set (:293, :338, :781, :721), plus
  `forgejo_pipelines.go:112`

**The delivery mechanism:** `agent/src/uzi-tools.ts` already runs an agent-facing MCP
tool server (`list_runs`, `get_run`, `get_run_messages`, `propose_issue`) in which the
**worker** holds the credential, makes the call, and returns the payload through
`wrapEvidence` — untrusted-evidence framing behind a per-call CSPRNG nonce, so a hostile
payload cannot forge a closing fence and break out.

**The call path:** `WorkerClient` (`agent/src/client.ts:56`) already carries run-scoped,
join-token-authenticated methods of exactly this shape — `getChatRun`, `getTrace`,
`createProposal`, `saveMemory`, `getMemory`. This PRD adds peers to those, not a new
pattern.

Note that `agent/src/forge.ts:66-67` keeps the TypeScript `ForgeClient` at one method on
purpose: *"the worker never reads issues, labels, or pipelines; that surface is the Go
driver's."* This PRD **respects that split** — no forge HTTP from the agent side. The
worker asks the uzi API; the API asks the forge.

## Solution Overview

Worker-mediated forge reads, exposed to the agent as MCP tools:

```
agent  --(MCP tool call)-->  worker  --(join-token, run-scoped)-->  uzi API
                                                                      |
                                                        existing Go forge driver
                                                                      |
                                                                   GitLab / Forgejo
```

Five verbs, scoped to **the run's own project**, **read-only**, every payload returned
through `wrapEvidence`:

| Tool | Backed by | Answers |
|---|---|---|
| `get_issue(iid)` | `GetIssue` | "what does issue #128 actually say?" |
| `get_merge_request(iid)` | `GetMergeRequest` | "did that MR merge, or is it still open?" |
| `get_pipeline_jobs(id)` | `ListPipelineJobs` | "did CI actually pass, and which job failed?" |
| `list_issues(filters)` | `ListIssues` | "is there already an issue for this?" |
| `list_issue_label_events(iid)` | `ListIssueLabelEvents` | "when was this triaged, and by whom?" |

The agent never holds a credential, never reaches the forge directly, and cannot address
another project.

## Scope decision: listing is IN, and what bounds it

The narrower option (the three point-lookup verbs the two judge recs literally needed)
was considered and **rejected by the owner in favour of including `list_issues` and
`list_issue_label_events`** (2026-07-27).

The cost is real and stated here rather than discovered later: **listing turns a targeted
lookup into project enumeration.** A run can iterate its project's issues, which a
point-lookup verb cannot do. The mitigations are therefore load-bearing rather than
decorative:

- **Own project only.** The project id is derived server-side from the run record. It is
  never a tool parameter, so no argument the model can write selects a different project.
- **Read-only.** No verb mutates. `propose_issue` remains the only write path and keeps
  its existing human confirmation.
- **Bounded results.** `list_issues` takes a hard server-side cap and paginates
  explicitly; truncation is stated in the payload (the `#157` lesson — a truncated result
  that reads as exhaustive is how a false belief gets manufactured).
- **No cross-project search.** Whatever the driver supports, the endpoint exposes only
  filters scoped within the run's project.

## Implementation Milestones

- [ ] **M1 — Run-scoped forge read endpoints on the API.** New worker-authenticated
  routes that resolve the run, derive its project from the run record (never from the
  request body), and call the existing Go forge driver. Register limiter mounts
  (`route_limiter_mounts_test.go` asserts every route has one). Apply the existing
  redaction path so no token or internal URL rides out in an error. Owner-scoped exactly
  like the `getTrace` / `saveMemory` precedents.
- [ ] **M2 — `WorkerClient` methods and the five MCP tools.** Peers to `getChatRun` /
  `getMemory` in `agent/src/client.ts`, then the tool definitions in
  `agent/src/uzi-tools.ts` added to `uziToolNames()` so they are actually callable.
  Every payload returned through `wrapEvidence`. Failures return `toolError` and never a
  silent empty result, so an agent cannot read "no issues" from a failed lookup.
- [ ] **M3 — Injection and truncation posture, measured not asserted.** Forge payloads
  are attacker-influencable prose (an issue body is user-authored text). Run a hostile
  corpus through the real handlers the way `#157` did for directory names, and record
  what is verified (fence construction, tag-forgery resistance, truncation surfaced)
  separately from what is not (that the model honours the fence — an inherited
  assumption, not something this work establishes). Truncated list results must say so.
- [ ] **M4 — Tests.** Go handler tests for project derivation (a request that names
  another project must fail), the result cap, and error redaction. Agent-side unit tests
  over the five handlers including the failure paths. Both suites are the repo's existing
  gate, not new machinery.
- [ ] **M5 — Make the capability reachable.** A tool nobody knows about does not get
  used. Update the `fact-checker` role guidance to reach for these when a claim concerns
  issue, MR or CI state, and state the reproducibility caveat (below) where the agent will
  read it. Without this milestone the feature ships and changes nothing.
- [ ] **M6 — Docs and an acceptance run.** Document the tools where the other agent-facing
  tools are documented, then perform a real run whose fact-checker verifies a claim
  against the forge and cites what it read. Per this repo's archive convention, the PRD
  moves to `prds/done/` only after that run has actually happened — a green e2e does not
  substitute for it.

## Success Criteria

1. A `fact-checker` can answer "what does issue #N say?" from inside a run, and cites the
   forge rather than the repo's restatement.
2. The three situations from the originating judge recs are all answerable:
   issue #128's content, `glab ci list`-style CI state, and MR state.
3. No credential reaches the agent environment. The `git.ts` and `sdk-env.ts` invariants
   are unchanged, and a test asserts it.
4. A tool call naming a different project fails rather than succeeding quietly.
5. A truncated list result is visibly truncated in the payload the agent receives.
6. The acceptance run of M6 has been performed and cited.

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| **Enumeration surface** from `list_issues` (accepted, owner decision) | Own project only, derived server-side; hard result cap; no cross-project filters |
| **Prompt injection** — issue bodies are attacker-influencable text entering agent context | `wrapEvidence` nonce fence, the same construction as memory and job logs; M3 measures it against a hostile corpus rather than asserting it |
| **Non-reproducible verdicts** — forge state is mutable, so re-judging a run later may read different content than the run saw | Tool payloads land in `run_messages` as `tool_result` frames, which is what the judge trace already reads (`judge.sql:45` covers `tool_use` + `tool_result`) — so the run's evidence is captured at read time. **M1 must verify this actually holds for MCP tool results** rather than assume it; if it does not, snapshotting becomes an explicit sub-task |
| **Forge API load** from a run that loops a listing call | Result caps plus the existing route limiter mounts |
| **Feature ships unused** | M5 exists precisely for this and is not optional |

## Related Work

- **#157** — the fence construction and its honest-limit register that M3 follows.
- **PRD #121** — the `wrapEvidence` / untrusted-evidence pattern in its current form.
- **MR !130** — added five worker tools and deliberately excluded `glab`; this PRD is the
  reason that exclusion is not a gap.
- **Judge recs** `8b7d83f7` (run `b64b98f3`) and `ca3a1bbd` (run `1dfc65b4`) — the
  originating observations.

## Decision Log

- **2026-07-27 — Worker-mediated tools, not `glab` + a token.** Installing a forge CLI
  requires a credential in the agent environment, which two PRDs closed by construction.
  The worker already mediates credentialed calls for runs, memory and proposals; this
  reuses that path and grants no new authority.
- **2026-07-27 — Reads stay in the Go driver; the TypeScript `ForgeClient` is not
  widened.** `agent/src/forge.ts:66-67` states the split deliberately. Growing a second
  read path in TypeScript would duplicate a working driver and double the surface to
  audit.
- **2026-07-27 — Listing and label events INCLUDED (owner decision).** The narrower
  three-verb option covered exactly what the two judge recs reached for and could not
  grow into enumeration. The owner chose the wider set for open-ended research value,
  accepting the enumeration surface; the bounds above are what keep that acceptable, so
  they are requirements rather than nice-to-haves.
- **2026-07-27 — Prefetching forge context into the prompt was considered and rejected as
  a substitute.** It is cheaper and needs no new endpoint, but it is static: it answers
  the questions predicted at prompt-assembly time. A fact-checker's job is asking the
  question nobody predicted, so prefetch cannot serve this use case. It remains viable as
  a separate feature.
