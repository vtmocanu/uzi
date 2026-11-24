import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, parseAttemptLedgerEntry, type RetainedDiscardOptions, type RetainedHoldsSnapshot } from "../src/git.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { freshJournalKey } from "../src/fresh-journal.js";
import { RetainedSourceServer } from "../src/ops/retained-source-server.js";

// issue #4628 m3-B: the guarded fresh discard. A fresh_unbound attempt (the server took the transfer back or refused it before any
// seed) is retired by the ordinary guarded discard together with its refs/uzi-fresh/<frid> ref and its fresh journal; a
// fresh_pending attempt stays blocked unless the caller proves the server reports the transfer refused or cancelled pre-seed.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const BRANCH = "agent/issue-78";
const KEY = "issue-78";
const RUN = "run-fresh-discard";
const GENERATION = 2;
const NO_HOLDS: RetainedHoldsSnapshot = { state: "read", holds: [] };

let fx: Fixture;
let cache: GitCache;
let bare: string;

const sh = (dir: string, args: string[]): string => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, stdio: "pipe" }).trim();
const config = (key: string): string[] => { try { return sh(bare, ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; } };
const refExists = (ref: string): boolean => { try { sh(bare, ["rev-parse", "--verify", "--quiet", ref]); return true; } catch { return false; } };

interface Seeded { frid: string; attemptId: string; clonePath: string; ref: string }

/** A seeded fresh attempt (L0 to L2) with its capture ref in place. */
async function seeded(): Promise<Seeded> {
  const sha = sh(fx.originPath, ["rev-parse", "main"]);
  const frid = randomUUID();
  const ref = `refs/uzi-fresh/${frid}`;
  sh(bare, ["update-ref", ref, sha]);
  const attemptId = mintAttemptId(GENERATION);
  const clone = await cache.seedFreshAttempt({ barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId, frid, generation: GENERATION,
    artifact: { kind: "capture", artifactRef: ref, artifactSha: sha }, isLive: () => false, beforeSeed: async () => {} });
  return { frid, attemptId, clonePath: clone.path, ref };
}

async function discard(s: Seeded, extra: Partial<RetainedDiscardOptions> = {}) {
  const selector = { runId: RUN, generation: GENERATION, attempt: s.attemptId };
  const inspected = await cache.inspectRetainedSources({ selector, holds: NO_HOLDS, isLivePath: () => false, barePath: bare });
  assert.equal(inspected.sources.length, 1);
  const result = await cache.discardStoppedSource({ selector, confirm: inspected.sources[0]!.descriptor, holds: NO_HOLDS, isLivePath: () => false,
    quiesce: async () => true, barePath: bare, ...extra });
  return { inspected: inspected.sources[0]!, result };
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
});
afterEach(() => fx.cleanup());

describe("the guarded fresh discard", () => {
  it("retires a fresh_unbound attempt, its capture ref and its fresh journal", async () => {
    const s = await seeded();
    assert.equal(await cache.markFreshUnbound({ barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: s.attemptId }), true);
    const { inspected, result } = await discard(s);
    assert.equal(inspected.ledgerState, "fresh_unbound");
    assert.equal(result.kind, "done");
    assert.equal(fs.existsSync(s.clonePath), false, "the seeded clone is gone");
    assert.equal(parseAttemptLedgerEntry(config(`uzi-attempts.${BRANCH}.entry`).at(-1)!)?.state, "retired");
    assert.equal(refExists(s.ref), false, "the capture ref is gone");
    assert.equal(await cache.readFreshJournal(bare, BRANCH), undefined, "the branch is free for a later transfer");
  });

  it("keeps a fresh_pending attempt, its ref and its journal: the ordinary discard never retires it", async () => {
    const s = await seeded();
    const { inspected, result } = await discard(s);
    assert.equal(inspected.ledgerState, "fresh_pending");
    assert.equal(result.kind, "refused");
    assert.ok(result.kind === "refused" && result.consumers.includes("ledger_fresh"), "refused for the fresh blocker");
    assert.equal(fs.existsSync(s.clonePath), true);
    assert.equal(refExists(s.ref), true);
    assert.equal((await cache.readFreshJournal(bare, BRANCH))?.attemptId, s.attemptId);
  });

  it("retires a fresh_pending attempt once the caller names it as refused or cancelled before any seed", async () => {
    const s = await seeded();
    const { result } = await discard(s, { freshTransferNegative: new Set([s.attemptId]) });
    assert.equal(result.kind, "done");
    assert.equal(fs.existsSync(s.clonePath), false);
    assert.equal(refExists(s.ref), false);
    assert.equal(await cache.readFreshJournal(bare, BRANCH), undefined);
  });

  it("a proof naming another attempt retires nothing", async () => {
    const s = await seeded();
    const { result } = await discard(s, { freshTransferNegative: new Set([mintAttemptId(GENERATION)]) });
    assert.equal(result.kind, "refused");
    assert.equal(fs.existsSync(s.clonePath), true);
    assert.equal(refExists(s.ref), true);
  });

  it("a journal that names another attempt keeps its ref and journal when this attempt is retired", async () => {
    const s = await seeded();
    await cache.markFreshUnbound({ barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: s.attemptId });
    const otherFrid = randomUUID();
    const journal = JSON.parse(config(freshJournalKey(BRANCH)).at(-1)!) as Record<string, unknown>;
    sh(bare, ["config", "--local", freshJournalKey(BRANCH), JSON.stringify({ ...journal, frid: otherFrid, attemptId: mintAttemptId(GENERATION) })]);
    const { result } = await discard(s);
    assert.equal(result.kind, "done");
    assert.equal(refExists(s.ref), true, "this attempt's capture ref is not the journal's: left alone");
    assert.notEqual(await cache.readFreshJournal(bare, BRANCH), undefined);
  });
});

describe("RetainedSourceServer.readFreshNegative (N-6)", () => {
  const negative = { state: "refused", seeded_at: null, destination_attempt_id: null, seeded_generation: null, current_authority: false };
  type Evidence = { kind: string; ledgerState: string; barePath: string; branch: string; attempt: string; runId: string; generation?: number };
  const evidenceFor = (s: Seeded, over: Partial<Evidence> = {}): Evidence =>
    ({ kind: "attempt", ledgerState: "fresh_pending", barePath: bare, branch: BRANCH, attempt: s.attemptId, runId: RUN, generation: 7, ...over });

  function serverWith(state: (runId: string, frid: string, generation: number) => Promise<unknown>) {
    const calls: Array<{ runId: string; frid: string; generation: number }> = [];
    const server = new RetainedSourceServer({
      dataDir: fx.dataDir, git: cache, gate: undefined, isIdle: () => true, log: nullLogger(),
      runner: {} as never,
      client: {
        listRecoveryHolds: async () => ({ run_id: RUN, holds: [] }), getRunOwnership: async () => ({ status: "completed", claim_generation: 1 }),
        getFreshResumeState: async (runId: string, frid: string, generation: number) => { calls.push({ runId, frid, generation }); return state(runId, frid, generation); },
      } as never,
    });
    const read = (ev: Evidence): Promise<ReadonlySet<string> | undefined> =>
      (server as unknown as { readFreshNegative(ev: Evidence): Promise<ReadonlySet<string> | undefined> }).readFreshNegative(ev);
    return { read, calls };
  }

  it("names the attempt when its own journal's transfer is negative, asking with the evidence's live generation", async () => {
    const s = await seeded();
    const { read, calls } = serverWith(async () => negative);
    assert.deepEqual([...(await read(evidenceFor(s)))!], [s.attemptId]);
    assert.deepEqual(calls, [{ runId: RUN, frid: s.frid, generation: 7 }]);
  });

  it("falls back to the journal's generation when the evidence carries none", async () => {
    const s = await seeded();
    const { read, calls } = serverWith(async () => negative);
    const { generation: _omit, ...noGeneration } = evidenceFor(s);
    assert.ok(await read(noGeneration));
    assert.equal(calls[0]!.generation, GENERATION);
  });

  it("answers nothing, and asks nothing, for a journal that names another attempt or an attempt that is not fresh_pending", async () => {
    const s = await seeded();
    const { read, calls } = serverWith(async () => negative);
    assert.equal(await read(evidenceFor(s, { attempt: mintAttemptId(GENERATION) })), undefined);
    assert.equal(await read(evidenceFor(s, { ledgerState: "fresh_unbound" })), undefined);
    assert.equal(await read(evidenceFor(s, { kind: "canonical" })), undefined);
    assert.deepEqual(calls, [], "the state route is never asked for these");
  });

  it("is not negative for a non-negative answer, and fails closed when the state route throws", async () => {
    const s = await seeded();
    const claimed = serverWith(async () => ({ ...negative, state: "claimed" }));
    assert.equal(await claimed.read(evidenceFor(s)), undefined);
    const missing = serverWith(async () => null);
    assert.equal(await missing.read(evidenceFor(s)), undefined);
    const broken = serverWith(async () => { throw new Error("state route unavailable"); });
    assert.equal(await broken.read(evidenceFor(s)), undefined);
    assert.equal(broken.calls.length, 1);
  });
});
