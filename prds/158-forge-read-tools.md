# PRD #158: Forge read tools — let a run check a claim against the forge instead of against the repo's copy of it

**GitLab Issue**: [#158](https://gitlab.example.com/vtmocanu/uzi/-/issues/158)
**Status**: Draft (created 2026-07-27; **substantially revised the same day** after an adversarial architect review found the first draft not implementable).
**Priority**: Medium

*What the review changed, because the corrections are the useful part: the draft built the
tools in the **chat** lane (`uzi-tools.ts`) while both originating recommendations came from
a **run**-lane `fact-checker` — so it shipped five verbs to the wrong agent. It also missed
that the feature would land **dead on every existing install** (the `fact-checker` tool
allowlist has no `mcp__*` entry, and the builtin seed never overwrites an existing row); that
no run-lane subagent has **ever** called an in-process MCP tool, an unproven interaction the
whole design rests on; that `ListIssues` cannot be capped where the draft said it would be;
and that three of six success criteria could not fail or could not be met. Seven code
citations were wrong. The reproducibility hedge was the one thing the draft got right, and it
is now **measured** rather than deferred. Decision Log has the full list.*

## Problem

A run can read its clone. It cannot read its forge. Issue state, MR state, pipeline results
and label history all live outside the working tree.

The judge raised this twice, independently, as `install_worker_tool: glab`:

- Run `1dfc65b4` — a `fact-checker` tried `glab ci list` to corroborate deploy state and got
  "No such file or directory."
- Run `b64b98f3` — a `fact-checker` verifying a load-bearing baseline number against GitLab
  issue #128 found no `glab` binary, so external corroboration was impossible. It **fell back
  to the in-repo PRD copies of that number.**

Both run ids are independently recorded at `agent/devbox-global/devbox.json:149`.

### Why this is worse than a missing tool

The claim under test came from the repo. The only source available to check it against was
**also** the repo. That is not verification, it is restatement — and it returns a confident
PASS. There is no error, no degraded-confidence marker, nothing distinguishing it from real
verification.

This is the failure class the agent-team workflow doc already names ("a verification that
cannot fail is not evidence"). Every other gap in the backlog fails loudly; this one
manufactures false confidence in the mechanism the factory uses to catch its own mistakes,
which is why it is worth more than its twice-in-21-runs frequency suggests.

**Precision, added in revision:** "no path to the forge" is not strictly true. The
`fact-checker` template carries `WebFetch` and `WebSearch`
(`api/internal/agenttmpl/builtins/fact-checker.md:4`). Against a **private** GitLab an
unauthenticated fetch cannot answer the question, so the argument survives — but the honest
claim is "no *authenticated* path", not "no path". Whether `WebFetch` is even reachable from
an agent container at runtime is an open question below.

### Why installing `glab` is not the fix

Stronger than the draft claimed. `glab` is not merely excluded from the toolchain — it is on
an **enforced denylist**: `api/internal/toolprofile/toolprofile.go:63-67` rejects
`glab`/`gh`/`hub`/`tea` "even if an admin allowlists one", with the reason stated there: a
logged-in credential-bearing CLI "would bypass the *worker holds the PAT, agent doesn't*
boundary" (Decision 6, test at `toolprofile_test.go:74-75`).

That boundary is real in two independent places:

- `agent/src/git.ts:172-176` — the PAT reaches authenticated git operations through
  env-scoped config only; never argv, never on-disk config, never a log line.
- `agent/src/sdk-env.ts:116-120,140` — `buildCheckEnv` builds a *scrubbed replacement* env
  rather than spreading `process.env`, so the join token and API URL are "ABSENT BY
  CONSTRUCTION" for anything agent-authored.

So an installed `glab` would be an unauthenticated binary, and authenticating it is
explicitly the thing the denylist exists to prevent. MR !130 (commit `2450bf7b`) added five
worker tools and excluded `glab` for exactly this reason.

### What already exists — and, critically, in which lane

**The reads, for both backends** (all verified):

- `api/internal/forge/gitlab.go` — `ListIssues` :246, `GetIssue` :277,
  `ListIssueLabelEvents` :347, `GetMergeRequest` :376, `ListPipelineJobs` :428
- `api/internal/forge/forgejo.go` — the same set at :293, :338, :781, :721, plus
  `forgejo_pipelines.go:112`
- Also present and **needed** (see M2): `LatestPipeline` (`forge.go:369`),
  `LatestMRPipeline` (`forge.go:378`), `JobLogTail` (`forge.go:393`)

**The evidence-framing pattern:** `wrapEvidence` (`agent/src/uzi-tools.ts:59`) — untrusted
evidence behind a per-call CSPRNG nonce, from **PRD #39 Decision 7**.

**THE LANE DISTINCTION, which the first draft got wrong:**

`agent/src/uzi-tools.ts` is the **chat** lane. Its own header says so ("the chat agent calls
to investigate its OWNER'S runs"), and it is imported only by `chat-runner.ts` and the chat
executor. The **run** lane builds its MCP servers at `agent/src/sdk-executor.ts:329-334`, and
there are exactly two: signal and memory.

The `fact-checker` is a run-lane subagent. Anything added to `uzi-tools.ts` is invisible to
it.

**And the run lane denies its MCP servers to subagents.** `SIGNAL_SERVER_NAME = "uzi"`
(`signals.ts:20`), and `agents.ts:102` puts both server denials in `disallowedTools` for
**every** subagent. So a new run-lane server must carry a **distinct name** and must **not**
be added to that denylist — and `mcp__uzi` is unavailable as a name.

`agent/src/forge.ts:65-67` keeps the TypeScript `ForgeClient` at one method deliberately:
"the worker never reads issues, labels, or pipelines; that surface is the Go driver's." This
PRD respects that split — no forge HTTP from the agent side.

## Solution Overview

```
fact-checker (run-lane subagent)
      |  MCP tool call to a NEW, distinctly-named in-process server
      v
   worker  --(join token, run-scoped)-->  uzi API  -->  Go forge driver  -->  GitLab / Forgejo
```

Read-only, scoped to the run's own project, every payload through `wrapEvidence`.

| Tool | Backed by | Answers |
|---|---|---|
| `get_issue(iid)` | `GetIssue` | "what does issue #128 actually say?" |
| `get_merge_request(iid)` | `GetMergeRequest` | "did that MR merge, or is it still open?" |
| `get_pipeline_jobs(id)` | `ListPipelineJobs` | "which job failed?" |
| `latest_pipeline(ref \| mr_iid)` | `LatestPipeline` / `LatestMRPipeline` | "did CI pass?" — **added in revision**; without it no agent can obtain a pipeline id, and the `glab ci list` recommendation stays unanswerable |
| `list_issues(filters)` | `ListIssues` | "is there already an issue for this?" |
| `list_issue_label_events(iid)` | `ListIssueLabelEvents` | "when was this triaged?" |

## Scope decision: listing is IN, with a stopping rule

The owner chose the wider set over the three point-lookup verbs (2026-07-27). The costs, all
identified in review:

- **Listing is enumeration.** A run can iterate its project's issues.
- **`list_issue_label_events` is *actor* enumeration** — who applied which label, when, across
  the project. A different disclosure class from issue prose, and not narrowed by "own project
  only".
- **The cap on listing does not bound content extraction.** List rows deliberately omit
  descriptions (`forge.go:310-312`); `get_issue` supplies them. So `list_issues` → N×
  `get_issue` is uncapped by any per-call limit.

**STOPPING RULE (new, and binding).** The verb set above is closed. It was chosen by what the
two originating recommendations needed, not by what the driver exposes. Adding a verb —
`JobLogTail`, MR diffs, cross-project search — requires a new Decision Log entry stating which
*observed* failure demands it. Without this rule the M6 acceptance run generates the next
three verbs by itself: an issue body saying "see #131 and !77" makes the model want both.

**Bounds that are requirements, not aspirations:**

- **Own project only**, derived server-side from the run record; never a tool parameter.
- **Read-only.** `propose_issue` (chat lane) remains the only write path.
- **A per-run call budget across all five verbs.** This, not a per-call cap, is the bound that
  actually limits extraction.
- **Explicit truncation markers** in the payload (see M1 — this is a *wire contract*, not an
  M3 afterthought).

## Implementation Milestones

- [ ] **M0 — SPIKE: prove a run-lane subagent can call an in-process MCP tool at all.**
  **Nothing below works if this is false, and it is currently unproven.** Both existing
  run-lane servers are denied to every subagent, the live DB has zero `mcp__*` rows in
  `tool_use`, and the repo states its own uncertainty at `signals.ts:145-147`: whether
  `disallowedTools` wins over a template's explicit `tools` allowlist is "unproven from the
  SDK types". Stand up a trivial non-denied server, give one subagent an allowlist entry for
  it, and observe a real call. Timebox it; a negative result reshapes the whole PRD and is
  worth knowing on day one rather than at M4.

- [ ] **M1 — Run-scoped forge read endpoints on the API, with the truncation contract.**
  Worker-authenticated routes that resolve the run and derive its project **from the run
  record**. Authorization follows `saveMemory`, whose test is the precedent worth copying
  (`agent_memory_test.go:84` `TestWorkerSaveMemoryDerivesIdentityFromRunClaim`, and :148
  `TestWorkerSaveMemoryRejectsIdentityInBody`). *Not* `getTrace` — that is judge-run-scoped,
  not owner-scoped (`judge.sql:15-21`).
  **Repoless runs**: `chat` and `judge` runs have `repo_id IS NULL`
  (`00058_run_judge_self_improve_kinds.sql:38-39`). Return the existing 409, matching
  `TestWorkerSaveMemoryRepolessRun409` (:160).
  **Rate limiting**: every `/api/worker/*` route is currently `noLimiter`
  (`route_limiter_mounts_test.go:199-204,260-267`). The precedent to copy is the **PerWorker**
  limiter on proposals (`handler.go:915`, `middleware/ratelimit.go:122-128`, budgets
  `config.go:582-583`). The route table needs a row either way — `noLimiter` is spelled out
  rather than omitted.
  **Truncation is a wire contract decided here**, not retrofitted by M3.

- [ ] **M2 — Widen the driver where the bounds require it, then the run-lane MCP server.**
  Two pieces the draft missed:
  1. **`ListIssuesOptions` is `{Labels, UpdatedAfter}` and nothing else** (`forge.go:285-292`),
     and the interface contract says implementations "paginate internally and return complete
     result sets" (:294-296). So a server-side cap truncates *after* fetching the whole
     project — it bounds what the agent sees and nothing about what uzi asks the forge for.
     Real pagination means widening the options struct, both driver implementations, and that
     doc comment.
  2. **The MCP server itself, in the RUN lane** (`sdk-executor.ts:329-334`), under a name that
     is not `uzi`, deliberately absent from the `agents.ts:102` denylist, with `WorkerClient`
     peers alongside `getChatRun`/`saveMemory` (`client.ts:179,234`).
  `wrapEvidence` currently lives only in `uzi-tools.ts:59` (chat lane) and must be lifted into
  a shared module — a file this milestone creates.
  **M2 is not independently shippable**: without M3 it ships tools no subagent can reach.

- [ ] **M3 — Make the capability REACHABLE (was M5, and it is the milestone the feature lives
  or dies on).** Prose guidance is the smallest part.
  1. **The `fact-checker` tool allowlist has no `mcp__*` entry**
     (`builtins/fact-checker.md:4`). A non-empty list is an explicit allowlist honoured
     verbatim (`agents.ts:104-110`), so today it *cannot* call the tools. Note `coder` and
     `lead` have NULL tools and inherit everything — so without this the lead would silently
     get forge access and the fact-checker would not, which is precisely backwards.
  2. **Editing the builtin does not propagate.** The startup reconciler is
     `ON CONFLICT (name) WHERE scope <> 'user' DO NOTHING`
     (`agent_templates.sql:67-74`) — "never overwrite an existing row (admin edits survive
     restarts)". The live DB confirms `fact-checker` still carries the old list. **On every
     existing install the feature ships dead** without a migration or a documented admin reset
     (`agent_templates.go:395-434`).
  3. Then the role guidance, including the reproducibility caveat.

- [ ] **M4 — Injection and truncation posture, measured not asserted.** Forge payloads are
  attacker-influencable prose. Run a hostile corpus through the real handlers as issue #157
  did for directory names, and record what is verified (fence construction, tag-forgery
  resistance, truncation surfaced) separately from what is not (that the model honours the
  fence — inherited from the existing `wrapEvidence` call sites, not established here).

- [ ] **M5 — Tests.** Go handler tests for project derivation (a **body-supplied** project id
  must be ignored, not honoured), the repoless 409, the result cap, error redaction. Agent-side
  unit tests over the handlers including failure paths (a failed lookup must not read as "no
  issues").

- [ ] **M6 — Docs and an acceptance run.** **The docs page does not exist and must be
  created**: `docs/chat.md` never lists the chat tools, `docs/configuration.md:230` names
  `propose_issue` only in a rate-limit table, and `docs/worker-tools.md` is about devbox CLI
  tools entirely. Also correct `docs/chat.md:36-37,41-42` if anything lands in the chat lane —
  it currently says the chat agent has "no network tools". Then a real run whose fact-checker
  verifies a claim against the forge and cites what it read. Per the archive convention, this
  PRD moves to `prds/done/` only after that run has happened.

## Success Criteria

1. A `fact-checker` — not the lead, not the chat agent — calls a forge read tool in a real run
   and cites the forge rather than the repo's restatement.
2. All three originating situations are answerable: issue #128's content, MR state, and CI
   status **including obtaining the pipeline id** (which is why `latest_pipeline` was added).
3. **No forge base URL, numeric project id, or credential fragment appears in any tool payload
   or error string the agent receives.** *(Rewritten: the draft asserted two files stay
   unchanged, which the design guarantees before a line is written — it could not fail. This
   targets the surface that actually changes.)*
4. **A body-supplied project id is ignored, not honoured** — the shape of
   `TestWorkerSaveMemoryRejectsIdentityInBody`. *(Rewritten: the draft's "a call naming another
   project fails" is unfalsifiable when the project is never a parameter.)*
5. A truncated list result is visibly truncated in the payload, and a payload that would exceed
   the tombstone threshold never silently disappears from the judge trace.
6. The M6 acceptance run has been performed and cited.

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| **M0 is false** — subagents cannot call in-process MCP tools | Spike first. A negative result reshapes the PRD; finding out at M5 wastes the whole build |
| **Ships dead on existing installs** | M3's migration/admin-reset, not prose. This is the draft's "feature ships unused" risk actually materialising |
| **Enumeration**, incl. actor graphs via label events (accepted, owner decision) | Server-derived project scope; real driver pagination (M2); **per-run call budget across all verbs** — the per-call cap does not bound `list_issues` → N× `get_issue` |
| **Forge API load** | Genuinely unmitigated until M2 widens `ListIssuesOptions`; today every list is a full-project fetch and no worker route is rate-limited |
| **Prompt injection** from issue bodies | `wrapEvidence` nonce fence; M4 measures it against a hostile corpus |
| **Oversize payloads vanish from the trace** | `batcher.ts:39` caps at 900 KiB and **tombstones** above it (`:340-348`), rewriting `kind` to `status`; `judge.sql:45` selects only `tool_use`/`tool_result`, so an oversize read shows as *nothing*. Makes the cap a **trace-integrity** requirement. Calibration: largest live `tool_result` today is 13,815 bytes, average 1,558 |
| **"What was read" is not in the result** | `tool_result` carries no command (`judge.sql:35-38`); the iid lives only in the paired `tool_use`, and `ListToolTraceForRun`'s `LIMIT` can split the pair |
| **Verb set grows** | The stopping rule above, enforced by requiring a Decision Log entry |

## Reproducibility: MEASURED, not assumed

The draft deferred this to M1. It is now settled: **MCP tool results do persist** to
`run_messages` as `tool_result` rows on the same channel as built-in tools, so the judge trace
captures what a run read. Established by querying the live DB for `tool_result` rows with no
matching `tool_use` — every orphan is verbatim the return string of an in-process SDK MCP tool
(e.g. `signals.ts:97`), orphaned precisely because `sdk-executor.ts:824` filters signal
`tool_use` out of the persisted stream. Corroborated by the pinned SDK types
(`sdk.d.ts:4583-4591`) and by subagent frames persisting with attribution.

Forge state remains mutable, so a re-judge may read different content than the run saw. The
captured `tool_use`/`tool_result` pair is what makes the run's own evidence auditable after
the fact.

## Open questions

1. **Can a run-lane subagent call a non-denied in-process MCP tool?** M0. Unproven from the SDK
   types by the repo's own admission; no instance exists to observe.
2. **Can the `fact-checker`'s existing `WebFetch` already reach the forge?** If yes, a cheaper
   partial fix may exist and the Problem section overstates. Untested — needs an
   agent-container egress check.
3. **Where does a real `list_issues` payload cross the 900 KiB tombstone threshold?** Sets the
   cap. Unmodelled.
4. **Is there a shared redaction helper the handler layer can apply?** Per-method redaction
   *tests* exist in the drivers, but no single reusable path was located. If none exists, M1
   writes one.
5. **The two rec ids** (`8b7d83f7`, `ca3a1bbd`) and the quoted error text are unverified; the
   two *run* ids are confirmed.

## The originating recommendations will keep firing

`install_worker_tool: glab` is generated by a **missing-executable scan over the tool trace**
(`judge.sql:29-33`), not a capability check, and `glab` is permanently denied
(`toolprofile.go:67`). So both recommendations recur verbatim after this ships and will read
as unaddressed. Either suppress the recommendation for denylisted binaries, or accept it and
record here that recurrence is expected. **Decide before M6**, or the acceptance run will look
like a failure.

## Related Work

- **PRD #39 Decision 7** (`prds/done/39-chat-agent.md`) — the `wrapEvidence` untrusted-evidence
  pattern. *(The draft cited PRD #121, which contains no reference to it.)*
- **Issue #157** — the fence's honest-limit register M4 follows. **Not a PRD**; its record is
  `specs/ai.md` §417 plus commits `145fe334`, `1b104198`, `4e6350e0`, `5953c75f`.
- **PRD #18 / Decision 6** — the credential-CLI denylist that makes `glab` non-viable.
- **MR !130** (`2450bf7b`) — added five worker tools, excluded `glab`.
- Judge recs `8b7d83f7` (run `b64b98f3`) and `ca3a1bbd` (run `1dfc65b4`).

## Decision Log

- **2026-07-27 — Worker-mediated tools, not `glab` + a token.** Reinforced in revision: `glab`
  is on an enforced denylist, so "install it and authenticate it" is not merely discouraged.
- **2026-07-27 — Reads stay in the Go driver.** `forge.ts:65-67` states the split deliberately.
- **2026-07-27 — Listing and label events INCLUDED (owner decision).** Enumeration surface
  accepted; the bounds are requirements. Revision adds the actor-enumeration cost, the
  list→get content path, and a per-run call budget.
- **2026-07-27 — Prefetching forge context rejected as a substitute.** Static; cannot serve a
  fact-checker whose job is the unpredicted question. Viable as a separate feature.
- **2026-07-27 (revision) — RUN lane, not chat.** The draft's M2 targeted `uzi-tools.ts`, which
  serves the chat agent. Both originating recommendations came from a run-lane `fact-checker`.
  The new server needs a distinct name (`uzi` is the signal server) and must stay off the
  subagent denylist.
- **2026-07-27 (revision) — `latest_pipeline` added.** Without it no agent can obtain a
  pipeline id, and the `glab ci list` recommendation was unanswerable by the proposed verb set.
- **2026-07-27 (revision) — M0 spike added.** The design rests on an interaction the repo
  itself calls unproven.
- **2026-07-27 (revision) — a stopping rule for the verb set.** The set was chosen by what the
  driver exposes; without a rule it grows on contact with the first issue body citing another.
