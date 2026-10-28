# ADR-1106: Codex harness boundary and SDK feasibility evidence

**Status**: Accepted (M0 design and feasibility; production implementation/conformance remain M3/M4)
**Date**: 2026-09-05
**PRD**: [PRD #1106](../prds/done/1106-codex-harness-phase1.md)

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
rotation, but does not claim the previous refresh token remains usable.

**Policy superseded by the user, 2026-09-06:** [PRD #1106 D5](../prds/done/1106-codex-harness-phase1.md#decision-log)
now permits multiple concurrent runs on one Codex subscription. The earlier
one-run-per-seat prescription is retired. Enforceable coordination of credential
refresh operations and durable latest-state reconciliation remain production
requirements; no prior-token reuse or concurrent-refresh guarantee follows from
these unchanged M0 measurements.

[ADR-3523](3523-codex-refresh-provider-error-reuse.md) adds one survivor retry
for a durable receipt of an original non-rejection refresh error, outside the
callback. It supersedes the no-reuse policy only for that case; these historical
measurements still provide no live proof of prior-token reuse.

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

**Amendment (2026-09-23, Codex 0.156.1 pin).** The picker adds `gpt-6-sol`. Its bundled
0.156.1 catalog entry requires client `>= 0.155.0`, lists `low` through `ultra` (so uzi's
`low | medium | high | xhigh | max` contract still maps one-to-one), and differs from the two
initial models in three catalog fields, each checked against the 0.156.1 source:
`shell_type: "shell_command"` is a serde alias of `unified_exec`, still gated off by
`shell_tool = false` (the native-bypass P suite now runs on both `gpt-6-astra` and `gpt-6-sol`);
`default_service_tier: "priority"` is applied only by the interactive TUI, so uzi requests carry no
tier, as for the other models. OpenAI then uses the API project's configured tier, which uzi cannot
observe, so Standard pricing remains the D5 estimation policy, not a measurement; its experimental
`send_user_message_async` / `clock` tools match what `gpt-6-astra` already had. The vocabulary
stays closed and product-owned.

**Superseded 2026-09-23** by [PRD #1551](../prds/done/1551-per-harness-worker-model-defaults.md) for
per-user defaults only: Claude and Codex now keep separate, retained per-user model defaults
instead of one shared slot, and Codex's per-user default gained a narrow, capability-gated custom
model ID. The closed curated vocabulary and closed effort contract recorded above still govern
schedule pins, per-role agent-template pins, and the judge/summary models.

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
`agents.enabled = false` and `multi_agent_v2 = false`, with effectful native
execution and native extensions disabled. Authority-free code-host run plumbing
and the accepted native exceptions `request_user_input_async` (catalog
`send_user_message_async`) and `clock.curr_time` (catalog `clock`) remain available,
with the measured boundaries below. Worker-owned dynamic callbacks provide the
permitted run-tool boundary; worker-created child threads define synchronous
delegation in the M3 adapter.
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

### Accepted native exceptions (#1566)

**Current characterization (0.159.3).** Option 1 explicitly accepts the two
native exceptions above. This documents the observed boundary; it does not add
runtime suppression or a security enforcement fix. Effectful run operations
remain behind worker-owned callbacks.

- `request_user_input_async` emits an AgentMessage with delivery `"async"`,
  questions and formatted text. Its direct tool output is JSON text
  `{"accepted":true}`: acceptance is immediate, with no pause for a user response
  and no worker effect. Uzi's root projects the message as ordinary text, without
  a questions workflow. Advice ignores text from items whose delivery is exactly
  `"async"` before extraction into its result; no questions are surfaced or
  answered. The native capability stays available. This resolves the earlier
  advice-filtering deferral under #2239. (AI-synced 2026-10-05)
- `clock.curr_time` is a human-authorized, read-only exception with no filesystem,
  network or process effects. The verified installed runtime returns successful
  UTC time without an observed `currentTime/read` request reaching uzi, then
  continues without a worker effect. A direct namespace function call returns
  `It is YYYY-MM-DD HH:MM:SS UTC.`; a code-mode nested
  `tools.clock__curr_time({})` call returns
  `{"current_time":"YYYY-MM-DD HH:MM:SS UTC"}`. Unknown server-request refusal
  is unchanged: root returns `-32601` / `unsupported request`, and advice returns
  `-32601` / `codex advice is tool-less`. The regression fails if a future
  `currentTime/read` request reaches uzi.

**Source/runtime discrepancy.** At pinned source commit
`01fc69f4026735edfdf6789820549727a4867b11`,
[`message_processor.rs:367`](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/app-server/src/message_processor.rs#L367)
installs `app_server_time_provider`, and
[`current_time.rs:110`](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/app-server/src/current_time.rs#L110)
sends `CurrentTimeRead`, suggesting a client time read. The verified runtime
instead returns time without an observed request reaching uzi. The underlying
time-provider path and cause remain **UNVERIFIED**; the source reading does not
establish that the current runtime refuses clock calls.

The existing [native-exceptions regression](../e2e/codex-m4/native-exceptions.test.ts)
passed 12 real-binary lifecycle/model runs: `gpt-6-astra`, `gpt-6-sol` and
`gpt-6.1-sol`, each on root start, saved resume, worker-created child and advice.
The binary was `/opt/uzi-codex/0.159.3/bin/codex`, reporting `codex-cli 0.159.3`.
`probeCodexRuntime` checked the installed receipt against the lock-pinned amd64
archive digest `3930f31ac5fca861ea3e444e2683f261190d96b63fba58e0a40a879174369cdf`
and checked each member on disk; provisioning retains its version assertion.
The fixture records namespace `tools` / `additional_tools` and nested inventories,
checks both UTC output forms on run paths, and supplies no answer. A subsequent
nested `uzi_bash` callback proves continuation and supplies the single worker
effect on run paths; advice has zero worker effects. These are bounded
characterization results, not proof of the unverified time-provider mechanism.

**Preferred future policy (Option 2).** A positive allowlist is unavailable via
app-server/TOML in 0.159.3. The internal
[`ToolPolicy` API](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/ext/extension-api/src/tool_policy.rs)
says “Supply through ExtensionDataInit before starting a thread.” and “Callers
must supply it again when resuming a thread.” However,
[`thread_processor.rs`](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/app-server/src/request_processors/thread_processor.rs)
creates `let mut thread_extension_init = ExtensionDataInit::new()` without
inserting `ToolPolicy`, and
[`ThreadStartParams`](https://github.com/openai/codex/blob/01fc69f4026735edfdf6789820549727a4867b11/codex-rs/app-server-protocol/src/protocol/v2/thread.rs)
exposes `config` and `dynamic_tools`, but no allowlist; the experimental binary
schema was also examined during planning. The internal registry filters dynamic
and core tools, so a future allowlist must include worker dynamic names plus the code-mode wrappers
`exec` / `wait` and be reapplied on resume, rather than being a raw native-only list. This policy
remains future work.

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

### Docker-wired command confinement limit

Docker-wired Codex commands can access DinD. On hosted Kubernetes workers, DinD
shares all of `/data/runner` (a `runner-shared` subPath of the data PVC since ADR-3153; `controller/internal/kube/render_dind.go` `dindWorkdirDir`), so a Docker bind mount can read or write a sibling run's clone
even when Landlock confines the command to its own run. The shared workdir may
contain sensitive repository files. This shared-clone, same-owner cross-run
disclosure and integrity residual is accepted for the opt-in Docker tier: hosted
workers claim only their owner's runs (`api/internal/workersvc/service.go:2362`,
`UserID: wkr.UserID`; `api/internal/store/queries/runtime.sql:768`,
`WHERE r.user_id = @user_id`). Compose DinD does not share the run workdir.

The DinD mount boundary prevents a direct Docker bind mount of the worker's
token, data, nix or Codex cache volumes (`controller/internal/kube/render_test.go:1120`,
`TestDindContainersMountNoneOfTheWorkersVolumes`). It does not guarantee
credential isolation after Docker API access: in the supported non-rootless
posture DinD runs privileged as real root, and a breakout can read node and
co-scheduled pod secrets, including other users' credentials. That separate,
owner-accepted node-root residual is documented in
[PRD #89](../prds/done/89-optional-nonrootless-dind.md#security-framing); the
Docker tier's broad-egress residual is recorded in
[ADR-0285](0285-worker-egress-tier-trust-model.md). Host and context screener
denials are regression checks, not containment after Docker API access.

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
a sparse credential-free environment. At a credentialed boundary, close admission,
settle accepted launches and callbacks, then drain and verify the registered roots
for that frozen epoch. The
[outer safety gate](../e2e/codex-m0/harness-contract.md#lifecycle-operation-order-and-evidence)
holds the matching-epoch permit through the runner action and its owned work,
preserving primary-error independence and the unchanged Claude branch.

**Slice A contract, Refs #2852 (AI-synced 2026-10-18):**
A separate credential-free GitCache capture callback holds safety serialization,
the epoch fence and closed admission through source reads, WIP commit, fetch-back,
verification, authorized publication and full supervised-child settlement. An
intentionally poisoned registry admits this narrow lane after confirmed settlement;
it admits no provider turn or normal callback, admission reopen, credential
reconciliation, refresh or release, PAT, credential-bearing environment or overlay.
Lost supervision or incomplete settlement grants no capture authority. This context
mints neither a credentialed nor a completion permit. See the
[sink contracts and remaining terminal gap](1783-run-quiescence-and-attempt-clone-paths.md#invariants-future-code-must-respect).

**Slice B contract, Refs #2852 (AI-synced 2026-10-23):** healthy Codex wall
park, completion hold and ordinary recovery hold the existing process permit from
before source inspection through capture and source-dependent settlement. The
private authenticated capability is bound to its exact flight/executor and callback
lifetime; Git children use the existing supervisor and await full cleanup. A's
credential-free fallback starts only after the primary scope joins, within the
same deadline. Unexpected Codex credential-switch entries retain without capture.
B left Claude behavior and terminal disposal unchanged; subsequent C strengthens
terminal disposal as described below. The linked
[sink contract](1783-run-quiescence-and-attempt-clone-paths.md#invariants-future-code-must-respect)
records the budget, held-finalize continuation, publication and remaining writer limits.

**Subsequent slice C contract, Refs #2852 (AI-synced 2026-10-24):**
`tablesCleared` synchronously and irrevocably closes admission at disposal entry.
`disposeTools` is single-flight; open/closing entry, pending launches/callbacks,
unmatched poison, late roots/admission faults or unclean cleanup stays incomplete.
The exact disposal result is revocable, bound to registry facts and successful
facade cleanup; cleared tables or an optional killAgentTree call are not proof.
Release-dependent coverage waits for disposal of the exact executor/facade.
Skipped/incomplete/swapped proof retains clone, journal, inventory and paired skills
with a distinct terminal-disposal diagnostic. A/B handler settlement defers this
request without reacquiring either safety queue. Post-disposal source reads are
inert and hardened; witness checks continue through destructive dispatches.
Current-process completion bindings record exact executor/facade in memory before
durable bind; knownCodex missing/non-disposed/swapped proof retains before inventory,
with no legacy fallback. KnownClaude is unchanged. Owner decision 2026-10-24
narrows R1857-02(e): persisted/unknown-provenance sources keep today's behavior for
both harnesses, without prior-process topology proof or durable format changes.
This is registered-process strengthening; Docker accepted invisible creates (#1860),
unregistered/sibling writers and Claude terminal residuals remain. The canonical
sink contract records the unchanged retention and ownership boundaries.

Pre-start capture uses the existing capture-only facade. A later owned capture can
also select a fresh production capture-only facade for the exact retired public
facade after verified full teardown. Ordinary evidence is created only by
`ProviderEpoch.dispose` replacing an abandoned epoch, with observed quiescence
and matching-epoch reaping, successful disposal, harness close and fileop cleanup
for that exact public facade; it remains valid only while that registry is disposed
and unpoisoned.
The exact registry's pre-reap epoch, root count and existing launch/callback
cursors, with initially zero pending launches and callbacks, must remain unchanged
through aggregate reaping, registry/harness/fileop cleanup and later eligibility
checks; a newly admitted root or pending reservation invalidates the clean result
and retains the old registry as unverified, even if disposal clears its tables.
Terminal run cleanup and partial-build cleanup do not create this
ordinary fact from cleared tracking tables. Intentional capture instead retains
the exact completed scope/epoch/registry proof and unchanged poison revision. The public safety marker
stays present, so normal boundaries still refuse rather than taking the legacy
branch. After setup, summary and installer joins, session save, attempt binding
and outstanding-registry drain checks, this evidence permits facade selection,
not capture authority. The fresh acquisition still calls `settleForCapture`,
intentionally poisons its new registry and uses its own settlement within the
original deadline. Arbitrary disposal, cleared tracking tables, failed cleanup,
new poison or roots, and foreign facades cannot substitute for either evidence
origin. This adds no model/provider, reconciliation, credential-release or PAT
authority and changes no command-cache eligibility or retention rule.
Refs #2852 (AI-synced 2026-10-18).

The optional fifth
`Executor.withCredentialFreeCapture` argument is worker-internal immutable
`marker`/`cloneKey`/`clonePath` metadata, not a wire field. The executor snapshots
it before yielding, selects `captureReaperAttempt` under `captureTail` after prior
ownership, setup/summary joins and session save, before safety acquisition, and
restores it after owned callback/child settlement. Both facade getters select
`captureReaperAttempt ?? reaperAttempt`; `run(ctx)` remains the normal binding
writer. Invalid metadata or a tuple foreign to that normal binding refuses before
the action. Omission retains normal fallback and pre-start scoped-reaper refusal;
it does not impose a blanket capture ban. No helper JSON, flags, bootstrap,
protocol, budget, component, model or credential authority changes.
Refs #2852 (AI-synced 2026-10-18).

In retained recovery, the initial predecessor quiescence proof acquires and releases
`runFreeCapture` before #3059's independently supervised
`normalizeRetainedSourceObjects`, outside A's Git-only scope. `ensureClone` also
runs outside free capture. A later held callback covers resumed-tip verification,
direct publication or capture through `recordRecoveryCapture`; successor creation
follows outside that callback. This integration adds no capture allowlist exception.
Refs #2852 (AI-synced 2026-10-18).

The scoped reaper uses registered supervisor roots for fixed `mktemp`, the existing
Node/tsx run-quiescence helper flag, and removal of the exact private temporary
directory. Ordinary scans send legacy JSON without `reportDeadline` and retain
unbounded kill-mode procfs reads. A genuine `reportBudget` adds the floored deadline
and selects no-kill report observation; no new procfs read bounds, bootstrap,
helper flag, request schema or supervisor protocol are introduced. The parent caps
operations at the minimum absolute scope, report (when present), helper and ambient
deadlines. Late completion, nonzero exit, output overflow or cancellation-cleanup
failure is unverified. Safety's exclusive guard refuses reaping with pending launches,
callbacks or unreaped capture children, and refuses ordinary child admission while
the pipeline is active. Started children join through full root completion before
pipeline guard and safety queue release. Raw scoped fallback is refused; exact
attempt, clone key and target binding remain required. Outside executing-flight
scope, maintenance and Claude retain the legacy route without added flight waits
or budgets. Refs #2852 (AI-synced 2026-10-18).

Free-capture roots and the closed-reaper branch bypass `cacheForLaunch` / `track`;
ordinary cached command roots keep their existing accounting. This adds no decorated
environment exception. The exact completed-capture cache attestation reader is
specified in [ADR-1783](1783-run-quiescence-and-attempt-clone-paths.md#invariants-future-code-must-respect).
Refs #2852 (AI-synced 2026-10-18).

GitCache binds the restricted launch policy: the canonical environment includes
exactly one empty `credential.helper` and one empty `core.askpass`, with no
credential-bearing override. The strict GitCache builder policy matches the complete
canonical environment's own keys and values, including exact indexed config order
(reordering is refused). The command-identity launch binds the sandbox mode to the
executor-selected `required`, `best-effort` or `off` mode, alongside the fixed
wrapper, cwd, argv and environment transformation. The immutable original request
and its sandbox-transformed request are both checked synchronously at launch.
Only the builder supplies the empty helper/askpass pins; duplicate entries and caller
helper/askpass argv overrides, even empty ones, are refused. The synchronous launch-request check
scans credential/askpass, includes, config-env, `GIT_CONFIG_PARAMETERS`, Authorization
and URL-userinfo channels. The fixed embedded cancellation-content helper is
allowed only through its supervised binding, not as generic helper execution.
Trusted-worker bare Git keeps its worker identity;
`worker_pat` is the historical identity name, not evidence of credential presence.
Credential presence is checked separately; free capture does not downgrade bare
Git to the isolated clone identity. Blanket refusal of free capture was rejected:
it strands capturable work, including work awaiting verified capture, in the source
clone instead of moving verified bytes into the persistent worker bare and regresses
verified park/pause preservation. Older Docker emptyDir layouts could lose that source
on pod replacement; [ADR-3153](3153-runner-shared-pvc-subpath.md) migrated workers
keep it on the data PVC. Neither layout nor successful capture grants deletion authority
(AI-synced 2026-10-23).
Local capture alone proves neither origin publication nor permission to dispose
of the source. Worker-provided global Git
config may configure drivers; ordinary seeding trust is unchanged. These
configuration pins alone prove neither driver inertness nor process exclusion.

Private `captureSynthesis` async-local state and `runSynthesisGit` bind dynamic
worker author/committer identity, dates and private temporary index/worktree paths
to the exact live Git invocation. The producers are `synthesizeWorkflowWrapper`
(called by `buildWorkflowOverlay` and `buildCheckpointChain`), `bridgeToFloors`
and `buildRecoveryCoverage`; `sizeAttrGit` receives no synthesis authority.
Shape-valid paths, dates or a generic code/path exception cannot authorize synthesis.
Canonical config order, the two empty helper/askpass pins and credential/config/env/URL
refusals still apply; no PAT or username overlay is admitted. Outside executing-flight
capture scopes, GitCache and maintenance keep their existing behavior.
Refs #2852 (AI-synced 2026-10-18).

Codex background JS dependency installation and Devbox `install` / `shellenv`
use registered supervisor roots for fixed normal setup, with the existing runner
identity, manager commands, replacement environment, HOME and caches. The private
setup binding grants no model or credential authority
and is unavailable inside the Git capture lane. A primary child exit or fulfilled
best-effort install result is insufficient: confirmed root cleanup is required
before capture or boundary credential reconciliation. Unconfirmed cleanup remains
separate from the dependency-install outcome. The PATH probe is a read-only
exception: an isolated trusted fixed filesystem-access script checks path entries
as data, without executing them. Claude's installer path is unchanged.

Executor-owned intent/plan summaries use private factory/parameter/harness bindings
in [summary-ownership.ts](../agent/src/codex/summary-ownership.ts), not labels or
structural lookalikes. Their factory, launch, auth, consume, full root disposal and
result POST stay joined after the delivery-abandonment bound. Registry cleanup
failure independently refuses free capture, healthy credential reconciliation and
epoch recreation; retain primary errors separately. Other judge, review, Now and
delivery advice outside this factory keep their existing behavior.

Auth close joins raw refresh and direct-handler work. Check the synchronous epoch
fence before token delivery and before/after release; closing admission prevents a
late reply from restoring authority. The original absolute authority deadline still
expires: owned cleanup stays joined under the fence without a renewed budget or
detached tail. These private bindings add no generic spawner, identities, privileges,
protocol or infrastructure.

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
Today's Claude adapter `agent/src/claude-advice-harness.ts` uses
`buildDenyAllHook` for every tool and exposes no calculator; `model-pass.ts`
retains compatibility and timeout/HOME lifecycle. M2 moved construction while
preserving that behavior under D0. Any future
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
  opaque uzi wire projection alongside normalized usage. Current Codex cost
  uses uzi's pinned Standard price table for either credential mode when
  response evidence prices cleanly; unknown, expired or incomplete evidence
  remains unreported. `metered` is API-equivalent cost, not proof of a charge.
  Legacy `subscription` means no estimate recorded, not metered zero. See
  [PRD #2559](../prds/done/2559-codex-subscription-cost.md) for the cost-only
  supersession; auth and capacity semantics remain unchanged.
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
- Both credential modes remain required. User-approved 2026-09-06
  [D5](../prds/done/1106-codex-harness-phase1.md#decision-log) allows same-subscription
  concurrency and supersedes the earlier run-long seat lock. Coordinated refresh
  and durable latest-state recovery remain production requirements; prior-token
  reuse remains unproven.

## Amendment: runtime 0.159.3 and independent effort (2026-10-01)

The pinned native package advances from 0.156.1 to 0.159.3 (source commit
01fc69f4026735edfdf6789820549727a4867b11). `gpt-6.1-sol` joins the curated
vocabulary and becomes the worker provider fallback. Explicit model defaults
remain authoritative; task-review and PR-description built-ins stay at
`gpt-6-sol`. The new model's catalog requires client 0.153.0 or newer, lists
low through ultra, uses shell_command and declares no default service tier.
Uzi exposes its existing five effort levels, deliberately excluding ultra.
The generated ReasoningEffort wire type is string. Its Rust parser rejects
empty strings but accepts unknown non-empty values as Custom, so it does not
enforce uzi's five-level vocabulary. API write validation and the renderer
enforce that closed product vocabulary.

Both product effort defaults are medium. The existing nullable default_effort
retains the Claude preference; nullable default_codex_effort retains Codex's.
Backfill copies existing non-NULL values verbatim, including blanks. Each
claim reads only the lane for its frozen harness, then resolves NULL/blank to
medium. There is no scheduled/per-run freeze and no new claim payload field.
The default is therefore picked at claim time for queued and resumed runs.
Down retains the original preference and drops the independent Codex value.
Storage stays unconstrained text, with current validation at write surfaces,
so a future runtime-default choice need not reshape the column.

The old Codex executor ignored the claim's effort, and run/advice transports
used modelReasoningEffort instead of the upstream turn/start wire key effort.
Root, advice and delegated child turns use the wire key effort. The gaps are
corrected with real-executor and wire-key regressions. Review
and PR-description helpers keep their existing absence-of-effort behavior.

The additive protocol capability codex_runtime_v2 proves the 0.159.3 runtime/
catalog baseline and effort consumption. Every Codex claiming, placement and
affinity predicate requires it independently of user requirements and their
kill-switch. Older workers queue until an updated image is available; a new
worker retains legacy capabilities for an older API to filter and use.

### Upstream compatibility review

Reviewed the stable release notes for 0.157.0, 0.157.1, 0.158.0 and
0.159.0 through 0.159.3, plus the full source delta from 0.156.1.
The sparse 0.157.1 notes were checked against its ten-file source delta:
the executable changes are Windows spawn fixes; the Unix paths are unchanged.

- App-server error enum: flexUnavailable and tooManyDenials are new scalar
  tags, classified through the bounded allowlist. Core InvalidPrompt projects
  to other; no arbitrary provider text or payload is echoed.
- Token usage: consumed camel-case total/last breakdowns are unchanged and
  still expose no service tier. Standard cost remains an estimate.
- Initialization/auth: initialized remains a notification without params.
  Explicit gateway OAuth is optional and unused by the fixed OpenAI provider.
- Trust/config: explicit project trust_level=untrusted continues to prevent
  automatic promotion. OpenAI still requires account authentication and enables
  WebSockets; its built-in provider cannot be replaced by a same-name table.
- Dynamic tools: consumed registration/call/response wire types are unchanged.
  The new model's native shell shape is included in the real-binary deny proof.
- Persistence: rollout compression remains opt-in and disabled by default;
  uzi uses neither the compression feature nor rollout/compress.
- New instant_interrupt is opt-in and disabled; thread-history pagination,
  gateway OAuth, Windows launch fixes, TUI changes and Bedrock catalogs are not
  used by uzi's fixed stdio/OpenAI composition.

The GPT-6.1 Sol Standard row was verified against the official model page on
2026-10-01: input/cached/cache-write/output are 2.00/0.10/2.50/10.00 per million
tokens, and 4.00/0.20/5.00/15.00 above 272K input, applying to the full response.
See https://developers.openai.com/api/docs/models/gpt-6.1-sol.

## Amendment: coordinated runtime 0.160.0 (2026-10-10)

The native package advances from 0.159.3, source
`01fc69f4026735edfdf6789820549727a4867b11`, to 0.160.0, source
`a956835d020762cb2b570053af06f643a11c0ecc`. Both official musl archives'
SHA-256 hashes were checked against downloaded bytes and the release identity.
The installer still uses its existing manifest, member and SHA checks.

A lock-only upgrade changes the installer destination but leaves the production
launcher pointing at the old version root. The candidate coordinates that path,
the fail-closed probe's version and both architecture digests, active test
fixtures, and M4 version assertions and evidence. A Docker-free regression imports
the actual launcher and probe and inspects the actual M4 assertions/evidence
against the lock; it was observed failing on the lock-only tree and passing after
coordination. It proves coordinate agreement, not execution of the new runtime.
No launcher authority, isolation, refusal, cleanup or installer behavior changes.

### Upstream compatibility review

Reviewed the official [0.160.0 release notes](https://github.com/openai/codex/releases/tag/rust-v0.160.0)
and the full direct pinned-source delta, comprising 363 changed paths
(296 modified, 65 added, two deleted). The linked
[GitHub comparison](https://github.com/openai/codex/compare/01fc69f4026735edfdf6789820549727a4867b11...a956835d020762cb2b570053af06f643a11c0ecc)
is a three-dot comparison from merge base `06971ec9aad037d7c32b7466031fbb8b3b407103`;
it is not the direct old/new delta. The release notes' broader 0.159.0 comparison
is context only.

- Initialization/auth: the app-server protocol tree and account/initialize
  processors are unchanged. `initialized` remains a notification without params;
  API-key, experimental ChatGPT token login and server-initiated refresh shapes
  remain unchanged. The new pin's
  [serialization test](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/app-server-protocol/src/protocol/common.rs#L2880)
  confirms the absent params.
- Trust/config: the unchanged thread processor still promotes unspecified trust
  under its existing conditions; explicit project `untrusted` avoids that branch.
  OpenAI still requires account auth, enables WebSockets and selects its endpoint
  by auth mode. Provider merging retains `or_insert` for OpenAI despite its
  Bedrock-specific override branch. See
  [provider implementation](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/model-provider-info/src/lib.rs#L520).
- Dynamic tools and token usage: consumed registration, request and response
  wire types are unchanged. Camel-case total/last usage buckets still carry no
  service tier. Existing accounting/pricing policy remains; the cold-resume
  ordering characterization remains dated to 0.159.3.
- Terminal errors: consumed app-server error variants are unchanged. Internal
  SSE `content_filter` handling and sampling retry guidance changed, but upstream's
  [bridge regression](https://github.com/openai/codex/blob/a956835d020762cb2b570053af06f643a11c0ecc/codex-rs/codex-api/src/api_bridge_tests.rs#L15)
  projects ContentFilter to the existing `other` wire tag. Sampling behavior is
  not claimed unchanged.
- Native exceptions: registration and async-question/UTC handlers are unchanged;
  async questions still emit `delivery: "async"` and return `{"accepted":true}`.
  UTC retains text and code-mode JSON forms. Uzi's native async exclusion in advice
  and read-only UTC contract remain.
- Persistence: rollout implementation is unchanged and compression remains
  default off; uzi uses neither that feature nor `rollout/compress`. SQLite and
  thread-store internals changed, so persistence as a whole is not claimed
  unchanged.
- Model catalogs: the bundled catalog is unchanged, including model defaults,
  shell shapes and effort vocabulary used by uzi. Explicit `model_catalog_url`
  now selects authoritative refresh/fallback/invalidation and exact-ID matching
  behavior; fixed OpenAI leaves this field unset. This is a source branch
  assessment, not new-package model-discovery proof.
- The remaining delta covers TUI input/reconnect/navigation/presentation and
  provider/project flows; core configuration, inherited environment, Guardian
  handoff/history/budget, skills, isolation/MCP, compaction and execution;
  app-server span logging/running-turn counts, plugin caches/configuration,
  analytics/storage; and build dependencies, CLI doctor, Git-root helpers and
  Windows/macOS execution changes. No required uzi adaptation beyond coordinates
  was established by this audit. Native environment/Guardian changes are not
  assumed to describe uzi's worker-created children.
- Packaging: the npm CLI wrapper is unchanged. Both candidate archives retain
  layout-version-1 manifests and the existing CLI, sibling code-mode host,
  ripgrep, bwrap and zsh layout. This inspection does not prove installation,
  execution or receipt/probe acceptance.

### Upgrade evidence and durable checks

The historical real worker-UID comparison held source/posture fixed:
[0.159.3 control 38019285046](https://github.com/vtmocanu/uzi/actions/runs/38019285046)
at `37e0a4e1` passed, while
[lock-only 0.160.0 run 38019435933](https://github.com/vtmocanu/uzi/actions/runs/38019435933)
at `9ed69ac7` failed. The old executable path was inconsistent with the new
installer destination. This establishes a coordinate defect consistent with the
refusal, not the historical failing syscall or errno. Local Alpine connectivity
prevented the implementing worker's image build; that was infrastructure
evidence, not a runtime regression.

[Coordinated candidate CI 38037485971](https://github.com/vtmocanu/uzi/actions/runs/38037485971)
at `93a4fd6b918236510ee093aebe18e50fd3efc7f7` installed the SHA-verified amd64
package, wrote its 0.160.0 receipt and executed `codex-cli 0.160.0` as the runner
UID. Its unchanged real worker-UID lane passed all 29 required leaves, including
the seven historical launch failures and both EACCES leaves. Those failures
cleared on the tested candidate; their historical errno and whether EACCES was
launch fallout remain unproven. This receipt applies to that head, not an
unobserved later revision.

The production probe was then checked against a real installed tree: an arm64
(`aarch64-unknown-linux-musl`) base image built from `73e794cba113b1cb627a109c81c2a3665dff228b`
installed the SHA-verified arm64 package, wrote its 0.160.0 receipt (lock digest
`7f0fe42ff22ecfa3a47bc4a34f5b22c4218b431a4ec0aba51c7d98299f07900c`), passed the
runner-version guard, and `probeCodexRuntime()` run as the worker UID returned
`capable: true`. This is one local build on one architecture; the amd64 receipt above
comes from CI.

Package installation and the runner-version guard are distinct from production
receipt/probe capability acceptance. An upgrade must also check the real probe
against its installed tree, rather than treating synthetic probe tests or
coordinate equality as that evidence. Architecture-specific execution evidence
must be identified separately from archive checksum/manifest verification.
Keep the full worker-UID inventory, two-shard union, M4 once and required checks;
never loosen refusal, isolation or retention to make an upgrade pass.

The reusable procedure is [Coordinated Codex runtime upgrades](../docs/dev-conventions.md#coordinated-codex-runtime-upgrades).
Broader runtime adaptations require their own verified cause and plan review.

## Amendment: coordinated runtime 0.162.1 (2026-10-20)

The native package advances from 0.160.1 to 0.162.1, source commit
`092d3acd6bec3e3a14bdc7e7a2810ab628ab759d`. The target was published on
2026-10-09 and meets the seven-day stability policy; 0.163.0 does not yet.
The lock, launcher, probe, active fixtures and M4 version evidence move together.

The adoption read covers official notes for
[0.161.0](https://github.com/openai/codex/releases/tag/rust-v0.161.0),
[0.162.0](https://github.com/openai/codex/releases/tag/rust-v0.162.0) and
[0.162.1](https://github.com/openai/codex/releases/tag/rust-v0.162.1), with the
consumed protocol and trust paths checked against the target source:

- Filesystem escalation and background permission inheritance do not replace
  worker callback admission. Native effectful tools stay disabled, approvals
  remain `never`, and unsupported server requests fail closed.
- Multi-agent V2 and hooks stay disabled. Hook matcher compilation changes do
  not give cloned-repository hooks authority; explicit project trust stays
  `untrusted` and project instruction discovery stays off.
- Managed worktree tools are TUI client tools, not app-server-native tools.
  Their `worktrees` flag is default-on, but uzi runs no TUI client or its tool
  registration. Ranked Code Mode tool search remains default-off.
- Dynamic callback request/response shapes remain compatible. New subagent
  model/effort metadata and thread goal provenance are additive. The upstream
  error enum accepts unknown tags; uzi retains its closed normalization map.
- Authoritative committed resume history and server retry guidance are runtime
  improvements. Resume requests retain `excludeTurns: true`; existing replay
  accounting and M4 resume checks remain required. The new partial-answer phase
  does not change uzi's text accumulation or its `turn/completed` terminal boundary.
- GPT-6.1 Sol becoming the bundled catalog default already matches uzi's worker
  fallback. Uzi explicitly supplies its chosen model and retains its curated
  picker, per-model pricing, and effort vocabulary. No model-policy change is
  bundled with this runtime upgrade.

The Docker-free coordinate tests failed all seven leaves on the lock-only
candidate and passed after reconciliation. This proves agreement of pins, not
execution of 0.162.1. Installed receipt/probe acceptance, real worker-UID and M4
execution remain separate evidence required before accepting the upgrade.

## Amendment: coordinated runtime 0.163.0 (2026-10-23)

The native package advances from 0.162.1 to 0.163.0, source commit
`83086188ae0614bd882818ff1aa23f3194962012`. The target was published on
2026-10-15 and meets the seven-day stability policy; 0.164.0 does not yet.
There is no intermediate stable release. The lock, launcher, probe, active
fixtures and M4 version evidence move together.

The adoption read covers the official
[0.163.0 notes](https://github.com/openai/codex/releases/tag/rust-v0.163.0)
and the stable-to-stable source diff for the consumed paths:

- The package no longer bundles patched zsh, and the `shell_zsh_fork` /
  `unified_exec_zsh_fork` flags are retired. Both verified archives otherwise
  carry the same members. `CODEX_MEMBERS` drops `codex-resources/zsh/bin/zsh`.
  Every remaining member stays required, so the installer still fails closed on
  a missing one. Uzi never enabled either flag (`shell_tool` and `unified_exec`
  stay off).
- Three feature defaults flip on. `code_mode_interrupt` lets an interrupt stop
  active Code Mode cells and their nested tool calls; worker callback admission
  and the pause/interrupt handling stay as they are. `cli_daybreak` applies only
  to the TUI and `codex exec`, and uzi drives neither. `api_key_cyber_access_programs`
  acts only when a turn requests an explicit access program, and uzi requests
  none. `instant_interrupt` was already on in 0.162.1.
- A resumed thread restores its saved permission profile and network grants
  before validation, and explicit overrides keep precedence. Uzi sets
  `approvalPolicy: "never"` and explicit `untrusted` project trust on every
  thread start and resume, and sends `environments: []` on every turn. The
  cold-resume native-denial proof stays required.
- Incomplete responses (for example `max_output_tokens`) are now terminal and
  non-retryable. They reach the app-server as `codexErrorInfo: "other"`, which
  uzi's closed map already classifies as `unknown`. The app-server
  `CodexErrorInfo` variant set is identical across both tags.
- The built-in provider definitions (`model-provider-info`) are unchanged.
  Dynamic tool output gains only an optional `retain` field. Thread read-state
  and item lookup RPCs are additive. The model catalog leaves `shell_type`,
  `default_service_tier` and `minimal_client_version` unchanged.
- Adoption candidates such as `features.multi_agent_v2.disable_native_tools`,
  tool-output retention and item lookup stay outside this upgrade.

`task codex-pins:sync` rewrote every registered pin from the lock. This proves
agreement of pins, not execution of 0.163.0. Installed receipt/probe acceptance,
real worker-UID and M4 execution remain separate evidence required before
accepting the upgrade.

## Amendment: temporary account admission (#3530, 2026-10-27)

The account admission policy is **no standing limit; temporary cooldown after rate-limit or repeated-overload evidence.**
The retired one-run-per-seat policy above stays retired. Credential selection
([ADR-0035](0035-run-limit-retry.md)), worker slots
([ADR-0042](0042-worker-run-concurrency.md)) and preservation
([ADR-0759](0759-protect-run-work-usage-limit-park.md)) keep their existing boundaries.
The user contract is [Temporary account admission waits](../docs/rate-limits.md#temporary-account-admission-waits);
[installation](../docs/installation.md#codex-account-admission-upgrades)
owns rollout, rollback and reporting-loss guidance.

### Evidence and persisted policy

State is keyed by user and canonical linked Codex subscription account,
independent of saved alias, credential refresh or re-import. Another
authorized account remains independent, without automatic rotation.
Bound thread/turn errors and terminals use the closed `codexErrorInfo` map:
`rateLimitExceeded` or accepted structured model HTTP 429 supplies rate
evidence; `serverOverloaded` supplies overload. Overload requires two distinct
run IDs within five minutes. Same-run retries cannot supply the second vote;
repeating a vote does not refresh its time. Idle/silence, generic transport
errors or 5xx, usage quota, auth and meter GET 429 do not qualify.
Retry-After is not consulted.

The independent schedules start at 30 seconds for rate and 15 seconds for
overload, double to caps of 15 minutes and five minutes before persisted
positive 0–20% jitter, and reset after 30 and 15 minutes quiet respectively.
Quiet clocks track qualifying unique class observations. In-episode evidence
does not extend the class expiry or advance its step. Later class expiry
controls, with rate winning a tie.

A common recovery episode gives each run a stable spread in 0..W, where W
is the smaller of the opening duration and 60 seconds. Recovery floors prevent
a new episode from shortening prior eligibility. Admission is unlimited after
the finite recovery window. Migration 00364 persists transitions/eligibility;
00365 projects active queued admission. Receipt and overload-vote ledgers
are each bounded at 256; receipts retain at least the 60-second retry horizon.
Capacity overflow refuses the observation rather than manufacturing evidence.

### Claim, lifecycle and trust boundaries

Claim and provisioning use database statement snapshots. Queued runs keep
status, claim generation and custody. An already in-flight statement may
admit after evidence commits; there is no custody-lock guarantee. The signal
path takes a run SHARE lock against lifecycle/generation changes, then checks
owned active subscription claims, enabled linked credentials, material/account
revisions and canonical identity in the following INSERT snapshot. It does
not lock account/alias rows; credential replacement committed after that
snapshot may race acceptance. See the enforcing
[queries](../api/internal/store/queries/codex_admission.sql) and
[service](../api/internal/workersvc/codex_admission.go).

This adds no standing seat lock, ceiling or active-count policy, per-request
limiter, mid-turn pause or force-park. Already dispatched turns and preservation
continue; supported new dispatches, including retries and live approval/revision
continuations, now use the [durable model-operation boundary](#durable-model-operation-boundary-3531-2026-10-28).
Usage and account-unavailability parked policies are independent. Resume,
restart, schedules, same-account aliases and provisioning pass through the next
claim gate. Checker deadlines include queued/setup time without exemption or
extension. API keys, Claude, chat, versioned jobs and isolated-lane eligibility
keep their prior policy.

The queued owner-only read-time class and eligibility including spread appear
on run detail/list/board and CLI; foreign shared/admin summaries redact them.
Active operation waits use the separate owner projection described below.
Finite expiry clears the copy on subsequent reads. Maintenance and account
unavailability retain precedence with independent admission details.
Eligibility is not a promised start. There is no per-run Slack burst,
account panel, settings control or Resume now override.

### Best-effort reporter and acceptance boundary

The evidence-reporting feature is `codex_account_admission_signal_v1`,
independent of strict `codex_account_operation_admission_v1`. Reporter loss or
an ACK neither revokes nor grants model-operation authority. References below
to claims/execution continuing concern telemetry loss; an engaged operation
flight still requires the fresh authorization described below.
[The reporter](../agent/src/codex/admission-signal.ts) queues without awaiting
network from provider/lifecycle handlers. It permits two HTTP attempts capped
at three seconds each within ten seconds monotonic total. The implementation
starts the budget at observation, including queue time, stricter than the
first-send contract. The 32-entry bound includes the active send; overflow
drops evidence. Nonclosing `running`/`awaiting_approval` state sends, pause
intent and completion attempts seal queued work and unsent retries, leaving
active HTTP to finish within its existing three-second attempt and ten-second
observation bounds under the server's live-state fence. Ending authority
permanently closes reporting and aborts active HTTP, allowing loss of the last
attempt. Lifecycle and preservation do not wait for a reporting ACK. This
signal path does not retain raw provider text.

Correction to the reported lifetime cutoff (AI-synced 2026-10-27):
[the client](../agent/src/client.ts) bounds `codexAdmission` at 1,024 live
reporters and `codexAdmissionClosed` at 1,024 recent closed/refused run-generation
high-water marks in FIFO order. Closing retires the live reporter and frees
capacity for future bindings; there is no permanent lifetime run cutoff.
Concurrent live overflow drops the telemetry binding but records its generation
to fence older pending claim responses. Closed-cache eviction rotates
`codexAdmissionEpoch`, so pre-eviction responses cannot rebind forgotten old
authority; this may also drop unrelated pending telemetry bindings. It does
not drop claims or execution. A fresh server `ClaimRun` increments
`claim_generation`; retained closed handles stay inert.

Registration and temporarily missing feature support seal pending work and
suspend new reporting while support is absent; fresh attempts on live reporters
resume when support is advertised. Claims started before registration or feature
invalidation are ignored for telemetry binding through the observation epoch,
without changing execution. Permanently closed, refused or stale-generation
reporters remain closed; old workers still obey server admission.

The supported API deployment remains singleton Recreate. Deploy the additive
schema/API before workers. Application rollback leaves the schema in place;
an old API admits new claims without reading this state, while already engaged
operation flights remain closed to fresh dispatch until admission service
returns or existing controls/deadlines settle them. Roll-forward uses still-live
finite state and the normal quiet-reset policy. Mixed old/new replicas outside
the singleton carry no enforcement promise. The M4 separate-pool/service
reconstruction test models persisted recovery, not a binary/Kubernetes
restart; execution evidence belongs to the integration handoff.

M5 remains external: **full agent suite: exact-head PR CI** requires
maintainer/CI success for required `validate-agent`, both
`test-agent-shard` legs and the worker-UID lane on the exact published head
before merge. The worker cannot trigger hosted jobs before publication;
these checks are not verified here. An optional operator rollout smoke
check is unperformed here and does not substitute for required gates.
The required `test-agent` aggregate is also pending; hosted queue/cache/rerun
and critical-path costs remain unmeasured.

### Durable model-operation boundary (#3531, 2026-10-28)

[#3531](https://github.com/vtmocanu/uzi/issues/3531) extends the same canonical
subscription-account cooldown policy to fresh supported model dispatches.
The [scope inventory](../agent/src/codex-operation-scope.ts) connects four sinks:
root `startTurnRpc`, worker-created `startChildTurn`, production advice
`startTurnRpc`, and the plan/code checker `run` path. Coverage includes lead
planning and initial implementation, human and automatic revisions,
prose/clarification continuations, classified-provider retries and fresh-session
fallbacks; child delegation; intent, plan, Now, delivery/PR-description summaries,
judge and diff-review advice; and both checker stages. Every fresh supported
dispatch reads authorization again; no admitted reply is reused across operations.
Caller-key PR-description evaluation and offline checker replay have no worker
subscription authority. Native HTTP retries, tool continuations and possible
compaction/internal requests within an already dispatched turn have no
demonstrated worker seam. This is not a gate on every upstream request.
Native `multi_agent` and `multi_agent_v2` remain disabled.

The [controller](../agent/src/codex-operation-controller.ts) waits in place,
retaining flight/session, positive claim generation, custody, gate presentation
and pending feedback in `claimed`, `running` or `awaiting_approval`. It holds
no account seat lock and creates no standing limit, active counter, provider
streak/backoff, rotation or Retry-After policy. A concurrent cooldown or
credential/source update may race the authorization read and send: this is
snapshot authorization, not atomic revocation. Already dispatched turns continue.

Elapsed waiting is charged once to the existing wall or whole-call deadline.
The idle watchdog is disarmed before dispatch; waiting fabricates no model
processing, progress or productivity and consumes/creates no owner recovery
grant. Cancel, reject, replacement revision, pause, wall, vault and ownership
routes remain effective; Resume grants no bypass. Capture/checkpoint, refresh
needed for preservation, tool settlement, persistence and teardown are not
gated. Children independently wait within deadlines including existing setup;
there is no exemption, extension or parent-held permit. Notification reading
and settlement continue without a demultiplexer deadlock. Optional summaries
may time out without dispatch.

`codex_account_operation_admission_v1` is negotiated independently of signal
reporting and optional active snapshots. A never-negotiated flight retains
legacy behavior; once negotiated, its latch survives feature loss, clear and
re-registration. **Admission-service outage blocks the next dispatch of every
negotiated subscription flight, including accounts with no known cooldown.**
Already dispatched turns and preservation continue with existing controls,
budgets and deadlines. Transport failures, 5xx, malformed/untyped responses and
untyped route 404 are unavailable waits, not allows or provider failures.
Strict typed authority/source refusals retain their existing control routes.
Deploy API first, workers second; rollback keeps additive migrations and
engaged flights closed until service returns or controls/deadlines settle them.
See [installation](../docs/installation.md#codex-account-admission-upgrades).

[Client reads](../agent/src/client.ts) have a three-second deadline, 4 KiB
actual request and 8 KiB actual response limits including error bodies;
the [server handler](../api/internal/handler/worker_codex_operation_admission.go)
uses a 2.5-second context. One serialized read per run/generation shares the
unsuccessful schedule across siblings and source replacements, so churn cannot
bypass it. Cooldown rechecks take 1–15 seconds from server-clock eligibility;
unavailable reads wait 15 seconds. Finite waits/extensions require a fresh read.
The [service](../api/internal/workersvc/codex_operation_admission.go) and
[authority query](../api/internal/store/queries/codex_operation_admission.sql)
reuse existing episode/class policy and finite per-run spread.

The owner projection carries `account_cooldown_class` (`rate_limit` or
`overload`) and absolute `account_cooldown_eligible_at`, with exact reason
`waiting for account cooldown` and health `waiting_account_cooldown`.
Eligibility is earliest eligibility, not a promised dispatch. This is expected
wait health, not lifecycle or `waiting_worker`. The
[projection boundary](../api/internal/store/migrations/00370_account_cooldown_projection.sql)
requires blocked required work, current positive generation, ownership,
credential binding and revision source plus a 45-second accepted snapshot lease.
Optional background advice cannot label a working root blocked.
Omission/expiry or source/status change clears metadata at an accepted fresh
read; foreign shared/admin summaries omit class and time. Missing/disabled
optional snapshots or an old API leave specialized visibility unknown while
operation enforcement remains independent. A read-time owner overlay can precede
the periodic health sweep; an operation read does not recompute every health.
Cooldown health remains quiet until a fresh health read, not an earliest-time
timer that invents activity. Independent fault/freshness and near-spent wall
signals outrank it; see [Run health](../docs/run-health.md#account-cooldown-waits).

Entry, extension, refresh, expiry and exit create no per-run Slack nudge.
Identical root-render deduplication is scoped to cooldown churn, not a broader
notifier change; an unknown or faulted root can still need an edit and a later
genuine fault can notify. The existing
[actual root-path tests](../api/internal/slacksvc/account_cooldown_test.go)
exercise state/health entry, exit, retries and direct-fault transitions.

Queued maintenance/weekly-reserve precedence, credential selection and slots
remain independent; waiting may occupy a worker slot. API keys, Claude, chat,
versioned jobs and isolated-lane eligibility retain their prior policy.
The M5 hosted exact-head acceptance and optional smoke boundaries above remain
pending; this documentation does not establish them as passed.
