import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";

import { type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { LimitReachedError } from "../src/limit.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import {
  createCodexExecutionSafety,
  type ReconcileBeforeBoundary,
} from "../src/codex/safety.js";
import { buildRunLaneReconcile } from "../src/codex/codex-executor.js";
import {
  ExecutionRegistry,
  newLocalExecutionEpoch,
  type RegisteredRoot,
} from "../src/codex/registry.js";
import { selectCodexBinding, type CodexBinding } from "../src/codex/select.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import {
  api,
  client,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// PRD #1171 m4 (Phase 2B) — every durability/publication sink in the runner routes through the
// optional Codex `withBoundary` reap facade, per-sink auth-mode reconcile, the pause-sink
// designation, and the F1 dispose relocation. Driven with a FAKE Codex executor whose `.safety`
// is a REAL {@link createCodexExecutionSafety} (over a real registry + a tracked provider root +
// the real {@link buildRunLaneReconcile} run-lane reconcile) wrapped to RECORD every withBoundary/
// dispose call. The runner stays harness-agnostic — it only ever sees `executor.safety`.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): void {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => {});
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint: drain the pack and return a LANDED publish, so a park/pause
 *  checkpoint confirmably lands (its boundary/permit routing is what these tests assert). */
function spyPublishLands(): () => void {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return () => {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
  };
}

const SUBSCRIPTION = {
  auth_mode: "subscription",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
  generation: 3,
};
const API_KEY = {
  auth_mode: "api_key",
  access_token: "claim-tok-XXXXXXXX",
  capability: "run-cap-XXXXXXXX",
};

function bindingOf(block: Record<string, unknown>): CodexBinding {
  const sel = selectCodexBinding({ codex: block });
  if (sel.kind !== "codex") throw new Error("expected a codex selection");
  return sel.binding;
}

/** A registry-ownable provider root whose reap/dispose are IDEMPOTENT and counted (a boundary in
 *  a retry loop re-quiesces a closed registry and re-reaps each root — see the recovery sink). */
function trackedRoot(): { root: RegisteredRoot; reaps: () => number; disposes: () => number } {
  let reaps = 0;
  let disposes = 0;
  const root: RegisteredRoot = {
    kind: "provider",
    reap: async () => {
      reaps += 1;
      return { ok: true };
    },
    dispose: async () => {
      disposes += 1;
    },
  };
  return { root, reaps: () => reaps, disposes: () => disposes };
}

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  if (reserved.kind === "reserved") reg.registerRoot(reserved.reservation, root);
}

interface CodexRig {
  registry: ExecutionRegistry;
  safety: CodexExecutionSafety;
  /** Every withBoundary(req.boundary) recorded, in order. */
  boundaries: string[];
  /** Every dispose(req.boundary) recorded (F1: the runner's terminal dispose). */
  disposeBoundaries: string[];
  refreshCalls: () => number;
  releaseCalls: () => number;
  registeredTokens: string[];
  reaps: () => number;
  disposes: () => number;
}

const RELEASE_TOK = "codex-release-tok-XXXXXXXX";
const REFRESH_TOK = "codex-refresh-tok-XXXXXXXX";

function codexRig(opts: { authMode?: "subscription" | "api_key"; blockReconcile?: boolean } = {}): CodexRig {
  const authMode = opts.authMode ?? "subscription";
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const tr = trackedRoot();
  registerRoot(registry, tr.root);
  const binding = bindingOf(authMode === "subscription" ? SUBSCRIPTION : API_KEY);

  let refreshCalls = 0;
  let releaseCalls = 0;
  const fakeClient = {
    releaseCodex: async (_runId: string, _req: { capability: string }) => {
      releaseCalls += 1;
      if (opts.blockReconcile) throw new Error("release contended");
      return { access_token: RELEASE_TOK };
    },
    refreshCodex: async (
      _runId: string,
      _req: { capability: string; operation_id: string; observed_generation: number },
    ) => {
      refreshCalls += 1;
      if (opts.blockReconcile) throw new Error("refresh contended");
      return { access_token: REFRESH_TOK, generation: 7, outcome: "advanced" };
    },
  };
  const registeredTokens: string[] = [];
  const reconcile: ReconcileBeforeBoundary = buildRunLaneReconcile(
    "run-codex-sink",
    fakeClient as never,
    binding,
    (t) => registeredTokens.push(t),
  );

  const inner = createCodexExecutionSafety(
    registry,
    async () => {
      throw new Error("boundary-action spawn is not used by these sink tests");
    },
    reconcile,
  );
  const boundaries: string[] = [];
  const disposeBoundaries: string[] = [];
  const safety: CodexExecutionSafety = {
    kind: "codex",
    withBoundary: (req, action) => {
      boundaries.push(req.boundary);
      return inner.withBoundary(req, action);
    },
    dispose: (req) => {
      disposeBoundaries.push(req.boundary);
      return inner.dispose(req);
    },
  };
  return {
    registry,
    safety,
    boundaries,
    disposeBoundaries,
    refreshCalls: () => refreshCalls,
    releaseCalls: () => releaseCalls,
    registeredTokens,
    reaps: tr.reaps,
    disposes: tr.disposes,
  };
}

/** A fake Codex executor: carries a `.safety`, NO killAgentTree, and runs `behavior`. */
class FakeCodexExecutor implements Executor {
  safety: CodexExecutionSafety;
  constructor(
    safety: CodexExecutionSafety,
    private readonly behavior: (ctx: RunContext) => Promise<ExecutorResult>,
  ) {
    this.safety = safety;
  }
  run(ctx: RunContext): Promise<ExecutorResult> {
    return this.behavior(ctx);
  }
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

// ================================================================================
describe("RunRunner m4 — Codex durability sinks route through withBoundary", () => {
  it("(1) phasePublish FINALIZE routes through withBoundary; the trusted push/MR runs inside it (subscription reconcile + reap before publish)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1200);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.ok(rig.boundaries.includes("finalize"), `finalize boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // The trusted action ran INSIDE the boundary: the MR was opened.
    assert.equal(calls.length, 1, "the MR (the finalize action) was opened inside the boundary");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The provider root was reaped as part of the finalize boundary, BEFORE the credentialed push.
    assert.ok(rig.reaps() >= 1, "the provider root was reaped inside the finalize boundary");
    // The subscription reconcile ran before the boundary (one refresh; zero standalone release).
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile refreshed before the boundary");
    // F1: the runner disposed the registry ONCE after the sinks settled.
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner disposed the registry once, in the finally");
    assert.equal(rig.registry.state(), "disposed", "the registry ends disposed");
    assert.ok(rig.disposes() >= 1, "the provider root was disposed");
  });

  it("(1) checkpoint reap:true routes through withBoundary (boundary=checkpoint)", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1201);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
    assert.ok(rig.boundaries.includes("checkpoint"), `checkpoint boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    // finalize still runs afterwards (a second boundary re-quiesces the closed registry).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary also runs");
  });

  it("(1) park routes through withBoundary (boundary=park)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-park-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "PARK.txt", "committed before the limit\n");
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
      });
      const claim = gitlabClaim(1202, { wait_on_limit: true });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
      assert.ok(rig.boundaries.includes("park"), `park boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(statuses(claim.run_id).includes("limit_wait"), "the run parked at limit_wait");
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the registry was disposed once even on the park path");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(1) graceful shutdown routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      const rig = codexRig();
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const runner = runnerWith(() => ({ executor: new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "SHUT.txt", "committed before shutdown\n");
        started();
        await new Promise<void>((_, reject) => ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }));
        return { branch: ctx.branch };
      }) }), gitlab);
      const p = runner.execute(gitlabClaim(1203, { wait_on_limit: true }));
      await startedP;
      runner.shutdown();
      await p;
      assert.ok(rig.boundaries.includes("shutdown"), `shutdown boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
    } finally {
      restore();
    }
  });

  it("(1) recovery publication routes through withBoundary (boundary=shutdown)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-rec-"));
    try {
      const rig = codexRig();
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "REC.txt", "recovered work\n");
        throw new TransientRecoveryError();
      });
      await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1204));
      assert.ok(rig.boundaries.includes("shutdown"), `recovery boundary recorded; got ${JSON.stringify(rig.boundaries)}`);
      assert.ok(
        api.states.some((s) => s.body.status === "recovery_wait"),
        "the run parked at recovery_wait",
      );
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});

// ================================================================================
describe("RunRunner m4 — per-sink reconcile before the boundary", () => {
  it("(2) refresh-failure BEFORE the action: reconcile blocked → CodexBoundaryError, action never ran, registry poisoned, later publication blocked", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "subscription", blockReconcile: true });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1210);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    // The finalize boundary was attempted, its reconcile blocked, and it threw a
    // CodexBoundaryError("reconcile") — the trusted publish/push never ran (no MR), the run
    // reported failed, and the registry is poisoned (later publication blocked).
    assert.ok(rig.boundaries.includes("finalize"), "the finalize boundary was attempted");
    assert.equal(calls.length, 0, "the trusted push/MR NEVER ran (publication blocked)");
    assert.ok(rig.reaps() === 0, "the provider root was NOT reaped (reconcile blocked before quiesce/reap)");
    assert.ok(statuses(claim.run_id).includes("failed"), "the failed-run report fires for a finalize CodexBoundaryError");
    assert.ok(rig.registry.isPoisoned(), "the registry is poisoned (sink counter zero)");
    assert.ok(rig.refreshCalls() >= 1, "the subscription reconcile attempted a refresh");
    // F1: the runner STILL disposes the registry in the finally even on the blocked/failed path —
    // the provider root is torn down (disposeTools disposes roots regardless of poison).
    assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the terminal dispose ran even though the boundary was blocked");
    assert.ok(rig.disposes() >= 1, "the provider root was torn down by the terminal dispose");
  });

  it("(3) api_key reconcile: zero refreshCodex calls, releaseCodex called, the action runs", async () => {
    const { gitlab, calls } = fakeGitlab();
    const rig = codexRig({ authMode: "api_key" });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "IMPL.txt", "codex work\n");
      return { branch: ctx.branch };
    });
    const claim = gitlabClaim(1211);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);

    assert.equal(rig.refreshCalls(), 0, "an api_key run performs ZERO subscription refresh calls");
    assert.ok(rig.releaseCalls() >= 1, "an api_key reconcile freshly releases (re-authorizes)");
    assert.equal(calls.length, 1, "the trusted push/MR ran (reconcile ready)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
  });
});

// ================================================================================
describe("RunRunner m4 — credential-free sinks mint NO permit", () => {
  it("(4) the pause sink mints NO permit — withBoundary is not called for the credential-free pause publish", async () => {
    const { gitlab } = fakeGitlab();
    const restore = spyPublishLands();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-m4-pause-"));
    try {
      const rig = codexRig();
      let boundariesAtPause: string[] = [];
      let parkedResult: boolean | undefined;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
        await ctx.checkpoint?.({ reap: false }); // fetch-back (also credential-free, no permit)
        const at = { completedCount: 2, total: 3 };
        parkedResult = await ctx.parkForPause?.(at);
        boundariesAtPause = [...rig.boundaries]; // capture the moment the pause sink returns
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      });
      const claim = gitlabClaim(1220);
      await runnerWith(() => ({ executor: exec, homeDir: path.join(homeRoot, "h") }), gitlab).execute(claim);
      assert.equal(parkedResult, true, "the pause parked (its credential-free publish landed)");
      assert.deepEqual(
        boundariesAtPause,
        [],
        `neither the reap:false checkpoint NOR the pause sink minted a permit; got ${JSON.stringify(boundariesAtPause)}`,
      );
      assert.ok(statuses(claim.run_id).includes("paused"), "the run reported paused");
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("(5) a reap:false checkpoint mints NO permit — no withBoundary for the iteration-boundary publish", async () => {
    const { gitlab } = fakeGitlab();
    const rig = codexRig();
    let boundariesAfterCheckpoint: string[] = [];
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "ITER.txt", "iteration work\n");
      await ctx.checkpoint!({ reap: false });
      boundariesAfterCheckpoint = [...rig.boundaries];
      return { branch: ctx.branch };
    });
    await runnerWith(() => ({ executor: exec }), gitlab).execute(gitlabClaim(1221));
    assert.deepEqual(
      boundariesAfterCheckpoint,
      [],
      `a reap:false checkpoint never minted a permit; got ${JSON.stringify(boundariesAfterCheckpoint)}`,
    );
    // ...and finalize still runs afterwards (the ONLY boundary of this run).
    assert.deepEqual(rig.boundaries, ["finalize"], "the only permit this run minted was the finalize sink");
  });
});

// ================================================================================
describe("RunRunner m4 — Claude/stub legacy path is byte-unchanged", () => {
  it("(6) an executor with killAgentTree only (no safety) takes the legacy branch — withBoundary is never referenced", async () => {
    const { gitlab, calls } = fakeGitlab();
    const kills: string[] = [];
    // A LEGACY executor: killAgentTree only, NO `safety`. reapForSink/withCodexBoundaryOnly must
    // take the literal killAgentTree branch (finalize wrapper does NOT reap: the security-boundary
    // killAgentTree already did). No `safety` ⇒ the runner's terminal dispose is skipped entirely.
    const legacy: Executor = {
      run: async (ctx) => {
        commitInTree(ctx.worktreePath, "IMPL.txt", "claude work\n");
        return { branch: ctx.branch };
      },
      killAgentTree: () => kills.push("kill"),
    };
    const claim = gitlabClaim(1230);
    await runnerWith(() => ({ executor: legacy }), gitlab).execute(claim);
    assert.equal(calls.length, 1, "the MR opened exactly as before (finalize wrapper is a plain call)");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    // The security-boundary reap fired EXACTLY once (per the untouched killAgentTree at the
    // boundary). `=== 1` — not `>= 1` — guards the double-reap hazard: if withCodexBoundaryOnly's
    // legacy branch erroneously re-reaped, this would be 2.
    assert.equal(kills.length, 1, "the legacy killAgentTree reap fired exactly once at the security boundary");
  });
});
