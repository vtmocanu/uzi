import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProof, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-epoch-discard";
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
function setJournal(value: string): void { gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`, value]); }

beforeEach(async () => {
  fx = makeFixture();
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
  assert.equal(inspection.sources[0]?.epochSettlement?.disposition.kind, "local_discard");
  pending = 0; stopped = false; live = true;
  // A scope contract double; native descriptor inheritance is deliberately not exercised here.
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio() { throw new Error("no native launch in registration tests"); },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
  options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => { throw new Error("no effect permitted yet"); } };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => fx?.cleanup());

async function refusal(proof: EpochDiscardProof, reason: string, over: Partial<RetainedDiscardOptions> = {}): Promise<void> {
  assert.deepEqual(await git.discardStoppedSource({ ...options, epochProof: proof, ...over }),
    { kind: "refused", reason, consumers: [] });
  assert.ok(fs.existsSync(clonePath));
}

describe("worker epoch discard registration (automatic effects remain disabled)", () => {
  it("keeps raw bytes outside the frozen opaque proof and refuses effects after fresh real-Git checks", async () => {
    const proof = git.createEpochDiscardProof(input);
    assert.equal(Object.isFrozen(proof), true);
    assert.equal(JSON.stringify(proof), "{}");
    assert.deepEqual(Reflect.ownKeys(proof), []);
    git.assertEpochDiscardSettled(proof);
    await refusal(proof, "epoch_discard_unavailable");
    assert.equal(journal(), raw);
  });

  it("rejects JSON, copies and capabilities from a different cache", async () => {
    const proof = git.createEpochDiscardProof(input);
    for (const forged of [JSON.parse(JSON.stringify(proof)), { ...proof }]) {
      await refusal(forged, "invalid_epoch_proof");
      assert.throws(() => git.assertEpochDiscardSettled(forged), /unregistered/);
    }
    const other = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const foreign = other.createEpochDiscardProof(input);
    await refusal(foreign, "invalid_epoch_proof");
  });

  it("copies selector and paths instead of retaining caller-owned containers", async () => {
    const proof = git.createEpochDiscardProof(input);
    const original = { ...options.selector };
    (input.selector as { runId: string }).runId = "changed";
    (input.paths as string[])[0] = clonePath + "-changed";
    await refusal(proof, "epoch_discard_unavailable", { selector: original });
    await refusal(proof, "invalid_epoch_proof");
  });

  it("rejects mismatched selector, digest, bare and protected disposal", async () => {
    const proof = git.createEpochDiscardProof(input);
    await refusal(proof, "invalid_epoch_proof", { confirm: "0".repeat(64) });
    await refusal(proof, "invalid_epoch_proof", { barePath: bare + "-other" });
    await refusal(proof, "invalid_epoch_proof", { selector: { ...options.selector, generation: 2 } });
    assert.deepEqual(await git.discardProtectedSource({ ...options, epochProof: proof }),
      { kind: "refused", reason: "invalid_epoch_proof", consumers: [] });
  });

  it("rejects malformed decisions, scope contracts and noncanonical or extra-field journals", () => {
    const bad = [
      { decision: { eligible: false, attested: true } },
      { decision: { eligible: true, attested: false } },
      { scope: {} },
      { expectedRawJournal: raw + " " },
      { expectedRawJournal: JSON.stringify({ ...JSON.parse(raw), restoreTip: "a".repeat(40) }) },
      { expectedRawJournal: JSON.stringify({ ...JSON.parse(raw), runId: "other" }) },
      { expectedRawJournal: raw.replace('"runId":', '"runId":"duplicate","runId":') },
      { paths: [clonePath, clonePath] },
      { deadline: NaN },
    ];
    for (const over of bad) {
      assert.throws(() => git.createEpochDiscardProof({ ...input, ...over } as EpochDiscardProofOptions), /invalid/);
    }
  });

  it("compares the exact raw journal under B even when the attempt descriptor did not change", async () => {
    const proof = git.createEpochDiscardProof(input);
    setJournal(raw + " ");
    await refusal(proof, "epoch_journal_changed");
    assert.equal(journal(), raw); // The raw NUL read below must detect what trimming would hide.
    const stored = execFileSync("git", ["-C", bare, "config", "--null", "--get", `uzi-recovery.${BRANCH}.clone`],
      { env: ENV, encoding: "utf8" });
    assert.equal(stored, raw + " \0");
  });

  it("requires current terminal lifecycle and known zero holds for every proof use", async () => {
    const proof = git.createEpochDiscardProof(input);
    await refusal(proof, "epoch_authority_refused", { lifecycle: { state: "unknown" } });
    await refusal(proof, "epoch_authority_refused", { holds: { state: "unknown" } });
    await refusal(proof, "epoch_authority_refused", { holds: { state: "read", holds: [{ id: "h", generation: 1 }] } });
  });

  it("keeps live, stopped, pending-ticket, abort and deadline checks in settlement", async () => {
    const proof = git.createEpochDiscardProof(input);
    const ticket = scope.beginMutation();
    assert.throws(() => git.assertEpochDiscardSettled(proof), /unsettled/);
    ticket.settle();
    git.assertEpochDiscardSettled(proof);
    live = false;
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /reservation/);
    live = true;
    scope.stop();
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /stopped/);
    const controller = new AbortController();
    controller.abort();
    assert.throws(() => git.createEpochDiscardProof({ ...input, signal: controller.signal }));
    assert.throws(() => git.createEpochDiscardProof({ ...input, deadline: Date.now() - 1 }), /invalid/);
  });

  it("revokes an already registered proof after abort", async () => {
    const controller = new AbortController();
    const proof = git.createEpochDiscardProof({ ...input, signal: controller.signal });
    git.assertEpochDiscardSettled(proof);
    controller.abort(new Error("registered proof cancelled"));
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /registered proof cancelled/);
    assert.equal(journal(), raw);
  });

  it("revokes an already registered proof when its deadline expires", async t => {
    const proof = git.createEpochDiscardProof(input);
    git.assertEpochDiscardSettled(proof);
    const clock = t.mock.method(Date, "now", () => input.deadline);
    try {
      await refusal(proof, "stale_epoch_proof");
      assert.throws(() => git.assertEpochDiscardSettled(proof), /deadline exhausted/);
    } finally { clock.mock.restore(); }
    assert.equal(journal(), raw);
  });

  it("rechecks fresh consumers rather than trusting the registration decision", async () => {
    const proof = git.createEpochDiscardProof(input);
    const tip = gitIn(["rev-parse", "HEAD"]);
    gitIn(["update-ref", `refs/uzi-owed/${RUN}/${tip}`, tip]);
    const result = await git.discardStoppedSource({ ...options, epochProof: proof });
    assert.equal(result.kind, "refused");
    if (result.kind === "refused") assert.equal(result.reason, "epoch_source_refused");
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(clonePath));
  });

  it("detects a present source disappearing after registration through the inspect digest", async () => {
    const proof = git.createEpochDiscardProof(input);
    fs.rmSync(clonePath, { recursive: true });
    const result = await git.discardStoppedSource({ ...options, epochProof: proof });
    assert.deepEqual(result, { kind: "refused", reason: "descriptor_changed", consumers: [] });
    assert.equal(journal(), raw);
  });

  it("can bind strict-live metadata with an already absent tree without performing retirement", async () => {
    fs.rmSync(clonePath, { recursive: true });
    const inspection = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    const source = inspection.sources[0]!;
    assert.equal(source.presence, "missing");
    assert.equal(source.epochSettlement?.disposition.kind, "local_discard");
    const proof = git.createEpochDiscardProof({ ...input, confirm: source.descriptor });
    assert.deepEqual(await git.discardStoppedSource({ ...options, confirm: source.descriptor, epochProof: proof }),
      { kind: "refused", reason: "epoch_discard_unavailable", consumers: [] });
    assert.equal(journal(), raw);
    const entries = gitIn(["config", "--get-all", `uzi-attempts.${BRANCH}.entry`]).split("\n");
    assert.equal(JSON.parse(entries.at(-1)!).state, "live");
  });

  it("preserves ordinary operator refusal of a live attempt named by its own journal", async () => {
    const result = await git.discardStoppedSource(options);
    assert.equal(result.kind, "refused");
    if (result.kind === "refused") assert.ok(result.consumers.includes("ledger_live"));
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(clonePath));
  });
});
