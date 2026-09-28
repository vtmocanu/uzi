import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawnSync, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { randomUUID } from "node:crypto";
import {
  compareAttemptIds,
  formatAttemptId,
  formatResidueName,
  isRetainedArtifactName,
  parseRetainedArtifactName,
} from "../src/attempt-path.js";
import { AttemptReleaseError, GitCache, PendingRecoveryCaptureError, gitEnv, type AttemptLedgerState, type AttemptSeedOptions } from "../src/git.js";
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
import { recordingLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";

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
function plantAttempt(s: Seeded, attemptId: string, runId: string, state: AttemptLedgerState): { dir: string; skills: string } {
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
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, opts);
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
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, opts);
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

  it("never counts an attempt live on this worker, a retired or reclaimed one, nor one without a ledger identity", async () => {
    const s = await keyFixture();
    const liveOnes = [0, 1, 2, 3, 4].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "live"));
    const reclaimed = [5, 6, 7, 8].map((i) => plantAttempt(s, idAt(i), `owner-${i}`, "reclaimed"));
    const retired = [9, 10, 11, 12].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "retired"));
    const unknown = `${s.canonical}.attempt-${idAt(20)}`;
    fs.mkdirSync(unknown);
    const live = new Set(liveOnes.map((p) => p.dir));
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts({ isLive: (p) => live.has(p) }));
    for (const p of [...liveOnes, ...reclaimed, ...retired]) assert.equal(fs.existsSync(p.dir), true, p.dir);
    assert.equal(fs.existsSync(unknown), true);
  });

  it("keeps at most 5 .uzi-residue-<key>.residue-<uuid> per key, oldest mtime first, only its own key's, only the pinned grammar", async () => {
    const s = await keyFixture();
    const t0 = Date.UTC(2026, 0, 1) / 1000;
    const residue = [0, 1, 2, 3, 4, 5, 6].map((i) => {
      const p = path.join(s.parent, formatResidueName(s.key, randomUUID()));
      fs.mkdirSync(p);
      fs.writeFileSync(path.join(p, "LEFTOVER"), "x");
      fs.utimesSync(p, t0 + i * 60, t0 + i * 60);
      return p;
    });
    const other = path.join(s.parent, formatResidueName(`${s.key}1`, randomUUID()));
    fs.mkdirSync(other);
    fs.utimesSync(other, t0 - 60, t0 - 60);
    // An older, non-pinned residue shape is never counted or deleted.
    const legacy = path.join(s.parent, `.uzi-residue-${s.key}.attempt-${idAt(1)}`);
    fs.mkdirSync(legacy);
    fs.utimesSync(legacy, t0 - 120, t0 - 120);
    const opts = seedOpts();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, opts);
    assert.deepEqual(residue.map((p) => fs.existsSync(p)), [false, false, true, true, true, true, true]);
    assert.equal(fs.existsSync(other), true, "issue-30011's residue is not issue-3001's");
    assert.equal(fs.existsSync(legacy), true, "a name outside the pinned grammar is never deleted");
    assert.deepEqual(opts.quiescentCalls, [[residue[0]], [residue[1]]], "each deletion is behind its own scoped scan");
  });

  it("residue: a journaled, live or not-quiescent residue is kept, and so is one the delete cannot remove (logged)", async () => {
    const s = await keyFixture();
    const t0 = Date.UTC(2026, 0, 1) / 1000;
    const residue = [0, 1, 2, 3, 4, 5, 6, 7, 8].map((i) => {
      const p = path.join(s.parent, formatResidueName(s.key, randomUUID()));
      fs.mkdirSync(p);
      fs.utimesSync(p, t0 + i * 60, t0 + i * 60);
      return p;
    });
    const [journaled, live, busy, locked] = residue;
    cfg(s.bare, "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "someone", clonePath: path.join(journaled!, "inner") }));
    const ro = path.join(locked!, "root-owned");
    fs.mkdirSync(ro);
    fs.writeFileSync(path.join(ro, "file"), "x");
    fs.chmodSync(ro, 0o500);
    unlock.push(ro);
    // Planting inside `locked` bumped its mtime: put it back in the oldest four.
    fs.utimesSync(locked!, t0 + 3 * 60, t0 + 3 * 60);
    await git.runnerCloneForBranch(
      s.bare,
      s.branch,
      s.key,
      noProofReseed,
      "run-new",
      false,
      undefined,
      seedOpts({ isLive: (p) => p === live, quiescent: async (paths) => paths[0] !== busy }),
    );
    assert.deepEqual(residue.map((p) => fs.existsSync(p)), [true, true, true, true, true, true, true, true, true]);
    assert.equal(fs.existsSync(path.join(ro, "file")), true);
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg?.includes("could not delete") && (l as { path?: string }).path === locked),
      "the failed deletion is logged",
    );
  });

  it("the residue grammar: formatResidueName round-trips through parseRetainedArtifactName; ambiguous and control-char names are refused", () => {
    const uuid = randomUUID();
    for (const key of ["issue-1", "fix.residue-x", "a.b", "task-6f1c2c1a-0000-4000-8000-000000000000", `k.residue-${randomUUID()}`]) {
      const name = formatResidueName(key, uuid);
      assert.equal(name, `.uzi-residue-${key}.residue-${uuid}`);
      assert.deepEqual(parseRetainedArtifactName(name), { kind: "residue", key, uuid }, name);
      assert.equal(isRetainedArtifactName(name), true);
    }
    const v4 = "0f8fad5b-d9cb-469f-a165-70867728950e";
    const refused = [
      ".uzi-residue-",
      ".uzi-residue-issue-1",
      `.uzi-residue-issue-1.residue-${v4.toUpperCase()}`,
      ".uzi-residue-issue-1.residue-0f8fad5b-d9cb-169f-a165-70867728950e", // v1
      ".uzi-residue-issue-1.residue-0f8fad5b-d9cb-469f-c165-70867728950e", // bad variant
      `.uzi-residue-issue-1.residue-${v4}x`,
      `.uzi-residue-issue-1.residue-${v4}\n`,
      `.uzi-residue-.residue-${v4}`,
      `.uzi-residue-..residue-${v4}`,
      `.uzi-residue-...residue-${v4}`,
      `.uzi-residue-a/b.residue-${v4}`,
      `.uzi-residue-iss\nue-1.residue-${v4}`,
      `.uzi-residue-iss\u0007ue-1.residue-${v4}`,
      `.uzi-residue-iss\u009bue-1.residue-${v4}`,
      `.uzi-residue-iss\u2028ue-1.residue-${v4}`,
      `.uzi-residue-issue-1.attempt-${idAt(1)}.residue-${v4}`,
      `.uzi-residue-issue-1.attempt-${idAt(1)}`,
    ];
    for (const name of refused) {
      assert.equal(parseRetainedArtifactName(name), undefined, JSON.stringify(name));
      assert.equal(isRetainedArtifactName(name), true, "still never a clone");
    }
    for (const [key, u] of [
      ["", v4],
      [".", v4],
      ["..", v4],
      [".hidden", v4],
      ["a/b", v4],
      ["a\nb", v4],
      ["a\u0000b", v4],
      [`issue-1.attempt-${idAt(1)}`, v4],
      ["issue-1", v4.toUpperCase()],
      ["issue-1", "not-a-uuid"],
      ["issue-1", `${v4}\n`],
    ] as const) {
      assert.throws(() => formatResidueName(key, u), JSON.stringify([key, u]));
    }
  });

  it("isRetainedArtifactName: skills and residue siblings, never a clone", () => {
    assert.equal(isRetainedArtifactName(".uzi-skills-issue-1.attempt-x"), true);
    assert.equal(isRetainedArtifactName(".uzi-residue-issue-1"), true);
    assert.equal(isRetainedArtifactName("issue-1"), false);
    assert.equal(isRetainedArtifactName(`issue-1.attempt-${idAt(1)}`), false);
    assert.deepEqual(parseRetainedArtifactName(`.uzi-skills-issue-1.attempt-${idAt(1)}`), { kind: "skills", cloneBasename: `issue-1.attempt-${idAt(1)}` });
    assert.equal(parseRetainedArtifactName(".uzi-residue-"), undefined);
    assert.equal(parseRetainedArtifactName(".uzi-skills-"), undefined);
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
      git.runnerCloneForBranch(s.bare, "agent/x", ".uzi-skills-evil", noProofReseed, "r", false, undefined, seedOpts()),
      /unsafe runner clone key/,
    );
  });
});

// ─── review round (N1–N5) ──────────────────────────────────────────────────────────────────

describe("issue #1783 M2 review: retention deletions are re-validated and run as the runner uid", () => {
  it("under the uid split every deletion is the setpriv-wrapped /bin/rm -rf -- <target>, never an in-process rm", async () => {
    const calls: Array<{ command: string; args: string[] }> = [];
    const rec = recordingLogger();
    git = new GitCache(
      fx.dataDir,
      rec.logger,
      undefined,
      testGitCacheOptions({
        gitleaksBin: defaultGitleaksShim(),
        retentionDelete: {
          split: true,
          run: async (command, args) => {
            calls.push({ command, args });
            fs.rmSync(args.at(-1)!, { recursive: true, force: true }); // stands in for the runner-uid rm
          },
        },
      }),
    );
    const s = await keyFixture();
    const planted = [0, 1, 2, 3].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "abandoned"));
    const residue = [0, 1, 2, 3, 4, 5].map((i) => {
      const p = path.join(s.parent, formatResidueName(s.key, randomUUID()));
      fs.mkdirSync(p);
      fs.utimesSync(p, 1_700_000_000 + i, 1_700_000_000 + i);
      return p;
    });
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(calls.map((c) => c.args.at(-1)), [planted[0]!.skills, planted[0]!.dir, residue[0]], "the skills sibling first (NB1)");
    for (const c of calls) {
      assert.equal(c.command, "/bin/setpriv", "wrapped by runnerCommand");
      assert.deepEqual(c.args.slice(-4), ["/bin/rm", "-rf", "--", c.args.at(-1)], "the fixed rm argv, target last");
      assert.ok(c.args.includes("--reuid"), "as the runner uid");
    }
    assert.equal(ledgerState(s, idAt(0)), "retired");
  });

  it("refuses a symlinked attempt; unlinks a symlinked or plain-file residue entry without following it (targets untouched)", async () => {
    const s = await keyFixture();
    const victim = path.join(fx.dataDir, "victim");
    fs.mkdirSync(victim);
    fs.writeFileSync(path.join(victim, "PRECIOUS"), "x");
    const oldest = `${s.canonical}.attempt-${idAt(0)}`;
    fs.symlinkSync(victim, oldest);
    cfg(s.bare, "--add", `uzi-attempts.${s.branch}.entry`, JSON.stringify({ attemptId: idAt(0), runId: "run-0", clonePath: oldest, state: "abandoned" }));
    for (const i of [1, 2, 3]) plantAttempt(s, idAt(i), `run-${i}`, "abandoned");
    const residueLink = path.join(s.parent, formatResidueName(s.key, randomUUID()));
    fs.symlinkSync(victim, residueLink);
    fs.lutimesSync(residueLink, 1_600_000_000, 1_600_000_000);
    // issue #1783 M3: the canonical free quarantines a planted symlink or file AS residue.
    const residueFile = path.join(s.parent, formatResidueName(s.key, randomUUID()));
    fs.writeFileSync(residueFile, "planted\n");
    fs.utimesSync(residueFile, 1_600_000_001, 1_600_000_001);
    for (let i = 0; i < 5; i++) fs.mkdirSync(path.join(s.parent, formatResidueName(s.key, randomUUID())));
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.equal(fs.lstatSync(oldest).isSymbolicLink(), true, "the symlinked attempt is kept");
    assert.equal(fs.lstatSync(residueLink, { throwIfNoEntry: false }), undefined, "the symlinked residue entry is unlinked, never kept forever");
    assert.equal(fs.existsSync(residueFile), false, "the plain-file residue entry is unlinked");
    assert.deepEqual(fs.readdirSync(victim), ["PRECIOUS"], "the link's target directory is untouched");
    assert.equal(fs.readFileSync(path.join(victim, "PRECIOUS"), "utf8"), "x");
    assert.equal(ledgerState(s, idAt(0)), "abandoned", "not marked retired");
    const refusals = lines.filter((l) => (l as { msg?: string }).msg?.includes("refusing to delete")).map((l) => (l as { path?: string }).path);
    assert.deepEqual(refusals, [oldest], "only the symlinked attempt is refused");
  });
});

describe("issue #1783 final: a timed-out retention rm is waited out, and an unsettled one stops the sweep", () => {
  /** A stand-in rm child: the first spawn never exits on its own (the busybox rm still walking);
   *  later ones delete their target and exit 0. */
  function fakeRm(events: string[], onKill: (child: ChildProcess) => boolean) {
    let first: ChildProcess | undefined;
    return {
      split: true,
      timeoutMs: 20,
      closeWaitMs: 5_000,
      spawn: (_command: string, args: string[]): ChildProcess => {
        const target = args.at(-1)!;
        events.push(`spawn ${path.basename(target)}`);
        const c = new EventEmitter() as unknown as ChildProcess;
        Object.assign(c, { pid: 920000 + events.length, stderr: null });
        if (first === undefined) {
          first = c;
        } else {
          setImmediate(() => {
            fs.rmSync(target, { recursive: true, force: true });
            c.emit("close", 0, null);
          });
        }
        return c;
      },
      kill: () => {
        events.push("kill");
        return onKill(first!);
      },
    };
  }

  async function sweepFixture() {
    const s = await keyFixture();
    const planted = [0, 1, 2, 3].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "abandoned"));
    const residue = [0, 1, 2, 3, 4, 5].map((i) => {
      const p = path.join(s.parent, formatResidueName(s.key, randomUUID()));
      fs.mkdirSync(p);
      fs.utimesSync(p, 1_700_000_000 + i, 1_700_000_000 + i);
      return p;
    });
    return { s, planted, residue };
  }

  function withRm(retentionDelete: ReturnType<typeof fakeRm>): void {
    const rec = recordingLogger();
    lines = rec.lines;
    git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim(), retentionDelete }));
  }

  it("the group kill fails and 'close' never arrives: the sweep stops, no further delete starts, the artifact is kept", async () => {
    const events: string[] = [];
    const rm = fakeRm(events, () => false);
    rm.closeWaitMs = 50;
    withRm(rm);
    const { s, planted, residue } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(events, [`spawn ${path.basename(planted[0]!.skills)}`, "kill"], "no delete after the unsettled one");
    assert.equal(fs.existsSync(planted[0]!.skills), true);
    assert.equal(fs.existsSync(planted[0]!.dir), true);
    assert.equal(fs.existsSync(residue[0]!), true);
    assert.equal(ledgerState(s, idAt(0)), "abandoned");
    const warn = lines.find((l) => (l as { msg?: string }).msg?.includes("retention sweep failed"));
    assert.match(String((warn as { error?: string } | undefined)?.error), /may still be running: group kill of pid \d+ failed after 20ms; no close within 50ms/);
  });

  it("N2: the group kill fails but 'close' arrives within the wait: the sweep goes on", async () => {
    const events: string[] = [];
    withRm(
      fakeRm(events, (child) => {
        setImmediate(() => {
          events.push("close");
          child.emit("close", 0, null);
        });
        return false;
      }),
    );
    const { s, planted, residue } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(events.slice(0, 4), [`spawn ${path.basename(planted[0]!.skills)}`, "kill", "close", `spawn ${path.basename(residue[0]!)}`]);
    assert.equal(fs.existsSync(residue[0]!), false, "the sweep went on after the ordinary timeout");
  });

  it("the kill lands but 'close' never arrives: the sweep stops, no further delete starts", async () => {
    const events: string[] = [];
    const rm = fakeRm(events, () => true);
    rm.closeWaitMs = 50;
    withRm(rm);
    const { s, planted, residue } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(events, [`spawn ${path.basename(planted[0]!.skills)}`, "kill"]);
    assert.equal(fs.existsSync(residue[0]!), true);
    const warn = lines.find((l) => (l as { msg?: string }).msg?.includes("retention sweep failed"));
    assert.match(String((warn as { error?: string } | undefined)?.error), /no exit within 50ms/);
    assert.doesNotMatch(String((warn as { error?: string } | undefined)?.error), /group kill of pid .* failed/);
  });

  it("'close' arrives after the kill: the next delete starts only after it, and the timed-out artifact is kept", async () => {
    const events: string[] = [];
    withRm(
      fakeRm(events, (child) => {
        setTimeout(() => {
          events.push("close");
          child.emit("close", null, "SIGKILL");
        }, 300);
        return true;
      }),
    );
    const { s, planted, residue } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(events, [`spawn ${path.basename(planted[0]!.skills)}`, "kill", "close", `spawn ${path.basename(residue[0]!)}`]);
    assert.equal(fs.existsSync(planted[0]!.skills), true, "the timed-out skills sibling is kept");
    assert.equal(fs.existsSync(planted[0]!.dir), true, "and its attempt with it (skills-first)");
    assert.equal(fs.existsSync(residue[0]!), false, "the sweep went on once the rm was gone");
  });

  it("NB-2: a later sweep never starts a second rm on a path an unsettled rm may still walk; it keeps and logs it", async () => {
    const events: string[] = [];
    const rm = fakeRm(events, () => false);
    rm.closeWaitMs = 50;
    withRm(rm);
    const { s, planted, residue } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    const skillsSpawn = `spawn ${path.basename(planted[0]!.skills)}`;
    assert.deepEqual(events, [skillsSpawn, "kill"], "the first sweep stopped on the unsettled rm");
    // A second seed of the same key: its sweep reaches the same skills sibling first.
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-newer", false, undefined, seedOpts());
    assert.equal(events.filter((e) => e === skillsSpawn).length, 1, "no second rm on the path the first rm may still walk");
    assert.equal(fs.existsSync(planted[0]!.skills), true, "the unsettled target is kept");
    assert.equal(fs.existsSync(planted[0]!.dir), true, "and its attempt with it (skills-first)");
    const refusal = lines.find(
      (l) => (l as { msg?: string }).msg?.includes("refusing to delete") && (l as { path?: string }).path === planted[0]!.skills,
    ) as { reason?: string } | undefined;
    assert.match(String(refusal?.reason), /runner-uid delete of this path .* may still be running/);
    assert.equal(fs.existsSync(residue[0]!), false, "the sweep went on past the kept target");
  });

  it("NB-3: the worker log records a path entering the unsettled record and leaving it on 'close'", async () => {
    const events: string[] = [];
    let late: ChildProcess | undefined;
    const rm = fakeRm(events, (child) => {
      late = child;
      return true;
    });
    rm.closeWaitMs = 50;
    withRm(rm);
    const { s, planted } = await sweepFixture();
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    const entered = lines.find((l) => (l as { msg?: string }).msg?.includes("recorded as unsettled")) as
      | { level?: string; path?: string; pending?: number }
      | undefined;
    assert.equal(entered?.level, "warn");
    assert.equal(entered?.path, path.resolve(planted[0]!.skills));
    assert.equal(entered?.pending, 1);
    assert.equal(lines.some((l) => (l as { msg?: string }).msg?.includes("left the unsettled record")), false, "nothing left yet");
    late!.emit("close", null, "SIGKILL");
    const left = lines.find((l) => (l as { msg?: string }).msg?.includes("left the unsettled record")) as
      | { level?: string; path?: string; pending?: number }
      | undefined;
    assert.equal(left?.level, "info");
    assert.equal(left?.path, path.resolve(planted[0]!.skills));
    assert.equal(left?.pending, 0);
  });
});

describe("issue #1783 final: a directory swapped in for a residue link fails the unlink (EISDIR on Linux, EPERM on macOS) and is kept", () => {
  it("the re-validation lstat reports a symlink while the entry is a real directory: the directory and its contents survive", async () => {
    const s = await keyFixture();
    const swapped = path.join(s.parent, formatResidueName(s.key, randomUUID()));
    fs.mkdirSync(path.join(swapped, "inner"), { recursive: true });
    fs.writeFileSync(path.join(swapped, "inner", "KEEP"), "k");
    fs.utimesSync(swapped, 1_600_000_000, 1_600_000_000);
    for (let i = 0; i < 5; i++) fs.mkdirSync(path.join(s.parent, formatResidueName(s.key, randomUUID())));
    const lied: string[] = [];
    const rec = recordingLogger();
    lines = rec.lines;
    git = new GitCache(
      fx.dataDir,
      rec.logger,
      undefined,
      testGitCacheOptions({
        gitleaksBin: defaultGitleaksShim(),
        retentionDelete: {
          lstat: async (p) => {
            const st = await fs.promises.lstat(p);
            if (p !== swapped) return st;
            lied.push(p);
            // A symlink at the check; a real directory by the time of the delete.
            return Object.assign(Object.create(st) as fs.Stats, { isSymbolicLink: () => true, isDirectory: () => false });
          },
        },
      }),
    );
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.deepEqual(lied, [swapped], "the sweep's re-validation saw the swapped entry");
    assert.equal(fs.lstatSync(swapped).isDirectory(), true, "the directory is kept");
    assert.equal(fs.readFileSync(path.join(swapped, "inner", "KEEP"), "utf8"), "k", "its contents survive");
    const refusal = lines.find((l) => (l as { path?: string; msg?: string }).path === swapped && (l as { msg?: string }).msg?.includes("refusing to delete"));
    assert.match(String((refusal as { reason?: string } | undefined)?.reason), process.platform === "linux" ? /EISDIR/ : /EISDIR|EPERM/);
  });
});

describe("issue #1783 M2 review: attempt-journal guards (A′ id mismatch, release re-validation)", () => {
  it("A′: a journal whose attemptId disagrees with its path's id is a path mismatch, even for the same run", async () => {
    const s = await keyFixture("issue-3301", "agent/issue-3301");
    const dir = plantAttempt(s, idAt(1), "run-a", "live").dir;
    cfg(s.bare, `uzi-recovery.${s.branch}.clone`, JSON.stringify({ runId: "run-a", clonePath: dir, attemptId: idAt(2) }));
    await assert.rejects(
      git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-a", false, undefined, seedOpts()),
      (err: unknown) => (err as Error).name === "CapturePathMismatchError",
    );
    assert.equal(fs.readFileSync(path.join(dir, "WORK.txt"), "utf8"), idAt(1));
  });

  it("releaseAttemptInPlace refuses unless the journal names exactly (runId, clonePath): nothing written", async () => {
    const s = await keyFixture("issue-3302", "agent/issue-3302");
    const attemptId = mintAttemptId(2);
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-b", false, undefined, seedOpts({ attemptId }));
    await git.markRecoveryCapture(s.bare, seeded.path, s.branch, "run-b", attemptId);
    const journal = cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`);
    const ledger = cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`);
    for (const [clonePath, runId] of [
      [seeded.path, "someone-else"],
      [`${s.canonical}.attempt-${idAt(9)}`, "run-b"],
    ] as const) {
      for (const state of ["abandoned", "reclaimed"] as const) {
        await assert.rejects(git.releaseAttemptInPlace(s.bare, clonePath, s.branch, runId, state), (e: unknown) => (e as Error).name === "CapturePathMismatchError");
      }
    }
    assert.deepEqual(cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`), journal, "the journal is kept");
    assert.deepEqual(cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`), ledger, "no ledger value appended");
  });

  it("a journal-clear failure after the ledger append is typed stage journal, and the journal is kept", async () => {
    const s = await keyFixture("issue-3303", "agent/issue-3303");
    const attemptId = mintAttemptId(2);
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-c", false, undefined, seedOpts({ attemptId }));
    await git.markRecoveryCapture(s.bare, seeded.path, s.branch, "run-c", attemptId);
    const seam = git as unknown as { runGit: (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string> };
    const real = seam.runGit.bind(git);
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "config" && args.at(-2) === `uzi-recovery.${s.branch}.clone` && args.at(-1) === "") throw new Error("injected");
      return real(cwd, args, ...rest);
    };
    await assert.rejects(git.releaseAttemptInPlace(s.bare, seeded.path, s.branch, "run-c", "abandoned"), (e: unknown) => (e as AttemptReleaseError).stage === "journal");
    assert.equal((JSON.parse(cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`).at(-1)!) as { clonePath: string }).clonePath, seeded.path);
    assert.equal(ledgerState(s, attemptId), "abandoned", "the ledger went first");
  });
});

describe("issue #1783 M2 review: the ledger stays bounded (N4)", () => {
  it("each seed compacts to the last value per attemptId and drops entries whose path is gone", async () => {
    const s = await keyFixture();
    const key = `uzi-attempts.${s.branch}.entry`;
    const kept = plantAttempt(s, idAt(1), "run-1", "live");
    cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(1), runId: "run-1", clonePath: kept.dir, state: "abandoned" }));
    const goneRetired = `${s.canonical}.attempt-${idAt(2)}`;
    cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(2), runId: "run-2", clonePath: goneRetired, state: "live" }));
    cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(2), runId: "run-2", clonePath: goneRetired, state: "retired" }));
    const presentRetired = plantAttempt(s, idAt(3), "run-3", "retired");
    const goneAbandoned = `${s.canonical}.attempt-${idAt(4)}`;
    cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(4), runId: "run-4", clonePath: goneAbandoned, state: "abandoned" }));
    cfg(s.bare, "--add", key, "not json");
    let nonLive: string[] = [];
    const attemptId = mintAttemptId(5);
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts({
      attemptId,
      beforeSeed: async (paths) => {
        nonLive = paths;
      },
    }));
    assert.deepEqual(
      cfgAll(s.bare, key).map((v) => JSON.parse(v) as { attemptId: string; state: string }).map((e) => `${e.attemptId}:${e.state}`),
      [`${idAt(1)}:abandoned`, `${idAt(3)}:retired`, `${attemptId}:live`],
    );
    assert.deepEqual(nonLive, [kept.dir, presentRetired.dir].sort(), "only paths that still exist are swept");
    assert.ok(fs.existsSync(seeded.path));
  });
});

type CompactSeam = { compactAttemptLedger: (bare: string, branch: string) => Promise<void> };
type RunGitSeam = { runGit: (cwd: string | undefined, args: string[], ...rest: unknown[]) => Promise<string> };

describe("issue #1783 M2 final review (NB2): a gone entry of any state is dead, except reclaimed-with-custody and journaled", () => {
  it("drops gone live/abandoned/retired entries and a gone reclaimed one with neither journal nor custody; keeps the rest", async () => {
    const s = await keyFixture();
    const key = `uzi-attempts.${s.branch}.entry`;
    const add = (i: number, runId: string, state: AttemptLedgerState): string => {
      const clonePath = `${s.canonical}.attempt-${idAt(i)}`;
      cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(i), runId, clonePath, state }));
      return clonePath;
    };
    add(0, "run-0", "live"); // gone, unjournaled → dropped
    add(1, "run-1", "abandoned"); // gone → dropped
    add(2, "run-2", "retired"); // gone → dropped
    add(3, "owner-3", "reclaimed"); // gone, no journal, no custody → dropped
    add(4, "owner-4", "reclaimed"); // gone, custody under recovery/ → kept
    add(5, "owner-5", "reclaimed"); // gone, custody under recovery-settlement/ → kept
    const journaledReclaimed = add(6, "owner-6", "reclaimed"); // gone, journaled → kept
    const journaledLive = add(7, "owner-7", "live"); // gone, a journal names it → kept
    const present = plantAttempt(s, idAt(8), "run-8", "live"); // present → kept
    fs.mkdirSync(path.join(fx.dataDir, "recovery", "owner-4"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", "owner-4", "capture.json"), "{}");
    fs.mkdirSync(path.join(fx.dataDir, "recovery-settlement", "owner-5"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery-settlement", "owner-5", "settle.json"), "{}");
    cfg(s.bare, "uzi-recovery.agent/other-6.clone", JSON.stringify({ runId: "owner-6", clonePath: journaledReclaimed }));
    cfg(s.bare, "uzi-recovery.agent/other-7.clone", JSON.stringify({ runId: "owner-7", clonePath: journaledLive }));
    await (git as unknown as CompactSeam).compactAttemptLedger(s.bare, s.branch);
    assert.deepEqual(
      cfgAll(s.bare, key).map((v) => (JSON.parse(v) as { attemptId: string }).attemptId),
      [idAt(4), idAt(5), idAt(6), idAt(7), idAt(8)],
    );
    assert.ok(fs.existsSync(present.dir));
  });
});

describe("issue #1783 M2 final review (NB3): the compaction rewrite is atomic under git's config.lock", () => {
  function plantCompactable(s: Seeded): { key: string; before: string[] } {
    const key = `uzi-attempts.${s.branch}.entry`;
    // A foreign owner's live entry a journal names (the value a partial rewrite could lose) plus
    // other survivors, and duplicate history that makes the compaction rewrite.
    const owner = plantAttempt(s, idAt(1), "owner-1", "live");
    cfg(s.bare, "uzi-recovery.agent/owner.clone", JSON.stringify({ runId: "owner-1", clonePath: owner.dir }));
    plantAttempt(s, idAt(2), "run-2", "abandoned");
    plantAttempt(s, idAt(3), "run-3", "reclaimed");
    cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(2), runId: "run-2", clonePath: `${s.canonical}.attempt-${idAt(2)}`, state: "abandoned" }));
    cfg(s.bare, "--add", key, "not json");
    return { key, before: cfgAll(s.bare, key) };
  }

  it("a failure part-way through the rewrite leaves every ledger value exactly as it was", async () => {
    const s = await keyFixture();
    const { key, before } = plantCompactable(s);
    const seam = git as unknown as RunGitSeam;
    const real = seam.runGit.bind(git);
    let replaced = false;
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "config" && args.includes("--replace-all")) replaced = true;
      else if (replaced && args[0] === "config" && args.includes("--add") && args.includes(key)) throw new Error("injected crash mid-compaction");
      return real(cwd, args, ...rest);
    };
    await (git as unknown as CompactSeam).compactAttemptLedger(s.bare, s.branch);
    seam.runGit = real;
    assert.ok(replaced, "the rewrite started");
    assert.deepEqual(cfgAll(s.bare, key), before, "the journaled owner's live entry (and every other value) survives");
    assert.equal(fs.existsSync(path.join(s.bare, "config.lock")), false, "no lock left behind");
    assert.deepEqual(fs.readdirSync(s.bare).filter((n) => n.startsWith("config.uzi-compact-")), [], "no temp file left behind");
  });

  it("while another git holds config.lock the compaction aborts: the ledger and the foreign lock are untouched", async () => {
    const s = await keyFixture();
    const { key, before } = plantCompactable(s);
    const lock = path.join(s.bare, "config.lock");
    fs.writeFileSync(lock, "[foreign]\n\tx = 1\n");
    await (git as unknown as CompactSeam).compactAttemptLedger(s.bare, s.branch);
    assert.equal(fs.readFileSync(lock, "utf8"), "[foreign]\n\tx = 1\n", "the foreign lock is not ours to touch");
    fs.rmSync(lock);
    assert.deepEqual(cfgAll(s.bare, key), before);
    assert.ok(lines.some((l) => (l as { msg?: string }).msg === "attempt ledger compaction failed; the ledger is left as it was"));
  });

  it("a successful compaction lands the whole set in one rewrite, keeps the rest of the config and leaves no lock or temp file", async () => {
    const s = await keyFixture();
    const { key } = plantCompactable(s);
    const otherKeys = cfg(s.bare, "--list").split("\n").filter((l) => l !== "" && !l.startsWith(`${key}=`));
    await (git as unknown as CompactSeam).compactAttemptLedger(s.bare, s.branch);
    assert.deepEqual(
      cfgAll(s.bare, key).map((v) => `${(JSON.parse(v) as { attemptId: string }).attemptId}:${(JSON.parse(v) as { state: string }).state}`),
      [`${idAt(1)}:live`, `${idAt(3)}:reclaimed`, `${idAt(2)}:abandoned`],
    );
    assert.deepEqual(cfg(s.bare, "--list").split("\n").filter((l) => l !== "" && !l.startsWith(`${key}=`)), otherKeys);
    assert.equal(fs.existsSync(path.join(s.bare, "config.lock")), false);
    assert.deepEqual(fs.readdirSync(s.bare).filter((n) => n.startsWith("config.uzi-compact-")), []);
  });
});

describe("issue #1783 M2 final review (NB1): no orphaned .uzi-skills sibling", () => {
  function failingOn(fail: (target: string) => boolean): void {
    const rec = recordingLogger();
    lines = rec.lines;
    git = new GitCache(
      fx.dataDir,
      rec.logger,
      undefined,
      testGitCacheOptions({
        gitleaksBin: defaultGitleaksShim(),
        retentionDelete: {
          split: true,
          run: async (_command, args) => {
            const target = args.at(-1)!;
            if (fail(target)) throw new Error("injected rm failure");
            fs.rmSync(target, { recursive: true, force: true });
          },
        },
      }),
    );
  }

  it("a failed skills delete keeps the attempt dir (the skills dir is never left without it)", async () => {
    failingOn((t) => path.basename(t).startsWith(".uzi-skills-"));
    const s = await keyFixture();
    const planted = [0, 1, 2, 3].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "abandoned"));
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.equal(fs.existsSync(planted[0]!.skills), true);
    assert.equal(fs.existsSync(planted[0]!.dir), true, "the attempt dir is kept with its skills sibling");
    assert.equal(ledgerState(s, idAt(0)), "abandoned");
  });

  it("a failed attempt delete after a successful skills delete orphans nothing, and the next seed retries it", async () => {
    let failAttempt = true;
    failingOn((t) => failAttempt && !path.basename(t).startsWith("."));
    const s = await keyFixture();
    const planted = [0, 1, 2, 3].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "abandoned"));
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts());
    assert.equal(fs.existsSync(planted[0]!.skills), false, "the skills sibling went first");
    assert.equal(fs.existsSync(planted[0]!.dir), true, "the attempt is kept");
    assert.equal(ledgerState(s, idAt(0)), "abandoned", "still disposable, not retired");
    const orphans = fs.readdirSync(s.parent).filter((n) => n.startsWith(".uzi-skills-") && !fs.existsSync(path.join(s.parent, n.slice(".uzi-skills-".length))));
    assert.deepEqual(orphans, [], "no skills dir without its attempt");
    failAttempt = false;
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new-2", false, undefined, seedOpts({ attemptId: mintAttemptId(10) }));
    assert.equal(fs.existsSync(planted[0]!.dir), false, "retried and deleted");
    assert.equal(ledgerState(s, idAt(0)), "retired");
  });

  it("sweeps an existing orphan skills dir (attempt gone, entry absent/abandoned/retired), never a reclaimed/live one's, a journaled one or another key's", async () => {
    const s = await keyFixture();
    const key = `uzi-attempts.${s.branch}.entry`;
    const skillsOf = (i: number, k = s.key): string => {
      const p = path.join(s.parent, `.uzi-skills-${k}.attempt-${idAt(i)}`);
      fs.mkdirSync(p);
      fs.writeFileSync(path.join(p, "plugin.json"), "{}");
      return p;
    };
    const entry = (i: number, runId: string, state: AttemptLedgerState): void => {
      cfg(s.bare, "--add", key, JSON.stringify({ attemptId: idAt(i), runId, clonePath: `${s.canonical}.attempt-${idAt(i)}`, state }));
    };
    const noEntry = skillsOf(0);
    const retiredOne = skillsOf(1);
    entry(1, "run-1", "retired");
    const reclaimedOne = skillsOf(2);
    entry(2, "owner-2", "reclaimed");
    fs.mkdirSync(path.join(fx.dataDir, "recovery", "owner-2"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", "owner-2", "c.json"), "{}"); // keeps the gone reclaimed entry in the ledger
    const liveOne = skillsOf(3);
    entry(3, "owner-3", "live");
    cfg(s.bare, "uzi-recovery.agent/other-3.clone", JSON.stringify({ runId: "owner-3", clonePath: `${s.canonical}.attempt-${idAt(3)}` }));
    const journaledOne = skillsOf(4);
    cfg(s.bare, "uzi-recovery.agent/other-4.clone", JSON.stringify({ runId: "x", clonePath: `${s.canonical}.attempt-${idAt(4)}` }));
    const otherKey = skillsOf(5, `${s.key}1`);
    const busy = skillsOf(6);
    const withAttempt = plantAttempt(s, idAt(7), "run-7", "abandoned");
    const scanned: string[][] = [];
    const opts = seedOpts({
      quiescent: async (paths) => {
        scanned.push(paths);
        return paths[0] !== busy;
      },
    });
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, opts);
    assert.equal(fs.existsSync(noEntry), false, "no ledger entry: swept");
    assert.equal(fs.existsSync(retiredOne), false, "retired: swept");
    for (const p of [reclaimedOne, liveOne, journaledOne, otherKey, busy, withAttempt.skills]) assert.equal(fs.existsSync(p), true, p);
    assert.ok(scanned.some((c) => c.length === 1 && c[0] === noEntry), "behind a scoped scan");
  });
});

describe("issue #1783 M2 final review (NB4): deleteRetainedArtifact's re-validation guards", () => {
  type DeleteSeam = { deleteRetainedArtifact: (parent: string, target: string, isExpected: (n: string) => boolean) => Promise<boolean> };
  const del = (parent: string, target: string, isExpected: (n: string) => boolean = () => true): Promise<boolean> =>
    (git as unknown as DeleteSeam).deleteRetainedArtifact(parent, target, isExpected);
  const refusedWith = (reason: string): boolean =>
    lines.some((l) => (l as { msg?: string }).msg?.includes("refusing to delete") && (l as { reason?: string }).reason === reason);
  const realDir = (p: string): string => {
    fs.mkdirSync(p, { recursive: true });
    fs.writeFileSync(path.join(p, "PRECIOUS"), "x");
    return p;
  };

  it("refuses a parent that is not directly under runnerRoot, and a target that is not directly under its parent", async () => {
    const s = await keyFixture();
    const root = path.dirname(s.parent);
    // A parent outside runnerRoot (a sibling tree of the data dir).
    const outsideParent = realDir(path.join(fx.dataDir, "not-runner", "repo"));
    const outside = realDir(path.join(outsideParent, "victim"));
    assert.equal(await del(outsideParent, outside), false);
    assert.equal(fs.existsSync(path.join(outside, "PRECIOUS")), true);
    // runnerRoot itself as the parent (one level too shallow).
    const shallow = realDir(path.join(root, "shallow-victim"));
    assert.equal(await del(root, shallow), false);
    assert.equal(fs.existsSync(path.join(shallow, "PRECIOUS")), true);
    // A target two levels below the parent.
    const deep = realDir(path.join(s.parent, "sub", "deep"));
    assert.equal(await del(s.parent, deep), false);
    assert.equal(fs.existsSync(path.join(deep, "PRECIOUS")), true);
    assert.ok(refusedWith("not a direct child of a runner repo dir"));
  });

  it("refuses a basename that is not the expected artifact of this key", async () => {
    const s = await keyFixture();
    const target = realDir(path.join(s.parent, `${s.key}.attempt-${idAt(1)}`));
    assert.equal(await del(s.parent, target, (n) => n === "something-else"), false);
    assert.equal(fs.existsSync(path.join(target, "PRECIOUS")), true);
    assert.ok(refusedWith("not the expected retained artifact of this key"));
    // Control: the same target, expected, is deleted.
    assert.equal(await del(s.parent, target, (n) => n === path.basename(target)), true);
    assert.equal(fs.existsSync(target), false);
  });

  it("refuses when the runner repo dir (the parent) is a symlink, even to a real directory", async () => {
    const s = await keyFixture();
    const root = path.dirname(s.parent);
    const elsewhere = realDir(path.join(fx.dataDir, "elsewhere-repo"));
    const victim = realDir(path.join(elsewhere, "victim"));
    const linkedParent = path.join(root, "linked-repo");
    fs.symlinkSync(elsewhere, linkedParent);
    assert.equal(await del(linkedParent, path.join(linkedParent, "victim")), false);
    assert.equal(fs.existsSync(path.join(victim, "PRECIOUS")), true);
    assert.ok(refusedWith("the runner repo dir is not a real directory"));
  });
});

describe("issue #1783 M2 review: crash-window orphans (N5)", () => {
  it("a live entry that is not live here, unjournaled and without custody is disposable under the cap; the others are not", async () => {
    const s = await keyFixture();
    const orphans = [0, 1, 2, 3].map((i) => plantAttempt(s, idAt(i), `run-${i}`, "live"));
    const journaledLive = plantAttempt(s, idAt(4), "run-4", "live");
    const custodyLive = plantAttempt(s, idAt(5), "run-5", "live");
    const liveHere = plantAttempt(s, idAt(6), "run-6", "live");
    cfg(s.bare, "uzi-recovery.agent/elsewhere.clone", JSON.stringify({ runId: "run-4", clonePath: journaledLive.dir }));
    fs.mkdirSync(path.join(fx.dataDir, "recovery", "run-5"), { recursive: true });
    fs.writeFileSync(path.join(fx.dataDir, "recovery", "run-5", "capture.json"), "{}");
    await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-new", false, undefined, seedOpts({ isLive: (p) => p === liveHere.dir }));
    assert.equal(fs.existsSync(orphans[0]!.dir), false, "the oldest crash-window orphan is deleted");
    assert.equal(fs.existsSync(orphans[0]!.skills), false);
    assert.equal(ledgerState(s, idAt(0)), "retired");
    for (const p of [...orphans.slice(1), journaledLive, custodyLive, liveHere]) assert.equal(fs.existsSync(p.dir), true, p.dir);
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
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-3101", false, undefined, seedOpts({ attemptId }));
    await git.markRecoveryCapture(s.bare, seeded.path, s.branch, "run-3101", seeded.attemptId);
    const ledger = cfgAll(s.bare, `uzi-attempts.${s.branch}.entry`);
    assert.deepEqual(ledger, [JSON.stringify({ attemptId, runId: "run-3101", clonePath: seeded.path, state: "live" })]);
    assert.deepEqual(Object.keys(JSON.parse(ledger[0]!) as object), ["attemptId", "runId", "clonePath", "state"]);
    const journal = cfgAll(s.bare, `uzi-recovery.${s.branch}.clone`);
    assert.deepEqual(journal, [JSON.stringify({ runId: "run-3101", clonePath: seeded.path, attemptId })]);

    // Run the script's own ledger filter (last value per attemptId wins) over what we wrote.
    const m = /"\$JQ" -rRn --arg rid "\$rid" '([\s\S]*?)' 2>\/dev\/null\)"/.exec(script);
    assert.ok(m, "the script's ledger jq filter");
    await git.releaseAttemptInPlace(s.bare, seeded.path, s.branch, "run-3101", "abandoned");
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
    const clone = await git.createOrAttachRunnerClone(bare, 3102, noProofReseed, "run-old");
    cfg(bare, "uzi-recovery.agent/issue-3102.clone", JSON.stringify({ runId: "run-old", clonePath: clone.path }));
    await assert.rejects(git.createOrAttachRunnerClone(bare, 3102, noProofReseed, "run-old"), PendingRecoveryCaptureError);
    await assert.rejects(
      git.createOrAttachRunnerClone(bare, 3102, noProofReseed, "run-old", false, undefined, seedOpts()),
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
    const seeded = await git.runnerCloneForBranch(s.bare, s.branch, s.key, noProofReseed, "run-3201", false, undefined, seedOpts());
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
