import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { GitCache, MemoryReceiptSourceError, type MemoryParkRecord } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

// issue #3639: discoverRetainedRecovery in expected-receipt mode is exact and strictly read-only. Real git and a real
// GitCache in temp dirs; every refusal is asserted to leave the journal, the bare config (attempt-mode keys, ledger,
// stopped markers) and the authority store byte-identical.

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const BRANCH = "agent/issue-3639";
const KEY = "issue-3639";
const RUN = "run-3639";
const UUID_A = "11111111-1111-4111-8111-111111111111";
const UUID_B = "22222222-2222-4222-8222-222222222222";
const UUID_C = "33333333-3333-4333-8333-333333333333";
const aid = (n: number) => formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, n.toString(16).padStart(16, "0"));

let fx: Fixture;
let cache: GitCache;
let bare: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const rawJournal = (): string => {
  try { return gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]); } catch { return ""; } // exit 1 = no such key
};
const parkBytes = (): string => JSON.stringify((JSON.parse(rawJournal()) as { memoryPark: unknown }).memoryPark);

function tree(dir: string): string {
  if (!fs.existsSync(dir)) return "(absent)";
  const out: string[] = [];
  const walk = (current: string): void => {
    for (const name of fs.readdirSync(current).sort()) {
      const full = path.join(current, name);
      if (fs.statSync(full).isDirectory()) walk(full);
      else out.push(`${path.relative(dir, full)}:${fs.readFileSync(full, "utf8")}`);
    }
  };
  walk(dir);
  return out.join("\n");
}
/** Everything a refused discovery must not touch. */
const snapshot = () => ({
  config: fs.readFileSync(path.join(bare, "config")), journal: rawJournal(), authority: tree(path.join(fx.dataDir, ".uzi-authority")),
});

const BINDING = { runId: RUN, claimGeneration: 7, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 3 };
const evidence = () => ({
  parkAck: { ...BINDING }, interventionIds: [] as string[], processJoin: "executor" as const, commands: "not_required" as const,
  steering: "joined" as const, auxiliaries: "none" as const, feedback: "drained" as const, sinkReceipts: "joined" as const,
  parkReap: "not_applicable" as const,
});
const settled = (clonePath: string, attemptId: string | null): MemoryParkRecord => ({
  version: 1, binding: BINDING, ack: "confirmed",
  origin: { flightId: UUID_A, attemptId, executorIncarnation: null, authorityIncarnation: UUID_B, sourceEpoch: UUID_C },
  settlement: { version: 1, confirmed: true, evidence: evidence(),
    sources: [{ path: clonePath, authorityKey: "a".repeat(64), attemptId, epoch: UUID_C, seq: 1 }] },
});

/** A real attempt clone whose active journal carries a settled record. */
async function attemptSource() {
  const attemptId = aid(5);
  const info = await cache.createOrAttachRunnerClone(bare, 3639, noProofReseed, RUN, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await cache.markRecoveryCapture(bare, info.path, BRANCH, RUN, attemptId);
  const source = { runId: RUN, clonePath: info.path, attemptId };
  assert.equal(await cache.recordMemoryPark(bare, BRANCH, source, settled(info.path, attemptId)), "settled");
  return { ...source, memoryPark: parkBytes() };
}
const discover = (expected?: { runId: string; clonePath: string; attemptId: string; memoryPark: string }) =>
  cache.discoverRetainedRecovery(fx.originPath, BRANCH, KEY, RUN, expected);
async function refused(expected: Parameters<typeof discover>[0], reason: MemoryReceiptSourceError["reason"]) {
  const before = snapshot();
  await assert.rejects(discover(expected), (error: unknown) => error instanceof MemoryReceiptSourceError && error.reason === reason);
  const after = snapshot();
  assert.deepEqual(after.config, before.config, "bare config (attempt mode, ledger, markers) unchanged");
  assert.equal(after.journal, before.journal, "journal bytes unchanged");
  assert.equal(after.authority, before.authority, "authority files unchanged");
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});
afterEach(() => fx.cleanup());

describe("discoverRetainedRecovery with an expected memory receipt", () => {
  it("returns the source for an exact match and writes nothing", async () => {
    const expected = await attemptSource();
    const before = snapshot();
    const found = await discover(expected);
    assert.equal(found?.barePath, bare);
    assert.equal(found?.journal.clonePath, expected.clonePath);
    assert.equal(found?.journal.attemptId, expected.attemptId);
    const after = snapshot();
    assert.deepEqual(after.config, before.config);
    assert.equal(after.authority, before.authority);
  });

  it("a missing bare still answers undefined before any lock", async () => {
    const expected = await attemptSource();
    assert.equal(await cache.discoverRetainedRecovery("file:///nonexistent/never-cloned.git", BRANCH, KEY, RUN, expected), undefined);
  });

  it("an absent journal throws absent", async () => {
    const expected = await attemptSource();
    gitIn(bare, ["config", "--unset-all", `uzi-recovery.${BRANCH}.clone`]);
    await refused(expected, "absent");
  });

  it("another run's journal throws foreign", async () => {
    const expected = await attemptSource();
    await refused({ ...expected, runId: "run-other" }, "foreign");
  });

  it("a changed clone path throws changed", async () => {
    const expected = await attemptSource();
    await refused({ ...expected, clonePath: `${expected.clonePath}-moved` }, "changed");
  });

  it("a changed attempt id throws changed", async () => {
    const expected = await attemptSource();
    await refused({ ...expected, attemptId: aid(6) }, "changed");
  });

  it("a changed or missing memory-park record throws changed", async () => {
    const expected = await attemptSource();
    await refused({ ...expected, memoryPark: expected.memoryPark.replace('"claimGeneration":7', '"claimGeneration":8') }, "changed");
    const journal = JSON.parse(rawJournal()) as Record<string, unknown>;
    delete journal.memoryPark;
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify(journal)]);
    await refused(expected, "changed");
  });

  describe("a stopped journal a plain discovery would heal", () => {
    const stop = async () => {
      const clonePath = (await cache.createOrAttachRunnerClone(bare, 3639, noProofReseed, RUN)).path;
      await cache.markRecoveryCapture(bare, clonePath, BRANCH, RUN);
      assert.equal(await cache.recordMemoryPark(bare, BRANCH, { runId: RUN, clonePath }, settled(clonePath, null)), "settled");
      gitIn(bare, ["config", "--add", `uzi-stopped.${RUN}.entry`, JSON.stringify({ version: 1, runId: RUN, generation: null,
        branch: BRANCH, key: KEY, clonePath, journalDigest: createHash("sha256").update(rawJournal()).digest("hex"),
        reason: "test", at: "2026-10-27T00:00:00.000Z" })]);
      return { runId: RUN, clonePath, attemptId: aid(5), memoryPark: parkBytes() };
    };

    it("expected mode refuses without healing: journal, marker and attempt mode stay as they were", async () => {
      await refused(await stop(), "changed");
      assert.notEqual(rawJournal(), "", "the bound journal was not cleared");
    });

    it("control: without expected the same state IS healed, so the refusal above is the receipt mode's", async () => {
      await stop();
      assert.equal(await discover(), undefined);
      assert.equal(gitIn(bare, ["config", "--get", `uzi-recovery.${BRANCH}.clone`]), "");
    });
  });

  it("a journal that would need legacy attribution repair throws and appends no ledger entry", async () => {
    const expected = await attemptSource();
    const head = gitIn(expected.clonePath, ["rev-parse", "HEAD"]);
    const source = { runId: RUN, clonePath: expected.clonePath, attemptId: expected.attemptId };
    const progress = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source, progress.attempts, head);
    const journal = JSON.parse(rawJournal()) as { recovery: Record<string, unknown> };
    const successor = { runId: RUN, clonePath: `${path.dirname(expected.clonePath)}/${path.basename(expected.clonePath).replace(/\.attempt-.*$/, "")}.attempt-${aid(6)}`,
      attemptId: aid(6), restoreTip: head };
    gitIn(bare, ["config", `uzi-recovery.${BRANCH}.clone`, JSON.stringify({ ...journal, recovery: { ...journal.recovery, successor, stage: "adopting" } })]);
    const exact = { ...expected, memoryPark: parkBytes() };
    const before = snapshot();
    await assert.rejects(discover(exact), /does not belong to this key/);
    const after = snapshot();
    assert.deepEqual(after.config, before.config, "no ledger append, no repair");
    assert.equal(after.journal, before.journal);
    assert.equal(after.authority, before.authority);
    // Control: the unchanged discovery repairs exactly this state (appends the successor's ledger entry).
    await discover();
    assert.notDeepEqual(fs.readFileSync(path.join(bare, "config")), before.config, "plain discovery repairs, so the refusal is the receipt mode's");
  });
});
