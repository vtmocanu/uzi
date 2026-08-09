import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { provisionRunTools, REASON_PROVISION_FAILED, type ProvisionRunDeps } from "../src/provision-run.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { ClaimConfig } from "../src/protocol.js";
import type { ProvisionInput, ProvisionResult } from "../src/provision.js";
import { nullLogger } from "./helpers.js";

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
      h.statusTexts().some((t) => t.includes("skipping this repo's extra tool(s)") && t.includes("ruby@3.3")),
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
      h.statusTexts().some((t) => t.includes("skipping this repo's extra tool(s)") && t.includes("ruby@3.3")),
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
});
