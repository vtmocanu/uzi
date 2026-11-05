import { afterEach, beforeEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { TestContext } from "node:test";
import { deflateSync } from "node:zlib";
import { createHash, randomBytes } from "node:crypto";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-content-roots";
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


function gitIn(args: string[]): string {
  return execFileSync("git", ["-C", bare, ...args], { env: ENV, encoding: "utf8" }).trim();
}
function journal(): string { return gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`]); }

beforeEach(async () => {
  fx = makeFixture();
  fs.writeFileSync(path.join(fx.originPath, "base"), "abcdefgh");
  fs.writeFileSync(path.join(fx.originPath, "target"), "abcdWXYZ");
  fs.writeFileSync(path.join(fx.originPath, "long-base"), longBase);
  command(fx.originPath, "add", "base", "target", "long-base");
  command(fx.originPath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "published delta objects");
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
  // Contract double counts real Git participation; descriptor ownership is tested separately.
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


// Invoke the registered automatic proof under its production native/limit scopes.
// The discard core intentionally refuses owed consumers before reaching this proof.
async function observation(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function hash(data: Buffer): Buffer { return createHash("sha1").update(data).digest(); }
function oid(text: string): string { return hash(Buffer.from("blob " + Buffer.byteLength(text) + "\0" + text)).toString("hex"); }
function integer(n: number): Buffer {
  const out: number[] = [];
  do { const b = n % 128; n = Math.floor(n / 128); out.push(b | (n ? 128 : 0)); } while (n);
  return Buffer.from(out);
}
function entry(type: number, body: Buffer, reference = Buffer.alloc(0), declared = body.length): Buffer {
  const out = [(type << 4) | (declared & 15)];
  let n = Math.floor(declared / 16);
  while (n) { out[out.length - 1]! |= 128; out.push(n % 128); n = Math.floor(n / 128); }
  return Buffer.concat([Buffer.from(out), reference, deflateSync(body)]);
}
function crc(data: Buffer): number {
  let value = 0xffffffff;
  for (const b of data) {
    value ^= b;
    for (let j = 0; j < 8; j++) value = (value >>> 1) ^ ((value & 1) ? 0xedb88320 : 0);
  }
  return (value ^ 0xffffffff) >>> 0;
}
function pair(records: { id: string; data: Buffer }[], cwd = clonePath, offsets?: number[]): string {
  const header = Buffer.alloc(12); header.write("PACK"); header.writeUInt32BE(2, 4); header.writeUInt32BE(records.length, 8);
  const body = Buffer.concat([header, ...records.map(r => r.data)]), checksum = hash(body);
  const pack = Buffer.concat([body, checksum]);
  let start = 12;
  const rows = records.map((r, i) => {
    const row = { ...r, start: offsets?.[i] ?? start }; start += r.data.length; return row;
  }).sort((a, b) => a.id.localeCompare(b.id));
  const idx = Buffer.alloc(8 + 1024 + records.length * 28 + 40);
  idx.writeUInt32BE(0xff744f63); idx.writeUInt32BE(2, 4);
  for (let i = 0; i < 256; i++) idx.writeUInt32BE(rows.filter(r => parseInt(r.id.slice(0, 2), 16) <= i).length, 8 + 4 * i);
  rows.forEach((r, i) => {
    Buffer.from(r.id, "hex").copy(idx, 1032 + 20 * i);
    idx.writeUInt32BE(crc(r.data), 1032 + 20 * rows.length + 4 * i);
    idx.writeUInt32BE(r.start, 1032 + 24 * rows.length + 4 * i);
  });
  checksum.copy(idx, idx.length - 40); hash(idx.subarray(0, -20)).copy(idx, idx.length - 20);
  const dir = path.join(cwd, cwd === bare ? "objects/pack" : ".git/objects/pack");
  fs.mkdirSync(dir, { recursive: true });
  const name = path.join(dir, "pack-" + checksum.toString("hex"));
  fs.writeFileSync(name + ".pack", pack); fs.writeFileSync(name + ".idx", idx);
  return name;
}
const base = "abcdefgh", target = "abcdWXYZ";
const longBase = Buffer.concat([Buffer.from("abcd"), Buffer.from(Array.from({ length: 256 }, (_, i) => i))]);
const program = Buffer.from([8, 8, 0x90, 4, 4, 87, 88, 89, 90]);
function verifierSpy(t: TestContext, delegate = false): () => number {
  const original = git["runGitWithEnv"].bind(git); let attempts = 0;
  t.mock.method(git as unknown as { runGitWithEnv: typeof original }, "runGitWithEnv", async (...args: Parameters<typeof original>) => {
    if (args[1].includes("verify-pack")) {
      attempts++;
      if (!delegate) throw new Error("verifier must not receive hazardous fixture");
    }
    return original(...args);
  });
  return () => attempts;
}
for (const format of ["OFS", "REF", "forwardREF"] as const) {
  it("accepts a real verified published " + format + " delta", async t => {
    const full = { id: oid(base), data: entry(3, Buffer.from(base)) };
    const delta = { id: oid(target), data: entry(format === "OFS" ? 6 : 7, program,
      format === "OFS" ? Buffer.from([full.data.length]) : Buffer.from(oid(base), "hex")) };
    const packed = pair(format === "forwardREF" ? [delta, full] : [full, delta]);
    assert.equal((fs.readFileSync(packed + ".pack")[12 + (format === "forwardREF" ? 0 : full.data.length)]! >> 4) & 7,
      format === "OFS" ? 6 : 7);
    const attempts = verifierSpy(t, true);
    assert.equal(await observation(), true);
    assert.ok(attempts() > 0);
  });
}
// Each damage case must be refused by the check it names, not by an earlier one.
const REASONS = {
  "compressed-bomb": /pack entry size limit/,
  "lying-inflated-bomb": /ERR_BUFFER_TOO_LARGE|Cannot create a Buffer larger/i,
  "lying-size": /pack compressed span or size/,
  "trailing": /pack compressed span or size/,
  "result-bomb": /delta logical byte limit/,
  "lying-result": /delta generated size mismatch/,
  "zero-opcode": /invalid delta insert/,
  "copy-outside": /delta copy outside base/,
  "insert-truncated": /invalid delta insert/,
  "duplicate-offset": /invalid pack offsets/,
  "missing-offset": /invalid pack offsets/,
  "interior-OFS": /OFS base is not an earlier record/,
  "self-REF": /cyclic pack delta graph/,
  "two-REF": /cyclic pack delta graph/,
  "missing-REF": /missing same-pack REF base/,
  "base-length": /delta base length mismatch/,
  "physical-bomb": /object metadata byte limit/,
  "reconstructed-bomb": /delta logical byte limit/,
  "outside-offset": /invalid pack offsets/,
  "truncated-zlib": /unexpected end of file/i,
  "overlong-integer": /delta integer overflow/,
  "overlong-header": /pack entry size integer overflow/,
  "short-delta": /short delta program/,
} as const;
for (const damage of Object.keys(REASONS) as (keyof typeof REASONS)[]) {
  it("rejects " + damage + " before any native verifier and preserves core evidence", async t => {
    let full = { id: oid(base), data: entry(3, Buffer.from(base)) };
    let delta = { id: oid(target), data: entry(7, program, Buffer.from(oid(base), "hex")) };
    let offsets: number[] | undefined;
    if (damage === "compressed-bomb" || damage === "lying-inflated-bomb") {
      const bomb = Buffer.alloc(16 * 1024 * 1024, 65);
      full = { id: oid("bomb"), data: entry(3, bomb, Buffer.alloc(0), damage === "compressed-bomb" ? bomb.length : 8) };
    }
    if (damage === "lying-size") full.data = entry(3, Buffer.from(base), Buffer.alloc(0), 7);
    if (damage === "trailing") full.data = Buffer.concat([full.data, Buffer.from([1])]);
    if (damage === "result-bomb") delta.data = entry(7, Buffer.concat([integer(8), integer(16 * 1024 * 1024), Buffer.from([0x90, 8])]), Buffer.from(oid(base), "hex"));
    if (damage === "lying-result") delta.data = entry(7, Buffer.from([8, 7, ...program.subarray(2)]), Buffer.from(oid(base), "hex"));
    if (damage === "zero-opcode") delta.data = entry(7, Buffer.from([8, 8, 0, 0]), Buffer.from(oid(base), "hex"));
    if (damage === "copy-outside") delta.data = entry(7, Buffer.from([8, 8, 0x91, 7, 8]), Buffer.from(oid(base), "hex"));
    if (damage === "insert-truncated") delta.data = entry(7, Buffer.from([8, 8, 8, 65]), Buffer.from(oid(base), "hex"));
    if (damage === "duplicate-offset") offsets = [12, 12];
    if (damage === "missing-offset") offsets = [13, 12 + full.data.length];
    if (damage === "interior-OFS") delta.data = entry(6, program, Buffer.from([full.data.length - 1]));
    if (damage === "self-REF") delta.data = entry(7, program, Buffer.from(oid(target), "hex"));
    if (damage === "two-REF") full.data = entry(7, program, Buffer.from(oid(target), "hex"));
    if (damage === "missing-REF") delta.data = entry(7, program, Buffer.from("f".repeat(40), "hex"));
    if (damage === "base-length") delta.data = entry(7, Buffer.from([9, ...program.subarray(1)]), Buffer.from(oid(base), "hex"));
    if (damage === "physical-bomb") full.data = entry(3, randomBytes(8 * 1024 * 1024));
    if (damage === "reconstructed-bomb") {
      // The base fits the budget; the delta's declared 16 MiB reconstruction is what refuses.
      full.data = entry(3, Buffer.alloc(8000, 65));
      delta.data = entry(7, Buffer.concat([integer(8000), integer(16 * 1024 * 1024), Buffer.from([0xb0, 0x40, 0x1f])]),
        Buffer.from(oid(base), "hex"));
    }
    // A 31-bit offset past the pack end (the 0xffffffff form is refused earlier as an invalid large-offset slot).
    if (damage === "outside-offset") offsets = [12, 0x7fffffff];
    if (damage === "truncated-zlib") delta.data = delta.data.subarray(0, -1);
    if (damage === "overlong-integer") delta.data = entry(7, Buffer.alloc(20, 0xff), Buffer.from(oid(base), "hex"));
    if (damage === "overlong-header") full.data = Buffer.concat([
      Buffer.from([0xb0, ...Array<number>(11).fill(0x80), 0]), deflateSync(Buffer.alloc(0)),
    ]);
    if (damage === "short-delta") delta.data = entry(7, Buffer.from([8, 0]), Buffer.from(oid(base), "hex"));
    const packed = pair([full, delta], clonePath, offsets);
    const config = fs.readFileSync(path.join(bare, "config"));
    const ledger = await git["readAllAttemptLedgerRaw"](bare);
    const attempts = verifierSpy(t);
    await assert.rejects(observation({ maxMetadataBytes: 65536 }), error => {
      assert.equal((error as Error).constructor.name, "AutomaticCompletenessUnknown");
      const e = error as Error & { cause?: { code?: string; message?: string } };
      assert.match(`${e.message} ${e.cause?.code ?? ""} ${e.cause?.message ?? ""}`, REASONS[damage]);
      return true;
    });
    assert.equal(attempts(), 0);
    const result = await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
    assert.equal(result.kind, "unknown"); assert.equal(stopped, true);
    assert.ok(fs.existsSync(clonePath)); assert.ok(fs.existsSync(packed + ".pack"));
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
    assert.deepEqual(await git["readAllAttemptLedgerRaw"](bare), ledger);
    assert.equal(attempts(), 0);
  });
}
/** Counts the worktree stage's Git reads (`ls-files`); the proof has no `status` launch since Node hashes tracked content. */
function worktreeReadSpy(t: TestContext): () => number {
  const original = git["execEpochNative"].bind(git); let launches = 0;
  t.mock.method(git as unknown as { execEpochNative: typeof original }, "execEpochNative", async (...args: Parameters<typeof original>) => {
    if (args[1].includes("ls-files")) launches++;
    return original(...args);
  });
  return () => launches;
}
it("keeps the source before any worktree Git read when a tracked file exceeds the byte budget", async t => {
  // Sparse and index-matched: no object is written, so object prechecks pass, yet an unbounded
  // `git status` would hash it because the mtime differs from the index.
  const tracked = path.join(clonePath, "base");
  fs.truncateSync(tracked, 1024 * 1024);
  command(clonePath, "update-index", "--info-only", "base");
  const future = new Date(Date.now() + 3600_000);
  fs.utimesSync(tracked, future, future);
  const launches = worktreeReadSpy(t);
  assert.equal(await observation({ maxMetadataBytes: 65536 }), false);
  assert.equal(launches(), 0, "no worktree Git read may run on an over-budget tracked file");
  assert.equal(journal(), raw);
});
it("still proves a normal clean tree under the default budget, running its worktree Git read", async t => {
  const launches = worktreeReadSpy(t);
  assert.equal(await observation(), true);
  assert.equal(launches(), 1);
});
it("charges aggregate inflation across stores before verifying an earlier pair", async t => {
  // Leave room for fixture metadata: each object fits 256 KiB, their sum does not.
  pair([{ id: oid("source"), data: entry(3, Buffer.alloc(150000, 65)) }]);
  pair([{ id: oid("bare"), data: entry(3, Buffer.alloc(150000, 66)) }], bare);
  const attempts = verifierSpy(t);
  await assert.rejects(observation({ maxMetadataBytes: 256 * 1024 }), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown");
  assert.equal(attempts(), 0); assert.equal(journal(), raw);
});
it("charges instruction-generated delta content globally before any verifier", async t => {
  const copies = Buffer.alloc(37500);
  for (let i = 0; i < copies.length; i += 2) { copies[i] = 0x90; copies[i + 1] = 8; }
  const instructions = Buffer.concat([integer(8), integer(150000), copies]);
  for (const cwd of [clonePath, bare]) pair([
    { id: oid(base), data: entry(3, Buffer.from(base)) },
    { id: oid("expanded"), data: entry(7, instructions, Buffer.from(oid(base), "hex")) },
  ], cwd);
  const attempts = verifierSpy(t);
  await assert.rejects(observation({ maxMetadataBytes: 256 * 1024 }),
    error => (error as Error).constructor.name === "AutomaticCompletenessUnknown");
  assert.equal(attempts(), 0); assert.equal(journal(), raw);
});

// Offset12 is always the first physical record and remains 32-bit; force the
// later record into the supported anomalous 64-bit table (not every record).
for (const version of ["1", "2,12"] as const) {
  it("accepts verified index version " + version + " without assuming the v2 offset layout", async t => {
    const full = { id: oid(base), data: entry(3, Buffer.from(base)) };
    const packed = pair([full, { id: oid(target), data: entry(7, program, Buffer.from(oid(base), "hex")) }]);
    command(clonePath, "index-pack", "--index-version=" + version, "-o", packed + ".idx", packed + ".pack");
    const idx = fs.readFileSync(packed + ".idx");
    if (version === "1") assert.notEqual(idx.readUInt32BE(0), 0xff744f63);
    else {
      assert.equal(idx.readUInt32BE(0), 0xff744f63);
      assert.ok([0, 1].some(i => idx.readUInt32BE(1032 + 24 * 2 + 4 * i) >= 0x80000000));
    }
    const attempts = verifierSpy(t, true);
    assert.equal(await observation(), true);
    assert.ok(attempts() > 0);
  });
}
it("accepts a multi-byte OFS carry after a real published base", async t => {
  const id = hash(Buffer.concat([Buffer.from("blob " + longBase.length + "\0"), longBase])).toString("hex");
  const full = { id, data: entry(3, longBase) };
  const encoded: number[] = [full.data.length % 128];
  let distance = Math.floor(full.data.length / 128);
  while (distance) { distance--; encoded.unshift(128 | (distance % 128)); distance = Math.floor(distance / 128); }
  assert.ok(encoded.length > 1);
  const instructions = Buffer.concat([integer(longBase.length), program.subarray(1)]);
  pair([full, { id: oid(target), data: entry(6, instructions, Buffer.from(encoded)) }]);
  const attempts = verifierSpy(t, true);
  assert.equal(await observation(), true);
  assert.ok(attempts() > 0);
});

it("bounds actual zlib output allocation when a small pack header lies", async t => {
  pair([{ id: oid("lying allocation"), data: entry(3, Buffer.alloc(16 * 1024 * 1024, 65), Buffer.alloc(0), 8) }]);
  const attempts = verifierSpy(t);
  await assert.rejects(observation({ maxMetadataBytes: 65536 }), error => {
    assert.equal((error as Error).constructor.name, "AutomaticCompletenessUnknown");
    assert.equal((error as Error & { cause?: NodeJS.ErrnoException }).cause?.code, "ERR_BUFFER_TOO_LARGE",
      "the inflater must stop before producing the oversized buffer, rather than charge it afterward");
    return true;
  });
  assert.equal(attempts(), 0);
  assert.equal(journal(), raw);
});
