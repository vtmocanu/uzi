// issue #3780: a stranded tracking-ownership update (pending receipt, CAS/stamp/commit failure or a hard
// boundary abort mid-update) is finished by a LATER import only when the same per-flight token's own
// attempt left it, the disk is in one of three exact states, and the admission predicate still holds.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, newTrackingAttemptToken, type BoundaryProcessSpawner, type FetchAgentBranchOptions, type TrackingUpdateResult } from "../src/git.js";

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-3780";
const TRACKING = `refs/uzi-runner/${BRANCH}`;
const OWNER_KEY = `uzi-trackowner.${BRANCH}.owner`;
const RUN = "37803780-1111-2222-3333-444444444444";
const OTHER = "37803780-aaaa-bbbb-cccc-dddddddddddd";
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
let admit: { on: boolean };

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}
function updated(result: TrackingUpdateResult): Extract<TrackingUpdateResult, { kind: "updated" }> {
  assert.equal(result.kind, "updated", JSON.stringify(result));
  assert.ok(result.kind === "updated");
  return result;
}
function root(label: string): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, [...IDENT, "commit-tree", tree, "-m", label]);
  gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  return sha;
}
async function fetch(): Promise<TrackingUpdateResult> {
  return cache.fetchAgentBranch(bare, clone, BRANCH, opts.context.runId, opts);
}
function tip(): string { return gitIn(bare, ["rev-parse", TRACKING]); }
function stamp(): string | undefined {
  try { return gitIn(bare, ["config", "--local", "--get", OWNER_KEY]); } catch { return undefined; }
}
function owedPath(prefix: "receipt" | "governed"): string {
  return path.join(bare, "uzi-owed", `${prefix}-${createHash("sha256").update(BRANCH).digest("hex")}.json`);
}
function receipt(): { phase: string; trackingSha: string } & Record<string, unknown> {
  return JSON.parse(fs.readFileSync(owedPath("receipt"), "utf8"));
}
/** Everything a refusal must leave byte-for-byte. */
function disk(): Record<string, string | undefined> {
  const read = (p: string) => fs.existsSync(p) ? fs.readFileSync(p, "utf8") : undefined;
  let ref: string | undefined;
  try { ref = tip(); } catch { ref = undefined; }
  return { ref, receipt: read(owedPath("receipt")), marker: read(owedPath("governed")), stamp: stamp(),
    governance: gitIn(bare, ["config", "--local", "--get-all", `uzi-trackowner.${BRANCH}.receiptversion`]) };
}
type Internals = {
  execScoped(command: string, args: string[], options: unknown): Promise<{ stdout: string; stderr: string }>;
  writeOwedFile(bare: string, name: string, value: unknown): Promise<void>;
};
function faultGit(match: (args: string[]) => boolean): () => void {
  const internals = cache as unknown as Internals;
  const original = internals.execScoped.bind(cache);
  internals.execScoped = async (command, args, options) => {
    if (command === "git" && match(args)) throw Object.assign(new Error("injected git failure"), { code: 128, stdout: "", stderr: "injected" });
    return original(command, args, options);
  };
  return () => { internals.execScoped = original; };
}
/** `observe` sees every committed write AFTER it landed; `fail` throws (before or after the write). */
function hookDisk(observe: (name: string, value: unknown) => void, fail?: { match: (n: string, v: unknown) => boolean; afterWrite: boolean }): () => void {
  const internals = cache as unknown as Internals;
  const original = internals.writeOwedFile.bind(cache);
  internals.writeOwedFile = async (b, name, value) => {
    if (fail?.match(name, value)) {
      if (fail.afterWrite) await original(b, name, value);
      throw new Error("injected metadata I/O failure");
    }
    await original(b, name, value);
    observe(name, value);
  };
  return () => { internals.writeOwedFile = original; };
}
function realSpawner(after?: (argv: readonly string[]) => void): BoundaryProcessSpawner {
  return async request => {
    const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
    const completed = new Promise<{ code: number }>((resolve, reject) => {
      child.once("error", reject);
      child.once("close", code => { after?.(request.argv); resolve({ code: code ?? -1 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { child.kill("SIGTERM"); await completed; } };
  };
}
function scoped<T>(ctl: AbortController, action: () => Promise<T>, after?: (argv: readonly string[]) => void): Promise<T> {
  return cache.withBoundaryProcessSpawner(realSpawner(after), ctl.signal, action);
}
function isPendingReceipt(name: string, value: unknown): boolean {
  return name.startsWith("receipt-") && (value as { phase?: string }).phase === "pending";
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-3780", noProofReseed, RUN)).path;
  admit = { on: true };
  opts = { context: { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } },
  attemptToken: newTrackingAttemptToken(() => admit.on) };
});
afterEach(() => fx.cleanup());

/** A later import at a NEW tip must finish the stranded update and then promote that tip normally. */
async function assertRepairedImport(stranded: string, prior?: string): Promise<void> {
  const current = root("current after failure");
  const result = updated(await fetch());
  assert.equal(result.candidateSha, current);
  assert.equal(tip(), current);
  assert.equal(receipt().phase, "committed");
  assert.equal(receipt().trackingSha, current);
  assert.equal(stamp(), RUN);
  assert.ok(result.retainedShas.includes(stranded), "the repaired attempt's pin is part of the result");
  assert.ok(result.retainedShas.includes(current));
  // The displaced prior tip was pinned by the stranded attempt; the follow-on promote (which displaces the
  // repaired tip, not the prior one) cannot re-derive it, so only the repaired attempt's record reports it.
  if (prior) assert.ok(result.retainedShas.includes(prior), "the prior tip the stranded attempt pinned is reported too");
  assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, current, 1)).kind, "owned");
}

describe("issue #3780 repaired imports", () => {
  it("tracking_update_failed (ref unchanged) is finished by the next import", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "tracking_update_failed" });
    restore();
    await assertRepairedImport(h2, h);
  });

  it("owner_stamp_failed (CAS landed, stale stamp) is finished by the next import", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("config") && args.includes("--replace-all") && args.includes(OWNER_KEY));
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "owner_stamp_failed" });
    restore();
    assert.equal(tip(), h2);
    await assertRepairedImport(h2, h);
  });

  for (const afterWrite of [false, true]) {
    it(`receipt_commit_failed (${afterWrite ? "committed bytes landed, then invalidated" : "write failed"}) is finished by the next import`, async () => {
      root("H"); updated(await fetch());
      const h2 = root("H2");
      const restore = hookDisk(() => undefined, { afterWrite,
        match: (name, value) => name.startsWith("receipt-") && (value as { phase?: string }).phase === "committed" });
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "receipt_commit_failed" });
      restore();
      assert.equal(tip(), h2);
      assert.equal(receipt().phase, "pending", "the best-effort invalidation leaves the CAS-landed state");
      await assertRepairedImport(h2);
    });
  }

  it("a hard boundary abort after the pending write (ref unchanged) is finished by the next import", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const ctl = new AbortController();
    const restore = hookDisk((name, value) => { if (isPendingReceipt(name, value)) ctl.abort(); });
    await assert.rejects(scoped(ctl, () => fetch()));
    restore();
    assert.equal(tip(), h);
    assert.equal(receipt().phase, "pending");
    await assertRepairedImport(h2);
  });

  it("a hard boundary abort after the CAS (CAS landed) is finished by the next import", async () => {
    root("H"); updated(await fetch());
    const h2 = root("H2");
    const ctl = new AbortController();
    await assert.rejects(scoped(ctl, () => fetch(), (argv) => {
      if (argv.includes("update-ref") && argv.includes(TRACKING) && argv.includes(h2)) ctl.abort();
    }));
    assert.equal(tip(), h2);
    assert.equal(receipt().phase, "pending");
    await assertRepairedImport(h2);
  });

  it("a legacy stamp-only bare stranded after the ancestor clear, before the marker (pre-pending) is finished by the next import", async () => {
    const { h2 } = await strandLegacyBeforeMarker();
    assert.equal(fs.existsSync(owedPath("governed")), false);
    assert.equal(fs.existsSync(owedPath("receipt")), false);
    await assertRepairedImport(h2);
  });

  it("a legacy bare hard-aborted right after the governance write, before the ancestor clear, is finished by the next import", async () => {
    const { h2 } = await strandLegacyBeforeClear();
    await assertRepairedImport(h2);
  });

  it("a hard abort between the repair's committed write and its owner readback does not rewrite the receipt to pending", async () => {
    root("H"); updated(await fetch());
    const h2 = root("H2");
    const strand = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
    try { assert.equal((await fetch()).kind, "not_updated"); } finally { strand(); }
    assert.equal(receipt().phase, "pending");
    const ctl = new AbortController();
    const writes: Array<{ name: string; phase?: string }> = [];
    const restore = hookDisk((name, value) => {
      writes.push({ name, phase: (value as { phase?: string }).phase });
      if (name.startsWith("receipt-") && (value as { phase?: string }).phase === "committed") ctl.abort();
    });
    try { await assert.rejects(scoped(ctl, () => fetch())); } finally { restore(); }
    const receiptWrites = writes.filter((w) => w.name.startsWith("receipt-")).map((w) => w.phase);
    assert.deepEqual(receiptWrites, ["committed"], "no pending rewrite after the hard abort");
    assert.equal(receipt().phase, "committed");
    assert.equal(receipt().trackingSha, h2);
  });

  it("a foreign owner displaced by a stranded attempt is finished by the next import (stamp rewritten)", async () => {
    const { h2 } = await strandDisplacingForeign();
    assert.equal(stamp(), OTHER);
    await assertRepairedImport(h2);
  });
});

/** A legacy stamp-only bare (tracking ref + stamp, no receipt); the attempt then fails writing the marker. */
async function strandLegacyBeforeMarker(): Promise<{ legacy: string; h2: string }> {
  const legacy = root("legacy H");
  gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
    `file://${clone}`, `refs/heads/${BRANCH}:${TRACKING}`]);
  gitIn(bare, ["config", "--local", OWNER_KEY, RUN]);
  const h2 = root("H2");
  const restore = hookDisk(() => undefined, { afterWrite: false, match: (name) => name.startsWith("governed-") });
  try { assert.deepEqual(await fetch(), { kind: "not_updated", reason: "receipt_pending_failed" }); } finally { restore(); }
  assert.equal(tip(), legacy);
  return { legacy, h2 };
}
/** A legacy stamp-only bare; a hard boundary abort lands right after the governance write, before the ancestor clear. */
async function strandLegacyBeforeClear(): Promise<{ legacy: string; h2: string }> {
  const legacy = root("legacy H");
  gitIn(bare, ["-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head",
    `file://${clone}`, `refs/heads/${BRANCH}:${TRACKING}`]);
  gitIn(bare, ["config", "--local", OWNER_KEY, RUN]);
  const h2 = root("H2");
  const ctl = new AbortController();
  await assert.rejects(scoped(ctl, () => fetch(), (argv) => {
    if (argv.includes("--replace-all") && argv.includes(`uzi-trackowner.${BRANCH}.receiptversion`)) ctl.abort();
  }));
  assert.equal(gitIn(bare, ["config", "--local", "--get", `uzi-trackowner.${BRANCH}.receiptversion`]), "1");
  assert.equal(fs.existsSync(owedPath("receipt")), false);
  assert.equal(tip(), legacy);
  return { legacy, h2 };
}
/** RUN displaces a FOREIGN committed owner; the CAS then fails, leaving a pending receipt over the foreign tip. */
async function strandDisplacingForeign(): Promise<{ foreign: string; h2: string }> {
  const foreign = root("foreign H");
  const other: FetchAgentBranchOptions = { context: { ...opts.context, runId: OTHER } };
  updated(await cache.fetchAgentBranch(bare, clone, BRANCH, OTHER, other));
  assert.equal(stamp(), OTHER);
  const h2 = root("H2");
  const restore = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
  try { assert.deepEqual(await fetch(), { kind: "not_updated", reason: "tracking_update_failed" }); } finally { restore(); }
  assert.equal(tip(), foreign);
  assert.equal(receipt().phase, "pending");
  return { foreign, h2 };
}

describe("issue #3780 refusals leave the disk untouched", () => {
  /** ref unchanged: CAS fault leaves the old tip and a pending receipt. */
  async function strandRefUnchanged(): Promise<{ h: string; h2: string }> {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
    assert.equal((await fetch()).kind, "not_updated");
    restore();
    return { h, h2 };
  }
  /** CAS landed: stamp fault leaves the new tip, the old stamp and a pending receipt. */
  async function strandCasLanded(): Promise<{ h: string; h2: string }> {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("config") && args.includes("--replace-all") && args.includes(OWNER_KEY));
    assert.equal((await fetch()).kind, "not_updated");
    restore();
    return { h, h2 };
  }
  async function refused(): Promise<void> {
    root("current after failure");
    const before = disk();
    assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(disk(), before);
  }

  it("a restart (new GitCache, fresh token) stays refused", async () => {
    await strandRefUnchanged();
    cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    opts = { ...opts, attemptToken: newTrackingAttemptToken(() => true) };
    await refused();
  });

  it("no token stays refused", async () => {
    await strandRefUnchanged();
    opts = { ...opts, attemptToken: undefined };
    await refused();
  });

  it("a forged token-shaped object is ignored", async () => {
    await strandRefUnchanged();
    opts = { ...opts, attemptToken: {} as never };
    await refused();
  });

  it("admission false stays refused, then a later admitted import completes", async () => {
    await strandCasLanded();
    admit.on = false;
    await refused();
    admit.on = true;
    const current = root("admitted again");
    assert.equal(updated(await fetch()).candidateSha, current);
    assert.equal(receipt().phase, "committed");
  });

  for (const [field, mutate] of [
    ["runId", (r: Record<string, unknown>) => { r.runId = "other-run"; }],
    ["generation", (r: Record<string, unknown>) => { r.generation = 2; }],
    ["context", (r: Record<string, unknown>) => { r.context = `context-${"0".repeat(64)}.json`; }],
    ["branch", (r: Record<string, unknown>) => { r.branch = "agent/other"; }],
    ["version", (r: Record<string, unknown>) => { r.version = 2; }],
    ["trackingSha", (r: Record<string, unknown>) => { r.trackingSha = base; }],
    ["phase", (r: Record<string, unknown>) => { r.phase = "committed"; }],
  ] as const) {
    // A committed phase over a CAS-landed ref whose stamp is already this run's IS a valid committed
    // owner (observation succeeds), so that one combination is not a refusal.
    for (const state of (field === "phase" ? ["ref-unchanged"] : ["ref-unchanged", "cas-landed"]) as Array<"ref-unchanged" | "cas-landed">) {
      it(`a mismatching receipt ${field} (${state}) stays refused`, async () => {
        if (state === "ref-unchanged") await strandRefUnchanged(); else await strandCasLanded();
        const r = receipt();
        mutate(r);
        fs.writeFileSync(owedPath("receipt"), JSON.stringify(r), { mode: 0o600 });
        await refused();
      });
    }
  }

  for (const state of ["ref-unchanged", "cas-landed"] as const) {
    it(`an extra receipt field (${state}) stays refused`, async () => {
      if (state === "ref-unchanged") await strandRefUnchanged(); else await strandCasLanded();
      fs.writeFileSync(owedPath("receipt"), JSON.stringify({ ...receipt(), extra: 1 }), { mode: 0o600 });
      await refused();
    });
    it(`a marker mismatch (${state}) stays refused`, async () => {
      if (state === "ref-unchanged") await strandRefUnchanged(); else await strandCasLanded();
      fs.writeFileSync(owedPath("governed"), JSON.stringify({ version: 1, branch: "agent/other" }), { mode: 0o600 });
      await refused();
    });
    it(`a governance mismatch (${state}) stays refused`, async () => {
      if (state === "ref-unchanged") await strandRefUnchanged(); else await strandCasLanded();
      gitIn(bare, ["config", "--local", `uzi-trackowner.${BRANCH}.receiptversion`, "2"]);
      await refused();
    });
  }

  it("a ref moved to another commit stays refused", async () => {
    await strandRefUnchanged();
    gitIn(bare, ["update-ref", TRACKING, base]);
    await refused();
  });

  it("a foreign stamp (CAS landed) stays refused", async () => {
    await strandCasLanded();
    gitIn(bare, ["config", "--local", OWNER_KEY, "foreign-run"]);
    await refused();
  });

  it("a removed incoming pin stays refused", async () => {
    const { h2 } = await strandRefUnchanged();
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${RUN}/${h2}`]);
    await refused();
  });

  it("a removed displaced-owner pin (foreign owner) stays refused", async () => {
    const { foreign } = await strandDisplacingForeign();
    gitIn(bare, ["update-ref", "-d", `refs/uzi-owed/${OTHER}/${foreign}`]);
    await refused();
  });

  it("a legacy bare stranded BEFORE the ancestor clear is refused when the repair's own clear returns false", async () => {
    await strandLegacyBeforeClear();
    // An unreadable ancestor sidecar (a governed marker that is not this ancestor's) makes
    // clearConflictingAncestorTrackingRefs return false, exactly as it would have originally.
    const ancestorMarker = path.join(bare, "uzi-owed", `governed-${createHash("sha256").update("agent").digest("hex")}.json`);
    fs.writeFileSync(ancestorMarker, JSON.stringify({ version: 1, branch: "agent/other" }), { mode: 0o600 });
    const markerBefore = fs.readFileSync(ancestorMarker, "utf8");
    await refused();
    assert.equal(fs.readFileSync(ancestorMarker, "utf8"), markerBefore);
    assert.equal(fs.existsSync(owedPath("receipt")), false);
    assert.equal(fs.existsSync(owedPath("governed")), false);
  });

  it("a second token on the SAME GitCache cannot repair token A's attempt; A then completes it", async () => {
    const tokenA = opts.attemptToken;
    const { h, h2 } = await strandRefUnchanged();
    opts = { ...opts, attemptToken: newTrackingAttemptToken(() => true) };
    await refused();
    opts = { ...opts, attemptToken: tokenA };
    await assertRepairedImport(h2, h);
  });

  it("an opts.context with a different generation stays refused", async () => {
    await strandRefUnchanged();
    opts = { ...opts, context: { ...opts.context, generation: 2 } as FetchAgentBranchOptions["context"] };
    await refused();
  });
});

describe("issue #3780 admission revoked between repair writes", () => {
  it("stops after the CAS: no stamp, no committed receipt, no rollback; a later admitted import completes", async () => {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const strand = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
    assert.equal((await fetch()).kind, "not_updated");
    strand();
    assert.equal(tip(), h);

    const gitCalls: string[][] = [];
    const writes: Array<{ name: string; phase?: string }> = [];
    const internals = cache as unknown as Internals;
    const originalGit = internals.execScoped.bind(cache);
    internals.execScoped = async (command, args, options) => {
      const result = await originalGit(command, args, options);
      if (command === "git") {
        gitCalls.push(args);
        // The repair CAS (h -> h2) has just landed: revoke authority before the next write.
        if (args.includes("update-ref") && args.includes(TRACKING) && args.includes(h2)) admit.on = false;
      }
      return result;
    };
    const restoreDisk = hookDisk((name, value) => writes.push({ name, phase: (value as { phase?: string }).phase }));
    const current = root("current after failure");
    try {
      assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" });
    } finally { restoreDisk(); internals.execScoped = originalGit; }

    assert.equal(tip(), h2, "the CAS had landed and is left in place");
    assert.equal(stamp(), RUN, "unchanged stamp");
    assert.equal(receipt().phase, "pending");
    assert.ok(!writes.some((w) => w.name.startsWith("receipt-")), JSON.stringify(writes));
    assert.ok(!gitCalls.some((a) => a.includes("--replace-all") && a.includes(OWNER_KEY)), "no stamp write");
    assert.equal(gitCalls.filter((a) => a.includes("update-ref") && a.includes(TRACKING)).length, 1, "the CAS ran once and nothing rolled it back");

    admit.on = true;
    const result = updated(await fetch());
    assert.equal(result.candidateSha, current);
    assert.equal(receipt().phase, "committed");
    assert.equal(stamp(), RUN);
    assert.ok(result.retainedShas.includes(h2));
  });

  /** Revoke authority right after the first git call matching `trigger` and return what the import wrote. */
  async function revokedAfter(trigger: (args: string[]) => boolean): Promise<{ writes: Array<{ name: string; phase?: string }>; gitCalls: string[][] }> {
    const gitCalls: string[][] = [];
    const writes: Array<{ name: string; phase?: string }> = [];
    const internals = cache as unknown as Internals;
    const originalGit = internals.execScoped.bind(cache);
    internals.execScoped = async (command, args, options) => {
      const result = await originalGit(command, args, options);
      if (command === "git") { gitCalls.push(args); if (trigger(args)) admit.on = false; }
      return result;
    };
    const restoreDisk = hookDisk((name, value) => {
      writes.push({ name, phase: (value as { phase?: string }).phase });
      if (name.startsWith("governed-") && trigger(["marker-written"])) admit.on = false;
    });
    try { assert.deepEqual(await fetch(), { kind: "not_updated", reason: "ownership_unknown" }); }
    finally { restoreDisk(); internals.execScoped = originalGit; }
    return { writes, gitCalls };
  }

  it("stops after the stamp write (foreign owner displaced): no committed receipt, no rollback; a later admitted import completes", async () => {
    const { h2 } = await strandDisplacingForeign();
    const current = root("current after failure");
    const { writes } = await revokedAfter((a) => a.includes("--replace-all") && a.includes(OWNER_KEY) && a.includes(RUN));
    assert.equal(stamp(), RUN, "the stamp write landed and is left in place");
    assert.equal(tip(), h2, "the CAS landed and is left in place");
    assert.equal(receipt().phase, "pending");
    assert.ok(!writes.some((w) => w.name.startsWith("receipt-")), JSON.stringify(writes));
    admit.on = true;
    const result = updated(await fetch());
    assert.equal(result.candidateSha, current);
    assert.equal(receipt().phase, "committed");
  });

  it("stops after the marker write (pre-pending): no pending receipt, no CAS, no rollback; a later admitted import completes", async () => {
    const { legacy } = await strandLegacyBeforeMarker();
    const current = root("current after failure");
    const { writes, gitCalls } = await revokedAfter((a) => a[0] === "marker-written");
    assert.equal(fs.existsSync(owedPath("governed")), true, "the marker write landed and is left in place");
    assert.equal(fs.existsSync(owedPath("receipt")), false, "no pending receipt after revocation");
    assert.equal(tip(), legacy, "no CAS");
    assert.ok(!writes.some((w) => w.name.startsWith("receipt-")), JSON.stringify(writes));
    assert.ok(!gitCalls.some((a) => a.includes("update-ref") && a.includes(TRACKING)), "no ref write");
    admit.on = true;
    const result = updated(await fetch());
    assert.equal(result.candidateSha, current);
    assert.equal(receipt().phase, "committed");
  });
});

describe("issue #3780 bridge path (updateTrackingRef)", () => {
  async function strandAndBridge(): Promise<{ h: string; h2: string; bridge: string }> {
    const h = root("H"); updated(await fetch());
    const h2 = root("H2");
    const restore = faultGit((args) => args.includes("update-ref") && args.includes(TRACKING));
    try { assert.equal((await fetch()).kind, "not_updated"); } finally { restore(); }
    const tree = gitIn(bare, ["rev-parse", `${base}^{tree}`]);
    const bridge = gitIn(bare, [...IDENT, "commit-tree", tree, "-p", h2, "-m", "bridge"]);
    return { h, h2, bridge };
  }

  it("repairs the stranded attempt first, then promotes the bridge SHA", async () => {
    const { h, h2, bridge } = await strandAndBridge();
    const result = updated(await cache.updateTrackingRef(bare, BRANCH, bridge, opts));
    assert.equal(result.candidateSha, bridge);
    assert.equal(tip(), bridge);
    assert.equal(receipt().phase, "committed");
    assert.equal(receipt().trackingSha, bridge);
    assert.equal(stamp(), RUN);
    assert.ok(result.retainedShas.includes(h2) && result.retainedShas.includes(bridge) && result.retainedShas.includes(h));
  });

  it("without an admitted token it keeps the ownership_unknown refusal and writes nothing", async () => {
    const { bridge } = await strandAndBridge();
    const before = disk();
    assert.deepEqual(await cache.updateTrackingRef(bare, BRANCH, bridge, { ...opts, attemptToken: undefined }),
      { kind: "not_updated", reason: "ownership_unknown" });
    assert.deepEqual(disk(), before);
  });

  it("a soft stop between observation and repair starts no repair write", async () => {
    const { bridge } = await strandAndBridge();
    const before = disk();
    const soft = new AbortController();
    const internals = cache as unknown as { observeTrackingUnderLock(b: string, br: string): Promise<unknown> };
    const observe = internals.observeTrackingUnderLock.bind(cache);
    internals.observeTrackingUnderLock = async (b, br) => { const value = await observe(b, br); soft.abort(); return value; };
    try {
      await assert.rejects(cache.withBoundaryProcessSpawner(realSpawner(), new AbortController().signal,
        () => cache.updateTrackingRef(bare, BRANCH, bridge, opts), { softSignal: soft.signal }),
      CheckpointSoftDeadlineError);
    } finally { internals.observeTrackingUnderLock = observe; }
    assert.deepEqual(disk(), before);
  });
});
