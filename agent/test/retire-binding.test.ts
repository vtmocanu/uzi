import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  GitCache, RETIRE_BINDING_MAX_PER_BARE, parseRetireBinding, parseRetireBindingKey, retireBindingKey, retireBindingValue,
  type RetireBinding,
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
  clonePath: "/data/runner/slug/issue-4263.attempt-x", phase: "intent", parentIno: "131072", cloneIno: "0", pod: "worker-0", ...over,
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
    assert.deepEqual(read, { state: "read", bindings: [{ uuid: UUID, raw: "" }] });
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

  it("still drops an entry when the binding names a different run or path", async () => {
    const clonePath = addRetired();
    setBinding(UUID, retireBindingValue(binding({ clonePath, runId: "run-other" })));
    setBinding(randomUUID(), retireBindingValue(binding({ clonePath: `${clonePath}-x` })));
    await compact();
    assert.deepEqual(ledgerValues(), []);
  });

  it("a malformed binding names nothing", async () => {
    addRetired();
    setBinding(UUID, "{");
    await compact();
    assert.deepEqual(ledgerValues(), []);
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
