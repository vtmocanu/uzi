import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import type { WorkerClient } from "../src/client.js";
import type { RunContext } from "../src/executor.js";
import { GitCache, OVERLAY_COMMIT_PREFIX, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { makeScratchRoot, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { commitInTree, GIT_ENV } from "./runner-codex-sinks-fixture.js";

const WF = ".github/workflows/ci.yml";
const OLD_WORKFLOW = "name: fixture\non: push\njobs: {}\n# old\n";
const NEW_WORKFLOW = "name: fixture\non: push\njobs: {}\n# advanced\n";
const TRACKED = "TRACKED-3101.txt";
const UNTRACKED = "UNTRACKED-3101.txt";
const COMMITTED = "COMMITTED-3101.txt";
const DIRTY_BYTES = "tracked dirty bytes\n";
const UNTRACKED_BYTES = "separate untracked bytes\n";
const COMMITTED_BYTES = "new commit C after earlier checkpoint\n";

function read(repo: string, ...args: string[]): string {
  return execFileSync("git", ["-C", repo, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" });
}

/** Local broker simulation, not real Go handler proof. Only the fixture default seed and
 * emitted packs can supply objects to receiver; receipt assertions run BEFORE each ACK. */
export function shutdownContentProof(fx: Fixture, client: WorkerClient, iid: number, expectedRunId?: string,
  publication: "overlay" | "park_marker" = "overlay") {
  const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
  fs.mkdirSync(scratch, { recursive: true });
  const dir = makeScratchRoot("3101-content-", scratch);
  const receiver = path.join(dir, "receiver.git");
  const ref = `refs/uzi-checkpoints/agent/issue-${iid}`;
  const tips: string[] = [];
  const publisherErrors: unknown[] = [];
  let commitC = "";
  let earlier = "";
  let beforeAck: ((signal: AbortSignal | undefined, generation: number | undefined) => void) | undefined;
  fs.mkdirSync(path.join(fx.originPath, path.dirname(WF)), { recursive: true });
  commitInTree(fx.originPath, WF, OLD_WORKFLOW);
  execFileSync("git", ["clone", "--bare", "--no-local", fx.originPath, receiver],
    { env: GIT_ENV, stdio: "pipe" });
  const original = client.publishCheckpoint;
  client.publishCheckpoint = async (_runId, tip, pack: Readable, signal, generation) => {
    if (expectedRunId !== undefined && _runId !== expectedRunId) {
      return original.call(client, _runId, tip, pack, signal, generation);
    }
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      execFileSync("git", ["-C", receiver, "index-pack", "--stdin", "--fix-thin"],
        { env: GIT_ENV, input: Buffer.concat(chunks), stdio: "pipe" });
      read(receiver, "update-ref", ref, tip);
      assert.equal(read(receiver, "rev-parse", ref).trim(), tip);
      if (tips.length === 0) {
        earlier = tip;
        assert.equal(read(receiver, "show", `${ref}:${TRACKED}`), "tracked before C\n");
      } else {
        assert.equal(tips.length, 1, "only earlier and latest preservation publications");
        assert.equal(read(receiver, "show", `${ref}:${COMMITTED}`), COMMITTED_BYTES);
        assert.equal(read(receiver, "show", `${ref}:${TRACKED}`), DIRTY_BYTES);
        assert.equal(read(receiver, "show", `${ref}:${UNTRACKED}`), UNTRACKED_BYTES);
        assert.equal(read(receiver, "show", `${ref}:${WF}`), publication === "overlay" ? NEW_WORKFLOW : OLD_WORKFLOW,
          "shutdown overlay and credential-free pause marker preserve their distinct workflow contracts");
        read(receiver, "merge-base", "--is-ancestor", earlier, tip);
        read(receiver, "merge-base", "--is-ancestor", commitC, tip);
        if (publication === "overlay") {
          assert.ok(signal, "shutdown broker publication carries its bounded boundary signal");
          assert.equal(signal.aborted, false, "spent execution signals do not abort the publication permit");
        } else {
          assert.ok(signal, "owner pause publication carries its bounded capture signal");
          assert.equal(signal.aborted, false, "pause capture signal remains live through the receiver ACK");
        }
        beforeAck?.(signal, generation);
      }
      tips.push(tip);
      return { ok: true, body: { published: true, ref } };
    } catch (error) {
      publisherErrors.push(error);
      throw error;
    }
  };
  return {
    tips,
    receiver,
    onShutdownReceipt(fn: (signal: AbortSignal | undefined, generation: number | undefined) => void) { beforeAck = fn; },
    async prepare(ctx: RunContext) {
      commitInTree(ctx.worktreePath, TRACKED, "tracked before C\n");
      assert.ok(ctx.checkpoint);
      await ctx.checkpoint({ reap: false });
      assert.equal(tips.length, 1, "earlier checkpoint acknowledged before C");
      commitInTree(ctx.worktreePath, COMMITTED, COMMITTED_BYTES);
      commitC = read(ctx.worktreePath, "rev-parse", "HEAD").trim();
      assert.notEqual(commitC, earlier);
      fs.writeFileSync(path.join(ctx.worktreePath, TRACKED), DIRTY_BYTES);
      fs.writeFileSync(path.join(ctx.worktreePath, UNTRACKED), UNTRACKED_BYTES);
      assert.match(read(ctx.worktreePath, "status", "--porcelain"), / M TRACKED-3101.txt/);
      assert.match(read(ctx.worktreePath, "status", "--porcelain"), /\?\? UNTRACKED-3101.txt/);
      // Advance ONLY the isolated fixture origin after the runner branch has been seeded.
      commitInTree(fx.originPath, WF, NEW_WORKFLOW);
    },
    async verify(local: GitCache) {
      // Resolve the independent receiver ref first: an old checkpoint or a missing
      // file must fail on the bytes lost by #3101, before count/overlay metadata.
      const latest = read(receiver, "rev-parse", ref).trim();
      const content = (repo: string, revision: string, file: string): string | undefined => {
        const files = read(repo, "ls-tree", "--name-only", revision, "--", file).trim();
        return files === file ? read(repo, "show", `${revision}:${file}`) : undefined;
      };
      assert.deepEqual([COMMITTED, TRACKED, UNTRACKED].map(file => content(receiver, latest, file)),
        [COMMITTED_BYTES, DIRTY_BYTES, UNTRACKED_BYTES], "latest receiver ref preserves C plus dirty and untracked bytes");
      // Runner may catch publisher failures; preserve their original assertion after
      // checking the latest received bytes and before checking ACK counts.
      if (publisherErrors.length > 0) throw publisherErrors[0];
      assert.equal(tips.length, 2, "the preservation sink emitted a newer checkpoint");
      const tip = tips[1]!;
      const bare = local.barePathFor(fx.originPath);
      const marker = read(bare, "rev-parse", `refs/uzi-runner/agent/issue-${iid}`).trim();
      assert.deepEqual([COMMITTED, TRACKED, UNTRACKED].map(file => content(bare, marker, file)),
        [COMMITTED_BYTES, DIRTY_BYTES, UNTRACKED_BYTES], "worker bare tracking marker preserves exact local bytes");
      assert.ok(read(bare, "log", "-1", "--format=%s", marker).startsWith(WIP_PARK_COMMIT_PREFIX));
      if (publication === "overlay") {
        assert.notEqual(marker, tip, "local tracking keeps the real marker, not overlay");
        assert.ok(read(receiver, "log", "-1", "--format=%s", tip).startsWith(OVERLAY_COMMIT_PREFIX));
        assert.equal(read(receiver, "rev-parse", `${tip}^1`).trim(), earlier,
          "overlay chains the previous acknowledged checkpoint first");
        const parents = read(receiver, "rev-list", "--parents", "-n", "1", tip).trim().split(/\s+/).slice(1);
        assert.equal(parents.at(-1), marker);
      } else {
        assert.equal(tip, marker, "owner pause publishes exactly its real WIP marker");
        assert.ok(read(receiver, "log", "-1", "--format=%s", tip).startsWith(WIP_PARK_COMMIT_PREFIX));
      }
      assert.equal(read(receiver, "rev-parse", `${marker}^1`).trim(), commitC);
      // A new cache and data directory cannot borrow the original worker's tracking objects.
      const dataDir = path.join(dir, "second-worker");
      fs.mkdirSync(dataDir);
      const recovered = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
      const recoveredBare = await recovered.ensureClone(receiver);
      const clone = await recovered.createOrAttachRunnerClone(recoveredBare, iid, noProofReseed,
        "3101-second-worker", true, tip);
      assert.equal(clone.seededFrom, "checkpoint");
      assert.equal(clone.wipRecovered, true);
      assert.equal(clone.baseCommit, commitC, "recovery peels overlay and soft-resets marker");
      assert.equal(read(clone.path, "rev-parse", "HEAD").trim(), commitC);
      assert.equal(fs.readFileSync(path.join(clone.path, COMMITTED), "utf8"), COMMITTED_BYTES);
      assert.equal(fs.readFileSync(path.join(clone.path, TRACKED), "utf8"), DIRTY_BYTES);
      assert.equal(fs.readFileSync(path.join(clone.path, UNTRACKED), "utf8"), UNTRACKED_BYTES);
      assert.equal(fs.readFileSync(path.join(clone.path, WF), "utf8"), OLD_WORKFLOW,
        "overlay workflow replacement is discarded on recovery");
      assert.deepEqual(read(clone.path, "diff", "--cached", "--name-only").trim().split("\n").sort(),
        [TRACKED, UNTRACKED], "marker edits are recovered staged; no overlay workflow edit remains");
      read(clone.path, "merge-base", "--is-ancestor", earlier, "HEAD");
    },
    cleanup() {
      client.publishCheckpoint = original;
      fs.rmSync(dir, { recursive: true, force: true });
    },
  };
}
