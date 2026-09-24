// Issue #1607: HOME cleanup under the REAL PRD #51 worker/runner uid split.
//
// Runs INSIDE the worker image, started through its root entrypoint, so this process is
// the `worker` uid (10001) holding only the controller's SETUID/SETGID, exactly as in
// production. The fixture trees are written as `runner` (10002) through the production
// setpriv wrapper, reproducing the ownership measured on leaked hosted HOMEs: a
// worker-owned HOME root, runner-owned `0555` Go module-cache directories and a
// runner-owned private `0700` `.claude/projects`.
//
// A host or single-uid run cannot manufacture that split, and a run executed wholly as
// root bypasses DAC, so the first test REFUSES to pass anywhere else instead of skipping.

import { describe, it, before } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

type RmtreeModule = typeof import("../../agent/src/rmtree.js");
type RunnerUidModule = typeof import("../../agent/src/runner-uid.js");
type ReclaimModule = typeof import("../../agent/src/home-reclaim.js");

// The code under test is loaded from the image's /app/src by default, so a built image
// proves what it ships. HOME_SPLIT_SRC points it at a mounted source tree instead.
const SRC = process.env.HOME_SPLIT_SRC ?? "/app/src";
const load = async <T>(name: string): Promise<T> =>
  (await import(pathToFileURL(path.join(SRC, name)).href)) as T;

const HOME_ROOT = "/data/agent-home";
const SENTINEL_DIR = "/data/home-split-sentinel";
const SENTINEL_FILE = path.join(SENTINEL_DIR, "keep.txt");

let rmtree: RmtreeModule;
let runnerUid: RunnerUidModule;
let reclaim: ReclaimModule;

/** Run a shell snippet as `runner` through the production setpriv wrapper. */
function asRunner(script: string, ...args: string[]): void {
  const wrapped = runnerUid.runnerCommand("/bin/sh", ["-c", script, "sh", ...args]);
  execFileSync(wrapped.command, wrapped.args, { stdio: "inherit", env: { PATH: "/usr/bin:/bin" } });
}

/**
 * Create a HOME the way production does (worker-owned root, group `runner` via the setgid
 * parent) and fill it as `runner` with the leaked shape, including symlinks that point at
 * a worker-owned sentinel outside the HOME.
 */
async function makeLeakedHome(name: string, rootMode: number): Promise<string> {
  const home = path.join(HOME_ROOT, name);
  await fs.mkdir(home);
  await fs.chmod(home, 0o2770);
  asRunner(
    `set -e
     h="$1"; s="$2"
     mod="$h/go/pkg/mod/gopkg.in/inf.v0@v0.9.1"
     mkdir -p "$mod" "$h/.claude/projects/-app"
     echo 'package inf' > "$mod/dec.go"
     echo '{}' > "$h/.claude/projects/-app/session.jsonl"
     ln -s "$s" "$h/escape-dir"
     ln -s "$s/keep.txt" "$mod/escape-file"
     chmod 0444 "$mod/dec.go"
     chmod 0555 "$mod" "$h/go/pkg/mod/gopkg.in"
     chmod 0700 "$h/.claude/projects" "$h/.claude/projects/-app"`,
    home,
    SENTINEL_DIR,
  );
  // The root mode last: advice HOMEs were observed at 2700 on a hosted worker.
  await fs.chmod(home, rootMode);
  return home;
}

async function exists(p: string): Promise<boolean> {
  return fs.lstat(p).then(() => true, () => false);
}

async function assertSentinelIntact(): Promise<void> {
  assert.equal(await fs.readFile(SENTINEL_FILE, "utf8"), "keep\n", "the sentinel outside the HOME must be untouched");
  const st = await fs.stat(SENTINEL_FILE);
  assert.equal(st.mode & 0o777, 0o644, "the sentinel's mode must be unchanged");
}

const silentLog = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} };

describe("HOME cleanup under the real uid split (#1607)", () => {
  before(async () => {
    rmtree = await load<RmtreeModule>("rmtree.ts");
    runnerUid = await load<RunnerUidModule>("runner-uid.ts");
    reclaim = await load<ReclaimModule>("home-reclaim.ts");
    await fs.mkdir(SENTINEL_DIR, { recursive: true });
    await fs.writeFile(SENTINEL_FILE, "keep\n");
    await fs.chmod(SENTINEL_FILE, 0o644);
  });

  it("runs as the worker uid under the split, not as root or single-uid", () => {
    assert.equal(process.getuid?.(), runnerUid.WORKER_UID, "the fixture must run as the worker uid");
    assert.equal(runnerUid.uidSplitActive(), true, "the entrypoint must have established UZI_UID_SPLIT=1");
  });

  it("POSITIVE CONTROL: the worker-uid rmTreeForce cannot remove a runner-written HOME", async () => {
    const home = await makeLeakedHome("00000000-0000-4000-8000-000000000001", 0o2770);
    await assert.rejects(rmtree.rmTreeForce(home), (e: NodeJS.ErrnoException) =>
      e.code === "EACCES" || e.code === "EPERM",
    );
    assert.equal(await exists(home), true, "the pre-fix path must strand the HOME in this fixture");
    await rmtree.rmHomeTree(home);
  });

  it("rmHomeTree removes a runner-written run HOME without following symlinks out of it", async () => {
    const home = await makeLeakedHome("00000000-0000-4000-8000-000000000002", 0o2770);
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false, "the whole run HOME must be gone");
    await assertSentinelIntact();
  });

  it("rmHomeTree removes an advice scratch HOME whose root is 2700", async () => {
    const home = await makeLeakedHome("uzi-summary-fixture", 0o2700);
    await rmtree.rmHomeTree(home);
    assert.equal(await exists(home), false, "the advice HOME must be gone");
    await assertSentinelIntact();
  });

  it("the startup reclaim removes only API-confirmed-terminal HOMEs", async () => {
    const terminal = await makeLeakedHome("00000000-0000-4000-8000-00000000000a", 0o2770);
    const running = await makeLeakedHome("00000000-0000-4000-8000-00000000000b", 0o2770);
    const unknown = await makeLeakedHome("00000000-0000-4000-8000-00000000000c", 0o2770);
    const statuses: Record<string, string | undefined> = {
      [path.basename(terminal)]: "completed",
      [path.basename(running)]: "running",
    };
    const summary = await reclaim.reclaimStrandedRunHomes(
      HOME_ROOT,
      async (id) => statuses[id],
      silentLog as never,
      { minAgeMs: 0 },
    );
    assert.equal(await exists(terminal), false, "the terminal run's HOME must be reclaimed");
    assert.equal(await exists(running), true, "a non-terminal run's HOME must be untouched");
    assert.equal(await exists(unknown), true, "an unknown-status HOME must be untouched");
    // The transcript sits in a runner-private 0700 dir the worker cannot stat, so check as runner.
    asRunner(`test -f "$1/.claude/projects/-app/session.jsonl"`, running);
    assert.equal(summary.removed, 1);
    assert.equal(summary.failed, 0);
    await assertSentinelIntact();
    await rmtree.rmHomeTree(running);
    await rmtree.rmHomeTree(unknown);
  });
});
