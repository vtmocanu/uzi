import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { GitCache } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { WorkerClient } from "../src/client.js";
import {
  TERMINAL_PRESERVATION_ENABLED,
  terminalPreservationActive,
} from "../src/terminal-preservation.js";
import { FakeRecoveryClient, FakeRecoveryGit } from "./codex-reap-fixture.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const SRC_DIR = path.resolve(import.meta.dirname, "..", "src");

const roots: string[] = [];
afterEach(() => {
  for (const r of roots.splice(0)) fs.rmSync(r, { recursive: true, force: true });
});

function srcFiles(): string[] {
  return fs.readdirSync(SRC_DIR, { recursive: true, encoding: "utf8" })
    .filter(f => f.endsWith(".ts"))
    .map(f => path.join(SRC_DIR, f));
}

// A client that claims every feature, including recovery_terminal_preservation_v1.
function allFeaturesClient(): WorkerClient {
  return { hasFeature: (_f: string) => true } as unknown as WorkerClient;
}

function withEnv<T>(env: Record<string, string>, fn: () => T): T {
  const saved: Record<string, string | undefined> = {};
  for (const k of Object.keys(env)) saved[k] = process.env[k];
  Object.assign(process.env, env);
  try {
    return fn();
  } finally {
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  }
}

const HOSTILE_ENV = {
  UZI_EPOCH_RECLAIM_DELETION: "1",
  UZI_EPOCH_SCRATCH_DELETION: "1",
  UZI_TERMINAL_PRESERVATION: "1",
  UZI_TERMINAL_PRESERVATION_ENABLED: "1",
  UZI_TERMINAL_PRESERVATION_V1: "1",
};

describe("terminal preservation gate (#4007 slice B, production-dark)", () => {
  it("the constant is false and only a test override turns the predicate on", () => {
    assert.equal(TERMINAL_PRESERVATION_ENABLED, false);
    assert.equal(terminalPreservationActive(undefined), false);
    assert.equal(terminalPreservationActive({}), true);
    assert.equal(terminalPreservationActive({ label: "x" }), true);
  });

  it("environment and server features cannot enable RunRunner or RecoveryCoordinator", () => {
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-tp-gate-"));
    roots.push(dataDir);
    const git = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
    withEnv(HOSTILE_ENV, () => {
      const factories: ExecutorFactory[] = [
        () => ({ executor: { run: async () => { throw new Error("unused"); } } }) as unknown as ReturnType<ExecutorFactory>,
        ((_runId: string, _codex?: unknown) => ({ executor: { run: async () => { throw new Error("unused"); } } })) as unknown as ExecutorFactory,
      ];
      for (const factory of factories) {
        const runner = new RunRunner(allFeaturesClient(), git, factory, nullLogger(), 20, "tok");
        assert.equal(runner.terminalPreservationEnabled(), false);
        const withOverride = new RunRunner(allFeaturesClient(), git, factory, nullLogger(), 20, "tok", {
          terminalPreservationTestOverride: {},
        });
        assert.equal(withOverride.terminalPreservationEnabled(), true);
      }
      const coord = new RecoveryCoordinator({
        client: new FakeRecoveryClient(),
        git: new FakeRecoveryGit(),
        log: nullLogger(),
        recoveryRoot: git.recoveryRoot,
        workerToken: "gate-test-worker-join-token-0123456789",
      });
      assert.equal(coord.terminalPreservationEnabled(), false);
      const on = new RecoveryCoordinator({
        client: new FakeRecoveryClient(),
        git: new FakeRecoveryGit(),
        log: nullLogger(),
        recoveryRoot: git.recoveryRoot,
        terminalPreservationTestOverride: {},
      });
      assert.equal(on.terminalPreservationEnabled(), true);
    });
  });

  it("source text: no production path can set the gate", () => {
    const main = fs.readFileSync(path.join(SRC_DIR, "main.ts"), "utf8");
    const worker = fs.readFileSync(path.join(SRC_DIR, "worker.ts"), "utf8");
    for (const [name, text] of [["main.ts", main], ["worker.ts", worker]] as const) {
      assert.ok(!text.includes("terminalPreservationTestOverride"), `${name} must not pass the test override`);
      assert.ok(!/TERMINAL_PRESERVATION_ENABLED\s*=/.test(text), `${name} must not assign the gate`);
    }
    const gatePath = path.join(SRC_DIR, "terminal-preservation.ts");
    for (const f of srcFiles()) {
      if (f === gatePath) continue;
      const text = fs.readFileSync(f, "utf8");
      assert.ok(!/TERMINAL_PRESERVATION_ENABLED\s*=[^=]/.test(text), `${path.basename(f)} must not assign the gate`);
    }
    const gate = fs.readFileSync(gatePath, "utf8");
    assert.match(gate, /export const TERMINAL_PRESERVATION_ENABLED: boolean = false;/);
    assert.ok(!gate.includes("process.env"), "the gate file must not read the environment");
  });
});
