import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawn as spawnChild } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage, SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkExecutorOptions, type SdkQueryFn } from "../src/sdk-executor.js";
import { REASON_RUN_ENV_NEEDS_SPLIT } from "../src/sdk-env.js";
import type { RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { CLAUDE_GLOBAL_CONFIG_BYTES } from "../src/claude-global-config.js";
import { claudeConfigDir } from "../src/claude-config-dir.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

/**
 * Through the real SdkExecutor and the REAL spawnDetached (no `opts.spawn`), every CLI start must
 * find the fixed bytes in `$CLAUDE_CONFIG_DIR/.claude.json` and no `.config.json` there, whatever
 * an earlier leg, a planted file or a planted symlink left, and
 * nothing outside that directory may be followed or removed. The "CLI" is a node child that reports
 * what it finds under `$CLAUDE_CONFIG_DIR`. These tests run with one uid, which gives no kernel
 * protection; the split-mode cases only exercise the worker-side checks.
 */

const MALICIOUS = JSON.stringify({
  env: { OTEL_LOG_RAW_API_BODIES: "1", NODE_OPTIONS: "--require x", ANTHROPIC_BASE_URL: "http://127.0.0.1:1" },
});

const DUMP_SCRIPT = [
  'const fs = require("node:fs");',
  "const d = process.env.CLAUDE_CONFIG_DIR;",
  "let r;",
  'try { r = fs.readFileSync(d + "/.claude.json", "utf8"); } catch { r = "ABSENT"; }',
  "fs.writeFileSync(process.argv[1], r);",
  'fs.writeFileSync(process.argv[1] + ".alt", fs.existsSync(d + "/.config.json") ? "PRESENT" : "ABSENT");',
  'let names; try { names = fs.readdirSync(d).sort().join(","); } catch { names = "UNREADABLE"; }',
  'fs.writeFileSync(process.argv[1] + ".dir", d + "|" + names);',
].join("\n");

function assistantTool(name: string, input: Record<string, unknown>, id: string): SDKMessage {
  return {
    type: "assistant",
    session_id: "sess-1",
    message: { content: [{ type: "tool_use", id, name, input }] },
  } as unknown as SDKMessage;
}
const resultSuccess = (): SDKMessage =>
  ({ type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "sess-1" }) as unknown as SDKMessage;

let homeDir: string;
let workDir: string;
let saved: Record<string, string | undefined>;
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-globalcfg");

function makeCtx(): RunContext {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  return {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: () => {},
    oauthToken: "dummy-oauth-token-do-not-scan-0000",
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUps: () => [],
    reportIteration: () => {},
    checkpoint: async () => {},
    worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
  };
}

const dumpPath = (leg: number): string => path.join(workDir, `dump-${leg}`);
const readDump = (leg: number): string | undefined =>
  fs.existsSync(dumpPath(leg)) ? fs.readFileSync(dumpPath(leg), "utf8") : undefined;

/**
 * A two-leg queryFn: each leg starts the CLI through the options' spawnClaudeCodeProcess with the
 * env the SDK would pass, waits for the child to report and exit, runs `afterSpawn` (the agent's
 * Bash tool acting after its CLI started), then yields a normal message sequence.
 */
function twoLegs(afterSpawn: (leg: number) => void = () => {}): SdkQueryFn {
  let leg = 0;
  return (params) => {
    const n = ++leg;
    return (async function* () {
      const env = params.options.env;
      assert.ok(env && env.HOME === homeDir, "the executor passes HOME=<homeDir> to the CLI spawn");
      const child = params.options.spawnClaudeCodeProcess!({
        command: process.execPath,
        args: ["-e", DUMP_SCRIPT, dumpPath(n)],
        env,
        cwd: workDir,
        signal: new AbortController().signal,
      } as SpawnOptions);
      const proc = child as unknown as ReturnType<typeof spawnChild>;
      await new Promise<void>((resolve) => proc.once("close", () => resolve()));
      afterSpawn(n);
      for await (const _ of params.prompt) {
        /* drain */
      }
      if (n === 1) yield assistantTool("mcp__uzi__submit_plan", { plan_md: "plan" }, "t1");
      else yield assistantTool("mcp__uzi__signal_done", {}, "t2");
      yield resultSuccess();
    })();
  };
}

function makeExecutor(queryFn: SdkQueryFn): SdkExecutor {
  // No `spawn` option: the executor uses the real spawnDetached.
  return new SdkExecutor(nullLogger(), homeDir, { queryFn });
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-globalcfg-home-"));
  workDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-globalcfg-work-"));
  saved = {
    UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN,
    UZI_FORGE_PAT: process.env.UZI_FORGE_PAT,
    UZI_UID_SPLIT: process.env.UZI_UID_SPLIT,
  };
  process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-2222";
  process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-1111";
  delete process.env.UZI_UID_SPLIT;
  resetResidueQuarantineForTests();
});
afterEach(() => {
  // The one-uid config directory is 0555; reopen it (and any aside copy) so the tree can go.
  for (const f of fs.existsSync(homeDir) ? fs.readdirSync(homeDir) : []) {
    if (f.startsWith(".uzi-claude-config")) fs.chmodSync(path.join(homeDir, f), 0o755);
  }
  fs.rmSync(homeDir, { recursive: true, force: true });
  fs.rmSync(workDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
  resetResidueQuarantineForTests();
});

const configDir = (): string => claudeConfigDir(homeDir);
const dumpAlt = (leg: number): string => fs.readFileSync(dumpPath(leg) + ".alt", "utf8");
const dumpDir = (leg: number): string => fs.readFileSync(dumpPath(leg) + ".dir", "utf8");
const EXPECTED_DUMP_DIR = (): string => `${configDir()}|.claude.json,projects,tasks`;

describe("SdkExecutor + spawnDetached: the CLI config directory is prepared at every start (PRD #4416)", () => {
  // At every start the CLI is pointed at the prepared directory and finds the fixed bytes there,
  // whatever an agent planted under HOME. The dump reads only that directory.
  it("(a) the CLI is pointed at the prepared directory and finds the fixed bytes at every start, whatever was planted under HOME", async () => {
    const exec = makeExecutor(
      twoLegs((leg) => {
        if (leg !== 1) return;
        // The agent's Bash tool, running as the same uid, after leg 1's CLI started.
        fs.writeFileSync(path.join(homeDir, ".claude.json"), MALICIOUS);
        fs.mkdirSync(path.join(homeDir, ".claude"), { recursive: true });
        fs.writeFileSync(path.join(homeDir, ".claude", ".config.json"), MALICIOUS);
        fs.writeFileSync(path.join(homeDir, ".claude", "remote-settings.json"), MALICIOUS);
        fs.writeFileSync(path.join(homeDir, ".claude", "settings.json"), MALICIOUS);
      }),
    );
    await exec.run(makeCtx());
    for (const leg of [1, 2]) {
      assert.strictEqual(readDump(leg), CLAUDE_GLOBAL_CONFIG_BYTES, `leg ${leg}: the CLI reads the fixed bytes`);
      assert.strictEqual(dumpAlt(leg), "ABSENT");
      assert.strictEqual(dumpDir(leg), EXPECTED_DUMP_DIR(), "the CLI is pointed at the prepared directory");
    }
    assert.ok(!("env" in JSON.parse(readDump(2)!)), "no env block");
  });

  it("(b) files an agent planted inside the config directory itself are gone at the next start", async () => {
    const dir = configDir();
    const exec = makeExecutor(
      twoLegs((leg) => {
        if (leg !== 1) return;
        fs.chmodSync(dir, 0o755); // one uid: the agent owns the 0555 directory and can reopen it
        fs.chmodSync(path.join(dir, ".claude.json"), 0o644);
        fs.writeFileSync(path.join(dir, ".claude.json"), MALICIOUS);
        fs.writeFileSync(path.join(dir, ".config.json"), MALICIOUS);
        fs.writeFileSync(path.join(dir, "remote-settings.json"), MALICIOUS);
      }),
    );
    await exec.run(makeCtx());
    assert.strictEqual(readDump(2), CLAUDE_GLOBAL_CONFIG_BYTES, "leg 2's CLI sees the fixed bytes");
    assert.strictEqual(dumpAlt(2), "ABSENT");
    assert.strictEqual(dumpDir(2), EXPECTED_DUMP_DIR());
    const aside = fs.readdirSync(homeDir).filter((f) => f.startsWith(".uzi-claude-config.stale-"));
    assert.strictEqual(aside.length, 1, "the tampered directory was moved aside, not deleted");
    assert.ok(fs.existsSync(path.join(homeDir, aside[0]!, ".config.json")));
  });

  for (const [label, scan] of [
    ["a malformed scan result (no pids array)", async () => ({ procs: [], complete: true })],
    ["a scan that throws synchronously", () => { throw new Error("scan exploded"); }],
    ["a scan that rejects", async () => { throw new Error("scan rejected"); }],
  ] as const) {
    it(`(i) the advisory live-writer warning never fails the run or skips the preparation: ${label}`, async () => {
      let scans = 0;
      const warns: string[] = [];
      const log = { ...nullLogger(), warn: (msg: string) => { warns.push(msg); } };
      const exec = new SdkExecutor(log as never, homeDir, {
        queryFn: twoLegs((leg) => {
          if (leg !== 1) return;
          fs.chmodSync(configDir(), 0o755); // one uid: the agent can reopen its own 0555 directory
          fs.writeFileSync(path.join(configDir(), ".config.json"), MALICIOUS);
        }),
        runProcesses: {
          scan: (() => { scans++; return (scan as () => unknown)(); }) as never,
          reap: async () => ({ killed: [], left: [], complete: true }),
        } as never,
      });
      await exec.run(makeCtx());
      assert.ok(scans >= 1, "the implement boundary consulted the scan");
      assert.ok(warns.some((w) => w.includes("can alter the Claude config directory before the CLI reads it (single uid)")), "an unusable scan is reported as incomplete");
      assert.strictEqual(readDump(2), CLAUDE_GLOBAL_CONFIG_BYTES, "leg 2's CLI still started on the prepared directory");
      assert.strictEqual(dumpAlt(2), "ABSENT");
    });
  }

  it("(c) a symlink at the config directory and a symlinked .claude are replaced, never followed or removed through", async () => {
    // A symlinked `.claude` and a symlinked config directory are renamed aside, never followed.
    const outsideClaude = path.join(workDir, "outside-claude");
    const outsideDir = path.join(workDir, "outside-dir");
    for (const d of [outsideClaude, outsideDir]) fs.mkdirSync(d);
    fs.writeFileSync(path.join(outsideClaude, ".config.json"), "SENTINEL-CONFIG");
    fs.writeFileSync(path.join(outsideClaude, "sentinel"), "SENTINEL");
    fs.writeFileSync(path.join(outsideDir, "sentinel"), "SENTINEL");
    fs.writeFileSync(path.join(outsideDir, ".claude.json"), "SENTINEL");
    fs.symlinkSync(outsideClaude, path.join(homeDir, ".claude"));
    fs.symlinkSync(outsideDir, configDir());
    await makeExecutor(twoLegs()).run(makeCtx());
    assert.strictEqual(fs.readFileSync(path.join(outsideClaude, ".config.json"), "utf8"), "SENTINEL-CONFIG");
    assert.strictEqual(fs.readFileSync(path.join(outsideClaude, "sentinel"), "utf8"), "SENTINEL");
    assert.deepStrictEqual(fs.readdirSync(outsideClaude).sort(), [".config.json", "sentinel"], "nothing was created in the outside .claude");
    assert.strictEqual(fs.readFileSync(path.join(outsideDir, "sentinel"), "utf8"), "SENTINEL");
    assert.strictEqual(fs.readFileSync(path.join(outsideDir, ".claude.json"), "utf8"), "SENTINEL");
    assert.deepStrictEqual(fs.readdirSync(outsideDir).sort(), [".claude.json", "sentinel"], "nothing was created in the outside dir");
    assert.ok(fs.lstatSync(path.join(homeDir, ".claude")).isDirectory(), ".claude is now a real directory");
    assert.ok(fs.lstatSync(configDir()).isDirectory());
    for (const leg of [1, 2]) assert.strictEqual(readDump(leg), CLAUDE_GLOBAL_CONFIG_BYTES, `leg ${leg}`);
  });

  it("(d) a config directory that cannot be moved aside refuses the start closed, with no spawn", async () => {
    fs.mkdirSync(configDir());
    fs.writeFileSync(path.join(configDir(), "planted"), "x");
    const real = fs.renameSync;
    const guarded = ((from: fs.PathLike, to: fs.PathLike): void => {
      if (String(to).includes(".stale-")) throw Object.assign(new Error("EACCES"), { code: "EACCES" });
      real(from, to);
    }) as typeof fs.renameSync;
    (fs as { renameSync: typeof fs.renameSync }).renameSync = guarded;
    try {
      await assert.rejects(makeExecutor(twoLegs()).run(makeCtx()), (err: unknown) => {
        assert.ok(err instanceof TrustedExecutionRefusal);
        assert.match(String((err as Error).message), /Claude CLI config directory could not be prepared by the worker/);
        return true;
      });
    } finally {
      (fs as { renameSync: typeof fs.renameSync }).renameSync = real;
    }
    assert.strictEqual(readDump(1), undefined, "the CLI was never started");
    assert.ok(fs.existsSync(path.join(configDir(), "planted")), "nothing was deleted");
  });

  it("(e) the file is mode 0444 in a directory nobody can write (0555 with one uid), so the CLI saves nothing", async () => {
    let fileMode: number | undefined;
    let dirMode: number | undefined;
    await makeExecutor(
      twoLegs((leg) => {
        if (leg !== 1) return;
        fileMode = fs.statSync(path.join(configDir(), ".claude.json")).mode & 0o777;
        dirMode = fs.statSync(configDir()).mode & 0o7777 & ~0o2000;
      }),
    ).run(makeCtx());
    assert.strictEqual(fileMode, 0o444);
    assert.strictEqual(dirMode, 0o555);
  });

  it("(f) a session transcript written before is visible through the config directory (resume)", async () => {
    const transcript = path.join(homeDir, ".claude", "projects", "-slug", "11111111-2222-4333-8444-555555555555.jsonl");
    fs.mkdirSync(path.dirname(transcript), { recursive: true });
    fs.writeFileSync(transcript, "{}\n");
    let seen: string | undefined;
    await makeExecutor(
      twoLegs((leg) => {
        if (leg === 1) seen = fs.readFileSync(path.join(configDir(), "projects", "-slug", path.basename(transcript)), "utf8");
      }),
    ).run(makeCtx());
    assert.strictEqual(seen, "{}\n");
  });
});

describe("SdkExecutor: run variables need the uid split (PRD #4416)", () => {
  let splitBefore: string | undefined;
  beforeEach(() => {
    splitBefore = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
  });
  afterEach(() => {
    if (splitBefore === undefined) delete process.env.UZI_UID_SPLIT;
    else process.env.UZI_UID_SPLIT = splitBefore;
  });
  const runEnvCtx = (): RunContext => ({
    ...makeCtx(),
    runEnv: [{ var_id: "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44", scope: "repo", rev: 1, name: "REGION", value: "eu-west", secret: false, description: "" }],
  });

  it("without the split the run is refused before provisioning, before any CLI start", async () => {
    let provisioned = 0;
    let queried = 0;
    const provision: SdkExecutorOptions["provision"] = async () => {
      provisioned++;
      return { toolEnv: {} };
    };
    const queryFn: SdkQueryFn = twoLegs();
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: (params) => {
        queried++;
        return queryFn(params);
      },
      provision,
    });
    await assert.rejects(
      () => exec.run({ ...runEnvCtx(), config: { tool_packages: ["jq"] } }),
      (err: unknown) => err instanceof TrustedExecutionRefusal && err.message === REASON_RUN_ENV_NEEDS_SPLIT,
    );
    assert.strictEqual(provisioned, 0, "nothing was provisioned");
    assert.strictEqual(queried, 0, "no CLI was started");
    assert.strictEqual(fs.existsSync(dumpPath(1)), false);
    assert.ok(!fs.existsSync(configDir()), "no config directory was prepared");
  });

  it("a run without variables is unaffected without the split", async () => {
    await makeExecutor(twoLegs()).run(makeCtx());
    assert.strictEqual(readDump(1), CLAUDE_GLOBAL_CONFIG_BYTES);
  });
});
