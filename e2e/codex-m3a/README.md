# e2e/codex-m3a — image-profile primitive validation (PRD #1156 M3a)

These controls prove that the **isolated Codex launch primitive** (the TS launcher in
`agent/src/codex/` and the static Go supervisor in `agent/codex/supervisor/`) behaves
correctly under the **real hardened worker image and its true production posture**, not a
slimmed stand-in. They exercise the ACTUAL packaged launcher (`launch-cli` /
`launchCodexRoot`) and the ACTUAL root-owned supervisor binary; they never edit those and
bake no test-only escape hatch into them.

There are **two suites, run under two different container postures**:

- The **shell profile controls** (`run.sh` -> `controls.sh`, `task test:codex-m3a:profile`)
  run under the **writable-root immutability posture** and prove channel nondumpability,
  profile fail-before-fork, ownership immutability, `ECHILD+__WALL` disposal and fresh-tree
  ownership (the five controls below).
- The **credential-free lifecycle + isolation suites** (TypeScript;
  `run-lifecycle.sh`, `task test:codex-m3a:lifecycle`) run under the **PRD confinement
  posture** and prove the real code-mode-host lifecycle and the m3 isolation controls
  (controls A/B/C below). This is the milestone-3 deliverable.

## Supported profile (do not deviate)

The primitive is supported ONLY under the PRD #51 **A1 uid split**: a trusted worker uid
(10001) controller and a distinct untrusted runner uid (10002). The container therefore
runs the real image through its root-start entrypoint under compose's security options:

```
docker run --rm --network none \
  --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add SETPCAP --cap-add SETUID --cap-add SETGID \
  --security-opt no-new-privileges \
  --entrypoint /usr/local/sbin/uzi-entrypoint <image> /bin/sh /m3a/controls.sh main
```

**Writable root filesystem, NO `--read-only`, NO `--user` override** — a read-only mount
or a `--user` override would MASK the real on-disk ownership these controls must prove. The
entrypoint establishes the split (`UZI_UID_SPLIT=1`, `/nix` -> runner-owned, `/app` ->
worker-owned, `/opt` + `/usr/local` root-owned) then execs the harness as the worker
(10001). The harness drops to the runner (10002) via the production `setpriv` wrapper
(`agent/src/runner-uid.ts` `setprivRunnerArgs`) wherever a control must act as the
untrusted uid.

## The five controls

1. **Channel boundary (nondumpability) + positive control** (`control_1`). Launches the
   supervisor via the real launcher with a stub child that, as the runner uid, tries to
   read `/proc/<supervisor>/fd/3` (control) and `/fd/4` (evidence): BOTH are DENIED
   (`EACCES`) because the supervisor set `PR_SET_DUMPABLE(0)`, so its `/proc` is root-owned.
   The **positive control** proves the denial is specifically nondumpability, not a blanket
   `/proc` restriction: a same-uid runner process holding an open fd with default dumpable
   IS readable by another runner process. An independent side-probe repeats the denial.

2. **Profile enforcement, fail-before-fork, no bypass** (`control_2a`, `control_nnp`).
   (2a) Drives the supervisor directly as the runner with a WRONG `--expect-uid 9999`:
   it emits `{"event":"abnormal","reason":"profile:uid"}`, exits non-zero, and NEVER forks
   the child (no `launched` marker). It also proves an unknown flag (`--disable-profile`) is
   REJECTED as `invalid arguments`, so no profile-disable flag exists. (2b) A container
   variant WITHOUT `--security-opt no-new-privileges` makes the supervisor's `NoNewPrivs`
   check fail-closed identically (`profile:noNewPrivs`, non-zero, no child).

3. **Immutability by OWNERSHIP** (`control_3`). As the runner uid, every append / chmod /
   replace / rm of `/usr/local/bin/uzi-codex-supervisor`, `/usr/local/bin/node`,
   `/app/src/codex/launcher.ts`, and a `/app/node_modules/` file is DENIED (root- or
   worker-owned). The deliberate CONTRAST: the `/nix` interpreter
   (`/opt/uzi-toolchain/bin/python3` target) is RUNNER-OWNED, so the runner can make it
   writable (chmod round-trip, sha256 unchanged) and can write under `/nix` — proving why
   the `/nix` interpreter cannot be the trust anchor and the static root-owned supervisor
   is required. The interpreter's mode is restored; no toolchain file is damaged.

4. **Disposal via ECHILD+__WALL, not group kill** (`control_4`). Launches a stub child that
   forks a `setsid`'d grandchild into its OWN process group. On dispose the launcher reports
   `state:"drained"`, `authority:"ECHILD+__WALL"`, and `reaped` includes BOTH the child and
   the differently-grouped grandchild; neither survives. A process-group kill would have
   missed the grandchild.

5. **Fresh-tree ownership + env delivery** (`control_5`). Drives `launch-cli` with a
   `kind:"provider"` spec (a dummy provider credential) and the fixed real Codex app-server.
   Asserts each per-launch HOME/CODEX_HOME/XDG_*/TMPDIR tree is owned by uid 10002 with
   exact 0700 access, and `config.toml` is uid 10002 mode 0600. The launcher clears any
   setgid bit inherited from `/data/runner`. It then asserts, **fail-closed**, that the
   real app-server's `/proc` environment is the
   env-delivery allowlist and nothing else: non-empty; EXACTLY `HOME`, `CODEX_HOME`, the four
   `XDG_*_HOME`, `TMPDIR`, `PATH`, `SHELL`, `LANG`, `TERM`, and the provider credential var
   (== the dummy value); `HOME`/`CODEX_HOME` pointing into the fresh 0700 trees; and NO
   host/worker leak (`UZI_WORKER_TOKEN{,_FILE}`, any `ANTHROPIC*`, any `*_PAT`/forge token,
   `NODE_OPTIONS`, and an exported test canary are all absent). This is the regression guard
   for the supervisor's `launchChild` env forwarding.

## The credential-free lifecycle + isolation suites (m3)

These TypeScript suites run **inside the real image** through its root-start entrypoint,
under the **PRD confinement posture** (distinct from the shell controls' writable-root
posture): **read-only root filesystem**, read-only mounted fixtures, and exactly three
explicit writable runtime mounts: runner-owned `/nix`, owned `/data`, and per-uid `/tmp`
(the real entrypoint must migrate/create all three), plus **`--network none`** (loopback
only, for the localhost fake provider). They run as the worker uid (10001) and drive the runner uid
(10002) via the production `setpriv` wrapper. Node's own `--test-timeout` is backed by an
**outer `timeout --kill-after` watchdog** (a leaked codex / code-mode-host handle can
outlive Node's timeout; `--rm` tears the container and any leaked descendant down on kill).

**The spike result (the make-or-break, proven empirically in the real image):** the real
`codex-code-mode-host` launches and runs a code-mode cell **WITHOUT any hook-trust bypass**
(no `--dangerously-bypass-hook-trust`, no per-thread `config:{bypass_hook_trust:true}`) and
**WITHOUT live credentials** (a runtime-assembled dummy bearer against a localhost fake
provider). M0 used `bypass_hook_trust` only to arm its PreToolUse HOOKS; the host launch and
the item/tool/call dynamic-callback path need none of it. The isolated config it needs:
`code_mode = code_mode_only = code_mode_host = true`, `unified_exec = false`, `hooks =
false`, native authority off (`[agents] enabled = false`, `multi_agent(_v2) = false`,
`environments: []` on thread/start), the canonical project `untrusted` with
`project_doc_max_bytes = 0`, and the localhost provider block. No `/etc/codex` fixture is
required for a dynamic-tool-only cell.

- **A. Real code-mode-host lifecycle** (`lifecycle.test.ts`). Both models `gpt-6-astra`
  and `gpt-5.6-sol`, **active AND yielded** (four launches, two tests each). Drives the
  PRODUCTION Go supervisor + real Codex DIRECTLY with a TEST-ONLY `code_mode_host=true`
  config (never `config.ts`), reproducing the frozen `supervisor.test.mjs` `running()`
  BEHAVIOUR with the production supervisor (it does not import `Probe`/`SupervisorProbe`).
  It asserts the real host launches below the supervisor in a DIFFERENT process group, the
  uninterrupted cell writes its delayed marker (positive control), and
  revoke+settle+interrupt+dispose reaps BOTH the app-server and the differently-grouped
  host via `ECHILD+__WALL` with the late marker absent. NO hook-trust bypass, NO creds.
  This is intentionally an e2e-only characterization config: production `config.ts`
  exposes no host-enabling toggle, so the test cannot become a production escape hatch.
- **B. Production-config app-server lifecycle** (`production-launcher.test.ts`). Launches
  the real app-server through the ACTUAL `launchCodexRoot` + `config.ts` hardened stock
  config (host DISABLED), runs a trivial credential-free turn against the fake provider,
  disposes clean, and asserts via `handle.snapshot()` that NO code-mode host process
  appears — the shipped stock config correctly does not launch one.
- **C. Isolation controls** (`isolation.test.ts`). The m3-specific additions on top of the
  shell controls: C1 reads the REAL Codex child's `/proc/<pid>/environ` and asserts exactly
  the replacement-env allowlist + credential with no host/worker leak; C2 asserts no
  auth/session/credential material is baked into the shipped image; C3 is a MUTATION control
  proving the C1 assertion rejects a leaked/mutated env (non-vacuous, fails after
  typecheck); C4 proves malicious repo config cannot enter the production launch
  construction (untrusted project, no folded repo config, injection-shaped identifiers
  rejected); C5 asserts the fresh HOME/CODEX_HOME/XDG/TMPDIR land on the owned writable
  `/data` mount, uid 10002, owner-only mode 0700.

## Integration seam (later M3)

M3a ships ONLY the reusable per-root primitive. The launch/control/result contract is:
`launchCodexRoot(spec)` launches ONE supervisor root and returns a handle whose
`snapshot()` / `dispose()` speak the fd3 control / fd4 evidence wire protocol
(`agent/codex/supervisor/doc.go`); the app-server transport is fds 0/1/2. A dispose is
`clean` ONLY when the supervisor reports `state:"drained"` with authority `ECHILD+__WALL`
AND exits 0 — never inferred from an app-server exit or a process-group kill.

**The per-root evidence limit is deliberate and load-bearing.** A drained/clean result is a
fact about EXACTLY ONE owned root, never a run-level `observed_empty` or a minted permit.
The later worker-adapter integration (M3/M4, NOT this child) still owns: the per-run
registry of ALL roots, immutable run/epoch launch reservations, callback admission /
settlement, child-thread quiescence, and the outer `Executor.safety.withBoundary` permit
that aggregates same-epoch evidence across every root before a run is called safe. A command
requested through a callback is its OWN root, not an app-server descendant, and must be
registered before admission. Complete production thread/tool conformance is M4.

## How to run

Prerequisites: a working docker daemon and, on a cold build, egress for the pinned Codex
and toolchain artifacts.

```
# Build the real image and run the shell profile controls (opt-in; NOT part of `task gate`):
task test:codex-m3a:profile

# Build both templates for amd64 and arm64; run three package negatives in every cell:
task test:codex-m3a:matrix

# Build the real image and run the credential-free lifecycle + isolation suites under the
# confinement posture (opt-in; NOT part of `task gate`):
task test:codex-m3a:lifecycle

# Or drive the orchestrators directly:
./e2e/codex-m3a/run.sh all          # profile: both containers (main + nnp)
./e2e/codex-m3a/run.sh main         # profile controls 1, 2a, 3, 4, 5 (no-new-privileges ON)
./e2e/codex-m3a/run.sh nnp          # profile control 2b (no-new-privileges OFF)
./e2e/codex-m3a/run-lifecycle.sh    # lifecycle + isolation suites (confinement posture)

# Reuse an already-built image (skip the docker build):
UZI_M3A_SKIP_BUILD=1 UZI_M3A_IMAGE=uzi-agent-m3a:base ./e2e/codex-m3a/run.sh all
UZI_M3A_SKIP_BUILD=1 UZI_M3A_IMAGE=uzi-agent-m3a:base ./e2e/codex-m3a/run-lifecycle.sh
```

The supervisor module's own unit tests (gofmt / vet / `go test`) run separately and
independently of the image:

```
task test:codex-supervisor
```

## Files

- `run.sh` — profile-controls host orchestrator: builds the static stub, builds the image,
  runs the hardened container(s).
- `controls.sh` — profile-controls in-container harness (runs as the worker uid; drops to
  runner via `setpriv`). `main` runs controls 1/2a/3/4/5; `nnp` runs control 2b.
- `stub/main.go` — the tiny STATIC stub child the supervisor launches in place of the real
  app-server. Compiled at run time by `run.sh` into `.bin/stub-child` (never committed as a
  binary). It is a static Go binary rather than a shell script so it depends on no external
  command or dynamic loader and can dump the child's environment byte-for-byte for the
  env-delivery assertion (see note).
- `evq.mjs` — a tiny NDJSON event-query helper for the launcher's stderr events (avoids
  parsing JSON in shell and avoids running a `/nix` `jq` as the worker uid).
- `run-lifecycle.sh` — lifecycle + isolation host orchestrator: builds the image and runs
  the three TypeScript suites inside it under the confinement posture, with the outer
  watchdog.
- `lifecycle.test.ts` / `production-launcher.test.ts` / `isolation.test.ts` — controls A /
  B / C (above).
- `fake-provider.ts` — the localhost `/v1/responses` SSE fake provider with runtime dummy
  bearer auth (the m3 analogue of the frozen M0 fixture's server; reuses only the frozen
  pure protocol helpers).
- `supervisor-driver.ts` — drives the PRODUCTION Go supervisor + real Codex app-server RPC
  for control A, plus the TEST-ONLY `code_mode_host=true` config builder (never `config.ts`).
- `harness-m0.d.ts` — ambient types for the FROZEN `../codex-m0/harness.mjs` pure protocol
  helpers (the frozen `.mjs` is not edited; the wildcard `declare module` types it for the
  strict `allowJs:false` tsconfig).
- `tsconfig.json` — NodeNext/strict typecheck config for the suites (cloned from
  `e2e/harness-m2/tsconfig.json`).

## Note: child environment delivery

The supervisor's `launchChild` (`agent/codex/supervisor/main.go`) builds the child
`ProcAttr` with `Env: os.Environ()`, so the child INHERITS the supervisor's own environment.
Under the isolated launcher that environment IS the replaced-env allowlist: the launcher
spawns the supervisor with an explicit, fully-replaced env (HOME/CODEX_HOME/XDG/TMPDIR/PATH,
SHELL/LANG/TERM, and the provider credential for a `kind:"provider"` root) and `setpriv`
passes it through unchanged (no `--reset-env`), so forwarding it verbatim delivers exactly
that allowlist to the child — and nothing from the host/worker. Control 5 asserts this
fail-closed (see the five checks above): the child env is non-empty, is exactly the
allowlist keys with the credential present, HOME/CODEX_HOME point into the fresh 0700 trees,
and no host/worker vars leak. An earlier revision of `launchChild` set `Env` nil (an EMPTY
child env, so the allowlist never reached the child); that one-line bug is now fixed and this
control is its permanent regression guard.
