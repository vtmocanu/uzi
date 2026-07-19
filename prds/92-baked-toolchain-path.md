# PRD #92: Baked worker toolchain (go/python3/gcc/pip) missing on the agent PATH after an image roll (stale once-only `/nix` seed)

**GitLab Issue**: [#92](https://gitlab.example.com/vtmocanu/uzi/-/issues/92) (judge-filed from a retrospective of run on vtmocanu/uzi#90)
**Status**: Root cause CONFIRMED live on dev-cluster (2026-07-20). Fable/architect review folded in (it caught a wrong mechanism in the first draft — see Decision Log). Ready to implement.
**Priority**: High. Every subagent on a rolled k8s worker gets `command not found` for go/python3/gcc/pip. Reviewers skip Go test execution; the lead hand-exports raw `/nix/store/...` paths. It defeats the "we test in k8s now" runtime, and rolling a fixed image does NOT heal existing workers (see Root cause).
**Depends on**: PRD #18 (devbox/nix per-run provisioning), PRD #83 M1 (baked the go/python3/pip global toolchain), PRD #89 follow-on (added the gcc package — the roll that first triggered this), PRD #58 (hosted k8s workers + the `seed-nix` init container in `controller/internal/kube/render.go`).
**Related**: Issue [#91](https://gitlab.example.com/vtmocanu/uzi/-/issues/91) (runner-writable, persistent `/nix` — same store; the GC-root work here intersects it). PRD #84 (pre-run readiness preflight — a boot-time toolchain preflight (M3) is the natural runtime analogue and DOES catch this, since the PATH is dead from pod start).

## Symptom (two reports, one bug)

Both on the SAME run (vtmocanu/uzi#90), on worker image `agent-base:0.8.3`:

1. **python3** — the agent Bash tool ran `python3` → `Exit code 127 / python3: command not found` (the user's screenshot).
2. **go** (this issue, #92) — the judge found the reviewer subagent hit `go: command not found` and reviewed the Go tests by inspection only; the lead worked around it by hand-exporting `.../nix/store/1xklj6...-go-1.26.4/bin`.

gcc and pip are affected the same way.

## What the judge got wrong (correct the record)

Issue #92's suggested fix — *"bake `go` onto the default PATH (as was done for gcc/ruby)"* — is a **misdiagnosis**. go/python3/pip are already baked, via `devbox global install` (`agent/devbox-global/devbox.json` + `agent/templates/base/Dockerfile` ~lines 125-152); the *current* image even bakes gcc. Nothing needs *baking*. The problem is delivery: the running worker's `/nix` volume was seeded by an **older** image and never re-seeded, so the current image's toolchain never reaches the store the agent runs against.

## Root cause (CONFIRMED live, 2026-07-20)

The `seed-nix` init container (`nixSeedScript` in `controller/internal/kube/render.go`) tars the image's `/nix` into the per-worker PVC **exactly once**, guarded by a `.uzi-nix-seeded` sentinel. But the devbox global-profile pointer that PATH depends on lives in the **image layer** (`/home/worker/.local/share/devbox/global/default/.devbox/nix/profile/default`), which advances with every image roll — while the seeded PVC does not. Any release that changes the toolchain profile hash strands rolled workers.

Evidence (`kubectl` against worker `uzi-hw-1011e7dd-…`, ns `uzi-workers-docker`):

- **ReplicaSet history for this worker**: `agent-base:0.8.1` (created 18:02:48) → `0.8.2` (18:30:04) → `0.8.3` (19:39:33, current pod).
- **Seed sentinel `.uzi-nix-seeded`: 18:03:41** — the PVC was seeded by **0.8.1**, ~53s after the first pod, and never re-seeded across the two later rolls (sentinel short-circuits).
- **The seeded store holds 0.8.1's profile**: `/nix/store/x5i3m0…-profile/bin/` has `go python3 pip` and **no gcc/cc/c++** (gcc became a package in 0.8.3). The raw go/python closures are present too (`…-go-1.26.4/bin/go`, the exact path the lead hand-exported).
- **PATH points at 0.8.3's hash, which the stale PVC lacks**: PATH carries `…/.devbox/nix/profile/default/bin`, resolving `default → default-1-link → /nix/store/zhlki70…-profile` — and `zhlki70…-profile` is **absent** from the store. So `.../profile/default/bin` is "No such file or directory" and `command -v python3 go gcc pip` → 127.
- **No runtime relink**: the symlink is generation-1 (`default-1-link`, never a `-2-link`) and its mtime tracks the 0.8.3 image build (≈ the 19:39 rollout), not a runtime event. uzi never runs `devbox global` at runtime, and per-run provisioning runs devbox under `HOME=/data/agent-home` (`agent/src/provision.ts`, `agent/src/main.ts`), so nothing can rewrite `/home/worker`'s global profile. The first draft's "H2 runtime relink" theory is refuted.

Consequences that shape the fix:

- **go/python3/pip are recoverable** from the stale seed (they're in `x5i3m0…`, merely off PATH); **gcc is genuinely absent** from this worker's store (its package was never in the 0.8.1 seed). So "the tools are physically present" is true for go/python/pip, false for the gcc package.
- **Rolling a fixed image does NOT heal existing workers.** The sentinel blocks re-seeding, so a broken PVC stays broken until the seed itself becomes version-aware (or the PVC is wiped). This is the crux the exposure-only fix missed.

The base Dockerfile header flagged this whole stanza "UNVERIFIED in this workspace." It shipped unverified and is broken across any toolchain-changing roll.

## Solution

Three layers. The seed fix is primary — without it, existing workers never heal and every future toolchain bump reintroduces the bug.

### 1. Version-aware `/nix` seed (PRIMARY) — `controller/internal/kube/render.go` (+ compose)

Bake the resolved toolchain identity into the image (e.g. `/etc/uzi-toolchain-profile` = the `readlink -f` of the devbox global `default` profile store path, or simply the image `appVersion`). `nixSeedScript` records that value alongside the sentinel and **re-seeds on mismatch** (full wipe + re-tar — the script already wipes-before-extract on a partial seed, and the store is a cache by its own doctrine). Then an image roll that changes the toolchain re-seeds the PVC automatically; a roll that doesn't stays a no-op. `render_test.go` gets cases for match (skip), mismatch (reseed), and fresh (seed). The compose `agentnix` path (auto-seeds only when empty) needs the same guard or a documented `down -v` + the boot preflight below; decide in-PR.

### 2. Expose the toolchain immutably (HARDENING) — both Dockerfiles, lockstep

Make the image self-consistent and PATH independent of devbox's mutable global symlink: after `devbox global install`, `readlink -f` the realized profile and create a stable `/opt/uzi-toolchain → <store profile>`, registered as an **indirect nix GC root** (`/nix/var/nix/gcroots/… → /opt/uzi-toolchain`) so the seed tar carries it and a runner-triggered `nix-collect-garbage` (see #91) can't reap it. Point PATH at `/opt/uzi-toolchain/bin` instead of `…/.devbox/…/default/bin`. Per-run `devbox shellenv` still **prepends** provisioned tools (buildSdkEnv folds `toolEnv.PATH`; `filterShellenv` resolves the `$PATH` back-ref against the base/runner PATH), so run-specific toolchains still win with the baked set as a durable floor. End both Dockerfiles with a **fail-closed** `RUN command -v python3 go gcc pip && go version && python3 --version && gcc --version` so a broken toolchain fails the build instead of shipping silent (this is the guard the "UNVERIFIED" stanza never had). **Guardrail prohibition to record in the entrypoint:** `/opt/uzi-toolchain/bin` must NEVER be added to the worker's stripped PATH (`agent/templates/entrypoint.sh` lines ~74-84) — it dereferences into runner-writable `/nix` and would pierce the PRD #51 M2-audit invariant; it belongs only on the image PATH that becomes `UZI_RUNNER_PATH`.

### 3. Boot-time toolchain preflight (SAFETY NET) — worker startup

At worker start, assert `command -v python3 go gcc pip` (and that `/opt/uzi-toolchain` resolves) and **fail registration loudly** on a miss. This converts any future recurrence — any mechanism, any cluster, compose included — from silent mid-run 127s into an operator-visible boot error. It's the runtime analogue of the M2 build assertion and the piece that also covers the compose staleness.

### Alternatives considered (Decision Log)

- **The first draft's mechanism (H2: per-run `devbox` relinks the global profile at runtime)** — WRONG, refuted by the fable/architect review and then confirmed against the live pod: the symlink is generation-1 with an image-build mtime, per-run devbox uses a `/data` HOME, and the seed timeline (0.8.1 seed → 0.8.3 pod) fully explains it. Recorded here because the first draft asserted an H2 timeline ("loses tools after the first per-run provision") as fact; the real trigger is the image roll, and the PATH is dead from pod start.
- **Issue #92's "bake go onto PATH"** — rejected: go is already baked; re-baking changes nothing. Correction recorded above.
- **Exposure-only fix (immutable `/opt/uzi-toolchain` + assertions, no seed change)** — rejected as sufficient: it makes fresh images correct but cannot heal existing PVCs and re-strands every future toolchain bump (the sentinel keeps the new hash out). Kept as hardening (layer 2), not the primary.
- **Entrypoint self-heal (`devbox global install` on boot)** — rejected as primary: slow, and re-realizing into a runner-writable `/nix` widens #91's surface; the version-aware seed is cleaner.

## Milestones + parallelization

| # | Milestone | Files | Depends on | Phase |
|---|---|---|---|---|
| M0 | Confirm mechanism live (DONE — evidence in Root cause) | — | — | done |
| M1 | Dockerfile exposure: `/opt/uzi-toolchain` symlink + GC root + PATH re-point + fail-closed assertion + bake toolchain-id marker (`/etc/uzi-toolchain-profile`); both templates in lockstep | `agent/templates/base/Dockerfile`, `agent/templates/jvm/Dockerfile` | M0 | 2 |
| M2 | Version-aware seed: `nixSeedScript` reads the M1 marker, records it with the sentinel, re-seeds on mismatch; compose parity decision | `controller/internal/kube/render.go`, `render_test.go`, compose | M1 (marker path contract) | 3 |
| M3 | Boot toolchain preflight (fail registration on miss) + record the entrypoint guardrail prohibition | `agent/src/*` (worker startup), `agent/templates/entrypoint.sh` (comment) | M1 | 3 (parallel with M2) |
| M4 | k8s verify on dev-cluster: fresh-PVC worker AND a worker rolled over a stale PVC both heal without manual PVC deletion; reviewer subagent runs `go test ./...`; compose e2e/smoke green | verification | M2, M3 + release | 4 |

Both Dockerfiles are edited by ONE agent (lockstep — jvm is not `FROM base`; parallel agents on mirrored stanzas invite divergence). M1 freezes the marker path (`/etc/uzi-toolchain-profile`) as the contract; M2 and M3 then proceed in parallel.

## Definition of done

- A worker **rolled onto the fixed image over its existing (stale) PVC** heals automatically — no PVC deletion — and `command -v python3 go gcc pip` all resolve.
- A fresh-PVC worker resolves all four; per-run provisioned tools still take precedence over the baked floor.
- The image **fails to build** if any of go/python3/gcc/pip is missing from the final PATH.
- A worker whose store is missing the toolchain **fails to register** (boot preflight), rather than surfacing 127s mid-run.
- Compose `./e2e/run-e2e.sh` + `./scripts/smoke.sh` stay green.
- Issue #92 closed, misdiagnosis corrected in the note; python3 (the user's screenshot) covered by the same fix.
