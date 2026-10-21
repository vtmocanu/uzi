import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { type ExecutorResult, type RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { LimitReachedError } from "../src/limit.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// ── PRD #628 M2 — publish a checkpoint on the limit-park path ─────────────────
//
// A limit_wait run re-claimed by a DIFFERENT worker must recover its committed tree
// from refs/uzi-checkpoints/<branch> rather than reseed off default and re-implement
// already-committed milestones. The park path (runner.ts catch/park block) now calls
// publishCheckpointBestEffort AFTER the fetch-back (so the tracking ref is current),
// one-shot and unconditional — the mid-run time-gate does not reach here. checkpointPack
// no-ops to null when there is no tracking ref. A park with no work beyond the floor re-sends
// only the run's own base commit (issue #3242: a bounded retransmission so the broker holds the
// declared tip even after the default branch moves), never content beyond origin's history. The publish stays on the join-token seam (client.publishCheckpoint),
// never a git push / PAT.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function commitInTree(treePath: string, file: string, content: string): string {
  fs.writeFileSync(path.join(treePath, file), content);
  execFileSync("git", ["-C", treePath, "add", file], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", treePath, ...IDENT, "commit", "-m", `add ${file}`], {
    env: GIT_ENV,
    stdio: "pipe",
  });
  return execFileSync("git", ["-C", treePath, "rev-parse", "HEAD"], {
    env: GIT_ENV,
    encoding: "utf8",
  }).trim();
}

/** A recorded publishCheckpoint call: the tip oid and the number of objects the pack
 *  carries, parsed from the packfile header (bytes 8..12 are the big-endian object count,
 *  0 for an empty/no-op pack). */
interface PublishCall {
  runId: string;
  tipOid: string;
  objectCount: number;
  /** The buffered pack bytes, for object-level assertions. */
  pack: Buffer;
}

/** Fully buffer a Readable and return the concatenated bytes. */
function drain(stream: Readable): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    stream.on("data", (c: Buffer) => chunks.push(Buffer.from(c)));
    stream.on("end", () => resolve(Buffer.concat(chunks)));
    stream.on("error", reject);
  });
}

/** Spy on client.publishCheckpoint: buffer the pack, parse its object count, record the
 *  call, and optionally throw to prove best-effort. Returns [calls, restore]. */
function spyPublish(opts: { throws?: boolean } = {}): {
  calls: PublishCall[];
  restore: () => void;
} {
  const calls: PublishCall[] = [];
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    runId: string,
    tipOid: string,
    pack: Readable,
  ) => {
    const bytes = await drain(pack);
    // Packfile: "PACK" (4) + version (4) + object count (4, big-endian).
    const objectCount = bytes.length >= 12 ? bytes.readUInt32BE(8) : -1;
    calls.push({ runId, tipOid, objectCount, pack: bytes });
    if (opts.throws) throw new Error("boom publish");
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/agent/issue-x` } };
  };
  return {
    calls,
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

function runnerWithGit(
  factory: ExecutorFactory,
  gitlab: ReturnType<typeof fakeGitlab>["gitlab"],
  gitCache = git,
  retryBackoffMs: readonly [number, number] = [0, 0],
): RunRunner {
  return new RunRunner(client, gitCache, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
    checkpointTestHooks: { retryBackoffMs },
  });
}

/** An executor that (optionally) commits `file` in the runner clone, then parks on a
 *  usage limit — the shape the tree-loss bug needs (work committed, nothing pushed). */
function parkFactory(
  homeRoot: string,
  file: string | null,
): { factory: ExecutorFactory; sha: () => string; startHead: () => string } {
  let sha = "", startHead = "";
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        // The clone's starting commit, read before the executor changes anything.
        startHead = execFileSync("git", ["-C", ctx.worktreePath, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
        if (file) sha = commitInTree(ctx.worktreePath, file, "work before the park\n");
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        throw new LimitReachedError({
          resetsAtMs: Date.now() + 5 * 3600_000,
          rateLimitType: "five_hour",
        });
      },
    },
  });
  return { factory, sha: () => sha, startHead: () => startHead };
}

const parked = (runId: string) =>
  api.states.some((s) => s.runId === runId && s.body.status === "limit_wait");

/** The `status`-kind feed lines the worker emitted for a run. */
const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

/** Spy on client.publishCheckpoint that drains the pack then returns a non-2xx PublishResult
 *  (issue #1030), to drive the park-publish FAILURE feed line. Returns [restore]. */
function spyPublishHttpError(httpStatus: number): { restore: () => void } {
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string,
    _tipOid: string,
    pack: Readable,
  ) => {
    await drain(pack);
    return { ok: false, httpStatus };
  };
  return {
    restore: () => {
      (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig;
    },
  };
}

describe("RunRunner — checkpoint on the limit-park path (PRD #628 M2)", () => {
  it("expired park permit interrupts retry backoff without a second upload", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-park-retry-"));
    const { factory } = parkFactory(homeRoot, "RETRY.txt");
    // A long backoff proves the expired permit, not the delay, ends the retry.
    const r = runnerWithGit(factory, gitlab, git, [60_000, 60_000]);
    const abort = new AbortController();
    const seam = r as unknown as {
      reapForSink: (executor: unknown, options: { boundary: string },
        body: (permit?: { signal: AbortSignal }) => Promise<unknown>, flight: unknown) => Promise<unknown>;
    };
    const reap = seam.reapForSink.bind(r);
    seam.reapForSink = (executor, options, body, flight) => reap(executor, options,
      options.boundary === "park" ? () => body({ signal: abort.signal }) : body, flight);
    const publish = client.publishCheckpoint.bind(client);
    let calls = 0;
    client.publishCheckpoint = async (_run, _tip, pack, signal) => {
      calls++;
      assert.equal(signal, abort.signal, "park forwards the original permit signal");
      await drain(pack);
      setTimeout(() => abort.abort(), 10);
      return { ok: false, httpStatus: 500 };
    };
    try {
      const claim = gitlabClaim(628991, { wait_on_limit: true });
      await r.execute(claim);
      assert.equal(calls, 1);
      assert.equal(abort.signal.aborted, true);
      assert.ok(parked(claim.run_id), "deadline cannot undo the park");
    } finally {
      client.publishCheckpoint = publish;
      seam.reapForSink = reap;
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("publishes a checkpoint carrying the committed work when a park has new commits", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-628-pub-"));
    const { calls, restore } = spyPublish();
    try {
      const { factory, sha } = parkFactory(homeRoot, "WORK.txt");
      const claim = gitlabClaim(601, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(parked(claim.run_id), "the run must have parked (else this is vacuous)");
      assert.strictEqual(calls.length, 1, "the park path published exactly one checkpoint");
      assert.strictEqual(
        calls[0]!.runId,
        claim.run_id,
        "the publish is brokered for this run over its join token",
      );
      assert.strictEqual(
        calls[0]!.tipOid,
        sha(),
        "the checkpoint carries the tracking-ref tip = the committed work",
      );
      assert.ok(
        calls[0]!.objectCount > 0,
        `the pack is non-empty (carries the new commit + tree + blob), got ${calls[0]!.objectCount} objects`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // With a tracking ref present but unmoved past the floor, checkpointPack does NOT return null
  // (it only nulls on a MISSING tracking ref). Since issue #3242 the zero range packs the run's base
  // commit over its parents, so the broker holds the declared tip even after the default branch
  // moves: a bounded retransmission of what origin already has. The invariant the #628 tree-loss
  // fix needs is that an empty park never brokers a SPURIOUS checkpoint: no tip but the run's own
  // starting commit, and no object beyond that commit's history.
  it("brokers only the run's own base on a park with no new work beyond the floor", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-628-noop-"));
    const { calls, restore } = spyPublish();
    try {
      const { factory, startHead } = parkFactory(homeRoot, null); // parks WITHOUT committing
      const claim = gitlabClaim(602, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(parked(claim.run_id), "the run must have parked (else this is vacuous)");
      const base = startHead();
      assert.match(base, /^[0-9a-f]{40}$/, "the clone's starting commit was captured");
      const ours = calls.filter((c) => c.runId === claim.run_id);
      assert.ok(ours.length > 0, `the park published for this run (else this is vacuous), got ${JSON.stringify(calls.map((c) => ({ runId: c.runId, tipOid: c.tipOid, objectCount: c.objectCount })))}`);
      const defaultTip = execFileSync("git", ["-C", fx.originPath, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
      const reachable = new Set(
        execFileSync("git", ["-C", fx.originPath, "rev-list", "--objects", base], { env: GIT_ENV, encoding: "utf8" })
          .split("\n").filter(Boolean).map((line) => line.split(" ")[0]),
      );
      for (const call of ours) {
        assert.equal(call.tipOid, base, "an empty park declares only the run's starting commit");
        execFileSync("git", ["-C", fx.originPath, "merge-base", "--is-ancestor", call.tipOid, defaultTip], { env: GIT_ENV });
        const probe = fs.mkdtempSync(path.join(homeRoot, "pack-"));
        execFileSync("git", ["init", "-q", "--bare", probe], { env: GIT_ENV });
        execFileSync("git", ["-C", probe, "unpack-objects", "-q"], { env: GIT_ENV, input: call.pack });
        const objects = execFileSync("git", ["-C", probe, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"],
          { env: GIT_ENV, encoding: "utf8" }).split("\n").filter(Boolean);
        assert.ok(objects.includes(base), "the pack carries the declared commit (#3242), so an empty pack cannot pass");
        const foreign = objects.filter((o) => !reachable.has(o));
        assert.deepEqual(foreign, [], "the pack carries nothing beyond the starting commit's history");
      }
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("a throwing publish still parks the run (best-effort, ADR-628 guardrail)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-628-boom-"));
    const { calls, restore } = spyPublish({ throws: true });
    try {
      const { factory } = parkFactory(homeRoot, "WORK.txt");
      const claim = gitlabClaim(603, { wait_on_limit: true });
      // Must NOT reject even though publishCheckpoint throws.
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(calls.length > 0, "the publish was attempted");
      assert.ok(
        parked(claim.run_id),
        "a failed checkpoint publish must not change the limit_wait outcome",
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("never publishes when the fetch-back left no tracking ref (checkpointPack returns null)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-628-null-"));
    const { calls, restore } = spyPublish();
    try {
      // A real GitCache whose ONLY broken method is the fetch-back: no tracking ref is
      // written, so checkpointPack finds no tip and returns null — no RPC at all.
      const brokenGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
      brokenGit.fetchAgentBranch = async () => {
        throw new Error("fetch-back boom");
      };
      const { factory } = parkFactory(homeRoot, "WORK.txt");
      const claim = gitlabClaim(604, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab, brokenGit).execute(claim);

      assert.ok(parked(claim.run_id), "a broken fetch-back still parks (best-effort)");
      assert.strictEqual(
        calls.length,
        0,
        `no tracking ref ⇒ checkpointPack null ⇒ no publish RPC, got ${JSON.stringify(calls)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  // ── issue #1030: the park-publish result is EXPLICIT on the feed ────────────
  it("states 'park checkpoint published to origin' on the feed when the publish lands", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-park-ok-"));
    const { restore } = spyPublish(); // returns { ok: true, body: { published: true, … } }
    try {
      const { factory } = parkFactory(homeRoot, "WORK.txt");
      const claim = gitlabClaim(605, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(parked(claim.run_id), "the run must have parked (else this is vacuous)");
      const feed = statusTexts(claim.run_id);
      assert.ok(
        feed.includes("park checkpoint published to origin"),
        `expected the park-published feed line, got ${JSON.stringify(feed)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });

  it("warns on the feed that a resume restarts from default when the park publish fails, and still parks", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1030-park-fail-"));
    const { restore } = spyPublishHttpError(500);
    try {
      const { factory } = parkFactory(homeRoot, "WORK.txt");
      const claim = gitlabClaim(606, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(
        parked(claim.run_id),
        "a failed checkpoint publish must not change the limit_wait outcome (best-effort)",
      );
      const feed = statusTexts(claim.run_id);
      assert.ok(
        feed.some((t) =>
          t.includes(
            "park checkpoint NOT published — a resume on another worker will restart from the default branch",
          ),
        ),
        `expected the park-not-published consequence line, got ${JSON.stringify(feed)}`,
      );
      assert.ok(
        feed.some((t) => t.includes("checkpoint publish failed: HTTP 500")),
        `expected the generic HTTP-status line naming the cause, got ${JSON.stringify(feed)}`,
      );
    } finally {
      restore();
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
  });
});
