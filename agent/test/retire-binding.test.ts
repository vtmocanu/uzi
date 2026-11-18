import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  GitCache, RETIRE_BINDING_MAX_PER_BARE, evaluateRetireBinding, evaluateRetireRecovery, parseAttemptLedgerEntry, parseRetireBinding, parseRetireBindingKey, retireBindingKey, retireCrossBareConflict, retireBindingValue,
  type RetireBinding, type RetireBindingRead, type RetireScratchBareEvidence,
} from "../src/git.js";
import { syncDirectory } from "../src/authority-lock.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #4263 M1: the retirement-scratch binding codec, its bounded reader, and the compaction rule that a bound
// attempt's ledger entry is never dropped. Real git bares in temp dirs.

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const BRANCH = "agent/issue-4263";
const KEY = "issue-4263";
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;
const ATTEMPT = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const UUID = "1a1a1a1a-1111-4111-8b1b-1c1c1c1c1c1c";

const binding = (over: Partial<RetireBinding> = {}): RetireBinding => ({
  v: 1, class: "completion", attemptId: ATTEMPT, runId: "run-4263", branch: BRANCH,
  clonePath: "/data/runner/slug/issue-4263.attempt-x", phase: "intent", parentIno: "131072", cloneIno: "131073", pod: "worker-0", ...over,
});
const asObject = (over: Record<string, unknown>): string => JSON.stringify({ ...JSON.parse(retireBindingValue(binding())), ...over });

describe("retire binding codec", () => {
  it("round-trips in the canonical field order with no device field", () => {
    const b = binding();
    const raw = retireBindingValue(b);
    assert.deepEqual(Object.keys(JSON.parse(raw)),
      ["v", "class", "attemptId", "runId", "branch", "clonePath", "phase", "parentIno", "cloneIno", "pod"]);
    assert.equal(raw.includes("dev"), false);
    assert.deepEqual(parseRetireBinding(UUID, raw), { ok: true, binding: b });
    for (const cls of ["completion", "verified_successor", "owner_terminal", "owner_terminal_covered", "other"] as const)
      for (const phase of ["intent", "settled", "removing"] as const)
        assert.equal(parseRetireBinding(UUID, retireBindingValue(binding({ class: cls, phase }))).ok, true);
    assert.equal(parseRetireBinding(UUID, retireBindingValue(binding({ pod: "" }))).ok, true, "an empty pod is allowed");
    assert.equal(parseRetireBinding(UUID, retireBindingValue(binding({ parentIno: "18446744073709551615" }))).ok, true, "2^64-1 is allowed");
    assert.equal(parseRetireBinding(UUID, retireBindingValue(binding({ branch: "b".repeat(4096) }))).ok, true, "4096 chars is allowed");
  });

  it("derives and parses the key", () => {
    assert.equal(retireBindingKey(UUID), `uzi-retire-scratch.${UUID}.binding`);
    assert.equal(parseRetireBindingKey(retireBindingKey(UUID)), UUID);
    for (const key of [`uzi-retire-scratch.${UUID.toUpperCase()}.binding`, `uzi-retire-scratch.${UUID}.other`, "uzi-retire-scratch.x.binding",
      `uzi-retire-scratch.${UUID}x.binding`, `uzi-attempts.${UUID}.binding`])
      assert.equal(parseRetireBindingKey(key), undefined, key);
  });

  const good = retireBindingValue(binding());
  const parsedGood = JSON.parse(good) as Record<string, unknown>;
  const reordered = JSON.stringify({ class: parsedGood.class, v: 1, ...Object.fromEntries(Object.entries(parsedGood).slice(2)) });
  const cases: Array<[string, string, string?]> = [
    ["empty value", ""],
    ["not JSON", "{"],
    ["an array", "[]"],
    ["unknown extra field", asObject({ dev: "2049" })],
    ["reordered keys", reordered],
    ["extra whitespace", JSON.stringify(JSON.parse(good), null, 1)],
    ["trailing whitespace", `${good} `],
    ["wrong v", asObject({ v: 2 })],
    ["string v", asObject({ v: "1" })],
    ["unknown class", asObject({ class: "bogus" })],
    ["unknown phase", asObject({ phase: "done" })],
    ["bad attempt id", asObject({ attemptId: "not-an-attempt" })],
    ["empty run id", asObject({ runId: "" })],
    ["run id with a dot", asObject({ runId: "run.1" })],
    ["empty branch", asObject({ branch: "" })],
    ["relative clonePath", asObject({ clonePath: "data/runner/x" })],
    ["unnormalized clonePath", asObject({ clonePath: "/data/runner/../runner/x" })],
    ["trailing slash clonePath", asObject({ clonePath: "/data/runner/x/" })],
    ["non-canonical inode 01", asObject({ parentIno: "01" })],
    ["numeric inode", asObject({ cloneIno: 5 })],
    ["negative inode", asObject({ cloneIno: "-1" })],
    ["oversized inode", asObject({ parentIno: "1".repeat(21) })],
    ["inode above 2^64-1", asObject({ parentIno: "18446744073709551616" })],
    ["zero parent inode", asObject({ parentIno: "0" })],
    ["zero clone inode", asObject({ cloneIno: "0" })],
    ["branch over 4096 chars", asObject({ branch: "b".repeat(4097) })],
    ["branch starting with a dash", asObject({ branch: "-x" })],
    ["branch with a newline", asObject({ branch: "agent/a\nb" })],
    ["branch with a NUL", asObject({ branch: "agent/a\u0000b" })],
    ["branch with DEL", asObject({ branch: "agent/a\u007Fb" })],
    ["branch with a bidi override", asObject({ branch: "agent/a‮b" })],
    ["branch with a bidi isolate", asObject({ branch: "agent/a⁦b" })],
    ["clonePath over 4096 chars", asObject({ clonePath: `/${"c".repeat(4096)}` })],
    ["clonePath with a tab", asObject({ clonePath: "/data/runner/a\tb" })],
    ["clonePath with a bidi control", asObject({ clonePath: "/data/runner/a‪b" })],
    ["pod with a space", asObject({ pod: "a b" })],
    ["pod too long", asObject({ pod: "p".repeat(161) })],
    ["uppercase key uuid", good, UUID.toUpperCase()],
    ["non-uuid key", good, "not-a-uuid"],
  ];
  for (const [name, raw, keyUuid] of cases) {
    it(`rejects ${name}`, () => assert.deepEqual(parseRetireBinding(keyUuid ?? UUID, raw), { ok: false }));
  }
});

let fx: Fixture;
let cache: GitCache;
let bare: string;
beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});
afterEach(() => fx.cleanup());

const gitIn = (args: string[]): string => execFileSync("git", ["-C", bare, ...args], { encoding: "utf8", env: ENV }).trim();
const setBinding = (uuid: string, raw: string): void => { gitIn(["config", "--local", retireBindingKey(uuid), raw]); };
const ledgerValues = (): string[] => { try { return gitIn(["config", "--local", "--get-all", LEDGER_KEY]).split("\n"); } catch { return []; } };
const compact = (): Promise<void> => (cache as unknown as { compactAttemptLedger(b: string, br: string): Promise<void> }).compactAttemptLedger(bare, BRANCH);

describe("bounded binding reader", () => {
  it("reads a decoded binding and reports an undecodable one as malformed", async () => {
    const other = randomUUID();
    setBinding(UUID, retireBindingValue(binding()));
    setBinding(other, "not json");
    const read = await cache.readRetireBindings(bare);
    assert.equal(read.state, "read");
    if (read.state !== "read") return;
    const byUuid = new Map(read.bindings.map(b => [b.uuid, b]));
    assert.deepEqual(byUuid.get(UUID)?.parsed, binding());
    assert.equal(byUuid.get(UUID)?.key, retireBindingKey(UUID));
    assert.equal(byUuid.get(other)?.parsed, undefined);
    assert.equal(byUuid.get(other)?.raw, "not json");
  });

  it("reads exactly the cap and refuses one more as unknown", async () => {
    for (let i = 0; i < RETIRE_BINDING_MAX_PER_BARE; i++) setBinding(randomUUID(), retireBindingValue(binding()));
    const atCap = await cache.readRetireBindings(bare);
    assert.equal(atCap.state, "read");
    if (atCap.state === "read") assert.equal(atCap.bindings.length, RETIRE_BINDING_MAX_PER_BARE);
    setBinding(randomUUID(), retireBindingValue(binding()));
    assert.deepEqual(await cache.readRetireBindings(bare), { state: "unknown" });
  });

  it("returns a valueless binding key as a malformed entry instead of throwing", async () => {
    fs.appendFileSync(path.join(bare, "config"), `[uzi-retire-scratch "${UUID}"]\n\tbinding\n`);
    const read = await cache.readRetireBindings(bare);
    assert.deepEqual(read, { state: "read", bindings: [{ key: retireBindingKey(UUID), uuid: UUID, raw: "" }] });
  });

  it("carries the full key and no uuid for a key that is not a well-formed binding key", async () => {
    gitIn(["config", "--local", "uzi-retire-scratch.junk.binding", "x"]);
    const read = await cache.readRetireBindings(bare);
    assert.deepEqual(read, { state: "read", bindings: [{ key: "uzi-retire-scratch.junk.binding", raw: "x" }] });
  });

  it("treats a duplicated key as malformed for every copy", async () => {
    gitIn(["config", "--local", "--add", retireBindingKey(UUID), retireBindingValue(binding())]);
    gitIn(["config", "--local", "--add", retireBindingKey(UUID), retireBindingValue(binding())]);
    const read = await cache.readRetireBindings(bare);
    assert.equal(read.state === "read" && read.bindings.length === 2 && read.bindings.every(b => b.parsed === undefined), true);
  });

  it("is empty for a bare with no bindings", async () => {
    assert.deepEqual(await cache.readRetireBindings(bare), { state: "read", bindings: [] });
  });
});

describe("compactAttemptLedger keeps an attempt a binding names", () => {
  const gone = (): string => attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT);
  const addRetired = (runId = "run-4263"): string => {
    const clonePath = gone();
    gitIn(["config", "--local", "--add", LEDGER_KEY, JSON.stringify({ attemptId: ATTEMPT, runId, clonePath, state: "retired" })]);
    return clonePath;
  };

  it("drops a gone retired entry no binding names (base behaviour)", async () => {
    addRetired();
    await compact();
    assert.deepEqual(ledgerValues(), []);
  });

  it("keeps a gone retired entry whose (attemptId, runId, clonePath) a binding names, in any phase", async () => {
    for (const phase of ["intent", "settled", "removing"] as const) {
      if (ledgerValues().length > 0) gitIn(["config", "--local", "--unset-all", LEDGER_KEY]);
      const clonePath = addRetired();
      setBinding(UUID, retireBindingValue(binding({ clonePath, phase })));
      await compact();
      assert.equal(ledgerValues().length, 1, phase);
    }
  });

  it("keeps the entry of an attempt whose bindings name a different identity", async () => {
    const clonePath = addRetired();
    setBinding(UUID, retireBindingValue(binding({ clonePath, runId: "run-other" })));
    setBinding(randomUUID(), retireBindingValue(binding({ clonePath: `${clonePath}-x` })));
    const before = ledgerValues();
    await compact();
    assert.equal(before.length, 1);
    assert.deepEqual(ledgerValues(), before);
  });

  it("keeps every entry when any binding is malformed (it may name the entry)", async () => {
    addRetired();
    setBinding(UUID, "{");
    const before = ledgerValues();
    await compact();
    assert.deepEqual(ledgerValues(), before);
    assert.equal(before.length, 1);
  });

  it("keeps the entry when the binding key is duplicated", async () => {
    const clonePath = addRetired();
    for (let i = 0; i < 2; i++) gitIn(["config", "--local", "--add", retireBindingKey(UUID), retireBindingValue(binding({ clonePath }))]);
    await compact();
    assert.equal(ledgerValues().length, 1);
  });

  it("keeps the entry when the binding value is a future version", async () => {
    const clonePath = addRetired();
    setBinding(UUID, asObject({ v: 2, clonePath }));
    await compact();
    assert.equal(ledgerValues().length, 1);
  });

  it("drops nothing when the binding read is unknown", async () => {
    addRetired();
    for (let i = 0; i <= RETIRE_BINDING_MAX_PER_BARE; i++) setBinding(randomUUID(), retireBindingValue(binding({ clonePath: "/elsewhere/x" })));
    const before = ledgerValues();
    await compact();
    assert.deepEqual(ledgerValues(), before);
    assert.equal(before.length, 1);
  });
});

describe("compactAttemptLedger keeps taint a reclaim evaluation relies on (#4263)", () => {
  const clonePath = (): string => attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT);
  const canonicalRetired = (): string => JSON.stringify({ attemptId: ATTEMPT, runId: "run-4263", clonePath: clonePath(), state: "retired" });
  const addLedger = (raw: string): void => { gitIn(["config", "--local", "--add", LEDGER_KEY, raw]); };
  const bindSettled = (): void => setBinding(UUID, retireBindingValue(binding({ class: "owner_terminal", phase: "settled", clonePath: clonePath() })));
  const evaluate = async () => {
    const read = await cache.readRetireBindings(bare);
    assert.equal(read.state, "read");
    return evaluateRetireBinding({
      evidence: { barePath: bare, bindings: read.state === "read" ? read.bindings : [],
        ledger: ledgerValues().map(raw => ({ branch: BRANCH, raw })), protectedPaths: [], restoreBranches: [], recovery: [], stoppedRuns: [] },
      uuid: UUID, runnerRoot: path.join(fx.dataDir, "runner"),
    });
  };

  it("control: a canonical bound attempt still compacts to its last value and stays reclaimable", async () => {
    addLedger(JSON.stringify({ attemptId: ATTEMPT, runId: "run-4263", clonePath: clonePath(), state: "live" }));
    addLedger(canonicalRetired());
    bindSettled();
    assert.equal((await evaluate()).kind, "ready");
    await compact();
    assert.deepEqual(ledgerValues(), [canonicalRetired()]);
    assert.equal((await evaluate()).kind, "ready");
  });

  it("keeps an unknown-state value naming a bound attempt byte-for-byte, so the evaluation stays ledger_mismatch", async () => {
    addLedger(canonicalRetired());
    const unknownState = JSON.stringify({ attemptId: ATTEMPT, runId: "run-4263", clonePath: clonePath(), state: "future-state" });
    addLedger(unknownState);
    bindSettled();
    await compact();
    assert.ok(ledgerValues().includes(unknownState), "the non-canonical value survives compaction verbatim");
    assert.deepEqual(await evaluate(), { kind: "keep", reason: "ledger_mismatch" });
  });

  it("keeps a reordered (parseable, non-canonical) value naming a bound attempt verbatim", async () => {
    addLedger(canonicalRetired());
    const reordered = JSON.stringify({ runId: "run-4263", attemptId: ATTEMPT, clonePath: clonePath(), state: "live" });
    addLedger(reordered);
    bindSettled();
    await compact();
    assert.ok(ledgerValues().includes(reordered));
    assert.deepEqual(await evaluate(), { kind: "keep", reason: "ledger_mismatch" });
  });

  it("keeps the relative order of a tainted value and the compacted entries, so the last state per attempt is unchanged", async () => {
    const other = "20261117T000000Z-g1-0123456789abcdef";
    const otherLive = JSON.stringify({ attemptId: other, runId: "run-other", clonePath: bare, state: "live" }); // a present path: never dropped
    const reordered = JSON.stringify({ runId: "run-4263", attemptId: ATTEMPT, clonePath: clonePath(), state: "live" });
    addLedger(canonicalRetired());
    addLedger(otherLive);
    addLedger(reordered);
    bindSettled();
    const lastState = (): string | undefined => ledgerValues().map(v => parseAttemptLedgerEntry(v)).filter(e => e?.attemptId === ATTEMPT).at(-1)?.state;
    assert.equal(lastState(), "live");
    await compact();
    assert.deepEqual(ledgerValues(), [canonicalRetired(), otherLive, reordered], "byte-identical and in input order");
    assert.equal(lastState(), "live", "a reader taking the last value per attempt still sees live, never the earlier retired");
  });

  it("with an undecodable binding set, preserves a non-canonical value byte-for-byte", async () => {
    setBinding(randomUUID(), "{");
    const junk = "not json at all";
    const reordered = JSON.stringify({ runId: "run-4263", attemptId: ATTEMPT, clonePath: clonePath(), state: "live" });
    addLedger(junk);
    addLedger(reordered);
    await compact();
    assert.deepEqual(ledgerValues(), [junk, reordered]);
  });
});

describe("conflicting canonical history for one attempt id (#4263)", () => {
  const RUNNER_ROOT = "/data/runner";
  const CLONE = "/data/runner/slug/attempt";
  const OTHER_BRANCH = "agent/other";
  const OTHER_KEY = `uzi-attempts.${OTHER_BRANCH}.entry`;
  const settled: RetireBinding = {
    v: 1, class: "owner_terminal", attemptId: ATTEMPT, runId: "run-current", branch: BRANCH, clonePath: CLONE,
    phase: "settled", parentIno: "100", cloneIno: "101", pod: "",
  };
  const ent = (state: string, over: Record<string, string> = {}): string => {
    const f = { attemptId: ATTEMPT, runId: "run-current", clonePath: CLONE, ...over };
    return JSON.stringify({ attemptId: f.attemptId, runId: f.runId, clonePath: f.clonePath, state });
  };
  const retired = ent("retired");
  type L = Array<{ branch: string; raw: string }>;
  const evidenceOf = (ledger: L, bindings = [{ key: retireBindingKey(UUID), uuid: UUID, raw: retireBindingValue(settled), parsed: settled as RetireBinding | undefined }]) =>
    ({ barePath: "/data/repos/fixture.git", bindings, ledger, protectedPaths: [], restoreBranches: [], recovery: [], stoppedRuns: [] });
  const exam = (ledger: L) => evaluateRetireBinding({ evidence: evidenceOf(ledger), uuid: UUID, runnerRoot: RUNNER_ROOT });
  const recover = (ledger: L) => evaluateRetireRecovery({
    evidence: evidenceOf(ledger), uuid: UUID, runnerRoot: RUNNER_ROOT, expectRaw: retireBindingValue(settled), canonical: { present: false },
  });
  const conflicts: Array<[string, L]> = [
    ["a different run id", [{ branch: BRANCH, raw: ent("live", { runId: "run-other" }) }, { branch: BRANCH, raw: retired }]],
    ["a different clone path", [{ branch: BRANCH, raw: ent("live", { clonePath: "/data/runner/slug/other" }) }, { branch: BRANCH, raw: retired }]],
    ["a different branch", [{ branch: OTHER_BRANCH, raw: ent("live") }, { branch: BRANCH, raw: retired }]],
  ];
  const control: L = [{ branch: BRANCH, raw: ent("live") }, { branch: BRANCH, raw: retired }];

  for (const [name, ledger] of conflicts) {
    it(`pure: an earlier canonical value with ${name} is ledger_mismatch and unresolved`, () => {
      assert.deepEqual(exam(ledger), { kind: "keep", reason: "ledger_mismatch" });
      assert.equal(recover(ledger), "unresolved");
    });
  }

  it("pure control: same-identity live then retired is ready and case1", () => {
    assert.equal(exam(control).kind, "ready");
    assert.equal(recover(control), "case1");
  });

  it("pure: a second binding naming the attempt with another identity is a conflict and unresolved", () => {
    const other: RetireBinding = { ...settled, runId: "run-other" };
    const otherUuid = "2b2b2b2b-2222-4222-8b2b-2c2c2c2c2c2c";
    const bindings = [
      { key: retireBindingKey(UUID), uuid: UUID, raw: retireBindingValue(settled), parsed: settled as RetireBinding | undefined },
      { key: retireBindingKey(otherUuid), uuid: otherUuid, raw: retireBindingValue(other), parsed: other as RetireBinding | undefined },
    ];
    assert.deepEqual(evaluateRetireBinding({ evidence: evidenceOf(control, bindings), uuid: UUID, runnerRoot: RUNNER_ROOT }), { kind: "keep", reason: "conflict" });
    assert.equal(evaluateRetireRecovery({
      evidence: evidenceOf(control, bindings), uuid: UUID, runnerRoot: RUNNER_ROOT, expectRaw: retireBindingValue(settled), canonical: { present: false },
    }), "unresolved");
  });

  // Real git fixture: compaction must not erase the conflicting history.
  const clonePath = (): string => attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT);
  const entry = (state: string, over: Record<string, string> = {}): string => {
    const f = { attemptId: ATTEMPT, runId: "run-4263", clonePath: clonePath(), ...over };
    return JSON.stringify({ attemptId: f.attemptId, runId: f.runId, clonePath: f.clonePath, state });
  };
  const addTo = (key: string, raw: string): void => { gitIn(["config", "--local", "--add", key, raw]); };
  const valuesOf = (key: string): string[] => { try { return gitIn(["config", "--local", "--get-all", key]).split("\n"); } catch { return []; } };
  const bindSettled = (): void => setBinding(UUID, retireBindingValue(binding({ class: "owner_terminal", phase: "settled", clonePath: clonePath() })));
  const compactBranch = (br: string): Promise<void> => (cache as unknown as { compactAttemptLedger(b: string, br: string): Promise<void> }).compactAttemptLedger(bare, br);
  const realEvidence = async (): Promise<{ ledger: L; bindings: RetireBindingRead[] }> => {
    const read = await cache.readRetireBindings(bare);
    assert.equal(read.state, "read");
    return {
      bindings: read.state === "read" ? read.bindings : [],
      // Config order across every branch, as the reclaim's evidence read sees it.
      ledger: gitIn(["config", "--local", "--null", "--list"]).split("\0").flatMap(item => {
        const nl = item.indexOf("\n");
        const m = nl < 0 ? null : /^uzi-attempts\.(.+)\.entry$/.exec(item.slice(0, nl));
        return m ? [{ branch: m[1]!, raw: item.slice(nl + 1) }] : [];
      }),
    };
  };
  const evalReal = async (ledger: L, bindings: RetireBindingRead[]) => {
    const evidence = { barePath: bare, bindings, ledger, protectedPaths: [], restoreBranches: [], recovery: [], stoppedRuns: [] };
    const runnerRoot = path.join(fx.dataDir, "runner");
    return {
      exam: evaluateRetireBinding({ evidence, uuid: UUID, runnerRoot }),
      recovery: evaluateRetireRecovery({ evidence, uuid: UUID, runnerRoot, expectRaw: bindings.find(b => b.uuid === UUID)!.raw, canonical: { present: false } }),
    };
  };

  for (const [name, over] of [["run id", { runId: "run-other" }], ["clone path", { clonePath: "/data/runner/slug/other" }]] as const) {
    it(`compaction preserves an earlier canonical entry with a different ${name} verbatim`, async () => {
      const input = [entry("live", over), entry("retired")];
      for (const v of input) addTo(LEDGER_KEY, v);
      bindSettled();
      await compactBranch(BRANCH);
      assert.deepEqual(ledgerValues(), input);
      const { ledger, bindings } = await realEvidence();
      assert.deepEqual(await evalReal(ledger, bindings), { exam: { kind: "keep", reason: "ledger_mismatch" }, recovery: "unresolved" });
    });
  }

  for (const order of [[BRANCH, OTHER_BRANCH], [OTHER_BRANCH, BRANCH]]) {
    it(`compaction keeps a cross-branch conflict, compacting ${order.join(" then ")}`, async () => {
      const live = entry("live");
      const ret = entry("retired");
      addTo(OTHER_KEY, live);
      addTo(LEDGER_KEY, ret);
      bindSettled();
      for (const br of order) await compactBranch(br);
      assert.deepEqual(valuesOf(OTHER_KEY), [live]);
      assert.deepEqual(valuesOf(LEDGER_KEY), [ret]);
      const { ledger, bindings } = await realEvidence();
      assert.deepEqual(await evalReal(ledger, bindings), { exam: { kind: "keep", reason: "ledger_mismatch" }, recovery: "unresolved" });
    });
  }

  // No binding yet when compaction runs: only the identity-count rule (over every branch) can keep the history.
  for (const order of [[BRANCH, OTHER_BRANCH], [OTHER_BRANCH, BRANCH]]) {
    it(`unbound: compaction keeps a gone cross-branch identity conflict, compacting ${order.join(" then ")}, for a later binding`, async () => {
      const live = entry("live");
      const ret = entry("retired");
      addTo(OTHER_KEY, live);
      addTo(LEDGER_KEY, ret);
      for (const br of order) await compactBranch(br);
      assert.deepEqual(valuesOf(OTHER_KEY), [live]);
      assert.deepEqual(valuesOf(LEDGER_KEY), [ret]);
      bindSettled();
      const { ledger, bindings } = await realEvidence();
      assert.deepEqual(await evalReal(ledger, bindings), { exam: { kind: "keep", reason: "ledger_mismatch" }, recovery: "unresolved" });
    });
  }

  it("unbound: compaction keeps a same-branch run id conflict verbatim, for a later binding", async () => {
    const input = [entry("live", { runId: "run-other" }), entry("retired")];
    for (const v of input) addTo(LEDGER_KEY, v);
    await compactBranch(BRANCH);
    assert.deepEqual(ledgerValues(), input);
    bindSettled();
    const { ledger, bindings } = await realEvidence();
    assert.deepEqual(await evalReal(ledger, bindings), { exam: { kind: "keep", reason: "ledger_mismatch" }, recovery: "unresolved" });
  });

  it("compaction keeps a gone entry whose run id differs from a decoded binding's", async () => {
    const only = entry("retired", { runId: "run-other" });
    addTo(LEDGER_KEY, only);
    bindSettled();
    await compactBranch(BRANCH);
    assert.deepEqual(ledgerValues(), [only]);
  });
});

// Compaction must not erase evidence another bare of the worker holds (retireCrossBareConflict refuses epoch reclaim on it).
describe("compactAttemptLedger keeps cross-bare evidence (#4263)", () => {
  const OTHER_ATTEMPT = formatAttemptId(new Date("2026-11-05T10:10:59Z"), 1, "0123456789abcdef");
  const Y_CLONE = "/data/runner/slug/y-gone-attempt";
  const gitAt = (b: string, args: string[]): string => execFileSync("git", ["-C", b, ...args], { encoding: "utf8", env: ENV }).trim();
  const valuesAt = (b: string): string[] => { try { return gitAt(b, ["config", "--local", "--get-all", LEDGER_KEY]).split("\n"); } catch { return []; } };
  const addAt = (b: string, raw: string): void => { gitAt(b, ["config", "--local", "--add", LEDGER_KEY, raw]); };
  const xClone = (): string => attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT);
  const entryOf = (attemptId: string, runId: string, clonePath: string, state = "live"): string => JSON.stringify({ attemptId, runId, clonePath, state });
  const xRetired = (id = ATTEMPT): string => entryOf(id, "run-4263", xClone(), "retired");
  const yValue = (): string => entryOf(ATTEMPT, "run-y", Y_CLONE);
  const bindX = (): void => setBinding(UUID, retireBindingValue(binding({ class: "owner_terminal", phase: "settled", clonePath: xClone() })));
  let y: string;
  beforeEach(() => {
    y = path.join(fx.dataDir, "repos", "y.git");
    execFileSync("git", ["init", "--bare", "-q", y], { env: ENV });
    fs.chmodSync(y, 0o700);
  });
  type Warn = { msg: string; fields: unknown };
  const cacheWith = (opts: GitCacheOptionsForTest = {}): { c: GitCache; warns: Warn[] } => {
    const warns: Warn[] = [];
    const log = nullLogger();
    log.warn = (msg: string, fields?: unknown) => { warns.push({ msg, fields }); };
    return { c: new GitCache(fx.dataDir, log, undefined, testGitCacheOptions(opts)), warns };
  };
  type GitCacheOptionsForTest = NonNullable<ConstructorParameters<typeof GitCache>[3]>;
  const compactOn = (c: GitCache, b: string): Promise<void> =>
    (c as unknown as { compactAttemptLedger(b: string, br: string): Promise<void> }).compactAttemptLedger(b, BRANCH);
  const SKIP = "attempt ledger compaction skipped: foreign retire evidence unavailable";
  const skipReasons = (warns: Warn[]): unknown[] => warns.filter(w => w.msg === SKIP).map(w => (w.fields as { reason: string }).reason);

  for (const order of [["x", "y"], ["y", "x"]] as const) {
    for (const [label, value] of [["canonical", yValue], ["non-canonical (reordered)", () => JSON.stringify({ runId: "run-y", attemptId: ATTEMPT, clonePath: Y_CLONE, state: "live" })]] as const) {
      it(`keeps another bare's unbound ${label} value of a bound attempt verbatim, compacting ${order.join(" then ")}`, async () => {
        const ret = xRetired();
        addAt(bare, ret);
        bindX();
        addAt(y, value());
        const { c } = cacheWith();
        for (const w of order) await compactOn(c, w === "x" ? bare : y);
        assert.deepEqual(valuesAt(y), [value()]);
        assert.deepEqual(valuesAt(bare), [ret]);
      });
    }
  }

  it("positive control: a different attempt id in the other bare does not stop normal compaction", async () => {
    addAt(bare, xRetired(OTHER_ATTEMPT));
    addAt(y, yValue());
    const { c, warns } = cacheWith();
    await compactOn(c, y);
    assert.deepEqual(valuesAt(y), [], "the gone, unbound, unshared value is dropped");
    assert.deepEqual(skipReasons(warns), []);
  });

  describe("falls back to no compaction, with the ledger byte-identical and a reason", () => {
    const expectSkipped = async (c: GitCache, warns: Warn[], reason: string): Promise<void> => {
      const before = fs.readFileSync(path.join(y, "config"));
      await compactOn(c, y);
      assert.ok(fs.readFileSync(path.join(y, "config")).equals(before), "the bare config is byte-identical");
      assert.deepEqual(valuesAt(y), [yValue()]);
      assert.deepEqual(skipReasons(warns), [reason]);
    };

    it("another bare has an undecodable binding", async () => {
      setBinding(UUID, "{");
      addAt(y, yValue());
      const { c, warns } = cacheWith();
      await expectSkipped(c, warns, "binding_undecodable");
    });

    it("another bare has an unknown (over-cap) binding set", async () => {
      for (let i = 0; i <= RETIRE_BINDING_MAX_PER_BARE; i++) setBinding(randomUUID(), retireBindingValue(binding()));
      addAt(y, yValue());
      const { c, warns } = cacheWith();
      await expectSkipped(c, warns, "binding_unknown");
    });

    for (const raw of ["{}", '{"attemptId":42}', "not json"]) {
      it(`another bare holds a ledger value whose attempt cannot be identified: ${raw}`, async () => {
        addAt(bare, raw);
        addAt(y, yValue());
        const { c, warns } = cacheWith();
        await expectSkipped(c, warns, "ledger_unidentified");
      });
    }

    for (const [name, make] of [
      ["a symlink named z.git", (): void => { fs.symlinkSync(bare, path.join(fx.dataDir, "repos", "z.git")); }],
      ["a regular file named f.git", (): void => { fs.writeFileSync(path.join(fx.dataDir, "repos", "f.git"), "x"); }],
    ] as const) {
      it(`an unreadable candidate: ${name}`, async () => {
        make();
        addAt(y, yValue());
        const { c, warns } = cacheWith();
        await expectSkipped(c, warns, "unreadable");
      });
    }

    it("the bare limit, lowered by the seam", async () => {
      addAt(y, yValue());
      const { c, warns } = cacheWith({ compactForeignScanForTest: { maxBares: 0 } });
      await expectSkipped(c, warns, "bare_cap");
    });

    it("the config byte limit, lowered by the seam", async () => {
      addAt(y, yValue());
      const { c, warns } = cacheWith({ compactForeignScanForTest: { maxConfigBytes: 8 } });
      await expectSkipped(c, warns, "bytes_cap");
    });

    it("the record limit, lowered by the seam", async () => {
      addAt(bare, xRetired(OTHER_ATTEMPT));
      addAt(bare, xRetired(OTHER_ATTEMPT));
      addAt(y, yValue());
      const { c, warns } = cacheWith({ compactForeignScanForTest: { maxRecords: 1 } });
      await expectSkipped(c, warns, "records_cap");
    });

    it("the record limit counts every foreign config entry, not only uzi records", async () => {
      addAt(y, yValue());
      // y holds one uzi record but several ordinary entries (core.*) from git init --bare.
      const { c, warns } = cacheWith({ compactForeignScanForTest: { maxRecords: 2 } });
      await expectSkipped(c, warns, "records_cap");
    });

    it("an expired deadline", async () => {
      addAt(y, yValue());
      let t = 0;
      const { c, warns } = cacheWith({ compactForeignScanForTest: { deadlineMs: 1000, now: () => (t += 10_000) } });
      await expectSkipped(c, warns, "deadline");
    });

    it("the seam can only lower a limit: a raised deadline is ignored, the production budget still expires", async () => {
      addAt(y, yValue());
      let t = 0;
      const { c, warns } = cacheWith({ compactForeignScanForTest: { deadlineMs: 1e9, now: () => (t += 10_000) } });
      await expectSkipped(c, warns, "deadline");
    });
  });

  describe("an own value no attempt can be attributed to (B1)", () => {
    const yBinding = (): void => { gitAt(y, ["config", "--local", retireBindingKey(UUID), retireBindingValue(binding())]); };
    const UNATTRIBUTABLE = ["not json", "{}", "", '{"attemptId":42}'];

    for (const raw of UNATTRIBUTABLE) {
      it(`survives verbatim next to a foreign binding: ${JSON.stringify(raw)}`, async () => {
        addAt(bare, raw);
        yBinding();
        const { c, warns } = cacheWith();
        await compactOn(c, bare);
        assert.deepEqual(skipReasons(warns), []);
        assert.deepEqual(valuesAt(bare), [raw]);
      });

      it(`is dropped as before when no other bare holds a binding: ${JSON.stringify(raw)}`, async () => {
        addAt(bare, raw);
        addAt(y, entryOf(OTHER_ATTEMPT, "run-y", Y_CLONE));
        const { c, warns } = cacheWith();
        await compactOn(c, bare);
        assert.deepEqual(skipReasons(warns), []);
        assert.deepEqual(valuesAt(bare), []);
      });
    }

    it("all of them survive together, in order, while an identified value of a different attempt is dropped", async () => {
      const other = JSON.stringify({ attemptId: OTHER_ATTEMPT, state: "future" });
      for (const raw of ["not json", other, "", "{}"]) addAt(bare, raw);
      yBinding();
      const { c } = cacheWith();
      await compactOn(c, bare);
      assert.deepEqual(valuesAt(bare), ["not json", "", "{}"]);
    });

    it("a non-canonical value naming the foreign id survives (the foreign-id clause alone)", async () => {
      const named = JSON.stringify({ attemptId: ATTEMPT, state: "future" });
      addAt(bare, named);
      yBinding();
      const { c } = cacheWith();
      await compactOn(c, bare);
      assert.deepEqual(valuesAt(bare), [named]);
    });
  });

  describe("interleaving at compact-foreign-scanned", () => {
    it("a foreign mention first written after the snapshot read is outside the guarantee", async () => {
      addAt(y, yValue());
      const { c } = cacheWith({
        retireBindingStep: step => { if (step === "compact-foreign-scanned") addAt(bare, xRetired()); },
      });
      await compactOn(c, y);
      assert.deepEqual(valuesAt(y), [], "the mention was absent from the snapshot, so the droppable value is dropped");
    });

    it("a mention written at that step of an attempt that WAS in the snapshot is preserved", async () => {
      addAt(bare, xRetired());
      addAt(y, yValue());
      const { c } = cacheWith({
        retireBindingStep: step => { if (step === "compact-foreign-scanned") { addAt(bare, entryOf(ATTEMPT, "run-late", "/data/runner/slug/late", "retired")); bindX(); } },
      });
      await compactOn(c, y);
      assert.deepEqual(valuesAt(y), [yValue()]);
    });
  });
});

describe("syncDirectory", () => {
  it("syncs a directory and refuses a symlink", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-syncdir-"));
    try {
      await syncDirectory(dir);
      const link = path.join(dir, "link");
      fs.symlinkSync(dir, link);
      await assert.rejects(syncDirectory(link));
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });
});

describe("retireCrossBareConflict", () => {
  const OTHER = formatAttemptId(new Date("2026-11-05T10:10:59Z"), 1, "0123456789abcdef");
  const bare = (over: Partial<RetireScratchBareEvidence> = {}): RetireScratchBareEvidence => ({
    barePath: "/data/repos/other.git", bindings: [], ledger: [], protectedPaths: [], restoreBranches: [], recovery: [], stoppedRuns: [], ...over,
  });
  const ledgerOf = (...raws: string[]) => bare({ ledger: raws.map(raw => ({ branch: "agent/other", raw })) });
  const canonical = (attemptId: string) => JSON.stringify({ attemptId, runId: "other-run", clonePath: "/data/runner/other/attempt", state: "retired" });

  it("no other bares, or empty ones, never conflict", () => {
    assert.equal(retireCrossBareConflict([], ATTEMPT), false);
    assert.equal(retireCrossBareConflict([bare()], ATTEMPT), false);
  });
  it("an undecodable binding entry is a hit whatever its raw", () => {
    for (const raw of ["", "{}", "garbage"])
      assert.equal(retireCrossBareConflict([bare({ bindings: [{ key: "uzi-retire-scratch.x.binding", raw }] })], ATTEMPT), true, raw);
  });
  it("a decoded binding naming the attempt is a hit, one naming another attempt is not", () => {
    const named = (attemptId: string): RetireBindingRead => ({ key: retireBindingKey(UUID), uuid: UUID, raw: "r", parsed: binding({ attemptId }) });
    assert.equal(retireCrossBareConflict([bare({ bindings: [named(ATTEMPT)] })], ATTEMPT), true);
    assert.equal(retireCrossBareConflict([bare({ bindings: [named(OTHER)] })], ATTEMPT), false);
  });
  it("a canonical ledger value naming the attempt is a hit, one naming a different attempt is not", () => {
    assert.equal(retireCrossBareConflict([ledgerOf(canonical(ATTEMPT))], ATTEMPT), true);
    assert.equal(retireCrossBareConflict([ledgerOf(canonical(OTHER))], ATTEMPT), false);
  });
  it("a non-canonical object with a different well-formed attempt id is not a hit", () => {
    assert.equal(retireCrossBareConflict([ledgerOf(JSON.stringify({ attemptId: OTHER, extra: 1 }))], ATTEMPT), false);
  });
  it("non-canonical values naming the attempt, literally or by JSON escape, are hits", () => {
    assert.equal(retireCrossBareConflict([ledgerOf(JSON.stringify({ attemptId: ATTEMPT, extra: 1 }))], ATTEMPT), true);
    assert.equal(retireCrossBareConflict([ledgerOf(`{"attemptId":"${ATTEMPT.replace(/^./, c => `\\u00${c.charCodeAt(0).toString(16)}`)}"}`)], ATTEMPT), true);
    assert.equal(retireCrossBareConflict([ledgerOf(`x ${ATTEMPT}`)], ATTEMPT), true);
  });
  it("values not positively identified as a different attempt are hits", () => {
    for (const raw of ["not json", "{}", "null", "[]", `[{"attemptId":"${OTHER}"}]`, '{"attemptId":42}', '{"attemptId":"bad id"}', '{"attemptId":null}', ""])
      assert.equal(retireCrossBareConflict([ledgerOf(raw)], ATTEMPT), true, raw);
  });
  it("one hit among several bares and values is enough", () => {
    assert.equal(retireCrossBareConflict([bare(), ledgerOf(canonical(OTHER), "{}")], ATTEMPT), true);
  });
});
