# e2e/codex-m3a — image-profile primitive validation (PRD #1156 M3a)

These controls prove that the **isolated Codex launch primitive** (the TS launcher in
`agent/src/codex/` and the static Go supervisor in `agent/codex/supervisor/`) behaves
correctly under the **real hardened worker image and its true production posture**, not a
slimmed stand-in. They exercise the ACTUAL packaged launcher (`launch-cli` /
`launchCodexRoot`) and the ACTUAL root-owned supervisor binary; they never edit those and
bake no test-only escape hatch into them.

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
   `kind:"provider"` spec (a dummy provider credential) and a stub child in `env` mode.
   Asserts each per-launch HOME/CODEX_HOME/XDG_*/TMPDIR tree is owned by uid 10002 with
   owner-only 0700 access, and `config.toml` is uid 10002 mode 0600. (The trees also carry an
   inherited setgid bit from the setgid `/data/runner` parent — benign; access stays
   owner-only.) It then asserts, **fail-closed**, that the child's dumped environment is the
   env-delivery allowlist and nothing else: non-empty; EXACTLY `HOME`, `CODEX_HOME`, the four
   `XDG_*_HOME`, `TMPDIR`, `PATH`, `SHELL`, `LANG`, `TERM`, and the provider credential var
   (== the dummy value); `HOME`/`CODEX_HOME` pointing into the fresh 0700 trees; and NO
   host/worker leak (`UZI_WORKER_TOKEN{,_FILE}`, any `ANTHROPIC*`, any `*_PAT`/forge token,
   `NODE_OPTIONS`, and an exported test canary are all absent). This is the regression guard
   for the supervisor's `launchChild` env forwarding.

## How to run

Prerequisites: a working docker daemon and, on a cold build, egress for the pinned Codex
and toolchain artifacts.

```
# Build the real image and run all controls (opt-in; NOT part of `task gate`):
task test:codex-m3a:profile

# Or drive the orchestrator directly:
./e2e/codex-m3a/run.sh all          # both containers (main + nnp)
./e2e/codex-m3a/run.sh main         # controls 1, 2a, 3, 4, 5 (no-new-privileges ON)
./e2e/codex-m3a/run.sh nnp          # control 2b (no-new-privileges OFF)

# Reuse an already-built image (skip the docker build):
UZI_M3A_SKIP_BUILD=1 UZI_M3A_IMAGE=uzi-agent-m3a:base ./e2e/codex-m3a/run.sh all
```

The supervisor module's own unit tests (gofmt / vet / `go test`) run separately and
independently of the image:

```
task test:codex-supervisor
```

## Files

- `run.sh` — host orchestrator: builds the static stub, builds the image, runs the
  hardened container(s).
- `controls.sh` — in-container harness (runs as the worker uid; drops to runner via
  `setpriv`). `main` runs controls 1/2a/3/4/5; `nnp` runs control 2b.
- `stub/main.go` — the tiny STATIC stub child the supervisor launches in place of the real
  app-server. Compiled at run time by `run.sh` into `.bin/stub-child` (never committed as a
  binary). It is a static Go binary rather than a shell script so it depends on no external
  command or dynamic loader and can dump the child's environment byte-for-byte for the
  env-delivery assertion (see note).
- `evq.mjs` — a tiny NDJSON event-query helper for the launcher's stderr events (avoids
  parsing JSON in shell and avoids running a `/nix` `jq` as the worker uid).

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
