# ADR-1106: Codex harness boundary and SDK feasibility evidence

**Status**: Accepted (M0 design and feasibility; production implementation/conformance remain M3/M4)
**Date**: 2026-09-05
**PRD**: [PRD #1106](../prds/1106-codex-harness-phase1.md)

## Context

PRD #1106 adds a whole-run Codex choice without changing the existing Claude
path. Its first milestone is a maintainer-local spike against pinned
`@openai/codex@0.153.2` and `@openai/codex-sdk@0.153.2`. The spike measured
process control, resume behaviour, subagent visibility, hook names, credential
refresh handling, and the initial model vocabulary. A subsequent whole-PRD
review found that the initial measurements did not establish a fail-closed
execution design or a complete provider-neutral boundary and reopened M0. The
continuation below resolved those design gaps; independent technical review and
the user's shared advice-policy decision on 2026-09-05 close M0. Acceptance covers
architecture and feasibility, not shipped adapters or production guardrail parity.

The initial probe harness and packages lived only in private temporary storage.
The credential-free app-server continuation is now committed in
[`e2e/codex-m0/`](../e2e/codex-m0/README.md); its pinned binary remains external.
No pod, worker image, deployment, or uzi worker was used, and no repository package
or lockfile changed. No credential value, hash, account identifier, or server-backed
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
`--dangerously-bypass-hook-trust`. The SDK has no option for that flag; this was
the exec/SDK probe's wiring, not an accepted app-server trust design. Under the
normal/v1 tool path the exact
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

The initial credential-free CLI subagent probe timestamped the local server's
child requests and responses. The parent SDK stream exposed a runtime-only
`collab_tool_call` and the parent's `agent_message`, but no child message or child
tool frame. A server response sent after parent `turn.completed` did not prove
that the child consumed it or acted afterwards. The committed app-server probe
below supplies that missing evidence through an actual post-parent marker and a
subsequent child request carrying the tool result.

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
- the app-server continuation measures interruption and terminal cleanup for
  known fixture children; complete child ownership and disposal remain open; and
- subagent signal tools must be denied before invocation, but the attributed
  happy-path hook observation does not prove that guarantee on hook failure.
  The parent stream observes only root MCP frames, so it cannot serve as a second
  classifier for hidden child frames. Any future frame whose origin cannot be
  proven is `unknown` and must fail closed for signal latching.

## App-server continuation evidence (2026-09-05)

The initial committed [characterization harness](../e2e/codex-m0/README.md)
drives real Codex 0.153.2 with fixed localhost Responses fixtures and the
`gpt-5.5` native tool surface. The subsequent code-mode continuation below
exercises the two intended models separately. No model decides which tools run. Recipes are the opt-in
`task test:codex-m0` and container-only `task test:codex-m0:managed` in
[`Taskfile.yml`](../Taskfile.yml); neither is part of `task gate`.
At executable commit `d4e6301b5ee64ec19607c9d57f8cc76d8a74c6fa`, the lead recorded
23/23 baseline and 6/6 managed cases passing on Linux with Node 24 Alpine; both
runs exited 0 with no failures, skips or cancellations. The final assertions
identify the failed hook events (exit 127, malformed output and one-second
timeout), and the original child terminal surviving interruption as the sole
listed session before termination. On macOS/Node 26, all 23 baseline cases passed
at `20502ed135d3beadd733589a85b9f05014a37d32`; the 12 cases in the changed suites
passed again at `d4e6301b5ee64ec19607c9d57f8cc76d8a74c6fa`. The Linux container used
`--network none`, `--cap-drop ALL`, `no-new-privileges`, uid 10002, a read-only root
filesystem and writable `/tmp` tmpfs, with fixtures and binary mounted read-only.
A fresh subprocess environment does not suppress
system configuration or macOS managed preferences; the isolated Linux container
controls those inputs and external network access.

| Probe | Discriminating observation and limit |
|---|---|
| Shell/patch approvals | `on-request` produced zero approvals for each measured action; `untrusted` produced one each. A shell prompt rule produced one approval: accept wrote its marker, decline and malformed decisions did not. This does not establish universal tool coverage. |
| Pending approval and EOF | The marker stayed absent through observed app-server exit after stdin EOF. This is not immediate denial on every disconnect or transport. |
| Hook trust and failure | These app-server hooks required `thread/start`'s `config: { bypass_hook_trust: true }`; the global CLI flag alone did not arm them. Healthy deny blocked the marker; missing executable, malformed output and timeout permitted it. Serialization failure remains source evidence, not a measured case. |
| Retained terminal input | The identical marker command was denied through `exec_command`, but succeeded through PTY `write_stdin`. Only interpreter startup emitted a hook and approval, with `write_stdin_approval` both false and true. |
| Child action after parent completion | Releasing the held child after parent completion created its marker and a second child request with actual tool output. Interrupting that waiting child produced its own `interrupted` terminal event, closed its HTTP request and prevented the action; the root remained usable. |
| Running child terminal | A ready marker and returned session proved execution started. Turn interruption left one terminal; explicit `thread/backgroundTerminals/terminate`, an empty registry and observation beyond the command's three-second delay established no late marker. The uninterrupted control wrote it. |
| Managed native one-shot | The read-only container [requirements fixture](../e2e/codex-m0/requirements.toml) pins `unified_exec = false` and `shell_tool = true` under `[features]`. It removed `write_stdin` from the schema and dispatcher. Forced `tty: true` still let `/bin/sh` exit 0 without a session; forced input returned exactly `unsupported call: write_stdin`. Accept wrote a marker; decline and malformed approval did not, each with one approval. |
| One-shot timeout | The same ready-marker / sleep-two-seconds / late-marker command completed at `timeout_ms: 4000`; at 150 it returned exit 124 after writing ready, left an empty terminal registry and no late marker after another 2.5 seconds. |
| Dynamic callback scope | `item/tool/call` supplied runtime `threadId`, `turnId` and `callId`. The handler wrote once for its known root and denied a second ordinary worker-created thread whose arguments spoofed that root. Malformed/error replies yielded failed tool results. Native-child inheritance and MCP replacement were not exercised. |

### Intended-model code-mode continuation

At `1750eb582083d77d10ee647b016b27f0bab98cc6`, Linux/Node 24 Alpine
passed 12 managed code-mode cases and six native-isolation cases, with baseline
23 and managed native six cases also passing; the recorded runs exited 0 with no
failures, skips or cancellations. These dummy-key localhost fixtures use the
real code-mode host, the same read-only managed requirements fixture and the
intended model names `gpt-6-astra` and `gpt-5.6-sol`; they establish no live model
access. Reproduction targets are in the [harness README](../e2e/codex-m0/README.md).

| Probe, on each intended model | Observation and limit |
|---|---|
| Managed shell approval through a code cell | Accept writes the marker; decline and malformed decisions prevent it; each produces one app-server approval. This is measured shell coverage, not all tools or per-role policy. |
| Metadata-selected code mode | With `code_mode` and `code_mode_only` false and the code-mode host enabled, the request still advertises custom `exec` and executes the fixed cell. Those two feature flags are not a sufficient isolation mechanism for these model fixtures. |
| Forced terminal/input | Forced `tty` exits without a session; `tools.write_stdin` is unavailable in the cell; the terminal registry is empty and the late-input marker absent. |
| Dynamic callback through a code cell | Runtime `threadId`, `turnId` and `callId` reach `item/tool/call`. The known root writes once; a second ordinary worker-created thread spoofing that root in arguments receives a failed result. Full role/phase admission remains separate. |

The native-isolation cases compare enabled controls against `environments: []`
and `agents.enabled = false`, with `multi_agent_v2 = false` in both. Enabled
controls execute direct and nested shell/patch actions, return real image output,
and start a direct native child that makes a request and completes. Disabled
controls return unsupported dispatch for direct shell, patch, image and native
spawn, expose no nested shell/patch/image functions, and create no marker or
child. Nested collaboration is unavailable in both controls, so that observation
is not disablement evidence. Two additional cells compute 42, while `process`,
`require`, `fetch`, `Deno`, `Bun`, `XMLHttpRequest` and `WebSocket` are undefined;
module import, file read, process-env and fetch attempts reject. These are bounded
runtime-input fixtures, not proof against arbitrary VM/runtime vulnerabilities.

### Worker policy and process ownership continuation

The committed fixture broker at `8aabd299ee98802512711134f24c352af429a697`
passed 14 cases on Linux/Node 24 Alpine, exit 0 with no failures, skips or
cancellations. It drives callbacks through real intended-model code-mode cells:
root-only signals, immutable role/phase write grants, synchronous worker-created
child completion, and denial of unknown/nested delegation. Held callback controls
compare allow, revoke-before-effect, thrown policy and invalid policy results;
disposal waits for admitted handlers. Unknown/stale/replayed identity checks are
**direct broker injections using captured real identities**, not forged upstream
notifications. The broker executes fixed marker effects, not the production
shell/path/forge handlers; these results do not establish all D6 clauses. Nine enforcement mutations
produced named assertion failures; this is not nine demonstrated unauthorized
side effects (the role mutation changes denial text while other guards still
deny). Initial malformed replay syntax and an initial drain-control survivor
were instrument corrections, excluded from that assertion-red evidence.

At `685acea`, the actual-host supervisor suite passed nine cases, and the
separate process-mechanism suite passed four controls, all exit 0 with no
failures, skips or cancellations in the Node test run. These ran in **Node 24
Debian with Python 3.11**, as uid 10002 with zero capabilities,
no-new-privileges, read-only root and mounted fixtures, writable `/tmp`, and
`--network none`; they were not Alpine measurements.

| Ownership probe | Observation and limit |
|---|---|
| Actual active/yielded code-mode host, both intended models | The recorded host has a different process group from its app-server. Uninterrupted controls produce the delayed dynamic marker. Revoke admission, settle the held callback, interrupt and dispose reaps both recorded PIDs with `ECHILD+__WALL`; no late callback or marker appears beyond the control delay. |
| Actual yielded host with signals suppressed | Disposal reaches its deadline with owned children still present and reports unconfirmed, never drained. A subsequent normal disposal recovers and reaps them. |
| Separate setsid/double-fork descendants | A PGID-only control lets the late marker through; subreaper cleanup adopts/reaps the descendants and prevents it. Repeating cleanup is idempotent. |
| Separate non-SIGCHLD clone | Ordinary wait can report `ECHILD` with a child still live; `__WALL` observes and reaps it. An injected no-signal drain has a bounded incomplete result and a successful cleanup. |

A separate reviewer control changed only the supervisor's normal exit after a
real drained report from 0 to 2. The named normal-exit assertion failed; the
unmutated case passed. This establishes refusal of abnormal exit after a drain
report, not cleanup of a crash before any drain report.

The supervisor owns real fixture processes; this does not implement the outer
runner permit across checkpoint publication, recovery and credentialed git.
The broker and supervisor are distinct characterization instruments. Their
passing results must not be read as an end-to-end production safety barrier.

The earlier subscription login/refresh evidence remains separate from these dummy-key
localhost tests. Both subscription and API-key support remain phase-1
requirements; this continuation establishes no live API-key authentication,
model access or entitlement.

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
- the initial child timing evidence did not establish consumption after parent
  completion, and no public child drain had been exercised at review time; and
- the proposed loopback MCP transport had no measured disposal, bearer-token
  or shell/process isolation. The worker-callback alternative below avoids a
  model-side bearer but still needs admission, handler and process disposal.

Therefore hook success on the measured happy path is not a fail-closed guardrail
design. The continuation measures bounded child/terminal cleanup and managed
one-shot enforcement on native and intended-model code-mode fixtures. The
accepted execution and ownership design below incorporates those bounded results;
production enforcement remains M3/M4 work. No alias-only reuse of Claude hooks,
inferred role-file restriction, or process-group kill substitutes for those proofs.

## Execution policy

**Accepted architecture.** Use the stock app-server with `environments: []`,
`agents.enabled = false` and `multi_agent_v2 = false`, with native tools and
extensions disabled. Worker-owned dynamic callbacks provide the permitted run-tool boundary;
worker-created child threads define synchronous delegation in the M3 adapter.
This replaces the earlier loopback-MCP/native-role candidate. Native authority
removal, representative role/phase callbacks and actual-host disposal now have
separate fixture evidence above. M0 is complete at the design/feasibility level;
production adapters and enforcement tests belong to M3/M4.

The worker must bind runtime thread/turn identity to an immutable run, role and
phase registry. Callback arguments never select their own authority. Admission
must check that registry before any action, reject unknown origins and stale
turns, allow lead-only signals only on the root, deny nested or unknown roles,
and apply each role's tool/skill allocation and plan/implement policy. A child
callback must resolve only after its owned child turn finishes; root completion
alone cannot establish that condition. Role TOML and healthy hook behavior are
not substitutes for these checks.

### Capability policy owners

These are design obligations for the worker-owned adapter, not existing production
handlers. M3 implements them; M4 exercises the full adversarial clause map.

| Capability | Owner and required rule |
|---|---|
| Shell | Worker validates its own explicit tool schema and applies `screenBashCommand`; unknown input/tool and screening exceptions deny. Native execution is disabled. |
| Files and patches | Worker parses its own file/patch schema and applies the path jail to canonical paths, including symlink resolution; secret paths, `.git` and outside-worktree paths deny. |
| Role and phase | Worker registry binds immutable runtime thread/turn identity to run, role and plan/implement grants; callback arguments cannot widen them. |
| Workflow signals | Worker admits root-only signals before invocation and retains unknown-origin denial in the reducer. |
| Delegation | Worker creates and awaits child threads synchronously; unknown roles and nested delegation deny. |
| Skills and instructions | Worker delivers only approved, sanitized skill content and instructions with the allocated per-agent grants; cloned-repo discovery supplies no authority. |
| Native extensions | Native web, MCP, plugins, apps and hooks are off; no hook-bypass dependency. Needed run capabilities use worker callbacks. |

Every run/advice thread start or resume builder must explicitly provision the
canonical project as `untrusted`, set `project_doc_max_bytes = 0`, and disable
native extensions and hooks. This cannot rely on an unset trust value: pinned
0.153.2 `app-server/src/request_processors/thread_processor.rs` promotes trust
only when `active_project.trust_level.is_none()` (with a requested cwd and the
applicable permission profile). Explicit untrusted state avoids that branch.
The characterization `Probe` hook bypass is fixture-only. Malicious-repo
instructions/config/role/rules tests remain M4 production-builder conformance.

### Process-root ownership

The worker must keep a **per-run registry of supervisor roots**. Every
provider or command root uses the measured generic-argv Linux subreaper wrapper
and is registered to an immutable run and epoch before launch admission. Pending
launch reservations belong to that epoch. Codex effects may not spawn direct
worker subprocesses outside this registry. An app-server/code-mode host and a
worker-launched command are different roots: each root's supervisor owns its own
descendants, including daemons that change process groups. A command is not an
app-server descendant merely because a callback requested it.

Provider roots receive only their own provider credential; command roots receive
a sparse credential-free environment. At a boundary, close admission, settle
accepted launches and callbacks, then drain and verify **every** root for that
frozen epoch. Lost supervision or an unconfirmed result poisons the run. The
[outer safety gate](../e2e/codex-m0/harness-contract.md#lifecycle-operation-order-and-evidence)
owns the matching-epoch permit and holds it through every runner sink; it also
preserves primary-error independence and the unchanged Claude branch.

The same supervisor mechanism has been measured with actual provider roots and
separate process controls. M0 needs no different process primitive for command
roots; registry coordination, environment enforcement and combined runner/handler
tests are M3/M4 work, not prerequisites requiring their implementation in M0.

### Shared advice capability ceiling

**User-approved 2026-09-05, for both Claude and Codex:** isolated pure in-memory
calculation is permitted. Shell/commands, filesystem access, network tools,
delegation, run/worker callbacks and credential access are denied. Run and advice
construction remain separately gated; advice receives no run workspace or run
handler registry. Provider authentication stays an isolated runtime input, never
a model-accessible calculation capability.

Permission is a ceiling, not a promise that both adapters expose a calculator.
Today's Claude `model-pass.ts` uses `buildDenyAllHook` for every tool and exposes
no calculator. M0/M2 leave that path and hook unchanged under D0. Any future
Claude calculation tool needs its own isolated implementation and conformance;
this decision adds no such feature and permits no shell-based calculator. Codex
pure-cell fixtures supply the bounded calculation evidence above; no Claude
calculator or live API-key calculation path was exercised.

Subscription and API-key credentials remain separate private construction inputs
with no fallback. Credential selection, refresh/CAS, routing and pricing remain
wider PRD blockers, independent of M0 architecture acceptance.

## Neutral harness boundary

The complete accepted interfaces and exact Claude preservation rules live in
[the contract companion](../e2e/codex-m0/harness-contract.md). It defines usage,
context, metrics, rate limits, errors, grouped events, skills, run/advice handles
and distinct bounded child-quiescence, process-reap and tool-disposal operations.
The earlier undefined contract types and lifecycle placeholder now have explicit
shapes accepted for the M2 extraction. Implementation must preserve D0; these
interfaces do not claim production code already exists.

The adapter owns immutable provider/isolation inputs, query construction,
wire decoding, session inspection and process ownership. Uzi retains planning,
approval, signal reduction, checkpoints, git and run-state transitions. A single
neutral reducer groups frames and preserves both the emitted uzi payload and
workflow outcome; raw provider events stay inside the adapter.

The source inventory is pinned to `bc5a0a8b11f5c98a7067c1fc4202d37a0f27f92e`:

- Keep display attribution distinct from signal authorization. Unknown Codex
  origins cannot latch signals; Claude's existing replay projections remain.
- Preserve Claude's init/error/result payloads, including unknown accounting
  members and explicit undefined keys before JSON serialization, through an
  opaque uzi wire projection alongside normalized usage. Subscription cost is
  not metered zero; API-key cost remains unreported until its source is defined.
- For run turns, preserve first-wins local timeout/cancel, then distinct iterator throws, then
  terminal failure authority; terminal accounting is emitted before failure.
  Deferred failure materialization preserves runtime subtype truthiness and
  conversion timing for the exact Claude typed-limit suffix.
  Claude clean EOF still returns accumulated output without a fabricated result.
- Preserve grouped signal filtering, first-surviving-item usage/model attachment,
  context-read timing, session-ID rules, skill allocations and tool inheritance.
- Preserve advice callback authority and existing outer fallbacks: judge catches
  even its inner `LimitReachedError` and posts deterministic completed advice;
  review returns a failed-review payload and summary returns null on failure.
  The synchronous advice policy runs inside terminal consumption before iterator
  closure, preserving its own callback-error precedence and classification clock.
- Preserve current Claude cleanup timing. Its legacy kill-dispatch and in-process
  tool states make no observed-empty claim and cannot satisfy a Codex barrier.
  Ordinary iteration fetch-back continues to use `reap: false`.

## Consequences

- M0 is complete and this ADR accepted for architecture and feasibility. The
  corrected neutral contract is accepted for M2; production handlers and the
  outer-gate implementation/conformance remain M3/M4 work.
- Intended-model native-authority controls, representative role/phase callbacks
  and actual-host disposal have bounded evidence. The complete production
  guardrail suite and outer runner permit are not implemented by those fixtures;
  their absence is a downstream implementation boundary, not an M0 dependency.
- Exec/SDK root-only visibility remains historical protocol evidence. An
  app-server adapter must use actual runtime identity and observed content,
  without fabricating child messages or treating hook identity as attribution.
- Both credential modes remain required. The seat lock and latest-state
  write-back remain conservative because prior-token replay was not tested.
