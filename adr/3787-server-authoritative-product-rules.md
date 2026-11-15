# ADR-3787: Product rules have one authoritative implementation on the server

**Status**: Proposed (prompted by the reporter's suggestion on #3787; for review with #3766, #3782 and #3788)

This ADR proposes the policy below; it does not assert that existing clients or APIs already implement it.

## Decision (summary)

Product decisions have one authoritative implementation in server-side domain services. Clients consume the resulting decisions and reasons; they may duplicate checks for usability or defense in depth, but those checks never replace server enforcement.

"Clients" means every consumer of those decisions: the web SPA, the `uzi` CLI and TUI, Slack, `/api/v1` callers, repo scripts, and in-process callers such as the operator `/server setup` command (PRD #3760), which call the same services directly rather than through HTTP.

## Context

uzi has several surfaces over one API (board and web, forge, the run lane, Slack, Chat, plus the CLI), and the repo already treats the CLI as a second consumer of the same API, so a behaviour change made only in `web/` leaves it stale (`CLAUDE.md`, Conventions). Without a recorded rule, each new feature re-decides where a rule lives, and the same question reaches reviewers repeatedly:

- #3787: should a script wait in the CLI for an issue to reach uzi's cache, or should run creation refresh it on the server?
- #3766: which run states need a person, and which state a waiting client should stop on, is today a list in `uzi run wait`'s help and defaults, and another one in a repo script.

The codebase already follows the rule in several places, without a general record:

- `PauseRequested` (`api/internal/apitypes/run.go`) is a server-decided pause boundary; the worker receives the computed answer instead of re-deriving it.
- `api/internal/workersvc/codex_account_action.go` derives the owner-facing action from the promoter's own classifier, so the surface cannot claim a run will resume when the next promoter tick would not resume it.
- PRD #88 derives clarification questions from the run feed, one source of truth with several presentations.
- Forge sync keeps the forge as the source of truth and `issues` as a cache (`ARCHITECTURE.md`, "Data model: forge as source of truth").

## Decision

### What the server decides

Server-side domain services hold the one authoritative implementation of:

- **Eligibility and policy**: whether an issue may run, branch-protection and privilege guards, quotas and caps, ownership and visibility.
- **State and transitions**: run lifecycle, which state a run is in, and what moves it.
- **Freshness**: when cached forge state is sufficient for an operation and when the operation refreshes it. This is a server rule, not "always fetch live". A change to it preserves the documented sync behaviour or amends it explicitly.
- **Attention**: whether a run needs a person, with the reason, the required actor and action, and the identity of the gate or question involved (so a client can tell an answered question from a new one).
- **Retry classification**: whether a refusal is transient (worth retrying) or final.

The server returns these as decisions and reasons in its responses. Clients do not reconstruct them from raw fields.

### What clients keep

- **Presentation**: layout, wording, rendering, filtering and ordering of what the server decided.
- **User-chosen behaviour**: whether and how long to wait, whether to notify, which already-observed events to suppress. "Wait past the question I answered" uses the server's gate or question identity as a watermark, not a client-side state list.
- **Input validation for usability**, as a fast local error; the server validates again.
- **Local safety enforcement where the protected resource is accessed**: terminal sanitization, credential-file handling, the worker's guardrails and execution checks. This ADR never weakens defense in depth; the four guardrail layers (`ARCHITECTURE.md`, "Guardrail layers (the primary directive)") stay independent.
- **Transport retries that are safe**: reads, and operations that are explicitly idempotent. A transient refusal does not make a mutation safe to repeat; an uncertain create outcome needs reconciliation or an idempotency key.
- **Orchestration in repo scripts and skills**: approvals, deadlines, cross-run coordination and external forge checks remain client concerns, but they consume uzi's decisions instead of re-deriving them.

### Boundaries

- **Mock and demo modes** model the server contract, preferably through shared fixtures and contract tests. They are not a second policy authority.
- **Distinct APIs keep their own semantics.** `/api/v1/jobs` shares the domain rules that apply to it but stays repo-less; it does not acquire issue-run eligibility.
- **Compatibility.** A new decision field states how older clients and servers behave without it. Removing a duplicated client list must not require a simultaneous server and client rollout.

## Consequences

- **#3787, if this ADR is adopted:** prefer a bounded server-side refresh when an authorized run-creation attempt encounters a missing cached issue or a cache-based eligibility refusal. Preserve the existing ownership, visibility and branch-protection preconditions before accessing the forge, then apply eligibility and deduplication checks before creating a run. This does not require refreshing every eligible cached issue or changing unrelated freshness policies. A client wait flag is not required to repair these two cache-freshness cases.
- **#3766, if this ADR is adopted:** expose the server's attention decision and relevant gate or question identity. Default human-attention stopping behavior in `uzi run wait` consumes that decision instead of reconstructing it from statuses and hold fields. Explicit user-selected wait targets, timeouts and already-observed-event watermarks remain client behavior.
- Reviews can cite this ADR instead of re-deciding placement. A change that adds a product rule to a client only, or derives one client-side from raw fields, needs a stated reason (usually defense in depth or a local safety concern).
- Existing client-side duplicates are not rewritten for their own sake; they move to the server when the surrounding code changes.

## Alternatives considered

- **Decide per feature (status quo).** Rejected: the same placement question recurs, and clients drift apart (the run-state lists behind #3766 are an instance).
- **Thin clients with no local checks at all.** Rejected: it would drop terminal sanitization, credential handling and worker guardrails, which must stay where the resource is accessed.
- **Server always fetches live forge state.** Rejected: freshness is a per-operation server rule; always-live would trade rate limits and latency for consistency the cache design already handles.
