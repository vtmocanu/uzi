import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync, type SpawnSyncReturns } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// The production API drains the request before reporting published:true. These subprocesses
// also cover an early peer response, where the HTTP result alone cannot confirm git's work.
type Mode = "early" | "abort" | "success";

async function child(mode: Mode): Promise<void> {
  const fx = makeFixture();
  const branch = "agent/issue-1725";
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath);
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  fs.writeFileSync(path.join(fx.originPath, "work.txt"), "checkpoint\n");
  execFileSync("git", ["-C", fx.originPath, "add", "work.txt"], { env });
  execFileSync("git", ["-C", fx.originPath, "commit", "-qm", "checkpoint"], { env });
  execFileSync("git", ["-C", bare, "fetch", "--no-tags", fx.originPath,
    `refs/heads/main:refs/uzi-runner/${branch}`], { env });

  const shimDir = fs.mkdtempSync(path.join(os.tmpdir(), "ckpt-git-shim-"));
  const release = path.join(shimDir, "release");
  const shim = path.join(shimDir, "git");
  fs.writeFileSync(shim, `#!/bin/sh\nfor arg in "$@"; do\n  if [ "$arg" = pack-objects ] && [ '${mode}' != success ]; then\n    printf PACK\n    while [ ! -f '${release}' ] && [ -d '${shimDir}' ]; do sleep 0.01; done\n    echo 'intentional pack failure' >&2\n    exit 47\n  fi\ndone\nexec /usr/bin/git "$@"\n`, { mode: 0o755 });
  const oldPath = process.env.PATH;
  process.env.PATH = `${shimDir}:${oldPath}`;

  const abort = new AbortController();
  const server = http.createServer((req, res) => {
    if (mode === "abort") {
      abort.abort();
      res.destroy();
      return;
    }
    const respond = (): void => {
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ published: true, ref: `refs/uzi-checkpoints/${branch}` }));
    };
    if (mode === "early") respond();
    else void (async () => { for await (const _chunk of req) { /* consume the whole pack */ } respond(); })();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as { port: number }).port;
  const client = new WorkerClient(`http://127.0.0.1:${port}`, "test-token", "test", nullLogger());
  const runner = new RunRunner(client, git, () => ({ executor: { run: async () => ({ branch }) } }), nullLogger(), 1);
  const privateGit = git as unknown as {
    spawnGit: (cwd: string, args: string[], stdin?: string) => Promise<{ exited: Promise<number> }>;
  };
  const realSpawn = privateGit.spawnGit.bind(git);
  let producerExit: Promise<number> | undefined;
  privateGit.spawnGit = async (cwd, args, stdin) => {
    const started = await realSpawn(cwd, args, stdin);
    if (args[0] === "pack-objects") producerExit = started.exited;
    return started;
  };
  const realPack = git.checkpointPack.bind(git);
  let packedTip: string | undefined;
  git.checkpointPack = (async (...args: Parameters<typeof git.checkpointPack>) => {
    const packed = await realPack(...args);
    packedTip = packed?.tipOid;
    return packed;
  }) as typeof git.checkpointPack;
  const flight = {
    runId: "run-1725",
    lastCheckpointRefTip: "OLD_CONFIRMED",
    lastAttemptedCheckpointRefTip: undefined as string | undefined,
    reportedPublishOutcomes: new Set<string>(),
    runLog: nullLogger(),
    batcher: { emit() {} },
  };
  try {
    const outcome = await (runner as unknown as {
      publishCheckpointOutcome: (flight: unknown, bare: string, branch: string, overlay?: unknown,
        signal?: AbortSignal) => Promise<unknown>;
    }).publishCheckpointOutcome(flight, bare, branch, undefined, abort.signal);
    fs.writeFileSync(release, "go");
    const exit = await producerExit;
    process.stdout.write(JSON.stringify({ outcome, confirmed: flight.lastCheckpointRefTip,
      attempted: flight.lastAttemptedCheckpointRefTip, packedTip, exit }) + "\n");
  } finally {
    process.env.PATH = oldPath;
    server.closeAllConnections();
    server.close();
    fx.cleanup();
    fs.rmSync(shimDir, { recursive: true, force: true });
  }
}

if (process.env.UZI_1725_CHILD === "1") {
  await child(process.env.UZI_1725_MODE as Mode);
} else {
  for (const mode of ["early", "abort", "success"] as const) it(`${mode} checkpoint publication settles the producer`, () => {
    const script = fileURLToPath(import.meta.url);
    // The child's own TMPDIR, owned and removed here (issue #2020): a timeout kill skips the
    // child's `finally`, which would otherwise leak its fixture and git shim dir.
    const childTmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1725-"));
    let result: SpawnSyncReturns<string>;
    try {
      result = spawnSync(process.execPath, ["--import", "tsx", script], {
        cwd: path.resolve(path.dirname(script), ".."),
        env: { ...process.env, TMPDIR: childTmp, UZI_1725_CHILD: "1", UZI_1725_MODE: mode },
        encoding: "utf8",
        timeout: 15_000,
      });
    } finally {
      // Retries as fixture cleanup does: a killed child's git may still be writing. Removing the
      // shim dir also ends an orphaned shim's wait loop, which exits once its dir is gone.
      fs.rmSync(childTmp, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
    }
    assert.equal(result.status, 0, result.stderr || result.error?.message);
    const observed = JSON.parse(result.stdout.trim());
    assert.match(observed.packedTip, /^[0-9a-f]{40}$/);
    assert.equal(typeof observed.exit, "number");
    if (mode === "success") {
      assert.equal(observed.exit, 0);
      assert.deepEqual(observed.outcome, { published: true });
      assert.equal(observed.confirmed, observed.packedTip);
      assert.equal(observed.attempted, undefined);
    } else {
      assert.notEqual(observed.exit, 0, JSON.stringify(observed));
      assert.equal(observed.confirmed, "OLD_CONFIRMED", JSON.stringify(observed));
      assert.equal(observed.attempted, observed.packedTip, JSON.stringify(observed));
      assert.equal(observed.outcome.published, false);
    }
  });
}
