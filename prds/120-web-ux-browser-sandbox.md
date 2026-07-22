# PRD #120: web-ux browser reliability — stop runs fighting the SUID sandbox & SPA navigation after PRD #87

**GitLab Issue**: [#120](https://gitlab.example.com/vtmocanu/uzi/-/issues/120)
**Status**: Draft (created 2026-07-22; revised same day after a fable adversarial review that verified all 8 load-bearing claims against code/git/live state — calibrated toward Hypothesis A as the strong prior, added the deploy-lag hedge, and recorded that the #87 builtin shares the repo template's missing `--no-sandbox` note; see the Decision Log)
**Priority**: Medium

## Problem

PRD #87 (v0.11.0) prebaked chromium into the worker image and standardized the
launch flags centrally: `AGENT_BROWSER_ARGS="--no-sandbox,--disable-dev-shm-usage"`
plus a crash-close shim on `PATH` ahead of the real `agent-browser` CLI
(`agent/templates/base/Dockerfile`, mirrored in `agent/templates/jvm`). The intent was
that every web-ux browser launch gets `--no-sandbox` (mandatory under the PRD #51
hardening: unprivileged uid + `cap_drop:ALL` + `no-new-privileges` make Chromium's
setuid sandbox impossible) without any agent having to know that.

Yet the judge review of run **2ebc093e** (issue #118, web-ux validation wave,
2026-07-22 15:05 UTC) recorded the web-ux agent burning ~15+ tool calls fighting
the browser:

- **Chromium aborted repeatedly on the SUID sandbox** until it rediscovered
  `--no-sandbox` by trial.
- **SPA hard-navigations returned empty bodies** until it switched to clicking
  nav links client-side.
- It also hit `resize` vs `set viewport` command confusion.

It reached a correct verdict, but inefficiently — the exact friction #87 was
supposed to remove.

### Why this is ambiguous (and needs investigating, not just patching)

The run executed on worker **`8e1fef71`**. That is the same worker PRD #113
documents as a **stuck upgrade**: the v0.11.0 release rolled it onto
`agent-base:0.11.0`, and its `seed-nix` init container went CrashLoopBackOff
reseeding the browser's nix closure —

```
tar: store/…-chromium-unwrapped-150.0.7871.128/libexec/chromium/extensions: Cannot open: Permission denied
```

— leaving it offline/mid-failed-upgrade for ~14 minutes. That `seed-nix` perm bug
was fixed in **v0.11.1 / issue #114** (`be9a45a3` normalize `/nix` store perms
after the browser build guard). The run started at 15:05 UTC, ~2h after v0.11.1
was released (13:03 UTC) — but a **release commit time is not an
image-on-worker time**: Harbor publish + ArgoCD sync + the controller's
Deployment roll all add lag, so "~2h after release" does **not** prove the worker
was actually running v0.11.1 at 15:05. That gap is exactly why Hypothesis A stays
live.

So there are two live hypotheses and the fix depends on which is true:

- **Hypothesis A — stale / half-healed image.** At 15:05 the worker was still on
  the broken v0.11.0 seed (or an older pre-#87 image), so the chromium closure
  was not correctly seeded and the launch fell back to the setuid sandbox path. If
  so, #114 already fixed the *root cause* and the residual work is verification +
  a cheap template note, not a new mechanism.
- **Hypothesis B — a real delivery gap.** The worker was healthy on v0.11.1 but
  the baked `--no-sandbox` never reached the web-ux agent's browser invocation.
  Known candidate causes, all already documented in the base Dockerfile header:
  - The SDK hands the agent a **sparse env** (`agent/src/sdk-env.ts` replaces the
    subprocess env), so the baked `AGENT_BROWSER_*` vars do **not** reach the
    agent's Bash tool by themselves — only the `agent-browser` shim re-establishes
    them. An agent that shells `chromium` directly, or invokes the real CLI by an
    absolute path that skips the shim, gets no `--no-sandbox`.
  - The run used `agent_source: repo`, so it ran the **repo** web-ux agent
    (`.claude/agents/web-ux.md`), not the PRD #87 **builtin** (roster
    ten→eleven, commit `30b06b94`). The repo template has SPA-nav guidance
    (lines ~58-61) but **no `--no-sandbox` note at all** — and **neither does the
    #87 builtin itself** (`api/internal/agenttmpl/builtins/web-ux.md`: SPA
    guidance at line 57, zero mentions of the sandbox). So *whichever* source a
    run uses, the shipping template never tells the agent about `--no-sandbox`.
    The SPA guidance also did not prevent the empty-body hard-navs — so it is
    either not reaching the agent or not actionable enough.

**The code makes A the strong prior.** On a healthy v0.11.x image a CLI-following
agent essentially cannot miss `--no-sandbox`: the shim is the only
PATH-resolvable `agent-browser` entry (the real bin at `/app/node_modules/.bin`
is off `PATH`) and it sets `AGENT_BROWSER_ARGS` unconditionally-if-unset, and the
web-ux template directs the agent to use the `agent-browser` CLI. So repeated
SUID aborts near-require either an unhealthy / pre-#87 / rolled-back image (A) or
the agent shelling `chromium` directly (which the judge summary does not
evidence) — B is hard to reach for a CLI-following agent. M1 still decides A vs B
on a **current** v0.11.x worker, but the likely outcome is that this PRD
collapses to **M3 + M5** after M1 (a possibility M1 already licenses). Note the
run-time image tag is probably **not recoverable retroactively** (a worker
self-reports a frozen version string, and k8s events expire), so M1 is
necessarily a **fresh reproduction**, not forensics on the 07-22 run.

## Solution Overview

Make the web-ux browser path reliable end-to-end so a web-ux run never has to
rediscover `--no-sandbox` or work around SPA hard-navigation:

1. **Root-cause on a current worker.** Reproduce a web-ux browser launch on a
   verified-healthy v0.11.1+ worker and confirm whether `--no-sandbox` is applied
   and SPA routes load. Settle Hypothesis A vs B with evidence (image tag the
   worker actually runs, whether the launch went through the shim, whether the
   baked env reached the process).
2. **Guarantee `--no-sandbox` reaches every web-ux browser launch**, independent
   of whether the agent uses the shim, the bare CLI, or a direct chromium call.
   The exact mechanism depends on the root cause (e.g. make the shim the only
   reachable path; or bake the flags where the sparse SDK env cannot strip them).
3. **Harden the web-ux template** (both repo and builtin): a short, explicit
   `--no-sandbox` note and a reinforced "reach SPA routes via in-app nav, not a
   fresh `open`" instruction, plus the `resize` vs `set viewport` correction.
4. **Reconcile the repo web-ux agent with the #87 builtin** so the guidance that
   actually ships (whichever `agent_source` a run uses) is the good one, and they
   do not drift.

Correctness is not at stake (runs still reach the right verdict); this is an
efficiency + reliability fix, so Medium priority.

## Implementation Milestones

- [ ] **M1 — Root-cause on a current v0.11.x worker.** On a verified-healthy
  worker (image tag confirmed, `seed-nix` succeeded), launch a browser the way the
  web-ux agent does and capture: the effective chromium argv, whether
  `--no-sandbox` was present, whether the launch went through the `agent-browser`
  shim, and whether SPA routes load. Record the verdict (Hypothesis A vs B) with
  evidence. If purely A (stale image, already fixed by #114), narrow the remaining
  milestones to M3/M5 and say so in the Decision Log.
- [ ] **M2 — Guarantee `--no-sandbox` on every browser launch.** Close the
  delivery gap found in M1 so a web-ux run cannot end up on the setuid-sandbox
  path — regardless of shim vs bare CLI vs direct chromium. Include a fail-closed
  check (a launch missing `--no-sandbox` should fail loudly in test, not silently
  abort at run time). Make the guarantee **agent-agnostic**: it must cover any
  agent that launches chromium, not only the web-ux agent going through the
  `agent-browser` shim — e.g. a coder rasterizing an SVG→PNG via a headless
  chromium screenshot. (That capability is why the dedicated-rasterizer rec from
  run 0ab992db was dismissed `wont-do`: chromium now covers SVG→PNG faithfully,
  but only once this guarantee reaches non-web-ux launches too.)
- [ ] **M3 — web-ux template hardening.** Add the `--no-sandbox` note, reinforce
  SPA in-app navigation, and fix the `resize` vs `set viewport` guidance in the
  web-ux agent template(s).
- [ ] **M4 — Reconcile repo web-ux agent vs the #87 builtin.** Ensure the shipping
  guidance is identical/equivalent across `agent_source: repo` and `own`/builtin,
  and note the single source of truth so they cannot drift.
- [ ] **M5 — Verified clean run.** A web-ux browser validation run on a current
  worker completes with no sandbox abort and no SPA hard-nav dead-end. Capture the
  evidence (activity log / judge review) showing the friction is gone.
- [ ] **M6 — Tests & docs.** Unit/integration coverage for the M2 guarantee;
  update the relevant docs/specs to match (`specs/ai.md` if a design decision
  lands, per repo convention).

## Success Criteria

- A web-ux browser launch on a current worker gets `--no-sandbox` with **zero**
  agent-side rediscovery, provably (test + a real run).
- SPA routes are reachable without the agent falling into empty-body hard-nav.
- The web-ux guidance is consistent across repo and builtin agent sources.
- No regression to the PRD #51 hardening posture (unprivileged uid, `cap_drop:ALL`,
  `no-new-privileges`) — `--no-sandbox` stays the sanctioned path, not a relaxation
  of the sandbox on a privileged launch.

## Risks & Mitigations

- **Risk: the friction was purely the #114-fixed stale image.** Then M2/M4 may be
  no-ops. Mitigation: M1 is investigation-first and explicitly allowed to narrow
  scope; we do not build a mechanism for a problem #114 already closed.
- **Risk: forcing `--no-sandbox` more broadly weakens security.** Mitigation:
  `--no-sandbox` is already mandatory and sanctioned under the PRD #51 hardening
  (the setuid sandbox is impossible for an unprivileged, cap-dropped uid); this PRD
  makes the *already-required* flag reliable, it does not disable a sandbox that
  would otherwise run.
- **Risk: repo vs builtin template drift reappears.** Mitigation: M4 fixes the
  drift and records the single source of truth.

## Related Work

- **PRD #87** (v0.11.0) — prebaked chromium + `agent-browser` shim + `--no-sandbox`
  env; the web-ux builtin.
- **Issue #114** (v0.11.1, `be9a45a3`) — `seed-nix` `/nix` perm normalization after
  the browser build guard (the stuck-upgrade root cause for worker `8e1fef71`);
  v0.11.1 also carried the browser crashpad-XDG fix, part of the same
  browser-reliability surface (the shim's XDG stanza) this PRD touches.
- **PRD #113** — worker upgrade/version health; documents the `8e1fef71` stuck
  upgrade this run rode on.
- **PRD #92** — version-aware `/nix` reseed + stable toolchain path (the "workers
  don't re-seed after an image roll" class of bug).
- **PRD #51** — worker/runner uid split + `cap_drop:ALL` hardening that makes the
  setuid sandbox impossible (why `--no-sandbox` is mandatory).

## Decision Log

- 2026-07-22 — Created. Split out of a judge-review triage of missing/friction
  tools across 16 runs. Sibling follow-ups placed elsewhere by owner decision:
  `openssl` → tier-2 in the uzi repo `devbox.json` (ruby@3.3 precedent) + admin
  tool_allowlist, done as a direct change; SVG rasterizer → no dedicated tool
  (chromium screenshot covers it), dismissed; `node_modules` prewarm + pre-scan
  `node_modules/.bin` false-positives → a separate future PRD. This PRD is the
  investigate-and-fix one (chromium/web-ux browser reliability).
- 2026-07-22 — Fable adversarial review verified all 8 load-bearing claims
  CONFIRMED against code, git, and live run/review state (incl. the sparse-env +
  shim mechanism, the `8e1fef71`↔#113/#114 link, and that `resize` vs `set
  viewport` is a real agent-browser distinction). Verdict: revise (light), no
  rethink. Applied: (1) calibrated the Problem section so Hypothesis A (stale /
  pre-#87 / rolled-back image) is the strong prior — on a healthy image the shim
  makes B hard to reach for a CLI-following agent, so the PRD likely collapses to
  M3+M5 after M1; (2) added the release-time≠image-on-worker-time hedge and noted
  M1 is a fresh reproduction (run-time image tag not recoverable retroactively);
  (3) recorded that the #87 **builtin** shares the repo template's missing
  `--no-sandbox` note; (4) path/line nits. Also added the M2 agent-agnostic
  `--no-sandbox` clause so the SVG→PNG rasterization use case (dismissed
  dedicated-rasterizer rec, run 0ab992db) is covered for non-web-ux launches too.
