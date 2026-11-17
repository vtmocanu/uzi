import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { enumerateAttributedPaths } from "../src/restore-boundary.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { nullLogger } from "./helpers.js";

// issue #4263 M1 frozen old-reader proof. This file writes RAW config keys only (no codec import) and calls only APIs that
// exist before the binding feature, so it can be run unchanged against a base export. What it pins for a bare config with
// `uzi-retire-scratch.<uuid>.binding` keys:
//  - the attribution readers ignore the `uzi-retire-scratch.` prefix entirely;
//  - `collectEpochScratchProtectedPaths` absorbs absolute paths found in the JSON value into its protected set, and returns
//    `unknown` on a valueless `uzi-*` key (both fail closed);
//  - binding entries count toward existing readers' config-entry caps like any other entry (the ACK reader's cap is 1024).

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const BRANCH = "agent/issue-4263";
const KEY = "issue-4263";
const RUN = "run-4263";
const ATTEMPT = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const BINDING_CLONE = "/data/runner/slug/issue-9.attempt-bound";
const UUIDS = Array.from({ length: 4 }, (_, i) => `2b2b2b2b-3c3c-4d4d-8e8e-${String(i).padStart(12, "0")}`);
const bindingRaw = (uuid: string, phase: string): string => JSON.stringify({
  v: 1, class: "completion", attemptId: ATTEMPT, runId: "run-other", branch: "agent/issue-9", clonePath: BINDING_CLONE,
  phase, parentIno: "131072", cloneIno: "0", pod: uuid.slice(-4),
});

let root: string;
let cache: GitCache;
let bare: string;
let clonePath: string;
const gitIn = (args: string[]): string => execFileSync("git", ["-C", bare, ...args], { encoding: "utf8", env: ENV }).trim();
const setKey = (key: string, value: string, add = false): void => { gitIn(["config", "--local", ...(add ? ["--add"] : []), key, value]); };
const budget = () => ({ signal: new AbortController().signal, deadline: Date.now() + 60_000 });

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-old-readers-"));
  fs.chmodSync(root, 0o700);
  const reposRoot = path.join(root, "repos");
  fs.mkdirSync(reposRoot, { mode: 0o700 });
  bare = path.join(reposRoot, "r1.git");
  execFileSync("git", ["init", "--bare", "--quiet", bare], { env: ENV });
  fs.chmodSync(bare, 0o700);
  cache = new GitCache(root, nullLogger());
  clonePath = attemptClonePath(cache.runnerClonePath(bare, KEY), ATTEMPT);
  setKey(`uzi-recovery.${BRANCH}.clone`, JSON.stringify({ runId: RUN, clonePath, attemptId: ATTEMPT }));
  setKey(`uzi-attempts.${BRANCH}.entry`, JSON.stringify({ attemptId: ATTEMPT, runId: RUN, clonePath, state: "live" }), true);
  setKey("user.name", "ignored");
});
afterEach(() => fs.rmSync(root, { recursive: true, force: true }));

const addBindings = (): void => { UUIDS.forEach((u, i) => setKey(`uzi-retire-scratch.${u}.binding`, bindingRaw(u, ["intent", "settled", "removing", "intent"][i]!))); };
const stable = (v: unknown): string => JSON.stringify(v);

/** Run `read` on the bare without bindings, then with them, and return both answers. */
async function beforeAfter<T>(read: () => Promise<T>): Promise<[T, T]> {
  const before = await read();
  addBindings();
  return [before, await read()];
}

describe("existing readers ignore uzi-retire-scratch binding keys", () => {
  it("attribution config and the census path enumeration are unchanged", async () => {
    const [a, b] = await beforeAfter(() => cache.readBareAttributionConfig(bare));
    assert.deepEqual(b, a);
    assert.equal(b.complete, true);
    const paths = (r: typeof a) => enumerateAttributedPaths(r.entries, r.complete);
    assert.deepEqual(paths(b), paths(a));
    assert.equal(stable(paths(b)).includes(BINDING_CLONE), false);
  });

  it("the retained-source gather (epoch discovery's per-bare read) is unchanged", async () => {
    const gather = () => cache.withBareLock(bare, () =>
      (cache as unknown as { gatherRetainedUnderLock(b: string): Promise<unknown> }).gatherRetainedUnderLock(bare));
    const [a, b] = await beforeAfter(gather);
    assert.equal(stable(b), stable(a));
  });

  it("the terminal attribution read is unchanged", async () => {
    const read = () => cache.readTerminalAttribution(bare, RUN, budget());
    const [a, b] = await beforeAfter(read);
    assert.deepEqual(b, a);
  });

  it("the branch journal resolver and discovery are unchanged", async () => {
    const [a, b] = await beforeAfter(() => cache.resolveBranchJournal(bare, BRANCH));
    assert.deepEqual(b, a);
    const [c, d] = await beforeAfter(() => cache.discoverOpaque(bare, BRANCH, RUN));
    assert.deepEqual(d, c);
  });

  it("the acknowledged-report reader is unchanged, and binding keys count like any other entry toward its 1024 cap", async () => {
    const read = () => cache.withReportProofBudget(budget(), () =>
      cache.withBareLock(bare, () => cache.readAcknowledgedReportSourcesUnderLock(bare, RUN)));
    // The snapshot embeds the config bytes and file identity, which legitimately differ; the sources must not.
    const view = (r: Awaited<ReturnType<typeof read>>) => r.kind === "verified" ? { kind: r.kind, heads: r.heads, clones: r.clones } : r;
    const [a, b] = await beforeAfter(read);
    assert.deepEqual(view(b), view(a));
    assert.equal(a.kind, "verified");
    const total = (): number => gitIn(["config", "--local", "--list"]).split("\n").filter(Boolean).length;
    // Fill to exactly the cap with neutral keys: still read. One more entry: refused, bindings or not.
    let n = 0;
    while (total() < 1024) setKey(`uzi-filler.k${n++}.v`, "x");
    assert.equal(total(), 1024);
    assert.equal((await read()).kind, a.kind);
    setKey("uzi-filler.over.v", "x");
    const over = await read();
    assert.equal(over.kind, "unknown");
    for (const u of UUIDS) gitIn(["config", "--local", "--unset", `uzi-retire-scratch.${u}.binding`]);
    assert.equal(await read().then(r => r.kind), a.kind, "dropping the bindings below the cap restores the answer");
  });

  it("the epoch scratch protected-path read stays readable and adds only the binding's own clone path", async () => {
    const collect = () => cache.collectEpochScratchProtectedPaths(budget());
    const [a, b] = await beforeAfter(collect);
    assert.equal(a.state, "read");
    assert.equal(b.state, "read");
    if (a.state === "read" && b.state === "read") assert.deepEqual(b.paths, [...new Set([...a.paths, BINDING_CLONE])].sort());
  });

  it("a malformed or valueless binding key is also ignored by the attribution readers", async () => {
    const before = await cache.readBareAttributionConfig(bare);
    setKey(`uzi-retire-scratch.${UUIDS[0]}.binding`, "not json");
    fs.appendFileSync(path.join(bare, "config"), `[uzi-retire-scratch "${UUIDS[1]}"]\n\tbinding\n`);
    assert.deepEqual(await cache.readBareAttributionConfig(bare), before);
    const gathered = await cache.withBareLock(bare, () =>
      (cache as unknown as { gatherRetainedUnderLock(b: string): Promise<{ records: unknown[] }> }).gatherRetainedUnderLock(bare));
    // Only non-throwing is asserted for the gather: the malformed and valueless keys must not break it.
    assert.ok(Array.isArray(gathered.records));
    // The epoch scratch reader is the one old reader that does look at `uzi-*` keys: a valueless one fails closed.
    const collected = await cache.collectEpochScratchProtectedPaths(budget());
    assert.equal(collected.state, "unknown");
  });
});
