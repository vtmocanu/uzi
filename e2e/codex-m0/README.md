# Codex M0 characterization

Opt-in, credential-free experiments for [PRD #1106](../../prds/1106-codex-harness-phase1.md).
They run the real **Codex CLI 0.153.2** app-server against deterministic Responses
fixtures served only on `127.0.0.1` at an ephemeral port. No model makes decisions:
the fixture emits fixed harmless tools and checks their real effects in its own
temporary directory. A passing characterization can demonstrate a stock weakness.
A passing case alone does not establish guardrail parity. M0 design/feasibility
is accepted under ADR-1106; production conformance remains M3/M4 work.

## Run

Use Node 24 or newer and the repository's pinned Task version. Install
`@openai/codex@0.153.2` **outside this repository**, for the machine that will run
the tests. For example:

```sh
m0_install=$(mktemp -d)
npm install --prefix "$m0_install" --no-audit --no-fund @openai/codex@0.153.2
```

Find the installed native `vendor/<target>/bin/codex` executable. Set
`M0_CODEX_BIN` to its absolute path, then run:

```sh
M0_CODEX_BIN=/absolute/path/to/vendor/target/bin/codex task test:codex-m0
```

The target never installs dependencies and is not a dependency of `task gate`.
The harness rejects an absent, relative, or wrong-version executable. Supply the
native binary directly, so neither an npm launcher nor an inherited shell config
is needed in the subprocess environment. Test command recipes and timeout flags
live in the root [Taskfile](../../Taskfile.yml).

The managed and process-ownership experiments use separate container targets:

```sh
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:managed
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:code-mode
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:native-isolation
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:policy-broker
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:supervisor
task test:codex-m0:supervisor-mechanism
```

Run those targets **inside a disposable Linux container**, with
[requirements.toml](requirements.toml) mounted read-only at
`/etc/codex/requirements.toml`. Never install that fixture in the host's `/etc`.
The Codex groups require Linux, the explicit opt-in, and byte equality with
the tracked requirements fixture; the standalone mechanism control needs only
its Linux/Python process environment. The managed feature constraint disables `unified_exec`;
an ordinary user-config toggle alone does not establish this mode in 0.153.2.
The supervisor targets additionally require Python 3.11 and Linux `/proc`; the
mechanism target requires no Codex binary. Keep the packaged
`codex-code-mode-host` sibling beside the native `codex` executable for code-mode
and supervisor tests. The native group uses `gpt-5.5`; the code-mode group uses `gpt-6-astra` and
`gpt-5.6-sol` with their local model metadata and the real code-mode host.

For isolation equivalent to the measured Linux runs, use Node 24 Alpine
for the native/code-mode/policy groups, and Node 24 Debian with Python 3.11 for
the supervisor groups, with `--network none`, `--cap-drop ALL`, `--security-opt no-new-privileges`,
`--user 10002:10002`, a read-only root filesystem, and a writable `/tmp` tmpfs.
Mount the test sources, matching Linux executable, and optional requirements
fixture read-only. Provide a Linux Task executable if invoking the target inside
that image. Name the disposable container outside the `uzi-` namespace and clean
up only that exact container.

## What the tests distinguish

| Experiment | Positive control and measured limit |
|---|---|
| Explicit shell prompt rule | Exactly one approval; accept creates the marker, decline or malformed decision prevents it. The rule covers its shell wrapper. |
| Policy coverage | `on-request` executes the shell and patch with zero approvals. `untrusted` asks once for each measured action. This does not establish coverage of every tool. |
| Client EOF with pending approval | The action remains absent through observed app-server exit. This is not a claim of immediate denial on arbitrary transport loss. |
| `PreToolUse` failures | Allow creates a marker and deny blocks it; missing executable, malformed output, and timeout still create it. These are fail-open stock behaviors. |
| Reusable PTY input | The identical marker command is denied through `exec_command`, but reaches the shell through `write_stdin`. Only startup emits the hook and approval, with `write_stdin_approval` both off and on. |
| Child held before action | The allowed child executes after parent completion and returns actual tool output. Child turn interruption closes the held response and prevents the action. |
| Already-running child shell | A ready marker proves execution started. Turn interruption leaves its background terminal alive; explicit terminal termination empties the registry and prevents the late marker beyond its delay. The uninterrupted command writes that marker. |
| Managed one-shot candidate | Accepted commands run; decline and malformed approval block. The schema omits `write_stdin`, the dispatcher returns `unsupported call: write_stdin`, forced `tty` still exits without a session, and timeout terminates a started delayed command. The identical longer-timeout control completes. |
| Dynamic callback scope | A fixed marker handler accepts its known root thread and denies a second thread despite arguments spoofing the root ID. Malformed/error callback replies produce failed tool results. This is a transport/scope candidate, not complete delegation or MCP replacement. |
| Intended-model code-mode approvals | The real custom `exec` cell invokes managed shell execution. Accept writes; decline/malformed decisions prevent the marker, each with one approval. |
| Metadata-selected code mode | Both `code_mode` and `code_mode_only` false still advertise and execute custom `exec` with the host enabled. These flags alone do not disable execution for the intended-model fixtures. |
| Code-mode retained input | Forced `tty` returns no session and `tools.write_stdin` is unavailable; the registry is empty and the late-input marker absent. |
| Code-mode callback scope | The root callback writes once; another ordinary thread spoofing it in arguments receives a failed result. Runtime thread/turn/call identity is measured; complete role/phase admission is not. |

The app-server path needs `thread/start`'s
`config: { bypass_hook_trust: true }` to arm these fixture hooks. The CLI's global
hook-trust flag alone did not do so in the measured setup. This fixture bypass
is not a production trust decision.

## Isolation and evidence boundaries

Each CLI receives a replacement environment containing fresh `HOME`,
`CODEX_HOME`, XDG directories and temp storage, a fixed system-only `PATH`, and a
dummy key for the localhost provider. User login/config, proxy settings and host
secrets are not inherited. Apps, plugins, web search, remote model discovery,
request compression are disabled for these fixtures. Native baseline cases disable
code mode in configuration; intended-model cases deliberately exercise the real
code-mode host, including metadata-selected mode with both mode flags false.

Fresh home directories do not suppress system `/etc/codex` configuration or
macOS managed preferences. The baseline refuses pre-existing fixed system config
files; the managed group permits only its exact requirements fixture. The
disposable Linux container controls those inputs;
the host run alone is not proof of machine-wide configuration isolation or
external-network denial. The Linux `--network none` run is the network-isolated
measurement. No credential-backed request, Kubernetes worker or production
adapter is exercised here.

The harness records its child process objects, thread IDs and terminal sessions.
Cleanup interrupts active owned turns, terminates listed owned terminals, waits
for app-server exit and escalates only its own process group if needed. Requests,
RPCs and waits are bounded. A cleanup failure retains the exact fixture directory
for investigation. Parent completion and process-leader exit are not used as a
substitute for the child-thread and terminal observations.

Source reference: OpenAI Codex tag `rust-v0.153.2`, commit `657a993c`:
`app-server-protocol` generated wire types, `core/src/exec_policy.rs`,
`core/src/tools/handlers/unified_exec/`, and app-server thread/turn processors.
The fake Responses server is an instrument for that binary; it is not an OpenAI
API compatibility suite. The managed code-mode cases extend shell/identity coverage to the intended
models. They do not establish complete native-authority removal, role/phase
authorization, arbitrary detached-process ownership or production guardrail parity.
The native-isolation, broker and actual-host supervisor groups add bounded
evidence below; the production outer safety permit and all production handlers
remain outside this harness. [ADR-1106](../../adr/1106-codex-harness.md) records
exact executable commits and environment-specific results.

Baseline observations were reproduced on the macOS arm64 host with Node 26 and
on Linux arm64 with Node 24 in the isolated container described above. The managed
one-shot, intended-model code-mode, native-isolation and policy-broker observations
were measured in the Linux arm64 Alpine container with Node 24. Supervisor
observations used the Debian/Python container described above. Exact cases and current totals come from
the named test runner output, not a fixed pass-count claim in this document.

## Additional authority and disposal evidence

| Group | Discriminating observation and boundary |
|---|---|
| Native authority disabled | Enabled controls perform direct/nested shell/patch, return real PNG output and start a native child. With no environments and agents disabled, the matching dispatches are unsupported and marker/child effects absent. Nested collaboration is unavailable in both states, so it is not disablement evidence. |
| Pure cells | A cell computes 42 while runtime I/O globals are undefined and module import/file/env/fetch attempts reject. This bounds the measured runtime surface, not arbitrary VM exploits or an executable Claude calculator. |
| Policy broker transport | Real code-mode callbacks enforce representative root/role/phase grants and synchronous worker-created delegation. Held allow/revoke/throw/invalid-policy controls establish fail-closed marker effects and handler settlement before disposal. These are fixed markers, not production shell/path/forge handlers. |
| Broker identity injection | Unknown/stale/replayed identities are injected directly into the broker using real captured callback IDs; these cases do not forge upstream transport frames. |
| Actual-host supervisor | Active/yielded cells on both intended models have positive delayed-effect controls. Revoke/settle/interrupt/dispose reaps the actual app-server and its differently grouped code-mode host with `ECHILD+__WALL`, then observes no late callback or marker beyond the control delay. |
| Supervisor failure/control | Suppressing signals produces a bounded unconfirmed result, then normal cleanup recovers. Separate setsid/double-fork and non-SIGCHLD clone controls test process-group escape and the need for `__WALL`. |

The ordinary `Probe` cleanup described above is distinct from `SupervisorProbe`:
the latter launches the real app-server beneath its own subreaper and records
owned process identities. Neither fixture implements the specified runner permit
held across a credentialed git/checkpoint publication action.

## Accepted adapter design

[ADR-1106](../../adr/1106-codex-harness.md#execution-policy) describes the
accepted stock app-server architecture: native environments/agents disabled,
worker-owned callbacks and synchronous worker-created child threads, with a
per-run registry of Linux supervisor roots. It replaces the earlier loopback-MCP/native-role
candidate; this characterization harness is not its production implementation.
The [neutral contract companion](harness-contract.md) defines the accepted M2
interfaces and Claude preservation rules. M0 design/feasibility is complete; both subscription
and API-key support remain required, and dummy-key fixtures prove neither live
API-key authentication nor entitlement.

M0 acceptance covers the design and measured feasibility; it does not require
production handler or runner-gate implementation. Those, the all-roots registry
coordination and combined adversarial enforcement tests belong to M3/M4. The
[capability/trust policy](../../adr/1106-codex-harness.md#capability-policy-owners)
and [root ownership](../../adr/1106-codex-harness.md#process-root-ownership)
define those obligations. The user approved the same advice ceiling for Claude
and Codex: isolated pure in-memory calculation is allowed; shell/commands,
filesystem access, network tools, delegation, run/worker callbacks and credential
access are denied. Claude's current deny-all-tools advice path exposes no
calculator and stays unchanged in M0/M2. A future Claude calculator needs its own
isolated implementation/conformance; none was added or tested here.
