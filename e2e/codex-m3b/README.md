# codex-m3b — the packaged DARK CodexExecutor lifecycle proof (PRD #1171 m5)

This directory is the **m5 packaging + e2e harness** for the DARK Codex adapter. It proves two
things about the *packaged* adapter (the code baked into the real worker images), end to end:

1. **The lifecycle + credential-isolation boundary.** The real, packaged `CodexExecutor` is
   driven through the run lifecycle — plan→approval / new-root resume → implement → synchronous
   subagent → root-only signal denial → checkpoint → cancel → finalization — and the **injected
   credential/capability canaries never reach a public message, a provider-visible request, a
   log line, or the command-root state.**
2. **The packaging.** The worker images install the static openat2 fileop helper and Landlock
   command wrapper (both root-owned `0555`) next to the process supervisor, and the packaged
   `CodexExecutor` resolves both by absolute path.

It is the sibling of `e2e/codex-m3a/` (the launcher/supervisor isolation proof) and reuses the
same posture and the frozen M0 protocol helpers.

## What runs where

| Leg | Where | What it proves | Who runs it |
|-----|-------|----------------|-------------|
| Host `tsc --noEmit` | host | the harness typechecks against the packaged adapter shapes | `task test:codex-m3b` (always), CI |
| `lifecycle.test.ts` **Block A** (injected fakes) | host `node --test` **and** in-image | the packaged `CodexExecutor` composes and enforces the canary boundary through the whole lifecycle | in-worker (host) + CI (image) |
| `controls.sh` (packaging) | in-image | supervisor + fileop + Landlock wrapper installed root-owned `0555`; node/tsx/codex present; fixed paths match | CI/maintainer |
| `codex-command-root-linux.test.ts` | in-image only | uid 10003 Landlock isolation hides sibling-run/token state; oversized output cap-kills and reaps the registered root | CI/maintainer |
| `lifecycle.test.ts` **Block B** (real launch) | in-image only | the packaged executor drives the **real** supervisor → real Codex → loopback fake provider with the canary boundary intact | CI/maintainer |

Block A is the **in-worker-validated** security proof; run in-image (`CODEX_M3B_SRC=/app/src`)
the *same* tests load the baked `/app/src` adapter, so it doubles as the packaged
unit-composition proof. Block B and the docker legs are **CI/maintainer-only**: in-worker image
builds are storage-flaky and arm64 is blocked, and the exact real app-server item framing is
still unconfirmed (`agent/src/codex/codex-harness.ts` marks the item types provisional and to be
confirmed here). Block B is therefore authored best-effort and **🔴 maintainer-verified**; it is
skipped host-side.

## The canaries

`fake-provider.ts` `codexCanaries()` assembles a fresh, secret-shaped set at **runtime** (no
literal secret in the source, every value well above the redactor's 8-char floor):

- `credential` — the fresh provider access token the fake `WorkerClient.releaseCodex` hands back.
- `capability` — the claim binding's capability (passed to `releaseCodex`).
- `bashArg` / `patchArg` / `spawnArg` / `planArg` — secret-shaped strings placed inside
  model-supplied **tool arguments**.

The security assertion is that `credential` **and** `capability` are absent from all four
surfaces. The tool-argument canaries are a deliberate contrast: a model-chosen `bashArg`
legitimately flows to its command effect, so the suite asserts it *does* appear in command state
(redaction is not over-broad) while the credential/capability do *not* — a leak is only a leak
when it is the injected credential/capability, never a tool arg the model itself chose.

## Running it

```sh
# In-worker validated (no docker):
task test:codex-m3b                     # unit aggregate + host tsc of this harness
cd agent && ./node_modules/.bin/tsc --noEmit -p ../e2e/codex-m3b/tsconfig.json
cd agent && node --import tsx --test ../e2e/codex-m3b/lifecycle.test.ts   # Block A

# CI/maintainer (docker; both images):
UZI_CODEX_M3B_PACKAGED=1 task test:codex-m3b:packaged
```

`test:codex-m3b:packaged` runs the host `tsc` (always) and, when `UZI_CODEX_M3B_PACKAGED=1` is
set with a working docker daemon, builds both `base` and `jvm` images and runs
`run-lifecycle.sh` inside each. `run-lifecycle.sh` runs the packaging controls (`--network none`)
**and** the lifecycle suite under the read-only-root confinement posture. For Block B's real-launch
proof the lifecycle container joins a purpose-built `docker network create --internal` (no external
egress) alongside a throwaway Postgres and a `codexm3btestserver` container that serves the real
worker Bearer routes; the loopback fake OpenAI provider stays inside the worker container. It is
bounded by an outer `timeout --kill-after` watchdog and asserts the positive per-image Block A
`tests` / `callbacks` / `delegations` / `roots` counts **and** the Block B real-path counts
(`ran===true`, nonzero login / provider turns / callbacks / delegation / registered+reaped
provider+command roots / checkpoints / signals / refresh advance+replay / finalization, api_key
refresh == 0). All throwaway containers/network/image are named outside the `uzi-` namespace and
torn down by exact name.

NOTE: the both-image docker EXECUTION is a CI/maintainer step — in-worker image builds are
storage-flaky (they can fail with `No space left on device` building the nix/devbox toolchain), so
the harness is authored + host-typechecked while the both-image run is executed where disk allows.

`run.sh` is the lighter standalone entry that builds an image and runs only `controls.sh`.

## `live.sh` — maintainer-only, secret-injection-only

`live.sh` is the **only** place the same lifecycle can be pointed at a **real** Codex provider,
and it is built so an automated / no-input invocation can never reach one:

- `./live.sh --dry-run` — the credential-free loopback proof (no real credential); safe to
  automate. It never touches a real provider even if a key is present in the environment.
- `./live.sh` (no argument, no injected key) — **refuses** with a non-zero exit and a clear
  message; there is no default that spends a real credential.
- `./live.sh --live` — real provider, requires an **explicitly injected**
  `CODEX_M3B_LIVE_PROVIDER_KEY` (+ `CODEX_M3B_LIVE_BASE_URL`). A real credential is never baked,
  never a default, and never read from any other variable. Wiring the injected key into a real
  run is a deliberate manual maintainer step, left guarded under review.

## Files

- `tsconfig.json` / `.gitignore` — NodeNext strict typecheck; run-log ignores (no committed binary).
- `packaged-modules.ts` — runtime loaders for the packaged `codex-executor` / `select` modules
  (image `/app/src`, or the host source tree). It does **not** import `main.ts` (which
  self-invokes the worker on import); the dark selection factory is loaded through its exported
  constituents.
- `fake-provider.ts` — the localhost loopback `/v1/responses` fake (accept-any-bearer, records the
  presented token for the canary-absence check) + the shared `codexCanaries()` and the image-leg
  `recordingLifecycleResponder` / `countToolCallbacks`.
- `testserver.Dockerfile` — builds `api/cmd/codexm3btestserver` (the real Bearer-route server) for
  the Block B internal-network container.
- `lifecycle.test.ts` — Block A (injected fakes, always) + Block B (real launch, image-only).
- `run-lifecycle.sh` / `run.sh` / `controls.sh` — the image orchestrators + packaging controls.
- `live.sh` — the maintainer-only real-provider entrypoint.
- `harness-m0.d.ts` — ambient types for the frozen M0 protocol helpers (typecheck only).
