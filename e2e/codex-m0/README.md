# Codex M0 characterization

Opt-in, credential-free experiments for [PRD #1106](../../prds/1106-codex-harness-phase1.md).
They run the real **Codex CLI 0.153.2** app-server against deterministic Responses
fixtures served only on `127.0.0.1` at an ephemeral port. No model makes decisions:
the fixture emits fixed harmless tools and checks their real effects in its own
temporary directory. A passing characterization can demonstrate a stock weakness.
It does not mean guardrail parity is satisfied or M0 is complete.

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

The managed experiment is a separate target:

```sh
M0_MANAGED_ONESHOT=1 M0_CODEX_BIN=/absolute/linux/vendor/bin/codex task test:codex-m0:managed
```

Run that target **inside a disposable Linux container**, with
[requirements.toml](requirements.toml) mounted read-only at
`/etc/codex/requirements.toml`. Never install that fixture in the host's `/etc`.
The tests require Linux, the explicit opt-in, and byte equality with the tracked
requirements fixture. The managed feature constraint disables `unified_exec`;
an ordinary user-config toggle alone does not establish this mode in 0.153.2.

For isolation equivalent to the measured Linux run, use a local Node 24 Alpine
image with `--network none`, `--cap-drop ALL`, `--security-opt no-new-privileges`,
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

The app-server path needs `thread/start`'s
`config: { bypass_hook_trust: true }` to arm these fixture hooks. The CLI's global
hook-trust flag alone did not do so in the measured setup. This fixture bypass
is not a production trust decision.

## Isolation and evidence boundaries

Each CLI receives a replacement environment containing fresh `HOME`,
`CODEX_HOME`, XDG directories and temp storage, a fixed system-only `PATH`, and a
dummy key for the localhost provider. User login/config, proxy settings and host
secrets are not inherited. Apps, plugins, web search, remote model discovery,
request compression and code mode are disabled for these native-tool cases.

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
API compatibility suite. Managed native one-shot results do not establish
code-mode, MCP/dynamic-tool, arbitrary detached-process or complete production
guardrail coverage. Those remain separate questions.

Baseline observations were reproduced on macOS arm64 and Linux arm64 with Node
24 in the isolated container described above. The managed one-shot observations
were measured in that Linux container. Exact cases and current totals come from
the named test runner output, not a fixed pass-count claim in this document.
