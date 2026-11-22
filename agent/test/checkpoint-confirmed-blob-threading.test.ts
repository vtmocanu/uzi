import { it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";
import { GitCache, type CheckpointPublicationContext } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { RunRunner } from "../src/runner.js";
import { nullLogger } from "./helpers.js";

const TIP = "a".repeat(40);
const CONFIRMED = "c".repeat(40);
const publicationContext: CheckpointPublicationContext = {
  sourceSha: TIP, candidateSha: TIP, excludeSha: TIP, packExcludeShas: [TIP],
  restrictionFacts: { enrolled: false, blocked: false, roots: [], flaggedOIDs: [],
    fingerprint: "fixture-confirmed-absence" },
};

function runnerWith(git: GitCache) {
  const client = new WorkerClient("http://unused", "test", "test", nullLogger());
  const runner = new RunRunner(client, git, () => ({ executor: { run: async () => ({ branch: "branch" }) } }), nullLogger(), 1,
    undefined, { checkpointTestHooks: { retryBackoffMs: [0, 0] as const } });
  return { client, runner };
}

it("scan-and-pin asks resolveCheckpointRange to capture the confirmed tip as the blob omission", async () => {
  const git = new GitCache("/unused", nullLogger());
  const seen: unknown[] = [];
  git.resolveCheckpointRange = async (_bare, _branch, opts) => { seen.push(opts); return null; };
  git.trackingTip = async () => null;
  const { runner } = runnerWith(git);
  const flight = { lastCheckpointRefTip: CONFIRMED, checkpointDefaultBranch: "main", publishedRealTips: [TIP] };
  const result = await (runner as unknown as {
    scanCheckpointForPublish: (f: unknown, bare: string, branch: string) => Promise<unknown>;
  }).scanCheckpointForPublish(flight, "bare", "branch");
  assert.deepEqual(result, { kind: "none" });
  assert.deepEqual(seen, [{ defaultBranch: "main", confirmedTip: CONFIRMED, captureConfirmedBlob: true,
    extraFloors: [TIP] }]);
});

it("checkpoint publish passes the confirmed tip to checkpointPack as confirmedBlobTip", async () => {
  const abort = new AbortController();
  const git = new GitCache("/unused", nullLogger());
  git.readRemediationRestrictionFacts = async () => publicationContext.restrictionFacts!;
  const { client, runner } = runnerWith(git);
  const seam = runner as unknown as {
    requireTrackingOwned: () => Promise<string>;
    checkpointRemediationHold: () => Promise<boolean>;
    publishCheckpointOutcome: (flight: unknown, bare: string, branch: string, overlay: undefined, signal: AbortSignal) => Promise<unknown>;
  };
  seam.requireTrackingOwned = async () => TIP;
  seam.checkpointRemediationHold = async () => false;
  const packOptions: unknown[] = [];
  git.checkpointPack = async (_bare, _branch, _overlay, _pinned, _step, _source, guard, _chain, options) => {
    packOptions.push(options);
    await guard?.(TIP, publicationContext);
    return { tipOid: TIP, pack: Readable.from(["pack"]), exited: Promise.resolve(0), cancel: () => undefined,
      replay: async () => { throw new Error("no replay expected"); } };
  };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const _chunk of pack) void _chunk;
    return { ok: true, body: { published: true } } as Awaited<ReturnType<typeof client.publishCheckpoint>>;
  };
  const flight = { cancel: abort, steering: { lifecycleSignal: () => abort.signal }, runId: "retry", runKind: "issue",
    barePath: "bare", branch: "branch", lastCheckpointRefTip: CONFIRMED, lastAttemptedCheckpointRefTip: undefined,
    checkpointDefaultBranch: "main", providerKeyForms: [], reportedPublishOutcomes: new Set<string>(), runLog: nullLogger(),
    batcher: { emit: () => undefined } };
  await seam.publishCheckpointOutcome(flight, "bare", "branch", undefined, abort.signal);
  assert.equal(packOptions.length, 1);
  const options = packOptions[0] as { confirmedBlobTip?: string; markerTrustedTip?: string; defaultBranch?: string };
  assert.equal(options.confirmedBlobTip, CONFIRMED);
  assert.equal(options.markerTrustedTip, CONFIRMED);
  assert.equal(options.defaultBranch, "main");
});
