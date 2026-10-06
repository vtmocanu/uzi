import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { provisionRunTools, REASON_PROVISION_FAILED, removeProvisionDir, type ProvisionRunDeps } from "../src/provision-run.js";
import { restoreTreeWritability } from "../src/rmtree.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { ClaimConfig } from "../src/protocol.js";
import type { ProvisionInput, ProvisionResult } from "../src/provision.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { RACED_FILES, seedRacedTree, startSwapRacer } from "./swap-racer.js";

let worktree: string;
let provisionRoot: string;
let homeDir: string;

beforeEach(async () => {
  worktree = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-prov-wt-"));
  provisionRoot = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-prov-root-"));
  homeDir = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-prov-home-"));
});
afterEach(async () => {
  await fs.rm(worktree, { recursive: true, force: true });
  // Unsupported production platforms retain read-only trees. Restore fixture
  // permissions here, before removal; a test-local after hook runs too late.
  await restoreTreeWritability(provisionRoot);
  await fs.rm(provisionRoot, { recursive: true, force: true });
  await fs.rm(homeDir, { recursive: true, force: true });
});

async function writeDevbox(packages: string[]): Promise<void> {
  await fs.writeFile(path.join(worktree, "devbox.json"), JSON.stringify({ packages }), "utf8");
}

interface Harness {
  ctx: RunContext;
  emits: EmittedMessage[];
  statusTexts(): string[];
}

function makeCtx(config: ClaimConfig | null): Harness {
  const emits: EmittedMessage[] = [];
  const ctx = {
    runId: randomUUID(),
    worktreePath: worktree,
    config,
    emit: (m: EmittedMessage) => emits.push(m),
  } as unknown as RunContext;
  return {
    ctx,
    emits,
    statusTexts: () => emits.filter((m) => m.kind === "status").map((m) => String(m.payload["text"] ?? "")),
  };
}

/** A recording injected `provision`. `behavior` decides success/throw per call. */
function recordingProvision(behavior: (packages: string[]) => Record<string, string>): {
  fn: ProvisionRunDeps["provision"];
  calls: string[][];
} {
  const calls: string[][] = [];
  const fn = (async (input: ProvisionInput): Promise<ProvisionResult> => {
    calls.push([...input.packages]);
    return { toolEnv: behavior(input.packages) };
  }) as unknown as ProvisionRunDeps["provision"];
  return { fn, calls };
}

function makeDeps(provision: ProvisionRunDeps["provision"]): ProvisionRunDeps {
  return { provisionRoot, homeDir, log: nullLogger(), provision };
}

describe("provisionRunTools tier-2 best-effort fallback (PRD #278 M2)", () => {
  it("(a) UZI CASE: opt-in, empty tier-1, repo extra; provision always throws → degrades to empty env", async () => {
    await writeDevbox(["ruby@3.3"]);
    const h = makeCtx({ tool_packages: [], repo_devbox_opt_in: true });
    const { fn, calls } = recordingProvision(() => {
      throw new Error("devbox install failed resolving ruby@3.3");
    });

    const result = await provisionRunTools(h.ctx, makeDeps(fn));

    assert.deepStrictEqual(result, { toolEnv: {} });
    assert.strictEqual(calls.length, 1, "provision called exactly once");
    assert.deepStrictEqual(calls[0], ["ruby@3.3"], "called with the tier-2 package");
    assert.ok(
      h.statusTexts().some((t) => t.includes("this repo's opt-in extra tool(s)") && t.includes("skipping them") && t.includes("ruby@3.3")),
      "a warning about skipping the repo extra was emitted",
    );
  });

  it("(b) FALLBACK: merged install fails on repo extra, retry with tier-1 only succeeds", async () => {
    await writeDevbox(["ruby@3.3"]);
    const h = makeCtx({ tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: true });
    const tier1Env = { PATH: "/tier1/bin" };
    const { fn, calls } = recordingProvision((packages) => {
      if (packages.includes("ruby@3.3")) throw new Error("devbox install failed resolving ruby@3.3");
      return tier1Env;
    });

    const result = await provisionRunTools(h.ctx, makeDeps(fn));

    assert.deepStrictEqual(result.toolEnv, tier1Env, "returns the tier-1 toolEnv");
    assert.strictEqual(calls.length, 2, "provision called twice (merged then tier-1-only)");
    assert.deepStrictEqual(calls[0], ["kubectl@1.31", "ruby@3.3"], "first call is the merged set");
    assert.deepStrictEqual(calls[1], ["kubectl@1.31"], "second call is tier-1 only");
    assert.ok(
      h.statusTexts().some((t) => t.includes("this repo's opt-in extra tool(s)") && t.includes("retrying without them") && t.includes("ruby@3.3")),
      "a warning was emitted",
    );
  });

  it("(c) TIER-1 RETRY ALSO FAILS: rejects with REASON_PROVISION_FAILED", async () => {
    await writeDevbox(["ruby@3.3"]);
    const h = makeCtx({ tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: true });
    const { fn } = recordingProvision(() => {
      throw new Error("devbox totally broken");
    });

    await assert.rejects(provisionRunTools(h.ctx, makeDeps(fn)), (err: Error) => {
      assert.ok(err.message.includes(REASON_PROVISION_FAILED));
      assert.match(err.message, /tool provisioning failed/);
      return true;
    });
  });

  it("(d) PURE TIER-1 FATAL: opt-in off, provision throws → rejects, no fallback", async () => {
    const h = makeCtx({ tool_packages: ["kubectl"], repo_devbox_opt_in: false });
    const { fn, calls } = recordingProvision(() => {
      throw new Error("devbox install failed");
    });

    await assert.rejects(provisionRunTools(h.ctx, makeDeps(fn)), /tool provisioning failed/);
    assert.strictEqual(calls.length, 1, "provision called exactly once (no fallback attempted)");
  });

  it("(e) HAPPY MERGED PATH: opt-in, tier-1 + repo extra, provision succeeds", async () => {
    await writeDevbox(["ruby@3.3"]);
    const h = makeCtx({ tool_packages: ["jq"], repo_devbox_opt_in: true });
    const env = { PATH: "/merged/bin" };
    const { fn, calls } = recordingProvision(() => env);

    const result = await provisionRunTools(h.ctx, makeDeps(fn));

    assert.deepStrictEqual(result.toolEnv, env, "returns the merged toolEnv");
    assert.strictEqual(calls.length, 1, "provision called once");
    assert.deepStrictEqual(calls[0], ["jq", "ruby@3.3"], "called with the merged set");
    assert.ok(
      h.statusTexts().some((t) => t.includes("merged 1 package(s)")),
      "the merged-1-package status was emitted",
    );
  });

  it("(f) NO PACKAGES: no tier-1, opt-in off → empty env, provision never called", async () => {
    const h = makeCtx({ tool_packages: [], repo_devbox_opt_in: false });
    const { fn, calls } = recordingProvision(() => ({ PATH: "/x" }));

    const result = await provisionRunTools(h.ctx, makeDeps(fn));

    assert.deepStrictEqual(result, { toolEnv: {} });
    assert.strictEqual(calls.length, 0, "provision never called");
  });

  it("(g) TIER-2 DENYLIST: a denied repo package is dropped by policy; tier-1 untouched (PRD #123 M1b)", async () => {
    // The repo's devbox.json ships a credential CLI (glab) plus a benign tool (jq).
    // With glab denied, glab must never reach provisioning while tier-1 (kubectl) and
    // the non-denied repo extra (jq) do.
    await writeDevbox(["glab@1.2", "jq"]);
    const h = makeCtx({ tool_packages: ["kubectl@1.31"], repo_devbox_opt_in: true, denied_tool_packages: ["glab", "vault"] });
    const env = { PATH: "/merged/bin" };
    const { fn, calls } = recordingProvision(() => env);

    const result = await provisionRunTools(h.ctx, makeDeps(fn));

    assert.deepStrictEqual(result.toolEnv, env);
    assert.strictEqual(calls.length, 1, "provision called once");
    // (a) the denied package never appears in any provision call's package list.
    for (const call of calls) {
      assert.ok(!call.some((p) => p === "glab@1.2" || p === "glab"), "glab must never be provisioned");
    }
    // (b) tier-1 is untouched and the non-denied repo extra survives the merge.
    assert.deepStrictEqual(calls[0], ["kubectl@1.31", "jq"], "tier-1 kept, jq merged, glab dropped");
    // (c) the "dropped ... blocked by policy" status text is emitted, naming glab.
    assert.ok(
      h.statusTexts().some((t) => t.includes("blocked by policy") && t.includes("glab@1.2")),
      "a blocked-by-policy status naming the dropped tool was emitted",
    );
  });
});

/** A provision dir as a finished install leaves it under the uid split: a read-only (0555)
 *  subtree holding a file, which a plain `fs.rm` as the worker cannot unlink. */
async function readOnlyInstall(runDir: string): Promise<void> {
  const ro = path.join(runDir, ".devbox", "nix", "profile");
  await fs.mkdir(ro, { recursive: true });
  await fs.writeFile(path.join(ro, "manifest.json"), "{}", "utf8");
  await fs.chmod(ro, 0o555);
}

async function exists(p: string): Promise<boolean> {
  return fs.lstat(p).then(() => true, () => false);
}

describe("provision dir cleanup is uid-aware (PRD #1809 M3)", () => {
  it("removeProvisionDir keeps outside names and contents intact during intermediate swaps", async (t) => {
    if (process.platform !== "linux") return t.skip("the swap racer exercises Linux removal");
    const target = path.join(provisionRoot, "run");
    await seedRacedTree(target, homeDir);
    const { logger, lines } = recordingLogger();
    const racer = await startSwapRacer(target, homeDir, worktree);
    let swaps: number;
    try {
      await removeProvisionDir(target, logger);
    } finally {
      swaps = await racer.stop();
    }
    assert.ok(swaps > 0, "the racer made positive intermediate swaps");
    const names = await fs.readdir(homeDir);
    console.log(JSON.stringify({ swaps, outsideRemaining: names.length, outsideLost: RACED_FILES - names.length }));
    assert.deepEqual(names.sort(), Array.from({ length: RACED_FILES }, (_, i) => `f${i}`).sort());
    for (const name of names) assert.equal(await fs.readFile(path.join(homeDir, name), "utf8"), "keep\n");
    assert.ok(!(await exists(target)) || lines.some((l) => JSON.stringify(l).includes("provision dir cleanup failed")));
  });
  it("removes a failed install's provision dir even with a read-only (0555) subtree", async () => {
    // Linux removes the failed install's tree; unsupported platforms retain it.
    // The file-level cleanup restores writability before removing retained fixtures.
    const h = makeCtx({ tool_packages: ["go@1.24"] });
    const provision = (async (input: ProvisionInput): Promise<ProvisionResult> => {
      await readOnlyInstall(input.runDir);
      throw new Error("devbox install failed");
    }) as unknown as ProvisionRunDeps["provision"];

    await assert.rejects(provisionRunTools(h.ctx, makeDeps(provision)), (err: Error) =>
      err.message.startsWith(REASON_PROVISION_FAILED),
    );

    assert.equal(await exists(path.join(provisionRoot, h.ctx.runId)), process.platform !== "linux", "Linux removes the tree; other platforms retain it");
  });

  it("removeProvisionDir never throws and logs a dir it cannot remove", async () => {
    const { logger, lines } = recordingLogger();
    // rmTeardownTree refuses a relative path outright: the failure is logged, not thrown.
    await removeProvisionDir("relative/provision/dir", logger);
    assert.ok(
      lines.some((l) => JSON.stringify(l).includes("provision dir cleanup failed")),
      "the failed cleanup is logged",
    );
  });
});
