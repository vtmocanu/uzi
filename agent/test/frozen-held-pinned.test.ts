import assert from "node:assert/strict";
import { after, describe, it, type TestContext } from "node:test";
import { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { prepareFrozenHeld, prepareFrozenHeldPinned, validFrozenHeldPolicy, type FrozenHeldPolicy, type HeldGit } from "../src/frozen-held.js";
import { parseCheckpointRange, serializeCheckpointRange } from "../src/terminal-range.js";
import type { CheckpointRange } from "../src/git.js";

const tip = "a".repeat(40);
const floor = "b".repeat(40);
const range: CheckpointRange = { tipSha: tip, excludeSha: floor };
const base: FrozenHeldPolicy = { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] };

interface Opts {
  calls: string[];
  scan?: { trusted: boolean; findings: unknown[] };
  workflows?: string; paths?: string[] | null; tipOid?: string; pack?: string | null; code?: number;
  perCommit?: Array<{ commit: string; paths: string[] }> | null; digests?: boolean;
  scanDigest?: string | null; transportDigest?: string | null;
  packFn?: () => unknown; ancestry?: string;
}
function fake(o: Opts) {
  const c = o.calls;
  const git: Record<string, unknown> = {
    resolveCheckpointRange: async () => { c.push("resolve"); return range; },
    secretScanCheckpointRange: async () => { c.push("scan"); return o.scan ?? { trusted: true, findings: [] }; },
    ancestry: async () => { c.push("ancestry"); return o.ancestry ?? "divergent"; },
    rangeTouchesWorkflows: async () => { c.push("workflows"); return o.workflows ?? "clean"; },
    changedFilesNoRenames: async () => { c.push("tipCi"); return o.paths === undefined ? [] : o.paths; },
    checkpointPack: async () => {
      c.push("pack");
      if (o.packFn) return o.packFn();
      if (o.pack === null) return null;
      return { tipOid: o.tipOid ?? tip, pack: Readable.from([Buffer.from(o.pack ?? "PACK")]), exited: Promise.resolve(o.code ?? 0) };
    },
    changedCiPathsPerCommit: async () => { c.push("perCommit"); return o.perCommit === undefined ? [] : o.perCommit; },
    terminalScanDigest: async () => { c.push("scanDigest"); return o.scanDigest === undefined ? "s1" : o.scanDigest; },
    terminalTransportDigest: async () => { c.push("transportDigest"); return o.transportDigest === undefined ? "t1" : o.transportDigest; },
  };
  if (o.digests === false) { delete git.terminalScanDigest; delete git.terminalTransportDigest; }
  return git as unknown as HeldGit;
}
const run = (git: HeldGit, extra: Record<string, unknown> = {}, policy = base) =>
  prepareFrozenHeld({ literals: [] as string[], git, barePath: "bare", branch: "task", sourceSha: tip, policy, deadlineMs: 30_000, ...extra });

const refusals: Array<[string, Opts, Record<string, unknown>?, FrozenHeldPolicy?]> = [
  ["secret_found", { calls: [], scan: { trusted: true, findings: [{}] } }],
  ["scan_untrusted", { calls: [], scan: { trusted: false, findings: [] } }],
  ["secret_remediation_pending", { calls: [] }, undefined, { ...base, blocked: true }],
  ["secret_remediation_pending", { calls: [], ancestry: "ancestor" }, undefined, { ...base, flaggedCommits: [floor] }],
  ["secret_remediation_pending", { calls: [] }, { remediationHold: async () => true }],
  ["workflow_change", { calls: [], workflows: "touches" }],
  ["workflow_check_unknown", { calls: [], workflows: "unknown" }],
  ["ci_config_unknown", { calls: [], paths: null }, undefined, { ...base, ciGlobs: [".github/**"] }],
  ["ci_config_guard", { calls: [], paths: [".github/workflows/x.yml"] }, undefined, { ...base, ciGlobs: [".github/**"] }],
  ["ci_config_unknown", { calls: [] }, { ciHold: async () => "unknown" }],
  ["ci_config_guard", { calls: [] }, { ciHold: async () => "blocked" }],
  ["pack_tip_mismatch", { calls: [], pack: null }],
  ["pack_tip_mismatch", { calls: [], tipOid: "c".repeat(40) }],
  ["pack_too_large", { calls: [], pack: "0123456789" }, { maxBytes: 4 }],
  ["pack_failed", { calls: [], code: 1 }],
];
for (const [reason, o, extra, pol] of refusals) {
  it(`legacy refusal ${reason}`, async () => {
    const r = await run(fake(o), extra, pol);
    assert.deepEqual(r, { reason });
  });
}

it("legacy range_unresolved", async () => {
  const g = { resolveCheckpointRange: async () => null } as unknown as HeldGit;
  assert.deepEqual(await run(g), { reason: "range_unresolved" });
});

it("legacy success returns exact pack bytes, never calls per-commit CI or digests", async () => {
  const calls: string[] = [];
  const r = await run(fake({ calls, pack: "PACKBYTES" }), {}, { ...base, ciGlobs: ["x/**"] });
  assert.ok("pack" in r && r.pack.toString() === "PACKBYTES");
  assert.equal(Object.keys(r).join(), "pack");
  assert.deepEqual(calls, ["resolve", "scan", "workflows", "tipCi", "pack"]);
});

it("callback order: onScan, remediationHold, ciHold", async () => {
  const seq: string[] = [];
  await run(fake({ calls: seq }), {
    onScan: () => seq.push("onScan"), remediationHold: async () => { seq.push("remediationHold"); return false; },
    ciHold: async () => { seq.push("ciHold"); return "clear"; },
  });
  assert.deepEqual(seq, ["resolve", "scan", "onScan", "remediationHold", "workflows", "ciHold", "pack"]);
});

it("abort cancels pack and awaits exited before returning", async () => {
  let now = 0, cancelled = 0, settled = false, returned = false;
  let release!: () => void;
  const exited = new Promise<number>(r => { release = () => { settled = true; r(0); }; });
  const stream = new Readable({ read() { /* never ends */ } });
  const budget = new MaintenanceBudget(100, () => now);
  const calls: string[] = [];
  const g = fake({ calls, packFn: () => ({ tipOid: tip, pack: stream, cancel: () => { cancelled++; }, exited }) });
  try {
    const pending = MaintenanceBudget.run(budget, () => run(g)).then(() => "ok", () => { returned = true; assert.ok(settled); return "aborted"; });
    while (!calls.includes("pack")) await new Promise<void>(r => setImmediate(r));
    await new Promise<void>(r => setImmediate(r));
    now = 100; assert.throws(() => budget.check());
    await new Promise<void>(r => setImmediate(r));
    assert.equal(cancelled, 1);
    assert.equal(returned, false);
    release();
    assert.equal(await pending, "aborted");
  } finally { release(); budget.dispose(); stream.destroy(); }
});

it("one runProducer without a current budget; none when inherited; no leak after return", async t => {
  const spy = t.mock.method(MaintenanceBudget, "runProducer");
  assert.equal(MaintenanceBudget.current(), undefined);
  await run(fake({ calls: [] }));
  assert.equal(spy.mock.callCount(), 1);
  assert.equal(MaintenanceBudget.current(), undefined);
  await prepareFrozenHeldPinned({ literals: [], git: fake({ calls: [] }), barePath: "b", branch: "t", sourceSha: tip, policy: base, deadlineMs: 1000 }, range);
  assert.equal(spy.mock.callCount(), 2);
  assert.equal(MaintenanceBudget.current(), undefined);
  const budget = new MaintenanceBudget(600_000);
  try {
    await MaintenanceBudget.run(budget, async () => {
      await run(fake({ calls: [] }));
      await prepareFrozenHeldPinned({ literals: [], git: fake({ calls: [] }), barePath: "b", branch: "t", sourceSha: tip, policy: base, deadlineMs: 1000 }, "resolve");
      assert.equal(MaintenanceBudget.current(), budget);
    });
    assert.equal(spy.mock.callCount(), 2, "inherited budget creates no producer");
  } finally { budget.dispose(); }
});

const pin = (git: HeldGit, r: CheckpointRange | "resolve", extra: Record<string, unknown> = {}, policy = base) =>
  prepareFrozenHeldPinned({ literals: [], git, barePath: "bare", branch: "task", sourceSha: tip, policy, deadlineMs: 30_000, ...extra }, r);

it("pinned: resolve exactly once; supplied range never resolves; digests returned", async () => {
  const a: string[] = [];
  const r1 = await pin(fake({ calls: a }), "resolve");
  assert.ok("pack" in r1);
  assert.deepEqual(r1.range, range);
  assert.deepEqual(r1.digests, { scan: "s1", transport: "t1" });
  assert.equal(a.filter(x => x === "resolve").length, 1);
  assert.ok(a.indexOf("scanDigest") < a.indexOf("pack") && a.indexOf("pack") < a.indexOf("transportDigest"));
  const b: string[] = [];
  const r2 = await pin(fake({ calls: b }), range);
  assert.ok("pack" in r2);
  assert.ok(!b.includes("resolve"));
});

it("pinned: refusals carry the range; unresolved has none", async () => {
  const r = await pin(fake({ calls: [], workflows: "touches" }), range);
  assert.deepEqual(r, { reason: "workflow_change", range });
  const g = { resolveCheckpointRange: async () => null } as unknown as HeldGit;
  assert.deepEqual(await pin(g, "resolve"), { reason: "range_unresolved" });
});

it("pinned: digest unavailable and mismatch", async () => {
  assert.equal((await pin(fake({ calls: [], digests: false }), range) as { reason: string }).reason, "digest_unavailable");
  assert.equal((await pin(fake({ calls: [], scanDigest: null }), range) as { reason: string }).reason, "digest_unavailable");
  assert.equal((await pin(fake({ calls: [], transportDigest: null }), range) as { reason: string }).reason, "digest_unavailable");
  const ok = await pin(fake({ calls: [] }), range, { expectDigests: { scan: "s1", transport: "t1" } });
  assert.ok("pack" in ok);
  const calls: string[] = [];
  const bad = await pin(fake({ calls }), range, { expectDigests: { scan: "zz", transport: "t1" } }) as { reason: string };
  assert.equal(bad.reason, "digest_mismatch");
  assert.ok(!calls.includes("pack"), "scan mismatch stops before packing");
  const bad2 = await pin(fake({ calls: [] }), range, { expectDigests: { scan: "s1", transport: "zz" } }) as { reason: string };
  assert.equal(bad2.reason, "digest_mismatch");
});

it("pinned: perCommitCi catches an intermediate-commit CI change; default stays tip-only", async () => {
  const pol = { ...base, ciGlobs: [".github/**"] };
  const per = [{ commit: "c".repeat(40), paths: [".github/workflows/x.yml"] }];
  const calls: string[] = [];
  const g = fake({ calls, perCommit: per });
  assert.equal((await pin(g, range, { perCommitCi: true, ciTarget: floor }, pol) as { reason: string }).reason, "ci_config_guard");
  assert.ok(calls.includes("perCommit"));
  const c2: string[] = [];
  assert.ok("pack" in await pin(fake({ calls: c2, perCommit: per }), range, {}, pol));
  assert.ok(!c2.includes("perCommit"));
  assert.equal((await pin(fake({ calls: [] }), range, { perCommitCi: true }, pol) as { reason: string }).reason, "ci_config_unknown");
  assert.equal((await pin(fake({ calls: [], perCommit: null }), range, { perCommitCi: true, ciTarget: floor }, pol) as { reason: string }).reason, "ci_config_unknown");
  assert.ok("pack" in await pin(fake({ calls: [], perCommit: [{ commit: tip, paths: ["src/a.ts"] }] }), range, { perCommitCi: true, ciTarget: floor }, pol));
});

it("pinned: perCommitCi hands the walk a predicate over the policy globs", async () => {
  const pol = { ...base, ciGlobs: [".github/**"] };
  const g = fake({ calls: [] }) as unknown as Record<string, unknown>;
  let seen: ((paths: string[]) => boolean) | undefined;
  g.changedCiPathsPerCommit = async (_b: string, _r: unknown, _t: string, o: { flag?: (paths: string[]) => boolean }) => { seen = o.flag; return []; };
  assert.ok("pack" in await pin(g as unknown as HeldGit, range, { perCommitCi: true, ciTarget: floor }, pol));
  assert.ok(seen, "a predicate is passed");
  assert.equal(seen!([".github/workflows/x.yml"]), true);
  assert.equal(seen!(["src/a.ts"]), false);
});

it("pinned: a supplied range whose tip is not the source is refused before any scan", async () => {
  const calls: string[] = [];
  const r = await pin(fake({ calls }), { ...range, tipSha: "c".repeat(40) });
  assert.deepEqual(r, { reason: "range_tip_mismatch" });
  assert.deepEqual(calls, []);
});

it("range serialization round-trips and distinguishes absent from empty", () => {
  const withEmpty: CheckpointRange = { tipSha: tip, excludeSha: tip, packExcludeShas: [], defaultBlobSha: floor, scanFloorShas: [floor] };
  const back = parseCheckpointRange(serializeCheckpointRange(withEmpty));
  assert.deepEqual(back, withEmpty);
  assert.ok(back && Array.isArray(back.packExcludeShas) && back.packExcludeShas.length === 0);
  const absent = parseCheckpointRange(serializeCheckpointRange(range));
  assert.deepEqual(absent, range);
  assert.ok(absent && !("packExcludeShas" in absent) && !("defaultBlobSha" in absent) && !("scanFloorShas" in absent));
  assert.notEqual(serializeCheckpointRange(range), serializeCheckpointRange({ ...range, packExcludeShas: [] }));
});

it("range parse is strict", () => {
  for (const bad of ["", "[]", "null", "{}", JSON.stringify({ tipSha: tip }),
    JSON.stringify({ tipSha: "x", excludeSha: floor }),
    JSON.stringify({ tipSha: tip, excludeSha: floor, extra: 1 }),
    JSON.stringify({ tipSha: tip, excludeSha: floor, packExcludeShas: ["nope"] }),
    JSON.stringify({ tipSha: tip, excludeSha: floor, scanFloorShas: Array(257).fill(tip) }),
    JSON.stringify({ tipSha: tip, excludeSha: floor, defaultBlobSha: 5 })]) {
    assert.equal(parseCheckpointRange(bad), undefined, bad);
  }
});

// ---- replay over a real GitCache and real bare repositories ---------------------------------------------------
// The retry of a pinned held publication must use the persisted range verbatim: the default branch moving between
// attempts changes what a fresh resolve would return, so a retry that re-resolved would publish different bytes.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const REAL_BRANCH = "agent/issue-77";
const sh = (cwd: string, ...args: string[]): string =>
  execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
function commitFile(repo: string, name: string, content: string): string {
  fs.writeFileSync(path.join(repo, name), content);
  sh(repo, "add", ".");
  sh(repo, "-c", "commit.gpgsign=false", "commit", "-m", `edit ${name}`);
  return sh(repo, "rev-parse", "HEAD");
}

describe("pinned replay over a real GitCache", () => {
  const roots: Fixture[] = [];
  after(() => { for (const f of roots.splice(0)) f.cleanup(); });

  async function rig() {
    const fx = makeFixture();
    roots.push(fx);
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const bare = await cache.ensureClone(fx.originPath);
    const work = fx.originPath;
    sh(work, "config", "user.email", "t@t");
    sh(work, "config", "user.name", "t");
    const sync = () => { sh(bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"); };
    const root = sh(work, "rev-parse", "HEAD");
    return { fx, cache, bare, work, sync, root };
  }
  type Rig = Awaited<ReturnType<typeof rig>>;

  /** The object set of a pack, read by unpacking it into a throwaway bare repository. */
  function objects(r: Rig, pack: Buffer): string[] {
    const probe = fs.mkdtempSync(path.join(r.fx.dataDir, "probe-"));
    sh(probe, "init", "-q", "--bare");
    execFileSync("git", ["-C", probe, "unpack-objects", "-q"], { env: GIT_ENV, input: pack });
    return execFileSync("git", ["-C", probe, "cat-file", "--batch-all-objects", "--batch-check"], { env: GIT_ENV, encoding: "utf8" })
      .split("\n").filter(Boolean).sort();
  }

  type Prepared = Exclude<Awaited<ReturnType<typeof prepareFrozenHeldPinned>>, { reason: string }>;
  const input = (r: Rig, tipSha: string, extra: Record<string, unknown> = {}) => ({
    literals: [] as string[], git: r.cache as unknown as HeldGit, barePath: r.bare, branch: REAL_BRANCH, sourceSha: tipSha, policy: base,
    deadlineMs: 60_000, defaultBranch: "main", ...extra,
  });

  /** First attempt resolves; the default branch moves; the retry carries the persisted range. */
  async function replay(t: TestContext, r: Rig, tipSha: string, moveDefault: () => void) {
    const resolve = t.mock.method(r.cache, "resolveCheckpointRange");
    const first = await prepareFrozenHeldPinned(input(r, tipSha), "resolve");
    assert.ok("pack" in first, `first attempt prepared: ${"reason" in first ? first.reason : ""}`);
    assert.equal(resolve.mock.callCount(), 1);
    const persisted = parseCheckpointRange(serializeCheckpointRange(first.range));
    assert.deepEqual(persisted, first.range, "the persisted record round-trips exactly");
    moveDefault();
    r.sync();
    const second = await prepareFrozenHeldPinned(input(r, tipSha, { expectDigests: first.digests }), persisted!);
    assert.ok("pack" in second, `retry prepared: ${"reason" in second ? second.reason : ""}`);
    assert.equal(resolve.mock.callCount(), 1, "the retry never re-resolves the range");
    assert.deepEqual(second.range, first.range);
    assert.deepEqual(second.digests, first.digests);
    assert.deepEqual(objects(r, second.pack), objects(r, first.pack), "the retry publishes the same object set");
    return { first: first as Prepared, persisted: persisted!, resolve };
  }

  async function tampered(r: Rig, tipSha: string, persisted: CheckpointRange, digests: Prepared["digests"], over: Partial<CheckpointRange>) {
    return prepareFrozenHeldPinned(input(r, tipSha, { expectDigests: digests }), { ...persisted, ...over });
  }

  it("zero range with no origin branch: packExcludeShas pinned, replay stable, tamper refused", async t => {
    const r = await rig();
    const parent = commitFile(r.work, "a.txt", "a\n");
    const tip = commitFile(r.work, "b.txt", "b\n");
    commitFile(r.work, "c.txt", "c\n");
    r.sync();
    const out = await replay(t, r, tip, () => { commitFile(r.work, "d.txt", "d\n"); });
    assert.deepEqual(out.first.range.packExcludeShas, [parent], "a zero range excludes the tip's literal parents");
    assert.equal(out.first.range.excludeSha, tip);
    const bad = await tampered(r, tip, out.persisted, out.first.digests, { packExcludeShas: [] });
    assert.equal((bad as { reason: string }).reason, "digest_mismatch");
  });

  it("zero range at a root commit: an empty packExcludeShas survives the round trip and the replay", async t => {
    const r = await rig();
    commitFile(r.work, "a.txt", "a\n");
    r.sync();
    const out = await replay(t, r, r.root, () => { commitFile(r.work, "d.txt", "d\n"); });
    assert.deepEqual(out.first.range.packExcludeShas, [], "a root commit has no parents to exclude");
    const bad = await tampered(r, r.root, out.persisted, out.first.digests, { packExcludeShas: [r.root] });
    assert.ok("reason" in bad && ["digest_mismatch", "digest_unavailable"].includes(bad.reason), "a tampered root range is refused, never published");
  });

  it("selective pack: the pinned defaultBlobSha is replayed after the default branch moves", async t => {
    const r = await rig();
    // The origin branch sits at an earlier checkpoint F0, so the pack floor is F0 and the default is merged above it.
    sh(r.work, "checkout", "-q", "-b", REAL_BRANCH);
    const f0 = commitFile(r.work, "p0.txt", "checkpointed\n");
    sh(r.work, "checkout", "-q", "-b", "wip");
    fs.writeFileSync(path.join(r.work, "u.txt"), "Z\n");
    commitFile(r.work, "p.txt", "private\n");
    sh(r.work, "checkout", "-q", "main");
    const d1 = commitFile(r.work, "s.txt", "X\n");
    sh(r.work, "checkout", "-q", "wip");
    sh(r.work, "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "-m", "merge default", "main");
    const tip = sh(r.work, "rev-parse", "HEAD");
    sh(r.work, "checkout", "-q", "main");
    r.sync();
    const out = await replay(t, r, tip, () => { commitFile(r.work, "u.txt", "Z\n"); });
    assert.equal(out.first.range.excludeSha, f0);
    assert.equal(out.first.range.defaultBlobSha, d1, "the selective pack is pinned to the default tip at first resolution");
    const blobX = sh(r.work, "rev-parse", `${d1}:s.txt`), blobP = sh(r.work, "rev-parse", `${tip}:p.txt`);
    const has = (pack: Buffer, blob: string) => objects(r, pack).some(l => l.startsWith(`${blob} blob`));
    assert.equal(has(out.first.pack, blobX), false, "a blob the pinned default carries is omitted");
    assert.equal(has(out.first.pack, blobP), true, "the run's own blob is carried");
    const fresh = await r.cache.resolveCheckpointRange(r.bare, REAL_BRANCH, { tipSha: tip, defaultBranch: "main" });
    assert.notEqual(fresh?.defaultBlobSha, d1, "control: a fresh resolve after the move would have changed the range");
    const bad = await tampered(r, tip, out.persisted, out.first.digests, { defaultBlobSha: undefined });
    assert.equal((bad as { reason: string }).reason, "digest_mismatch");
  });
});

// Owner decision, issue #4505: the held publication scans for the run's provider key like every other upload.
describe("provider key scan on the held publication", () => {
  const KEY_FORMS = ["Zq9xLm/4tR+7", "Zq9xLm\\/4tR+7"];
  const bound: FrozenHeldPolicy = { ...base, providerKeyScan: true };

  it("hands the key forms to the scan and refuses secret_found on a key finding, before any pack", async () => {
    const calls: string[] = [];
    let seen: unknown;
    const git = fake({ calls, scan: { trusted: true, findings: [{ ruleId: "uzi-provider-key" }] } });
    (git as unknown as Record<string, unknown>).secretScanCheckpointRange = async (_b: string, _r: unknown, o: unknown) => {
      seen = o; calls.push("scan"); return { trusted: true, findings: [{ ruleId: "uzi-provider-key" }] };
    };
    assert.deepEqual(await run(git, { literals: KEY_FORMS }, bound), { reason: "secret_found" });
    assert.deepEqual((seen as { literals: string[] }).literals, KEY_FORMS);
    assert.ok(!calls.includes("pack"), "no pack is produced after a finding");
  });

  it("a provider-bound policy with NO key forms (a caller with no claim) fails closed as scan_untrusted without scanning", async () => {
    const calls: string[] = [];
    assert.deepEqual(await run(fake({ calls }), { literals: [] }, bound), { reason: "scan_untrusted" });
    assert.ok(!calls.includes("scan") && !calls.includes("pack"));
  });

  it("a vendor policy with no forms is unchanged: it scans and packs", async () => {
    const calls: string[] = [];
    const r = await run(fake({ calls, pack: "PACK" }), { literals: [] }, base);
    assert.ok("pack" in r);
    assert.deepEqual(calls, ["resolve", "scan", "workflows", "pack"]);
  });

  it("the pinned (terminal) path refuses the same way", async () => {
    const calls: string[] = [];
    const out = await prepareFrozenHeldPinned({ literals: [], git: fake({ calls }), barePath: "b", branch: "t", sourceSha: tip, policy: bound, deadlineMs: 1000 }, range);
    assert.equal((out as { reason: string }).reason, "scan_untrusted");
  });

  it("validFrozenHeldPolicy accepts the flag only as true or absent", () => {
    assert.equal(validFrozenHeldPolicy(base), true);
    assert.equal(validFrozenHeldPolicy({ ...base, providerKeyScan: true }), true);
    for (const bad of [false, "true", 1, null, {}]) assert.equal(validFrozenHeldPolicy({ ...base, providerKeyScan: bad }), false, String(bad));
  });
});
