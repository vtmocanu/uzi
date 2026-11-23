import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, type FetchAgentBranchOptions, type TrackingUpdateResult } from "../src/git.js";

// issue #4627: retention refs (owed pins, refused-head refs) are only ever created, never moved, and
// owedTipRetained is the write-free twin of retainCurrentOwedCandidate. Real Git throughout.
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-4627";
const RUN = "46274627-aaaa-bbbb-cccc-444444444444";
const CAP = 6;
const ZERO = "0".repeat(40);
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
let seq = 0;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}
function commit(label: string, parents: string[] = [base]): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, [...IDENT, "commit-tree", tree, ...parents.flatMap((p) => ["-p", p]), "-m", `${label} ${seq++}`]);
  gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  return sha;
}
const fetchHead = (): Promise<TrackingUpdateResult> => cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
const owedRef = (sha: string): string => `refs/uzi-owed/${RUN}/${sha}`;
const refusedRef = (sha: string): string => `refs/uzi-incoming/${RUN}/1/refused-${sha}`;
const refDump = (): string => gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
const dirSnapshot = (dir: string): string => !fs.existsSync(dir) ? "[]" : JSON.stringify(fs.readdirSync(dir).sort().map((n) => {
  const full = path.join(dir, n);
  return [n, fs.statSync(full).isFile() ? fs.readFileSync(full, "utf8") : "<dir>"];
}));
const owedDir = (): string => path.join(bare, "uzi-owed");
const refusalDir = (): string => path.join(bare, "info", "uzi-refusals");
/** Everything a retention write could touch: refs, config, owed and refusal metadata. */
const everything = (): string => JSON.stringify([refDump(), fs.readFileSync(path.join(bare, "config"), "utf8"),
  dirSnapshot(owedDir()), dirSnapshot(refusalDir())]);
const candidateFile = (sha: string): string => fs.readdirSync(owedDir()).find((n) => n.startsWith(`candidate-${RUN}-${sha}-`))!;

type Internals = {
  runGit(dir: string, args: string[]): Promise<string>;
  writeOwedFile(bare: string, name: string, value: unknown, ...rest: unknown[]): Promise<void>;
  assertBareClear(bare: string, op: string): Promise<void>;
  log: { warn: (message: string, fields?: unknown) => void };
  refusalRecordingFailed: Set<string>;
};
const internals = (): Internals => cache as unknown as Internals;
/** Records, in order, every update-ref and every owed-file write the cache performs. */
function recordEvents(): { events: string[]; updateRefs: string[][]; restore: () => void } {
  const i = internals();
  const events: string[] = [];
  const updateRefs: string[][] = [];
  const runGit = i.runGit, write = i.writeOwedFile;
  i.runGit = function (this: unknown, dir: string, args: string[]) {
    if (args[0] === "update-ref") { events.push("update-ref"); updateRefs.push(args); }
    return runGit.call(this, dir, args);
  };
  i.writeOwedFile = function (this: unknown, b: string, name: string, ...rest: unknown[]) {
    events.push("write:" + name.replace(/-.*/, ""));
    return write.call(this, b, name, ...(rest as [unknown]));
  } as typeof i.writeOwedFile;
  return { events, updateRefs, restore: () => { i.runGit = runGit; i.writeOwedFile = write; } };
}
function captureWarnings(): string[] {
  const warnings: string[] = [];
  internals().log = { ...internals().log, warn: (message: string, fields?: unknown) => { warnings.push(message + " " + JSON.stringify(fields)); } };
  return warnings;
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP }));
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-4627", noProofReseed, RUN)).path;
  opts = { context: { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
});
afterEach(() => {
  if (fs.existsSync(refusalDir())) fs.chmodSync(refusalDir(), 0o700);
  fx.cleanup();
});

/** A head H owned by this run as the tracking head, with its owed pin and candidate record. */
async function trackedHead(): Promise<string> {
  const h = commit("H");
  assert.equal((await fetchHead()).kind, "updated");
  assert.equal(gitIn(bare, ["rev-parse", owedRef(h)]), h);
  return h;
}

describe("issue4627 retention ref guard: owed pin", () => {
  it("creates an absent pin with exactly update-ref --no-deref REF SHA ZERO, after the candidate write", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    fs.unlinkSync(path.join(owedDir(), candidateFile(h)));
    const spy = recordEvents();
    try {
      const result = await cache.retainCurrentOwedCandidate(bare, opts);
      assert.equal(result.kind, "updated");
    } finally { spy.restore(); }
    assert.deepEqual(spy.updateRefs, [["update-ref", "--no-deref", owedRef(h), h, ZERO]]);
    assert.ok(spy.events.indexOf("write:candidate") >= 0 && spy.events.indexOf("write:candidate") < spy.events.indexOf("update-ref"),
      "the candidate record is written before the owed ref exists: " + spy.events.join(","));
    assert.equal(gitIn(bare, ["rev-parse", owedRef(h)]), h);
  });

  it("reuses a same-SHA pin without any update-ref and changes nothing", async () => {
    await trackedHead();
    const before = everything();
    const spy = recordEvents();
    try { assert.equal((await cache.retainCurrentOwedCandidate(bare, opts)).kind, "updated"); }
    finally { spy.restore(); }
    assert.deepEqual(spy.updateRefs, []);
    assert.equal(everything(), before);
  });

  it("a wrong-target pin is left untouched: not_updated, one warning, no marker, context or candidate write", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    fs.unlinkSync(path.join(owedDir(), candidateFile(h)));
    gitIn(bare, ["update-ref", owedRef(h), base]);
    const before = everything();
    const warnings = captureWarnings();
    const spy = recordEvents();
    try { assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "preservation_failed" }); }
    finally { spy.restore(); }
    assert.deepEqual(spy.events, [], "no write of any kind");
    assert.equal(gitIn(bare, ["rev-parse", owedRef(h)]), base);
    assert.equal(everything(), before);
    assert.equal(warnings.length, 1);
    assert.match(warnings[0]!, /retention ref target mismatch; ref left untouched/);
    assert.ok(warnings[0]!.includes(owedRef(h)) && warnings[0]!.includes(base));
  });

  it("a non-commit wrong target is refused the same way and reads as unretained", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    const tree = gitIn(bare, ["rev-parse", `${h}^{tree}`]);
    gitIn(bare, ["update-ref", owedRef(h), tree]);
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" });
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "preservation_failed" });
    assert.equal(gitIn(bare, ["rev-parse", owedRef(h)]), tree);
    assert.equal(everything(), before);
  });

  it("ordering: a faulted candidate write leaves no owed ref", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    fs.unlinkSync(path.join(owedDir(), candidateFile(h)));
    const i = internals();
    const write = i.writeOwedFile;
    i.writeOwedFile = async function (this: unknown, b: string, name: string, ...rest: unknown[]) {
      if (name.startsWith("candidate-")) throw new Error("injected metadata I/O failure");
      return write.call(this, b, name, ...(rest as [unknown]));
    } as typeof i.writeOwedFile;
    try { assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "preservation_failed" }); }
    finally { i.writeOwedFile = write; }
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname)", owedRef(h)]), "");
  });

  it("a second public pin path (the fetch import) leaves a wrong-target pin untouched", async () => {
    const h = await trackedHead();
    const h2 = commit("H2", [h]);
    gitIn(bare, ["update-ref", owedRef(h2), base]);
    const result = await fetchHead();
    assert.deepEqual(result, { kind: "not_updated", reason: "preservation_failed" });
    assert.equal(gitIn(bare, ["rev-parse", owedRef(h2)]), base);
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), h, "the tracking ref did not move");
  });
});

describe("issue4627 retention ref guard: refusal ref", () => {
  /** At the cap with the tracking head's pin removed, so pinning it again hits the cap. */
  async function atCapUnpinned(): Promise<string> {
    let last = base;
    for (let i = 0; i < CAP; i++) {
      last = commit("rewrite");
      const r = await fetchHead();
      assert.equal(r.kind, "updated", JSON.stringify(r));
    }
    assert.equal(await cache.admitRefusalProvenance(opts.context as never), "written");
    gitIn(bare, ["update-ref", "-d", owedRef(last)]);
    gitIn(bare, ["update-ref", owedRef("b".repeat(40)), base]);
    return last;
  }

  it("creates the refused ref with the create-only form", async () => {
    const tip = await atCapUnpinned();
    const spy = recordEvents();
    try { assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" }); }
    finally { spy.restore(); }
    assert.deepEqual(spy.updateRefs, [["update-ref", "--no-deref", refusedRef(tip), tip, ZERO]]);
    assert.equal(gitIn(bare, ["rev-parse", refusedRef(tip)]), tip);
  });

  it("a wrong-target refused ref stays untouched and the existing fail-closed latch and incomplete marker follow", async () => {
    const tip = await atCapUnpinned();
    gitIn(bare, ["update-ref", refusedRef(tip), base]);
    const warnings = captureWarnings();
    assert.deepEqual(await cache.retainCurrentOwedCandidate(bare, opts), { kind: "not_updated", reason: "owed_limit" });
    assert.equal(gitIn(bare, ["rev-parse", refusedRef(tip)]), base);
    assert.equal(internals().refusalRecordingFailed.has(RUN), true);
    assert.ok(fs.existsSync(path.join(refusalDir(), `incomplete-${RUN}.json`)));
    const ledgerFile = path.join(refusalDir(), `refusals-${RUN}.json`);
    assert.ok(fs.existsSync(ledgerFile), "the ledger row step still runs");
    const ledger = JSON.parse(fs.readFileSync(ledgerFile, "utf8")) as { entries: { sha: string }[] };
    assert.ok(ledger.entries.some((e) => e.sha === tip), "the ledger records the refused tip: " + JSON.stringify(ledger.entries));
    assert.ok(warnings.some((w) => /retention ref target mismatch; ref left untouched/.test(w) && w.includes(refusedRef(tip))));
  });
});

describe("issue4627 owedTipRetained", () => {
  it("is not_updated when the tracking ref is not owned by this run", async () => {
    commit("H");
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(everything(), before);
  });

  it("is not_updated when the producing context cannot be read", async () => {
    await trackedHead();
    const i = cache as unknown as { producingContext: () => Promise<never> };
    const original = i.producingContext;
    i.producingContext = async () => { throw new Error("producing context mismatch"); };
    const before = everything();
    try {
      const twin = await cache.retainCurrentOwedCandidate(bare, opts);
      assert.deepEqual(await cache.owedTipRetained(bare, opts), twin);
      assert.deepEqual(twin, { kind: "not_updated", reason: "preservation_failed" });
    } finally { i.producingContext = original; }
    assert.equal(everything(), before);
  });

  it("is not_updated for a sealed generation", async () => {
    await trackedHead();
    fs.mkdirSync(refusalDir(), { recursive: true, mode: 0o700 });
    fs.writeFileSync(path.join(refusalDir(), `final-${RUN}-1.json`), "{}\n", { mode: 0o600 });
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(everything(), before);
  });

  it("is updated with no pin and no write when the tip is remotely covered", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    const before = everything();
    const result = await cache.owedTipRetained(bare, { ...opts, remotelyConfirmedSha: h });
    assert.equal(result.kind, "updated");
    assert.equal(everything(), before);
  });

  it("an exact pin with its candidate record and context is updated, writing nothing", async () => {
    const h = await trackedHead();
    const before = everything();
    const spy = recordEvents();
    let result;
    try { result = await cache.owedTipRetained(bare, opts); } finally { spy.restore(); }
    assert.equal(result.kind, "updated");
    assert.ok(result.kind === "updated");
    assert.deepEqual(result.retainedShas, [h]);
    assert.deepEqual(spy.events, []);
    assert.equal(everything(), before);
  });

  it("a pin without its candidate record is unretained, writing nothing", async () => {
    const h = await trackedHead();
    fs.unlinkSync(path.join(owedDir(), candidateFile(h)));
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" });
    assert.equal(everything(), before);
  });

  it("an exact pin and candidate record with the owed context file deleted is not retained, writing nothing", async () => {
    await trackedHead();
    for (const n of fs.readdirSync(owedDir()).filter((x) => x.startsWith("context-"))) fs.unlinkSync(path.join(owedDir(), n));
    const before = everything();
    const spy = recordEvents();
    let result;
    try { result = await cache.owedTipRetained(bare, opts); } finally { spy.restore(); }
    // Ownership itself needs the context file, so this is refused before the retention comparison.
    assert.deepEqual(result, { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(spy.events, []);
    assert.equal(everything(), before);
  });

  it("an exact pin and candidate record with the owed context file differing from the canonical record is unretained, writing nothing", async () => {
    await trackedHead();
    const names = fs.readdirSync(owedDir()).filter((x) => x.startsWith("context-"));
    assert.ok(names.length > 0);
    for (const n of names) {
      const file = path.join(owedDir(), n);
      const record = JSON.parse(fs.readFileSync(file, "utf8"));
      // Same fields and values, other key order: still parses and still names the same context, but is not the stored form.
      fs.writeFileSync(file, JSON.stringify(Object.fromEntries(Object.entries(record).reverse())) + "\n", { mode: 0o600 });
    }
    const before = everything();
    const spy = recordEvents();
    try { assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" }); } finally { spy.restore(); }
    assert.deepEqual(spy.events, []);
    assert.equal(everything(), before);
  });

  it("a tip reachable only through an episode ref is unretained, writing nothing", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", "-d", owedRef(h)]);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-episode/${RUN}/1`, h]);
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" });
    assert.equal(everything(), before);
  });

  it("a pin at the wrong target is unretained, writing nothing", async () => {
    const h = await trackedHead();
    gitIn(bare, ["update-ref", owedRef(h), base]);
    const before = everything();
    assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" });
    assert.equal(everything(), before);
  });

  it("a refused bare-clear check is unretained, writing nothing", async () => {
    await trackedHead();
    const i = internals();
    const original = i.assertBareClear;
    i.assertBareClear = async () => { throw new Error("authority refuses"); };
    const before = everything();
    try { assert.deepEqual(await cache.owedTipRetained(bare, opts), { kind: "unretained" }); }
    finally { i.assertBareClear = original; }
    assert.equal(everything(), before);
  });
});
