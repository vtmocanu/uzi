import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, RunnerCloneImportError } from "../src/git.js";

// Streaming git producers (bundle create, pack-objects) must cap the stderr they retain: a
// producer that floods stderr and fails must neither hang (pipe drained) nor carry the whole
// flood into the thrown error. A PATH shim stands in for a misbehaving git.

const FLOOD_BYTES = 300_000;
const STDERR_CAP = 64 * 1024;
const PREFIX_ALLOWANCE = 1024;

const GIT_ENV: NodeJS.ProcessEnv = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
delete GIT_ENV.GIT_ALTERNATE_OBJECT_DIRECTORIES;
delete GIT_ENV.GIT_OBJECT_DIRECTORY;
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

let fx: Fixture;
let git: GitCache;
let shimDir: string;
let origPath: string | undefined;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
}

/** Written by the shim only once its whole stderr flood was accepted (i.e. drained). */
function ackPath(): string {
  return path.join(shimDir, "flood-written");
}

function assertFloodDrained(): void {
  assert.ok(fs.existsSync(ackPath()), `the shim's full ${FLOOD_BYTES}-byte stderr write did not complete`);
}

function realGit(): string {
  return execFileSync("sh", ["-c", "command -v git"], { encoding: "utf8" }).trim();
}

/** Install a `git` shim first on PATH: it floods stderr and exits 3 for `bundle` and
 *  `pack-objects`, and execs the real git for everything else. */
function installShim(): void {
  const real = realGit();
  shimDir = fs.mkdtempSync(path.join(os.tmpdir(), "git-stderr-shim-"));
  const full = `#!/bin/sh
real='${real}'
sub=""
skip=0
for a in "$@"; do
  if [ "$skip" -gt 0 ]; then skip=$((skip-1)); continue; fi
  case "$a" in
    -C|-c) skip=1 ;;
    -*) ;;
    *) sub="$a"; break ;;
  esac
done
case "$sub" in
  bundle|pack-objects)
    # The ack is written only after all ${FLOOD_BYTES} bytes reached stderr: a collector that
    # closed the pipe early would SIGPIPE tr and leave no ack.
    if head -c ${FLOOD_BYTES} /dev/zero | tr '\\0' e >&2; then : > '${ackPath()}'; fi
    exit 3 ;;
esac
exec "$real" "$@"
`;
  const shim = path.join(shimDir, "git");
  fs.writeFileSync(shim, full, { mode: 0o755 });
  origPath = process.env.PATH;
  process.env.PATH = `${shimDir}${path.delimiter}${origPath ?? ""}`;
}

function removeShim(): void {
  if (origPath === undefined) delete process.env.PATH;
  else process.env.PATH = origPath;
  fs.rmSync(shimDir, { recursive: true, force: true });
}

async function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`did not settle within ${ms}ms`)), ms);
  });
  try {
    return await Promise.race([p, timeout]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

function assertCappedFlood(message: string): void {
  assert.match(message, /e{1000}/, "stderr flood should be captured");
  assert.ok(
    message.length <= STDERR_CAP + PREFIX_ALLOWANCE,
    `error message is ${message.length} chars, expected <= ${STDERR_CAP + PREFIX_ALLOWANCE}`,
  );
}

beforeEach(() => {
  fx = makeFixture({ "a.txt": "base\n" });
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
});

afterEach(() => fx.cleanup());

describe("streaming git producers cap retained stderr", () => {
  it("produceRecoveryBundle: a stderr-flooding bundle create rejects promptly with bounded stderr", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const sourceSha = gitIn(bare, ["rev-parse", "HEAD"]);
    const outPath = path.join(fx.dataDir, "out.bundle");
    installShim();
    try {
      const err = await withTimeout(
        git.produceRecoveryBundle(bare, { sourceSha, outPath }).then(
          () => assert.fail("expected rejection"),
          (e: unknown) => e,
        ),
        60_000,
      );
      assertCappedFlood(err instanceof Error ? err.message : String(err));
      assertFloodDrained();
    } finally {
      removeShim();
    }
  });

  it("ensureRunnerCloneObjects: a stderr-flooding pack-objects rejects promptly with bounded stderr", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1, noProofReseed, "run-1", false, undefined, undefined, { selfContained: true });
    fs.writeFileSync(path.join(fx.originPath, "main-only.txt"), "main advances\n");
    gitIn(fx.originPath, ["add", "."]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "main advances"]);
    const defaultTip = await git.fetchDefaultTip(bare, "main");
    installShim();
    try {
      const err = await withTimeout(
        git.ensureRunnerCloneObjects(bare, rc.path, defaultTip, []).then(
          () => assert.fail("expected rejection"),
          (e: unknown) => e,
        ),
        60_000,
      );
      assert.ok(err instanceof RunnerCloneImportError, `unexpected error type: ${String(err)}`);
      assertCappedFlood(err.message);
      assertFloodDrained();
    } finally {
      removeShim();
    }
  });
});
