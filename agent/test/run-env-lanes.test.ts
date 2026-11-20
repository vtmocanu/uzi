import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { buildSdkEnv } from "../src/sdk-env.js";

// PRD #4416: only the receiving run lanes (the Claude run executor and the Codex run executor) take the
// run's environment variables. Chat, cross-check, the isolated executor, the advice harnesses, the job
// runner, judge and review runners and the Codex app-server launch never see them. The builders those
// lanes use take no claim, so this pins the call sites structurally (a source scan) and the builder
// behaviourally (a variable present anywhere else in the process never reaches a sparse env).

const SRC = fileURLToPath(new URL("../src/", import.meta.url));
const read = (rel: string): string => readFileSync(`${SRC}${rel}`, "utf8");

const NON_RECEIVING = [
  "chat-executor.ts", "chat-runner.ts", "claude-cross-check.ts", "cross-check-runner.ts",
  "isolated-executor.ts", "isolated-runner.ts", "claude-advice-harness.ts", "job-runner.ts",
  "judge-runner.ts", "review-runner.ts", "codex/cross-check.ts", "codex/codex-advice-harness.ts",
  "codex/launcher.ts", "codex/launch-cli.ts",
];

function walk(dir: string, prefix = ""): string[] {
  const out: string[] = [];
  for (const e of readdirSync(`${SRC}${prefix}${dir}`, { withFileTypes: true })) {
    const rel = `${prefix}${dir}${dir ? "/" : ""}${e.name}`;
    if (e.isDirectory()) out.push(...walk(e.name, `${prefix}${dir}${dir ? "/" : ""}`));
    else if (e.name.endsWith(".ts")) out.push(rel);
  }
  return out;
}

describe("run_env reaches only the receiving run lanes", () => {
  for (const file of NON_RECEIVING) {
    it(`${file} never reads or passes run_env`, () => {
      assert.equal(/run_?env/i.test(read(file)), false);
    });
  }

  it("the claim's variables are handed to an executor only through RunContext.runEnv from runner.ts", () => {
    const users = walk("").filter((f) => /\bctx\.runEnv\b/.test(read(f))).sort();
    assert.deepEqual(users, ["codex/codex-executor.ts", "sdk-executor.ts"]);
    const builders = walk("").filter((f) => /\bclaimRunEnv\(/.test(read(f))).sort();
    assert.deepEqual(builders, ["run-kind.ts", "runner.ts"]);
  });

  it("a sparse Claude env built the way the non-receiving lanes build it carries no ambient variable", () => {
    process.env.RUN_ENV_LANE_SENTINEL = "lane-sentinel-value";
    try {
      for (const credential of [
        { kind: "oauth", token: "dummy-oauth-token-do-not-scan-0000" } as const,
        {
          kind: "provider", baseUrl: "https://gw.example.test/a", authStyle: "bearer", key: "gw-key-do-not-scan-0000",
          modelMap: { haiku: { model: "h" } },
        } as const,
      ]) {
        const env = buildSdkEnv(credential, "/data/home");
        assert.equal(Object.values(env).includes("lane-sentinel-value"), false);
        assert.equal(Object.hasOwn(env, "RUN_ENV_LANE_SENTINEL"), false);
      }
    } finally {
      delete process.env.RUN_ENV_LANE_SENTINEL;
    }
  });
});
