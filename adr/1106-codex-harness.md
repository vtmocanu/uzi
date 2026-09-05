# ADR-1106: Codex harness boundary and SDK feasibility evidence

**Status**: Proposed (M0 reopened by whole-PRD review)
**Date**: 2026-09-05
**PRD**: [PRD #1106](../prds/1106-codex-harness-phase1.md)

## Context

PRD #1106 adds a whole-run Codex choice without changing the existing Claude
path. Its first milestone is a maintainer-local spike against pinned
`@openai/codex@0.153.2` and `@openai/codex-sdk@0.153.2`. The spike measured
process control, resume behaviour, subagent visibility, hook names, credential
refresh handling, and the initial model vocabulary. A subsequent whole-PRD
review found that those measurements do not yet establish a fail-closed
execution design or a complete provider-neutral boundary. The evidence below
stands; the boundary is not accepted and M0 is reopened.

The probe harness and packages lived only in private temporary storage. No pod,
worker image, deployment, or uzi worker was used, and no repository package or
lockfile changed. No credential value, hash, account identifier, or server-backed
thread identifier is recorded here.

## Process and session evidence

The credential-free fake-executable harness passed three discriminating tests:

| Probe | Positive control | Observation | Bound on the conclusion |
|---|---|---|---|
| Environment | the fake executable observed the explicitly supplied canary | `CodexOptions.env` replaced, rather than merged with, the parent environment | SDK subprocess construction only |
| Resume | the fake executable observed the thread identifier returned by the first invocation | `resumeThread` supplied the same identifier on the resume invocation | argv/protocol plumbing only |
| Abort | the fake executable was running before the signal fired | aborting `TurnOptions.signal` interrupted the local CLI child promptly | local child-process interruption only |

The live server-backed abort/resume probe used `gpt-5.6-sol` at `low` effort.
The stream reported a `command_execution` starting exactly `sleep 30`; aborting
the turn's signal produced `AbortError`. `resumeThread` on that same thread then
succeeded and returned terminal usage. This establishes that uzi can cancel at
its existing turn boundary and resume the server-backed thread afterwards. It
does not establish that arbitrary external side effects have been rolled back,
so the existing checkpoint and process-reap boundaries remain necessary.

## Hooks and enforcement

A credential-free real CLI+SDK probe used a dummy API key, a local fake Responses
server, and a `codexPathOverride` wrapper that injected
`--dangerously-bypass-hook-trust`. The SDK has no option for that flag, so the
production wrapper must inject it. Under the normal/v1 tool path the exact
`PreToolUse` stdin names were:

| Operation | Exact `tool_name` | Matcher aliases in Codex source |
|---|---|---|
| shell command | `Bash` | none |
| patch | `apply_patch` | `Write`, `Edit` |
| ordinary file read through `exec_command` | `Bash` | none |
| delegation | `spawn_agent` | `Agent` |
| MCP server `m0`, tool `mark` | `mcp__m0__mark` | none |

`Write`/`Edit` and `Agent` are matcher aliases, not the exact stdin names observed
for `apply_patch` and `spawn_agent`. The observed sequence was `Bash`,
`apply_patch`, `Bash`, `spawn_agent`, `mcp__m0__mark`; the measured Codex path
had no separate `Read` hook. The deny decision reached the model's follow-up
request, while the harmless shell and patch canaries remained absent; the probe
therefore established enforcement rather than hook observation alone.

The approved credential-backed probe exercised the actual code-mode path. Its
exact delegation hook names were `collaborationspawn_agent` and
`collaborationwait_agent`, even with `multi_agent_v2 = false`. `SubagentStart`
and `SubagentStop` carried `agent_id` and `agent_type = "m0_child"`. Production
matchers must cover the normal/v1 names, their source aliases, and these code-mode
names; none is safely inferred from another. Earlier denied attempts started no
child, and neither those attempts nor the successful probe exposed a secret.

Before the credential-backed probe, an independent macOS permissions profile
served as a positive control: a workspace read succeeded, while a canary in the
future credential-path class was denied and could not be copied into the
workspace. Hook efficacy was therefore not used as the protection for the
credential whose hook behaviour was being tested.

The live root MCP probe produced `mcp_tool_call` started/completed frames for
server `m0`, tool `mark`; its hook name was `mcp__m0__mark`, and the server's
independent invocation counter was exactly one. With `approvalPolicy: "never"`,
the MCP configuration also required `default_tools_approval_mode = "approve"`;
otherwise the tool is unavailable before the hook question is exercised.

## Delegation visibility

The credential-free real CLI subagent probe gave the local server an independent
view of two child requests and completions. The parent SDK stream exposed a
runtime-only `collab_tool_call` and the parent's `agent_message`, but no child
message or child tool frame. A delayed child response arrived after the parent's
`turn.completed`, proving that a child can outlive its parent turn.

The live code-mode probe agreed on the visibility boundary. The parent stream
reported `collab_tool_call` started/completed with a sender id and an empty
`receiver_thread_ids` list, but no child content. The hook lifecycle, not the
parent stream, carried the child's `agent_id` and `agent_type`.
`collab_tool_call` is absent from the 0.153.2 SDK declarations; the exact 0.153.2
exec source filters notifications to the primary thread and turn. The adapter
therefore needs a defensive runtime decoder for `collab_tool_call`, and it must
not manufacture child messages, tools, or lead attribution from a root-only
stream.

Consequences for the lifecycle are explicit:

- delegation lifecycle status may use only identifiers actually present in the
  runtime frame or hook event;
- a child may still be running when the parent completes, but no public child
  drain or close operation has been measured; and
- subagent signal tools must be denied before invocation, but the attributed
  happy-path hook observation does not prove that guarantee on hook failure.
  The parent stream observes only root MCP frames, so it cannot serve as a second
  classifier for hidden child frames. Any future frame whose origin cannot be
  proven is `unknown` and must fail closed for signal latching.

## Credential refresh evidence and limit

A synthetic expired access-token JWT in one isolated seat copy forced
`codex debug models` to refresh without invoking a model or tool. The persisted
auth state changed, including rotation of the refresh token. A fresh
`models_cache.json` recorded client version `0.153.2`, a fetch time after probe
start, and a non-empty server catalog. The normal Codex home was then restored
from the newer valid copy, and `codex login status` reported a ChatGPT login.

The prior refresh token was deliberately not replayed. Exact source classifies
`refresh_token_reused`, expired, and revoked refresh tokens as permanent
re-login failures, so a replay experiment could revoke the token family and was
not safe under the user's conditional approval. M0 therefore proves refresh and
rotation, but does not claim the previous refresh token remains usable. Phase 1
does not rely on prior-token usability: D5's one-in-flight-run-per-seat database
invariant remains required, and the latest successfully issued auth state must be
written back before another pod can use the seat.

## Model and effort decision

The refreshed server catalog visible to 0.153.2 contained:

| Model | Default effort | Listed efforts |
|---|---|---|
| `gpt-6-astra` | `medium` | `low`, `medium`, `high`, `xhigh`, `max`, `ultra` |
| `gpt-5.6-sol` | `low` | `low`, `medium`, `high`, `xhigh`, `max`, `ultra` |
| `gpt-5.6-terra` | `medium` | `low`, `medium`, `high`, `xhigh`, `max`, `ultra` |
| `gpt-5.6-luna` | `medium` | `low`, `medium`, `high`, `xhigh`, `max` |
| `gpt-5.5` | `medium` | `low`, `medium`, `high`, `xhigh` |
| `gpt-5.4-mini` | `medium` | `low`, `medium`, `high`, `xhigh` |
| `gpt-5.3-codex-spark` | `high` | `low`, `medium`, `high`, `xhigh` |

The initial product picker is deliberately narrower: `gpt-6-astra` and
`gpt-5.6-sol`. Uzi's existing `low | medium | high | xhigh | max` contract maps
one-to-one to Codex `modelReasoningEffort`; provider-only values are not added to
the uzi contract in phase 1. Real no-tool turns at `max` succeeded on both initial
models and returned the exact requested response. `persistent` failed, but the
sanitized error could not distinguish unsupported effort from a narrower server
or entitlement failure. The SDK's open model string and wider effort union are
therefore transport types, not picker authority.

## Review disposition / blockers

The whole-PRD review found six blockers that the successful probes did not
exercise:

- upstream 0.153.2 `PreToolUse` handling fails open when input serialization,
  hook spawn, timeout, or output parsing fails;
- `write_stdin` emits no hook, so an initially allowed reusable interpreter can
  receive a command later that the initial spawn hook never screened;
- role-file loading admits only bounded overrides and filters sandbox, web and
  MCP changes, preserving the parent sandbox, approval and MCP configuration;
- the current Claude `buildPathGuardHook` and `buildAgentGuardHook` neither
  accept Codex's canonical names nor parse its payloads, including
  `apply_patch` paths;
- a child was measured outliving `turn.completed`, but no public child
  close/drain control was found or exercised; and
- the per-run MCP transport still needs a defined disposal lifecycle and proven
  bearer-token and shell/process isolation.

Therefore hook success on the measured happy path is not a fail-closed guardrail
design. M0 cannot close until the failure modes above have a fail-closed execution
design and Codex child draining is measured. No alias-only reuse of the Claude
hooks, inferred role-file restriction, or process-group kill is accepted as a
substitute for those proofs.

## Provisional harness boundary

The useful shape remains one per-run object. Construction owns immutable provider and
isolation inputs: the credential, home, workspace and secret paths, available
skills, MCP handlers, guardrails, and process launcher. Mutable uzi policy stays
outside it: planning and approval, checkpoints and git operations, run-state
transitions, and signal latches. This shape is incomplete and not an
implementation decision.

```ts
type SessionPresence = "present" | "absent" | "unknown";

type HarnessOrigin =
  | { kind: "main" }
  | {
      kind: "subagent";
      role?: string;
      name?: string;
      instanceId?: string;
      label?: string;
    }
  | { kind: "unknown" };

interface HarnessAgent {
  description: string;
  prompt: string;
  tools?: readonly string[];
  model?: string;
}

interface RunTurnRequest {
  prompt: string;
  resumeSessionId?: string;
  signal: AbortSignal;
  systemPrompt: string;
  model?: string;
  effort?: "low" | "medium" | "high" | "xhigh" | "max";
  agents: Readonly<Record<string, HarnessAgent>>;
}

type HarnessItem =
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | {
      kind: "tool";
      phase: "started";
      id?: string;
      name?: string;
      input?: unknown;
    }
  | {
      kind: "tool";
      phase: "finished";
      id?: string;
      name?: string;
      output?: unknown;
      isError?: boolean;
    };

type HarnessErrorCategory =
  | "aborted"
  | "authentication"
  | "authorization"
  | "rate_limit"
  | "model"
  | "effort"
  | "transport"
  | "tool"
  | "protocol"
  | "session_missing"
  | "unknown";

type HarnessEvent =
  | { kind: "turn_started" }
  | { kind: "session_id"; sessionId: string }
  | {
      kind: "frame";
      origin: HarnessOrigin;
      items: readonly HarnessItem[];
      usage?: HarnessUsage;
      model?: string;
    }
  | {
      kind: "delegation";
      phase: "started" | "completed" | "failed";
      identifiers: {
        agentId?: string;
        agentType?: string;
        senderId?: string;
        receiverThreadIds?: readonly string[];
      };
      error?: HarnessError;
    }
  | {
      kind: "turn_finished";
      outcome: "success" | "failed";
      usage?: { basis: "turn" | "session"; value: HarnessUsage };
      context?: HarnessContext;
      metrics?: HarnessMetrics;
      error?: HarnessError;
      rateLimit?: HarnessRateLimit;
    };

interface HarnessError {
  category: HarnessErrorCategory;
  message: string;
}

interface RunHarness {
  inspectSession(id: string): Promise<SessionPresence>;
  runTurn(request: RunTurnRequest): AsyncIterable<HarnessEvent>;
  // OPEN PLACEHOLDER: an async provider-child drain/close surface belongs here.
}

interface AdviceRequest {
  systemPrompt: string;
  prompt: string;
  model?: string;
  output: { kind: "text" } | { kind: "json"; schema: unknown };
  signal: AbortSignal;
}

interface AdviceHarness {
  run(request: AdviceRequest): Promise<{ text: string; usage?: HarnessUsage }>;
}
```

The interface sketch intentionally leaves contract work visible rather than
guessing at it:

- `HarnessUsage`, `HarnessContext`, `HarnessMetrics` and `HarnessRateLimit` are
  not yet defined;
- D0 requires exact preservation of Claude init/result/error mapping, and the
  authority between a thrown adapter error and a terminal error event is open;
- per-agent skill allocation is absent from `HarnessAgent`;
- `AdviceHarness` does not yet preserve the advice lane's typed rate-limit
  behaviour;
- per-run MCP disposal is not represented; and
- no signature is accepted yet for the distinct operations of reaping the CLI
  process tree and draining/closing Codex child threads.

The current runner deliberately uses `reap: false` at ordinary iteration
boundaries. Full process-tree reap occurs only at safe checkpoint, terminal, or
pre-credentialed-git boundaries; that process operation has not been shown to
drain a Codex child thread. The placeholder above must become an asynchronous,
measured lifecycle contract before this boundary can be accepted.

The eventual usage, context, metrics and rate-limit shapes must be
provider-neutral uzi contracts.
`HarnessOrigin` carries the current message-mapping concepts: optional role/name,
invocation instance and label. Hook `agent_id`/`agent_type` remain delegation or
hook-lifecycle identifiers; they are not silently translated into message
attribution. `inspectSession` is tri-state so a dropped resume id is cleared only
on proven absence; an I/O or protocol failure returns `unknown`. `session_id` is
lazy because Codex does not make it available at construction.

Provider `item.updated` events reset liveness only; they do not become persisted
frames. A frame keeps its mapped items grouped because the current Claude mapper
can expand one provider assistant frame into several items, after which
`driveTurn` filters signal tools and attaches that frame's usage/model to the
first surviving item. One reducer consumes the normalized stream and is the sole
producer of `EmittedMessage[]`, signal latches, the current session id, and the
terminal turn result. Raw Claude or Codex events never reach lifecycle code.
Origin `unknown` is preserved and fails closed for signals.

The advice sketch follows the current judge/review/summary call shape: one
isolated tool-less pass returning accumulated text plus optional usage.
Structured-output validation and tolerant parsing remain with the caller, while
typed rate-limit and terminal-error behaviour still require a contract.

## Consequences

- M2 cannot treat this sketch as accepted until M0 closes the lifecycle, error,
  usage and advice gaps without changing Claude behaviour.
- Covering every observed normal/v1 and code-mode hook name is necessary but not
  sufficient; the failure and `write_stdin` paths need a fail-closed design.
- Codex feeds show root frames plus honest delegation lifecycle status. They do
  not fabricate child content that 0.153.2 does not expose.
- The seat lock and checkpoint/write-back design remain conservative despite
  proven rotation, because previous-token usability was intentionally not tested.
