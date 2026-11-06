// Contract tests for the read-only packed-store completeness precondition that gates
// automatic epoch discard (issue #3431, owner decision, question seq 9857). Rows R1..R7
// of that contract are named in each describe block.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync, spawn, type SpawnOptions } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-pack-contract";
const BRANCH = "agent/issue-3431";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
let fx: Fixture;
let git: GitCache;
let bare: string;
let clonePath: string;
let raw: string;
let options: RetainedDiscardOptions;
let input: EpochDiscardProofOptions;
let scope: PodEpochReclaimScope;
let pending: number;
let stopped: boolean;
let live: boolean;
// A runner clone borrows the bare through objects/info/alternates; tests extend that file.
let seededAlternates: string;

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function journal(): string { return command(bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`); }

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-3431", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  raw = journal();
  const selector = { runId: RUN, generation: 1, attempt };
  const holds = { state: "read" as const, holds: [] };
  const lifecycle = { state: "terminal" as const, status: "completed" as const };
  const inspection = await git.inspectRetainedSources({ selector, barePath: bare, holds, lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  pending = 0; stopped = false; live = true;
  seededAlternates = fs.readFileSync(path.join(clonePath, ".git/objects/info/alternates"), "utf8");
  assert.ok(seededAlternates.endsWith("\n"));
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
  options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => true };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => fx?.cleanup());

async function observation(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
async function unknown(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {},
  reason?: RegExp): Promise<void> {
  await assert.rejects(observation(limits), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown" &&
    (!reason || reason.test((error as Error).message)));
}

/** The object-store snapshot alone ends in a settled keep for exactly this limit. */
async function exceeded(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number }, reason: RegExp): Promise<void> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  await assert.rejects(git["epochRetained"].run(git["epochLimits"](limits), () => git["withEpochNativeReads"](proof, () => {
    const automatic = { roots: [], checkLive: () => git["checkEpochDiscardLive"](state) };
    return git["automaticObserveStores"](bare, clonePath, automatic as never);
  })), error => (error as Error).constructor.name === "AutomaticBoundExceeded" && reason.test((error as Error).message));
}

const sourceObjects = () => path.join(clonePath, ".git", "objects");
const bareObjects = () => path.join(bare, "objects");
function packObjects(cwd: string, ids: string[]): string {
  const dir = path.join(cwd === bare ? bareObjects() : sourceObjects(), "pack");
  const hash = execFileSync("git", ["-C", cwd, "pack-objects", path.join(dir, "pack")],
    { env: ENV, encoding: "utf8", input: ids.join("\n") + "\n" }).trim();
  return path.join(dir, "pack-" + hash);
}
function allIds(): string[] {
  return command(clonePath, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
}
/** Pack every object into both stores: indexed and covered by the published bare. */
function packBoth(): { source: string; bare: string } {
  const ids = allIds();
  return { source: packObjects(clonePath, ids), bare: packObjects(bare, ids) };
}
function hashObject(text: string): string {
  return execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
    { env: ENV, encoding: "utf8", input: text }).trim();
}
function unpublishedCommit(): string {
  return command(clonePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unpublished indexed commit");
}

// Records the argv of every real native launch while delegating to the real spawn.
function spyNative(): { argv: string[][]; stop(): void } {
  const argv: string[][] = [];
  const internal = git as unknown as { epochNativeTestDependencies?: unknown };
  internal.epochNativeTestDependencies = {
    spawn: ((cmd: string, args: string[], opts: SpawnOptions) => {
      argv.push([cmd, ...args]);
      return spawn(cmd, args, opts);
    }) as typeof spawn,
    alive: (pid: number) => {
      try { process.kill(-pid, 0); return true; }
      catch (error) { return (error as NodeJS.ErrnoException).code === "EPERM"; }
    },
    kill: (pid: number) => {
      try { process.kill(-pid, "SIGKILL"); } catch (error) { if ((error as NodeJS.ErrnoException).code !== "ESRCH") throw error; }
    },
    joinMs: 2_000,
  };
  return { argv, stop() { internal.epochNativeTestDependencies = undefined; } };
}
const SUBCOMMANDS = ["verify-pack", "cat-file", "rev-list", "for-each-ref"];
const subcommand = (argv: string[]) => argv.find(arg => SUBCOMMANDS.includes(arg));

// Content hash of every file, directory and link under the roots: names, types, sizes,
// mtimes and bytes. Any store mutation by the observation changes it.
function storeHash(...roots: string[]): string {
  const hash = createHash("sha256");
  const walk = (p: string) => {
    const st = fs.lstatSync(p, { bigint: true });
    hash.update(`${p}|${st.mode}|${st.size}|${st.mtimeNs}|${st.ctimeNs}\n`);
    if (st.isDirectory()) for (const name of fs.readdirSync(p).sort()) walk(path.join(p, name));
    else if (st.isSymbolicLink()) hash.update(fs.readlinkSync(p));
    else hash.update(fs.readFileSync(p));
  };
  for (const root of roots) walk(root);
  return hash.digest("hex");
}

// A managed alternate store (a bare repo beside the worker bare) wired from `from`.
function alternate(name: string): string {
  const alt = path.join(path.dirname(bare), name);
  command(bare, "init", "--bare", alt); fs.chmodSync(alt, 0o700);
  return alt;
}
// Keeps the seeded borrow of the bare and appends the given alternate objects directories.
function setAlternates(extra: string[], terminated = true): void {
  const text = seededAlternates + extra.map(dir => dir + "\n").join("");
  fs.writeFileSync(path.join(sourceObjects(), "info/alternates"), terminated ? text : text.slice(0, -1));
}
function link(alt: string): void { setAlternates([path.join(alt, "objects")]); }

describe("R1 coverage spans clone, bare and a bounded alternates closure", () => {
  it("accepts a complete managed alternate closure of the source", async () => {
    const alt = alternate("closure-alt.git");
    link(alt);
    assert.equal(await observation(), true, "control: complete managed alternate closure");
  });
  it("refuses a missing alternate directory", async () => {
    const alt = alternate("closure-alt.git");
    link(alt);
    assert.equal(await observation(), true);
    fs.rmSync(alt, { recursive: true });
    await unknown();
    assert.equal(journal(), raw);
  });
  it("refuses an unreadable alternate directory", async t => {
    const alt = alternate("closure-alt.git");
    link(alt);
    assert.equal(await observation(), true);
    const root = path.join(alt, "objects");
    const opendir = fsp.opendir.bind(fsp);
    t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (args[0] === root) throw Object.assign(new Error("owned unreadable seam"), { code: "EACCES" });
      return opendir(...args);
    });
    await unknown();
  });
  it("refuses an unterminated alternates file", async () => {
    const alt = alternate("closure-alt.git");
    link(alt);
    assert.equal(await observation(), true);
    setAlternates([path.join(alt, "objects")], false);
    await unknown({}, /unterminated alternates/);
  });
  it("refuses an http-alternates entry", async () => {
    assert.equal(await observation(), true);
    fs.writeFileSync(path.join(sourceObjects(), "info/http-alternates"), "http://127.0.0.1:1/objects\n");
    await unknown({}, /unsupported object info entry/);
  });
  it("refuses an alternate outside the managed repositories", async () => {
    const outside = path.join(fx.dataDir, "outside.git");
    command(bare, "init", "--bare", outside);
    setAlternates([path.join(outside, "objects")]);
    await unknown();
  });
  it("refuses an alternates chain beyond the store bound and accepts a shorter one", async () => {
    const template = alternate("chain-template.git");
    const names = Array.from({ length: 48 }, (_, i) => `chain-${i}.git`);
    for (const name of names) fs.cpSync(template, path.join(path.dirname(bare), name), { recursive: true });
    const chain = (length: number) => {
      for (let i = 0; i < names.length; i++) {
        const file = path.join(path.dirname(bare), names[i]!, "objects/info/alternates");
        if (i + 1 < length) fs.writeFileSync(file, path.join(path.dirname(bare), names[i + 1]!, "objects") + "\n");
        else fs.rmSync(file, { force: true });
      }
      link(path.join(path.dirname(bare), names[0]!));
    };
    const limits = { maxMetadataRecords: 40 };
    chain(3);
    assert.equal(await observation(limits), true, "control: a short chain is within the bound");
    chain(names.length);
    // Past the store bound is a positively identified overflow: a settled keep, not UNKNOWN.
    await exceeded(limits, /object store limit/);
    assert.equal(await observation(limits), false);
    assert.equal(journal(), raw);
  });
});

describe("R2 every pack has a matching valid idx verified by verify-pack on the idx", () => {
  it("verifies each pair with verify-pack -- <idx> and never --stat-only", async t => {
    const packs = packBoth();
    const spy = spyNative(); t.after(() => spy.stop());
    assert.equal(await observation(), true);
    const verifies = spy.argv.filter(argv => argv.includes("verify-pack"));
    assert.equal(verifies.length, 2);
    const idxs = new Set([packs.source + ".idx", packs.bare + ".idx"]);
    for (const argv of verifies) {
      const at = argv.indexOf("verify-pack");
      assert.equal(argv[at + 1], "--");
      assert.ok(idxs.delete(argv[at + 2]!), "one verification per idx path");
      assert.equal(argv.length, at + 3, "no further verify-pack options");
    }
    assert.equal(idxs.size, 0);
    assert.deepEqual(spy.argv.filter(argv => argv.some(arg => /stat-only/.test(arg))), []);
  });
  it("refuses a pack without an idx and never verifies it", async t => {
    const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
    fs.unlinkSync(packed + ".idx");
    const spy = spyNative(); t.after(() => spy.stop());
    await unknown();
    assert.deepEqual(spy.argv.filter(argv => argv.includes("verify-pack")), []);
  });
  for (const damage of ["trailer", "pack-checksum-resealed"] as const) {
    it("refuses a structurally valid idx with a bad " + damage + " through verify-pack", async t => {
      const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
      assert.equal(await observation(), true, "control: the intact pair is accepted");
      fs.chmodSync(packed + ".idx", 0o600);
      const idx = fs.readFileSync(packed + ".idx");
      if (damage === "trailer") idx[idx.length - 1]! ^= 1;
      else {
        // The idx names a different pack checksum but its own trailer is recomputed.
        idx[idx.length - 21]! ^= 1;
        createHash("sha1").update(idx.subarray(0, -20)).digest().copy(idx, idx.length - 20);
      }
      fs.writeFileSync(packed + ".idx", idx);
      assert.equal(idx.readUInt32BE(0), 0xff744f63, "the header stays valid");
      const spy = spyNative(); t.after(() => spy.stop());
      await unknown();
      const verifies = spy.argv.filter(argv => argv.includes("verify-pack"));
      assert.equal(verifies.length, 1, "the corrupt idx reached the native verifier");
      assert.equal(verifies[0]!.at(-1), packed + ".idx");
      assert.deepEqual(spy.argv.filter(argv => argv.some(arg => /stat-only/.test(arg))), []);
      assert.equal(journal(), raw);
      assert.ok(fs.existsSync(packed + ".pack"));
    });
  }
});

describe("R3 pack sidecars are recognized and temporary or unsupported entries refuse", () => {
  for (const ext of ["keep", "promisor", "rev", "bitmap", "mtimes"]) {
    it("accepts a ." + ext + " sidecar beside a valid pair", async () => {
      const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
      assert.equal(await observation(), true, "control: the pair alone");
      if (!fs.existsSync(packed + "." + ext)) fs.writeFileSync(packed + "." + ext, "");
      assert.equal(await observation(), true);
    });
  }
  const names: Record<string, (packed: string) => string> = {
    "tmp_pack_XXXX": packed => path.join(path.dirname(packed), "tmp_pack_aB3dEf"),
    ".tmp-123-pack-<oid>": packed => path.join(path.dirname(packed), ".tmp-123-" + path.basename(packed) + ".pack"),
    "pack-<oid>.idx.tmp": packed => packed + ".idx.tmp",
    "pack-<oid>.pack.tmp": packed => packed + ".pack.tmp",
    "unknown suffix": packed => packed + ".unknown",
    "uppercase oid": packed => path.join(path.dirname(packed), path.basename(packed).toUpperCase() + ".keep"),
    "short oid": packed => path.join(path.dirname(packed), "pack-abc.keep"),
  };
  for (const [label, name] of Object.entries(names)) {
    it("refuses " + label + " beside a valid pair", async () => {
      const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
      assert.equal(await observation(), true, "control: the pair alone");
      fs.writeFileSync(name(packed), "");
      await unknown();
      assert.equal(journal(), raw);
    });
  }
  it("refuses a sidecar whose pack and idx are absent", async () => {
    packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
    fs.writeFileSync(path.join(sourceObjects(), "pack", "pack-" + "c".repeat(40) + ".keep"), "");
    await unknown();
  });
});

describe("R4 acceleration metadata is bypassed", () => {
  // Real bitmap, reverse index and commit-graph in both stores.
  function accelerate(): string[] {
    const files: string[] = [];
    for (const cwd of [clonePath, bare]) {
      // -n: no update-server-info, whose `.git/info/refs` the clone shape stage does not admit.
      command(cwd, "repack", "-a", "-d", "-b", "-n");
      command(cwd, "commit-graph", "write", "--reachable");
      const pack = path.join(cwd === bare ? bareObjects() : sourceObjects(), "pack");
      for (const name of fs.readdirSync(pack)) if (/\.(bitmap|rev)$/.test(name)) files.push(path.join(pack, name));
      files.push(path.join(cwd === bare ? bareObjects() : sourceObjects(), "info/commit-graph"));
    }
    assert.ok(files.some(file => file.endsWith(".bitmap")), "fixture wrote a real bitmap");
    assert.ok(files.some(file => file.endsWith("commit-graph")), "fixture wrote a real commit-graph");
    return files;
  }
  const damages: Record<string, (file: string) => void> = {
    garbage: file => fs.writeFileSync(file, "not acceleration metadata at all"),
    truncated: file => fs.truncateSync(file, 4),
    "flipped bytes": file => {
      const data = fs.readFileSync(file);
      for (let i = 0; i < data.length; i += 7) data[i]! ^= 0xff;
      fs.writeFileSync(file, data);
    },
  };
  for (const [label, damage] of Object.entries(damages)) {
    it("does not change an eligible result when bitmap, rev and commit-graph are " + label, async () => {
      const files = accelerate();
      assert.equal(await observation(), true, "control with intact acceleration metadata");
      for (const file of files) { fs.chmodSync(file, 0o600); damage(file); }
      assert.equal(await observation(), true);
    });
    it("does not release an indexed unpublished commit when acceleration metadata is " + label, async () => {
      const files = accelerate();
      packObjects(clonePath, [unpublishedCommit()]);
      assert.equal(await observation(), false, "control with intact acceleration metadata");
      for (const file of files) { fs.chmodSync(file, 0o600); damage(file); }
      assert.equal(await observation(), false);
    });
  }
  it("pins every native read with acceleration disabled", async t => {
    accelerate();
    const spy = spyNative(); t.after(() => spy.stop());
    assert.equal(await observation(), true);
    const seen = new Set<string>();
    for (const argv of spy.argv) {
      const sub = subcommand(argv);
      if (!sub) continue;
      seen.add(sub);
      for (const pin of ["core.multiPackIndex=false", "core.commitGraph=false", "pack.readReverseIndex=false",
        "pack.writeReverseIndex=false", "pack.useBitmaps=false"]) {
        const at = argv.indexOf(pin);
        assert.ok(at > 0 && argv[at - 1] === "-c", `${sub} carries -c ${pin}`);
      }
      assert.ok(!argv.some(arg => /bitmap/i.test(arg) && !arg.startsWith("pack.useBitmaps=")),
        "no read asks for a bitmap");
    }
    for (const sub of SUBCOMMANDS) assert.ok(seen.has(sub), "observed " + sub);
  });
});

describe("R5 topology is fingerprinted before and after and a change is UNKNOWN", () => {
  const mutations: Record<string, () => void> = {
    "a pack added to the source": () => { packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]); },
    "a pack added to the bare": () => { packObjects(bare, [command(clonePath, "rev-parse", "HEAD")]); },
    "a bitmap sidecar added": () => {
      for (const file of fs.readdirSync(path.join(sourceObjects(), "pack")).filter(name => name.endsWith(".pack")))
        fs.writeFileSync(path.join(sourceObjects(), "pack", file.replace(/\.pack$/, ".bitmap")), "late");
    },
    "a commit-graph added": () => { fs.writeFileSync(path.join(bareObjects(), "info/commit-graph"), "late"); },
    "a multi-pack-index added": () => { fs.writeFileSync(path.join(sourceObjects(), "pack/multi-pack-index"), "late"); },
  };
  for (const [label, mutate] of Object.entries(mutations)) {
    it("refuses when " + label + " between the snapshots", async t => {
      packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
      assert.equal(await observation(), true, "control without the change");
      const original = git["runGitAsRunner"].bind(git);
      let inventories = 0;
      t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner",
        async (...args: Parameters<typeof original>) => {
          if (args[1].includes("--batch-all-objects") && ++inventories === 2) mutate();
          return original(...args);
        });
      await unknown();
      assert.equal(inventories, 2);
      assert.equal(journal(), raw);
    });
  }
});

describe("R6 the observation is read-only", () => {
  const roots = (...extra: string[]) => [sourceObjects(), bareObjects(), ...extra];
  it("leaves clone, bare and alternate stores byte-identical on accept", async () => {
    const alt = alternate("readonly-alt.git");
    link(alt);
    packBoth();
    const before = storeHash(...roots(path.join(alt, "objects")));
    assert.equal(await observation(), true);
    assert.equal(storeHash(...roots(path.join(alt, "objects"))), before);
  });
  it("leaves the stores byte-identical on refuse", async () => {
    packObjects(clonePath, [unpublishedCommit()]);
    const before = storeHash(...roots());
    assert.equal(await observation(), false);
    assert.equal(storeHash(...roots()), before);
  });
  it("leaves the stores, journal, ledger and source unchanged on UNKNOWN", async () => {
    const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
    fs.unlinkSync(packed + ".idx");
    const before = storeHash(...roots());
    await unknown();
    assert.equal(storeHash(...roots()), before);
    const config = fs.readFileSync(path.join(bare, "config"));
    const ledger = await git["readAllAttemptLedgerRaw"](bare);
    const result = await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
    assert.equal(result.kind, "unknown");
    assert.ok(fs.existsSync(clonePath), "the source is kept");
    assert.equal(journal(), raw);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
    assert.deepEqual(await git["readAllAttemptLedgerRaw"](bare), ledger);
    assert.equal(storeHash(...roots()), before);
  });
});

describe("R7 matrix", () => {
  it("indexed covered packs are eligible", async () => {
    packBoth();
    assert.equal(await observation(), true);
  });
  for (const held of ["no ref", "a local ref"] as const) {
    it("an indexed unpublished commit not reachable from the preserved roots blocks with " + held, async () => {
      packBoth();
      assert.equal(await observation(), true, "control");
      const sha = unpublishedCommit();
      packObjects(clonePath, [sha]);
      if (held === "a local ref") command(clonePath, "update-ref", "refs/private/indexed", sha);
      assert.equal(await observation(), false);
      assert.equal(journal(), raw);
    });
  }
  it("an indexed unpublished blob blocks even when only the source holds it", async () => {
    packBoth();
    packObjects(clonePath, [hashObject("indexed unpublished blob")]);
    assert.equal(await observation(), false);
  });
});

describe("F2 required objects are the clone's own store, not the shared bare's", () => {
  it("keeps a published clone eligible when the shared bare holds an unrelated blob and another run's unpublished branch", async () => {
    assert.equal(await observation(), true, "control");
    const blob = execFileSync("git", ["-C", bare, "hash-object", "-w", "--stdin"],
      { env: ENV, encoding: "utf8", input: "another run's unrelated blob" }).trim();
    const commit = command(bare, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
      "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "another run's unpublished commit");
    command(bare, "update-ref", "refs/heads/agent/issue-9999", commit);
    assert.equal(command(bare, "cat-file", "-t", blob), "blob");
    assert.equal(await observation(), true);
  });
  it("keeps a commit that only the clone's own store holds and no root reaches", async () => {
    assert.equal(await observation(), true, "control");
    unpublishedCommit();
    hashObject("clone-only blob");
    assert.equal(await observation(), false);
  });
  it("keeps a clone-only object even when no ref of the clone names it", async () => {
    assert.equal(await observation(), true, "control");
    hashObject("clone-only blob, unreferenced");
    assert.equal(await observation(), false);
  });
});
