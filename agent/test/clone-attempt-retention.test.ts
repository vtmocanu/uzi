import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawnSync } from "node:child_process";
import {
  compareAttemptIds,
  formatAttemptId,
  isRetainedArtifactName,
  parseRetainedArtifactName,
} from "../src/attempt-path.js";
import { GitCache, PendingRecoveryCaptureError, gitEnv, type AttemptSeedOptions } from "../src/git.js";
import { screenToolPath } from "../src/guardrails.js";
import { deriveCloneKey } from "../src/run-kind.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import {
  CodexCallbackBroker,
  type FileopRequest,
  type FileopResponse,
  type SpawnCommandOptions,
  type SpawnCommandResult,
} from "../src/codex/broker.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { recordingLogger, testGitCacheOptions } from "./helpers.js";

// issue #1783 M2 — the git-layer half of per-attempt clone paths: the retention sweep that bounds
// what retained attempts cost, the attempt ledger / recovery journal contract the watcher's backup
// script reads, the owner-path predicate (d′), and that an attempt path is an ordinary working
// tree to every consumer that screens or runs in it.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const REPO_ROOT = path.resolve(import.meta.dirname, "..", "..");

let fx: Fixture;
let git: GitCache;
let lines: unknown[];
const unlock: string[] = [];

beforeEach(() => {
  fx = makeFixture();
  const rec = recordingLogger();
  lines = rec.lines;
  git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
});

afterEach(() => {
  for (const p of unlock.splice(0)) fs.chmodSync(p, 0o755);
  fx.cleanup();
});

function cfg(bare: string, ...args: string[]): string {
  return execFileSync("git", ["-C", bare, "config", "--local", ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" });
}

function cfgAll(bare: string, key: string): string[] {
  try {
    return cfg(bare, "--get-all", key).split("\n").filter((l) => l !== "");
  } catch {
    return [];
  }
}

function hex16(n: number): string {
  return n.toString(16).padStart(16, "0");
}

/** An attempt id that sorts by `second` (older first). */
function idAt(second: number, gen = 1): string {
  return formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, second)), gen, hex16(second + 1));
}

interface Seeded {
  bare: string;
  branch: string;
  key: string;
  canonical: string;
  parent: string;
}

async function keyFixture(key = "issue-3001", branch = "agent/issue-3001"): Promise<Seeded> {
  const bare = await git.ensureClone(fx.originPath);
  const canonical = git.runnerClonePath(bare, key);
  fs.mkdirSync(path.dirname(canonical), { recursive: true });
  return { bare, branch, key, canonical, parent: path.dirname(canonical) };
}

/** A retained attempt dir + its skills sibling + a ledger value. */
function plantAttempt(s: Seeded, attemptId: string, runId: string, state: "live" | "abandoned" | "retired"): { dir: string; skills: string } {
  const dir = `${s.canonical}.attempt-${attemptId}`;
  const skills = path.join(s.parent, `.uzi-skills-${path.basename(dir)}`);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "WORK.txt"), attemptId);
  fs.mkdirSync(skills, { recursive: true });
  fs.writeFileSync(path.join(skills, "plugin.json"), "{}");
  cfg(s.bare, "--add", `uzi-attempts.${s.branch}.entry`, JSON.stringify({ attemptId, runId, clonePath: dir, state }));
  return { dir, skills };
}

function seedOpts(over: Partial<AttemptSeedOptions> = {}): AttemptSeedOptions & { quiescentCalls: string[][] } {
  const quiescentCalls: string[][] = [];
  return {
    attemptId: mintAttemptId(9),
    isLive: () => false,
    beforeSeed: async () => {},
    quiescent: async (paths) => {
      quiescentCalls.push(paths);
      return true;
    },
    ...over,
    quiescentCalls,
  };
}

function ledgerState(s: Seeded, attemptId: string): string | undefined {
  let state: string | undefined;
  for (const raw of cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`)) {
    const e = JSON.parse(raw) as { attemptId: string; state: string };
    if (e.attemptId === attemptId) state = e.state;
  }
  return state;
}

// ─── P-retention ───────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-retention: the seed-time retention sweep", () => {
  it("keeps the newest 3 abandoned attempts, deleting the oldest first, each with its skills sibling", async () => {
    const s = await keyFixture();
    const planted = [0, 1, 2, 3, 4, 5].map((i) => ({ id: idAt(i), ...plantAttempt(s, idAt(i), `run-${i}`, "abandoned") }));
    const opts = seedOpts();
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-new", false, undefined, opts);
    for (const p of planted.slice(0, 3)) {
      assert.equal(fs.existsSync(p.dir), false, `oldest ${p.id} deleted`);
      assert.equal(fs.existsSync(p.skills), false, "with its skills sibling");
      assert.equal(ledgerState(s, p.id), "retired");
    }
    for (const p of planted.slice(3)) {
      assert.equal(fs.existsSync(p.dir), true, `newest ${p.id} kept`);
      assert.equal(fs.existsSync(p.skills), true);
      assert.equal(ledgerState(s, p.id), "abandoned");
    }
    assert.equal(fs.existsSync(seeded.path), true, "the new attempt is seeded");
    assert.deepEqual(opts.quiescentCalls[0], [planted[0]!.dir, planted[0]!.skills], "the scan covers the pair");
  });

  it("orders by generation numerically within one second (g10 after g9)", () => {
    const t = new Date(Date.UTC(2026, 0, 1));
    const ids = [formatAttemptId(t, 10, hex16(1)), formatAttemptId(t, 9, hex16(2)), formatAttemptId(t, undefined, hex16(3))];
    assert.deepEqual([...ids].sort(compareAttemptIds), [ids[2], ids[1], ids[0]]);
  });

  it("never deletes a journaled, custody-held, live, not-quiescent or undeletable entry", async () => {
    const s = await keyFixture();
    const planted = [0, 1, 2, 3, 4, 5, 6, 7].map((i) => ({ id: idAt(i), run: `run-${i}`, ...plantAttempt(s, idAt(i), `run-${i}`, "abandoned") }));
    const [journaled, custody, live, busy, locked] = planted;
    // (1) another branch's recovery journal names it.
    cfg(s.bare, "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "someone", clonePath: journaled!.dir }));
    // (2) a durable-recovery record of its run.
    fs.mkdirSync(path.join(fx.dataDir, "recovery", custody!.run), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", custody!.run, "capture.json"), "{}");
    // (5) a deletion that fails (a root-owned subtree, simulated read-only).
    const ro = path.join(locked!.dir, "root-owned");
    fs.mkdirSync(ro);
    fs.writeFileSync(path.join(ro, "file"), "x");
    fs.chmodSync(ro, 0o500);
    unlock.push(ro);
    const opts = seedOpts({
      isLive: (p) => p === live!.dir,
      quiescent: async (paths) => paths[0] !== busy!.dir,
    });
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-new", false, undefined, opts);
    for (const p of [journaled, custody, live, busy]) {
      assert.equal(fs.existsSync(p!.dir), true, `${p!.id} kept`);
      assert.equal(fs.existsSync(p!.skills), true, `${p!.id} skills kept`);
      assert.equal(ledgerState(s, p!.id), "abandoned");
    }
    assert.equal(fs.existsSync(path.join(ro, "file")), true, "the undeletable entry is kept");
    assert.equal(ledgerState(s, locked!.id), "abandoned", "and not marked retired");
    assert.ok(
      lines.some((l) => (l as { msg?: string; path?: string }).msg?.includes("could not delete") && (l as { path?: string }).path === locked!.dir),
      "the failed deletion is logged",
    );
    for (const p of planted.slice(5)) assert.equal(fs.existsSync(p.dir), true);
  });

  it("never counts a live or retired attempt, nor one without a ledger identity", async () => {
    const s = await keyFixture();
    const liveOnes = [0, 1, 2, 3, 4].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "live"));
    const unknown = `${s.canonical}.attempt-${idAt(20)}`;
    fs.mkdirSync(unknown);
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-new", false, undefined, seedOpts());
    for (const p of liveOnes) assert.equal(fs.existsSync(p.dir), true);
    assert.equal(fs.existsSync(unknown), true);
  });

  it("keeps at most 5 .uzi-residue-* per key, oldest first, and only for its own key", async () => {
    const s = await keyFixture();
    const residue = [0, 1, 2, 3, 4, 5, 6].map((i) => {
      const id = idAt(i);
      const p = path.join(s.parent, `.uzi-residue-${s.key}.attempt-${id}`);
      fs.mkdirSync(p);
      cfg(s.bare, "--add", `uzi-attempts.${s.branch}.entry`, JSON.stringify({ attemptId: id, runId: `run-${i}`, clonePath: `${s.canonical}.attempt-${id}`, state: "abandoned" }));
      return p;
    });
    const otherId = idAt(0, 2);
    const other = path.join(s.parent, `.uzi-residue-${s.key}1.attempt-${otherId}`);
    fs.mkdirSync(other);
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-new", false, undefined, seedOpts());
    assert.deepEqual(residue.map((p) => fs.existsSync(p)), [false, false, true, true, true, true, true]);
    assert.equal(fs.existsSync(other), true, "issue-30011's residue is not issue-3001's");
  });

  it("isRetainedArtifactName: skills and residue siblings, never a clone", () => {
    assert.equal(isRetainedArtifactName(".uzi-skills-issue-1.attempt-x"), true);
    assert.equal(isRetainedArtifactName(".uzi-residue-issue-1"), true);
    assert.equal(isRetainedArtifactName("issue-1"), false);
    assert.equal(isRetainedArtifactName(`issue-1.attempt-${idAt(1)}`), false);
    assert.deepEqual(parseRetainedArtifactName(`.uzi-skills-issue-1.attempt-${idAt(1)}`), { kind: "skills", cloneBasename: `issue-1.attempt-${idAt(1)}` });
    assert.equal(parseRetainedArtifactName(".uzi-residue-"), undefined);
  });

  it("no key derivation or attempt formatter can produce a .uzi-residue-* or .uzi-skills-* name", async () => {
    const branchCandidates = [
      "main",
      "agent/issue-7",
      "feature/x.y",
      ".uzi-skills-evil",
      ".uzi-residue-evil",
      "x/.uzi-skills-evil",
      "release/1.0",
      "ci-fix/pipeline-9",
    ];
    const validBranch = (b: string): boolean =>
      spawnSync("git", ["check-ref-format", "--branch", b], { env: GIT_ENV, stdio: "pipe" }).status === 0;
    const keys: string[] = [];
    for (const iid of [1, 42, 1769]) keys.push(deriveCloneKey({ kind: "issue", runId: "r", issueIid: iid })!.slug);
    keys.push(deriveCloneKey({ kind: "self_improve", runId: "6f1c2c1a-0000-4000-8000-000000000000" })!.slug);
    keys.push(deriveCloneKey({ kind: "prompt", runId: "6f1c2c1a-0000-4000-8000-000000000000" })!.slug);
    keys.push(deriveCloneKey({ kind: "task", runId: "6f1c2c1a-0000-4000-8000-000000000000", branch: "whatever" })!.slug);
    for (const b of branchCandidates.filter(validBranch)) {
      keys.push(deriveCloneKey({ kind: "mr_rework", runId: "r", branch: b })!.slug);
      keys.push(deriveCloneKey({ kind: "ci_fix", runId: "r", pipelineRef: b, pipelineId: 3, defaultBranch: "main" })!.slug);
    }
    assert.ok(!branchCandidates.filter(validBranch).some((b) => b.startsWith(".")), "git refuses a dot-leading branch");
    for (const k of keys) {
      assert.equal(isRetainedArtifactName(k), false, k);
      assert.equal(isRetainedArtifactName(`${k}.attempt-${mintAttemptId(1)}`), false, k);
      assert.equal(k.startsWith("."), false, k);
    }
    // And the git layer refuses such a key outright before seeding an attempt.
    const s = await keyFixture();
    await assert.rejects(
      git.runnerCloneForBranch(s.bare, "agent/x", ".uzi-skills-evil", "r", false, undefined, seedOpts()),
      /unsafe runner clone key/,
    );
  });
});

// ─── P-ledger-contract ─────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-ledger-contract: the keys and JSON shapes backup-runs.sh reads", () => {
  const script = fs.readFileSync(path.join(REPO_ROOT, ".agents", "skills", "uzi-watcher", "scripts", "backup-runs.sh"), "utf8");

  it("the script reads uzi-recovery.<branch>.clone and uzi-attempts.<branch>.entry by these names", () => {
    assert.ok(script.includes('config --get "uzi-recovery.$branch.clone"'));
    assert.ok(script.includes('config --get-all "uzi-attempts.$branch.entry"'));
    for (const field of [".runId", ".clonePath", ".attemptId"]) assert.ok(script.includes(field), field);
  });

  it("the worker writes exactly those keys and JSON field names, and the script's own jq filter reads them", async () => {
    const s = await keyFixture("issue-3101", "agent/issue-3101");
    const attemptId = mintAttemptId(4);
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-3101", false, undefined, seedOpts({ attemptId }));
    await git.markRecoveryCapture(s.bare, seeded.path, s.branch, "run-3101", seeded.attemptId);
    const ledger = cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`);
    assert.deepEqual(ledger, [JSON.stringify({ attemptId, runId: "run-3101", clonePath: seeded.path, state: "live" })]);
    assert.deepEqual(Object.keys(JSON.parse(ledger[0]!) as object), ["attemptId", "runId", "clonePath", "state"]);
    const journal = cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`);
    assert.deepEqual(journal, [JSON.stringify({ runId: "run-3101", clonePath: seeded.path, attemptId })]);

    // Run the script's own ledger filter (last value per attemptId wins) over what we wrote.
    const m = /"\$JQ" -rRn --arg rid "\$rid" '([\s\S]*?)' 2>\/dev\/null\)"/.exec(script);
    assert.ok(m, "the script's ledger jq filter");
    await git.releaseAttemptInPlace(s.bare, seeded.path, s.branch, "run-3101");
    // The same jq the script runs: its `JQ="${UZI_JQ:-jq}"` default, read from the script itself.
    const jqBin = process.env.UZI_JQ ?? /^JQ="\$\{UZI_JQ:-([^}]+)\}"$/m.exec(script)?.[1];
    assert.ok(jqBin, "the script names its jq");
    const jq = spawnSync(jqBin, ["-rRn", "--arg", "rid", "run-3101", m[1]!], {
      input: `${cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`).join("\n")}\n`,
      encoding: "utf8",
    });
    if (jq.error) return; // no jq on this host: the string contract above still binds
    assert.equal(jq.status, 0, jq.stderr);
    assert.equal(jq.stdout.trim(), `${attemptId}\t${seeded.path}`);
    const states = cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`).map((v) => (JSON.parse(v) as { state: string }).state);
    assert.deepEqual(states, ["live", "abandoned"], "appended, the last value wins");
    assert.deepEqual(cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`), [], "the journal is cleared (an empty value)");
  });

  it("an older worker's journal without attemptId is still read", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const clone = await git.createOrAttachRunnerClone(bare, 3102, "run-old");
    cfg(bare, "uzi-recovery.agent/issue-3102.clone", JSON.stringify({ runId: "run-old", clonePath: clone.path }));
    await assert.rejects(git.createOrAttachRunnerClone(bare, 3102, "run-old"), PendingRecoveryCaptureError);
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, 3102, "run-old", false, undefined, seedOpts()),
      (err: unknown) => err instanceof PendingRecoveryCaptureError && err.clonePath === clone.path,
    );
  });
});

// ─── (d′) unit ─────────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-foreign: classifyOwnerClonePath (d′)", () => {
  it("accepts the owner's canonical path and its ledger-recorded attempt; rejects another key, a traversal, an unrecorded id", async () => {
    const s = await keyFixture("issue-1769", "agent/issue-1769");
    const id = idAt(3);
    const recorded = plantAttempt(s, id, "owner", "live");
    const cls = (p: string, owner = "owner") => git.classifyOwnerClonePath(s.bare, s.branch, s.key, owner, p);
    assert.equal(await cls(s.canonical), "canonical");
    assert.equal(await cls(recorded.dir), "attempt");
    assert.equal(await cls(recorded.dir, "someone-else"), undefined, "recorded for another run");
    assert.equal(await cls(path.join(s.parent, `issue-17.attempt-${id}`)), undefined, "another key");
    const traversal = [s.parent, "..", path.basename(s.parent), `issue-1769.attempt-${id}`].join(path.sep);
    assert.ok(fs.existsSync(traversal), "the traversal resolves to the recorded attempt");
    assert.equal(await cls(traversal), undefined, "a traversal");
    assert.equal(await cls(`${s.canonical}.attempt-${idAt(4)}`), undefined, "an id the ledger does not record");
    assert.equal(await cls(`${s.canonical}.attempt-${id}/sub`), undefined, "below an attempt");
  });
});

// ─── P-separator ───────────────────────────────────────────────────────────────────────────

describe("issue #1783 M2 P-separator: an attempt path is an ordinary working tree", () => {
  it("passes screenToolPath, git status under the pinned gitEnv, the Codex broker's path check and require.resolve", async () => {
    const s = await keyFixture("issue-3201", "agent/issue-3201");
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, "run-3201", false, undefined, seedOpts());
    const p = seeded.path;
    assert.match(path.basename(p), /^issue-3201\.attempt-/);

    // guardrails
    assert.equal(screenToolPath("src/x.ts", p, p).denied, false);
    assert.equal(screenToolPath(path.join(p, "lib", "y.ts"), p, p).denied, false);
    assert.equal(screenToolPath(path.join(p, ".git", "config"), p, p).denied, true, ".git stays denied");
    assert.equal(screenToolPath(`${s.canonical}/x`, p, p).denied, true, "the canonical sibling is outside");

    // git status, pinned env
    const st = spawnSync("git", ["-C", p, "status", "--porcelain"], { env: { ...gitEnv(), GIT_CONFIG_GLOBAL: "/dev/null" }, encoding: "utf8" });
    assert.equal(st.status, 0, st.stderr);
    assert.notEqual(await git.worktreeStatus(p), null);

    // the Codex broker's cwd and write path checks
    const spawnCalls: SpawnCommandOptions[] = [];
    const fileops: FileopRequest[] = [];
    const broker = new CodexCallbackBroker({
      registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
      spawnCommand: async (_argv, opts): Promise<SpawnCommandResult> => {
        spawnCalls.push(opts);
        return { code: 0, stdout: "", stderr: "" };
      },
      fileop: {
        op: async (req: FileopRequest): Promise<FileopResponse> => {
          fileops.push(req);
          return { ok: true, size: 1 } as FileopResponse;
        },
      } as never,
      worktreePath: p,
      grants: { role: "coder", phase: "implement", allowedTools: new Set(["Bash", "apply_patch", "Read"]), allowedSkills: new Set(), isRoot: true },
      delegate: async () => ({ ok: true, output: {} }),
      allowedRoles: new Set(["coder"]),
    });
    fs.mkdirSync(path.join(p, "sub"), { recursive: true });
    const sh = await broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c1" }, "Bash", { command: "pwd", cwd: path.join(p, "sub") }, "root");
    assert.equal(sh.ok, true, JSON.stringify(sh));
    assert.equal(spawnCalls[0]?.cwd, path.join(p, "sub"));
    const wr = await broker.handleToolCall({ threadId: "t", turnId: "u", callId: "c2" }, "Write", { path: path.join(p, "src", "n.ts"), content: "x" }, "root");
    assert.equal(wr.ok, true, JSON.stringify(wr));

    // node module resolution inside the attempt path
    fs.mkdirSync(path.join(p, "lib"), { recursive: true });
    fs.writeFileSync(path.join(p, "lib", "local-mod.js"), "module.exports = 1;\n");
    const out = execFileSync(process.execPath, ["-e", "process.stdout.write(require.resolve('./lib/local-mod'))"], { cwd: p, encoding: "utf8" });
    assert.equal(out, path.join(fs.realpathSync(p), "lib", "local-mod.js"));
  });
});
