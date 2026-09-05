# ADR-1106: Codex harness boundary and SDK feasibility evidence

**Status**: Proposed (M0 partially measured; live credential-backed probes remain)
**Date**: 2026-09-05
**PRD**: [PRD #1106](../prds/1106-codex-harness-phase1.md)

## Context

PRD #1106 adds a whole-run Codex choice without changing the existing Claude
path. Its first milestone is a maintainer-local spike against the pinned Codex
packages. The spike must settle process control, resume behaviour, subagent
visibility, hook names and enforcement, credential refresh semantics, the
initial model vocabulary, and the provider-neutral executor boundary before
the production adapter is written.

This record is deliberately partial. No pod, worker image, deployment, uzi
worker, or live Codex request was used for the evidence below. The sanitized
live harness exists only under a private temporary directory and is not a
repository artifact.

## Environment and package surface

The spike installed `@openai/codex@0.153.2` and
`@openai/codex-sdk@0.153.2` only under a private temporary directory. npm
reported integrity metadata for both pinned packages during installation. No
package or lockfile in this repository changed.

The pinned SDK declarations expose:

- `TurnOptions.signal` and `resumeThread`;
- `CodexOptions.env`, documented as a full replacement for the inherited
  process environment;
- `modelReasoningEffort` values `minimal`, `low`, `medium`, `high`, `xhigh`,
  `max`, `ultra`, and `persistent`;
- `model` as an arbitrary string rather than a closed model union; and
- public `ThreadEvent` values with no parent/child attribution field.

These declarations describe the public type surface. They do not prove that a
server accepts a model or effort, that a server-backed thread resumes after an
interrupted turn, or that child events are absent from some other observable
channel.

## Evidence recorded so far

A credential-free fake-executable harness has three passing tests:

| Probe | Positive control | Observation | Bound on the conclusion |
|---|---|---|---|
| Environment | the fake executable observed the explicitly supplied canary | `CodexOptions.env` replaced, rather than merged with, the parent environment | proves SDK subprocess construction only |
| Resume | the fake executable observed the thread identifier returned by the first invocation | `resumeThread` supplied the same identifier on the resume invocation | proves argv/protocol plumbing only, not server-backed session recovery |
| Abort | the fake executable was running before the signal fired | aborting `TurnOptions.signal` interrupted the CLI child promptly | proves local child-process interruption only, not the state a remote turn leaves resumable |

The tests were designed so a fake executable that never started, or a resume
that invented a new identifier, could not satisfy them. Even so, a fake CLI is
not an oracle for server state. The server half of M0(a) therefore remains
open.

## Credential-backed safety gate

The live tool probe has not run. A copied `CODEX_HOME` isolates files but does
not isolate the account or make its refresh token harmless. More importantly,
the probe is intended to establish that `PreToolUse` hooks enforce the deny;
using that same unproven hook as the protection for a model that can read the
copied `auth.json` would be circular. If the hook did not fire, the model could
exfiltrate a real credential.

Live probing is therefore paused for explicit informed approval and a design
that does not expose a readable credential to the tool process before hook
efficacy is independently established. No credential value, hash, account or
thread identifier is recorded here. M0(e) must additionally treat refresh as
server-side shared state: a temporary home cannot prevent a refresh from
invalidating another login, and the newest successfully issued state must not
be lost.

## Provisional harness boundary

The production boundary should keep uzi lifecycle policy outside provider
adapters. The following shape is provisional until the live abort, resume and
event probes finish:

```ts
type HarnessOrigin =
  | { attribution: "main" }
  | { attribution: "agent"; agentId: string; agentType?: string }
  | { attribution: "ambiguous" };

type HarnessEvent =
  | { kind: "session_started"; sessionId: string }
  | { kind: "message"; origin: HarnessOrigin; text: string }
  | { kind: "tool"; origin: HarnessOrigin; phase: "started" | "updated" | "completed"; tool: string; payload: unknown }
  | { kind: "usage"; usage: HarnessUsage }
  | { kind: "completed" }
  | { kind: "error"; message: string };

interface RunHarness {
  start(options: RunHarnessOptions): Promise<HarnessSession>;
  resume(sessionId: string, options: RunHarnessOptions): Promise<HarnessSession>;
}

interface HarnessSession {
  runTurn(prompt: string, options: { signal: AbortSignal }): AsyncIterable<HarnessEvent>;
}

interface AdviceHarness {
  run(request: AdviceRequest, options: { signal: AbortSignal }): Promise<{
    output: string;
    structuredOutput?: unknown;
    usage: HarnessUsage;
  }>;
}
```

`HarnessUsage`, `RunHarnessOptions`, and `AdviceRequest` are provider-neutral
uzi contracts; raw Claude and Codex event types stay inside their adapters.
Credential persistence, checkpoints, git operations, run-state transitions,
and signal latching do not belong on either interface. In particular,
ambiguous Codex event origin must remain explicit and fail closed; it must not
be silently promoted to the main thread. One shared event-mapping contract is
the input to persistence and live-view rendering for both harnesses.

The final M0(g) decision must re-read the live run, advice, and message-mapping
call sites and adjust these signatures to their actual needs after M0(a) and
M0(b) establish what Codex can report.

## Remaining evidence before acceptance

- **M0(a), server half**: abort a real streamed turn and resume that exact
  server-backed thread, recording what context and terminal events survive.
- **M0(b)**: prove independently that a child ran, then determine which child
  items reach the parent stream, how origin is attributed, and whether a child
  can outlive the parent turn. Absence in the parent stream alone is not
  evidence.
- **M0(c), live half**: record exact hook `tool_name` values for shell,
  `apply_patch`, file reads, `spawn_agent`, and MCP, then deny one harmless
  action and prove its side effect did not occur. Seeing hook input without a
  blocked side effect proves observation, not enforcement.
- **M0(e)**: establish a reproducible forced-refresh lever and whether the old
  refresh token remains usable after rotation, without losing the newest valid
  auth state.
- **M0(f), live half**: distinguish accepted model/effort combinations from
  entitlement, authentication, rate-limit, and transient failures. The open
  `model: string` type cannot answer this.
- **M0(g), final**: reconcile the provisional interfaces above with the live
  event and resume results and the current uzi call sites.

Until these are complete, M0 remains open and the provisional boundary is not
an implementation decision.
