import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type RetainedHoldsSnapshot, type RetainedLifecycleSnapshot, type RetainedSourceEvidence } from "../src/git.js";
import { latchResidueQuarantine, resetResidueQuarantineForTests } from "../src/residue-quarantine.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Read-only strict-plain epoch evaluation: a stale `live` attempt named solely by its exact plain
// journal gets an `epochSettlement`; nothing is written, and operator inspection never sets it.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const NO_HOLDS: RetainedHoldsSnapshot = { state: "read", holds: [] };
const TERMINAL: RetainedLifecycleSnapshot = { state: "terminal", status: "completed" };
const RUN = "run-strict";
const BRANCH = "agent/issue-3431";

let fx: Fixture;
let git: GitCache;
let bare: string;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
function idAt(n: number): string {
  return formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, n)), 1, n.toString(16).padStart(16, "0"));
}
function journalRaw(branch = BRANCH): string {
  return gitIn(bare, ["config", "--local", `uzi-recovery.${branch}.clone`]);
}
function setJournal(raw: string, branch = BRANCH): void {
  gitIn(bare, ["config", "--local", `uzi-recovery.${branch}.clone`, raw]);
}

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});
afterEach(() => {
  resetResidueQuarantineForTests();
  fx.cleanup();
});

/** A REAL live attempt clone seeded through production, with its production plain journal. */
async function seed(n = 1) {
  const key = "issue-3431";
  const attemptId = idAt(n);
  const clone = await git.runnerCloneForBranch(bare, BRANCH, key, noProofReseed, RUN, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, BRANCH, RUN, attemptId);
  return { attemptId, clonePath: clone.path, key };
}
function strictJournal(a: { attemptId: string; clonePath: string }): string {
  return JSON.stringify({ runId: RUN, clonePath: a.clonePath, attemptId: a.attemptId });
}
async function inspect(a: { attemptId: string }, over: { epoch?: boolean; holds?: RetainedHoldsSnapshot;
  lifecycle?: RetainedLifecycleSnapshot | undefined; beforeTree?: (p: string) => void } = {}): Promise<RetainedSourceEvidence | undefined> {
  const r = await git.inspectRetainedSources({
    selector: { runId: RUN, generation: 1, attempt: a.attemptId }, holds: over.holds ?? NO_HOLDS,
    lifecycle: "lifecycle" in over ? over.lifecycle : TERMINAL,
    isLivePath: () => false, barePath: bare, ...(over.epoch === false ? {} : { epoch: true }),
    ...(over.beforeTree ? { beforeTree: over.beforeTree } : {}),
  });
  assert.ok(r.sources.length <= 1);
  return r.sources[0];
}
async function settlementOf(a: { attemptId: string }, over: Parameters<typeof inspect>[1] = {}, sourceExpected = true) {
  const ev = await inspect(a, over);
  if (sourceExpected) assert.ok(ev, "the source is still gathered, so the refusal comes from the strict-plain guard");
  return ev?.epochSettlement;
}

describe("strict-plain epoch settlement", () => {
  it("settles an exact plain journal on a stale live attempt while the ordinary disposition stays refused", async () => {
    const a = await seed();
    assert.equal(journalRaw(), strictJournal(a), "production writes the exact canonical spelling");
    const ev = await inspect(a);
    assert.equal(ev!.ledgerState, "live");
    assert.deepEqual(ev!.epochSettlement, { basis: "strict_plain", disposition: { kind: "local_discard" } });
    assert.equal(ev!.disposition?.kind, "refused");
    const consumers = (ev!.disposition as { consumers: string[] }).consumers;
    assert.ok(consumers.includes("ledger_live"));
    assert.ok(consumers.some(c => c.startsWith("named_by_journal:")));
  });

  const sha = () => gitIn(bare, ["rev-parse", "HEAD"]);
  const extras: Array<[string, () => unknown, boolean]> = [
    ["restoreTip", sha, true],
    ["retainedSources", () => [], true],
    ["peeledMarkers", () => [sha()], true],
    ["unknownKey", () => 1, false],
    // `recovery` is excluded from the plain journal classification and has a strict inner grammar:
    // this invalid payload makes the whole bare unreadable, so it proves only that no source (hence no
    // settlement) is reported, not the exact-key-set guard.
    ["recovery", () => ({ stage: "captured" }), false],
    // These two are admitted by the broad plain classification, so they need VALID payloads (the same
    // shapes git-memory-park.test.ts and completion-retirement.test.ts persist): the source is gathered
    // and only the exact-key-set guard refuses the waiver.
    ["memoryPark", () => ({
      version: 1,
      binding: { runId: RUN, claimGeneration: 7, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 3 },
      ack: "sent_unconfirmed",
      origin: { flightId: "11111111-1111-4111-8111-111111111111", attemptId: null, executorIncarnation: null,
        authorityIncarnation: "22222222-2222-4222-8222-222222222222", sourceEpoch: "33333333-3333-4333-8333-333333333333" },
    }), true],
    ["completionSourceFence", () => ({
      version: 1, sourceUseEpoch: "99999999-9999-4999-8999-999999999999", sourceCoreDigest: "a".repeat(64),
    }), true],
  ];
  for (const [field, value, mustSurvive] of extras) {
    it(`refuses a journal carrying an extra ${field} field`, async () => {
      const a = await seed();
      setJournal(JSON.stringify({ ...JSON.parse(strictJournal(a)), [field]: value() }));
      const ev = await inspect(a);
      if (mustSurvive) assert.ok(ev, "the journal still parses, so the refusal is the strict guard's");
      assert.equal(ev?.epochSettlement, undefined);
    });
  }

  it("refuses a runId, clonePath or attemptId mismatch", async () => {
    const a = await seed();
    const base = JSON.parse(strictJournal(a));
    for (const over of [{ runId: "other-run" }, { clonePath: a.clonePath + "-x" }, { attemptId: idAt(9) }]) {
      setJournal(JSON.stringify({ ...base, ...over }));
      assert.equal(await settlementOf(a), undefined, JSON.stringify(over));
    }
  });

  it("refuses a duplicate-key or reformatted raw journal", async () => {
    const a = await seed();
    const dup = `{"runId":"stale","runId":${JSON.stringify(RUN)},"clonePath":${JSON.stringify(a.clonePath)},"attemptId":"${a.attemptId}"}`;
    for (const raw of [dup, JSON.stringify(JSON.parse(strictJournal(a)), null, 1), strictJournal(a) + " "]) {
      setJournal(raw);
      assert.equal(await settlementOf(a), undefined, raw);
    }
  });

  it("refuses when another journal also names the path", async () => {
    const a = await seed();
    setJournal(strictJournal(a), "agent/issue-other");
    assert.equal(await settlementOf(a), undefined);
  });

  it("refuses when a second non-retired attempt shares the clone key, and ignores a retired one", async () => {
    const a = await seed();
    const canonical = git.runnerClonePath(bare, a.key);
    const entry = (n: number, state: string) => JSON.stringify({ attemptId: idAt(n), runId: "other-run",
      clonePath: `${canonical}.attempt-${idAt(n)}`, state });
    gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, entry(5, "retired")]);
    assert.equal((await settlementOf(a))?.disposition.kind, "local_discard", "retired entries do not count");
    for (const state of ["abandoned", "reclaimed", "live"]) {
      gitIn(bare, ["config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, entry(6, state)]);
      assert.equal(await settlementOf(a), undefined, state);
    }
  });

  it("keeps an owed pin or an episode pin as a consumer that refuses the basis", async () => {
    const a = await seed();
    const tip = gitIn(bare, ["rev-parse", "HEAD"]);
    gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${tip}`, tip]);
    assert.equal(await settlementOf(a), undefined, "owed pin");
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${tip}`]);
    assert.equal((await settlementOf(a))?.disposition.kind, "local_discard");
    gitIn(bare, ["update-ref", `refs/uzi-recovery-episode/${RUN}/e1`, tip]);
    assert.equal(await settlementOf(a), undefined, "episode pin");
  });

  it("does not settle when holds are unknown or a hold is open", async () => {
    const a = await seed();
    assert.deepEqual(await settlementOf(a, { holds: { state: "unknown" } }),
      { basis: "strict_plain", disposition: { kind: "refused", consumers: ["holds_unknown"] } }, "unknown holds");
    assert.deepEqual(await settlementOf(a, { holds: { state: "read", holds: [{ id: "h1", generation: 1 }] } }),
      { basis: "strict_plain", disposition: { kind: "owner_hold_discard", holdIds: ["h1"] } }, "open hold");
  });

  it("requires a positive terminal lifecycle verdict", async () => {
    const a = await seed();
    for (const status of ["completed", "failed", "cancelled"] as const) {
      assert.equal((await settlementOf(a, { lifecycle: { state: "terminal", status } }))?.disposition.kind, "local_discard", status);
    }
    const cases: Array<[string, RetainedLifecycleSnapshot | undefined]> = [
      ["running", { state: "not_terminal", status: "running" }],
      ["unknown", { state: "unknown" }],
      ["absent", undefined],
    ];
    for (const [name, lifecycle] of cases) assert.equal(await settlementOf(a, { lifecycle }), undefined, name);
  });

  it("keeps every other hard blocker: a quarantined worker is refused with the blocker named", async () => {
    const a = await seed();
    assert.equal((await settlementOf(a))?.disposition.kind, "local_discard");
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    const s = await settlementOf(a);
    assert.equal(s?.disposition.kind, "refused");
    assert.ok((s!.disposition as { consumers: string[] }).consumers.includes("worker_quarantined"));
  });

  it("does not settle a reclaimed or abandoned attempt named by its exact plain journal", async () => {
    const a = await seed();
    const ledgerKey = `uzi-attempts.${BRANCH}.entry`;
    const own = JSON.parse(gitIn(bare, ["config", "--local", "--get-all", ledgerKey]).split("\n").filter(l => l.includes(a.attemptId)).pop()!);
    for (const state of ["reclaimed", "abandoned"]) {
      gitIn(bare, ["config", "--local", "--add", ledgerKey, JSON.stringify({ ...own, state })]);
      const ev = await inspect(a);
      assert.equal(ev?.ledgerState, state, "last ledger entry wins");
      assert.equal(ev?.epochSettlement, undefined, state);
    }
  });

  it("clears the settlement when the tree binding check fails", async () => {
    const a = await seed();
    assert.ok(await settlementOf(a));
    const r = await git.inspectRetainedSources({
      selector: { runId: RUN, generation: 1, attempt: a.attemptId }, holds: NO_HOLDS, lifecycle: TERMINAL,
      isLivePath: () => false, barePath: bare, epoch: true, beforeTree: () => { throw new Error("binding"); },
    });
    assert.equal(r.partial, true);
    assert.ok(r.sources.length >= 1);
    for (const s of r.sources) {
      assert.equal(s.epochSettlement, undefined);
      assert.equal(s.disposition, undefined);
    }
  });

  it("operator inspection never sets it and reports exactly what an epoch read reports", async () => {
    const a = await seed();
    const operator = (await inspect(a, { epoch: false }))!;
    const epoch = (await inspect(a))!;
    assert.equal("epochSettlement" in operator, false);
    assert.equal(operator.disposition?.kind, "refused");
    assert.deepEqual(epoch.disposition, operator.disposition);
    assert.equal(epoch.descriptor, operator.descriptor);
    assert.deepEqual(epoch.consumers, operator.consumers);
  });

  it("flags a marker-less canonical plain journal that carries extra fields, for epoch inspection only", async () => {
    const canonical = await git.createOrAttachRunnerClone(bare, 3431, noProofReseed, RUN);
    await git.markRecoveryCapture(bare, canonical.path, BRANCH, RUN);
    const selector = { runId: RUN, generation: "none" as const, attempt: "canonical" };
    const read = async (epoch: boolean) => {
      const r = await git.inspectRetainedSources({ selector, holds: NO_HOLDS, lifecycle: TERMINAL, isLivePath: () => false,
        barePath: bare, ...(epoch ? { epoch: true } : {}) });
      assert.equal(r.sources.length, 1);
      return r.sources[0]!;
    };
    const plain = await read(true);
    assert.equal(plain.kind, "canonical");
    assert.equal(plain.journalState, "plain");
    assert.equal(plain.markerState, "none");
    assert.equal("epochJournalExtraFields" in plain, false, "{runId, clonePath} alone is not flagged");

    setJournal(JSON.stringify({ runId: RUN, clonePath: canonical.path, restoreTip: gitIn(bare, ["rev-parse", "HEAD"]) }));
    const epoch = await read(true);
    const operator = await read(false);
    assert.equal(epoch.journalState, "plain");
    assert.equal(epoch.markerState, "none");
    assert.equal(epoch.epochJournalExtraFields, true);
    assert.equal("epochJournalExtraFields" in operator, false);
    // Report-only: the disposition and the descriptor are identical to an operator read.
    assert.deepEqual(epoch.disposition, operator.disposition);
    assert.equal(epoch.disposition?.kind, "local_discard");
    assert.equal(epoch.descriptor, operator.descriptor);
  });

  it("writes nothing", async () => {
    const a = await seed();
    const snapshot = () => ({
      config: gitIn(bare, ["config", "--local", "--list"]),
      refs: gitIn(bare, ["for-each-ref"]),
      parent: fs.readdirSync(path.dirname(a.clonePath)).sort(),
      clone: fs.readdirSync(a.clonePath).sort(),
    });
    const before = snapshot();
    assert.equal((await settlementOf(a))?.disposition.kind, "local_discard");
    assert.deepEqual(snapshot(), before);
  });
});
