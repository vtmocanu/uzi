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
import { LimitReachedError } from "../src/limit.js";
import { nullLogger } from "./helpers.js";
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
// no-ops to null when there is no tracking ref / no work beyond the floor, so an empty
// park brokers nothing. The publish stays on the join-token seam (client.publishCheckpoint),
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
    calls.push({ runId, tipOid, objectCount });
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
): RunRunner {
  return new RunRunner(client, gitCache, factory, nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    questionTimeoutMs: 600,
    gitlab,
  });
}

/** An executor that (optionally) commits `file` in the runner clone, then parks on a
 *  usage limit — the shape the tree-loss bug needs (work committed, nothing pushed). */
function parkFactory(
  homeRoot: string,
  file: string | null,
): { factory: ExecutorFactory; sha: () => string } {
  let sha = "";
  const factory: ExecutorFactory = (runId) => ({
    homeDir: path.join(homeRoot, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        if (file) sha = commitInTree(ctx.worktreePath, file, "work before the park\n");
        fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
        throw new LimitReachedError({
          resetsAtMs: Date.now() + 5 * 3600_000,
          rateLimitType: "five_hour",
        });
      },
    },
  });
  return { factory, sha: () => sha };
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

  // Verified 2026-08-23: with a tracking ref present but unmoved past the floor,
  // checkpointPack does NOT return null (it only nulls on a MISSING tracking ref, git.ts:749-750)
  // — it returns a non-null but EMPTY (zero-object) pack, so the one-shot park publish fires an
  // RPC carrying nothing. That is harmless (origin gains no objects); the invariant the tree-loss
  // fix needs is only that no SPURIOUS non-empty checkpoint is brokered on an empty park.
  it("brokers no non-empty delta on a park with no new work beyond the floor (checkpointPack empty-pack no-op)", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-628-noop-"));
    const { calls, restore } = spyPublish();
    try {
      const { factory } = parkFactory(homeRoot, null); // parks WITHOUT committing
      const claim = gitlabClaim(602, { wait_on_limit: true });
      await runnerWithGit(factory, gitlab).execute(claim);

      assert.ok(parked(claim.run_id), "the run must have parked (else this is vacuous)");
      // No new work beyond origin/default: checkpointPack yields no delta, so either no RPC
      // fires (null pack) or an empty pack (zero objects) is brokered — never a spurious
      // non-empty checkpoint.
      const nonEmpty = calls.filter((c) => c.objectCount > 0);
      assert.strictEqual(
        nonEmpty.length,
        0,
        `an empty park must broker no delta, got ${JSON.stringify(calls)}`,
      );
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
      const brokenGit = new GitCache(fx.dataDir, nullLogger());
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
