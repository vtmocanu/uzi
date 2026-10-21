// Issue #3153 (section 5): capacity for the Docker lane. With `<dataDir>/runner` now a subPath of
// the persistent data volume, the disk-full guard must keep measuring the volume the runner
// clones and run HOMEs actually live on. These tests pin the PRODUCTION inputs, not a
// filesystem-wide statfs: the path DataVolumeGuard samples is config.dataDir, the runner root a
// production GitCache derives from that same dataDir resolves to the same device under the
// Docker layout, and currentOccupancy does not short-circuit to "not_disk_full" for a worktree
// under it. Admission ("disk sample unknown; claims fail open") and thresholds are not touched.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { loadConfig } from "../src/config.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import { GitCache } from "../src/git.js";
import type { StatfsSample } from "../src/stats.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const GIB = 1024 ** 3;
const bsize = 4096;
const FULL: StatfsSample = { bsize, blocks: (25 * GIB) / bsize, bfree: 1024 * 1024 / bsize, bavail: 1024 * 1024 / bsize, files: 1_600_000, ffree: 1_500_000 };
const VOLUME_DEV = 4242; // the data PVC, including its runner-shared subPath mount at <dataDir>/runner
const OTHER_DEV = 7;     // anything else: an emptyDir, the container layer, /tmp

let fx: Fixture;
beforeEach(() => { fx = makeFixture(); });
afterEach(() => fx.cleanup());

/** A stat seam that models the Docker layout's mounts by path: the data volume owns `dataDir`
 *  and everything beneath it (the runner-shared subPath is a bind mount of the SAME filesystem,
 *  so it reports the volume's device); every other path is a different device. */
function layoutStat(dataDir: string): (p: string) => Promise<{ dev: number }> {
  return async (p) => {
    const rel = path.relative(dataDir, path.resolve(p));
    return { dev: rel === "" || (!rel.startsWith("..") && !path.isAbsolute(rel)) ? VOLUME_DEV : OTHER_DEV };
  };
}

describe("data-volume capacity under the Docker lane layout (#3153)", () => {
  it("samples config.dataDir, and the runner root derived from it is on that same device", async () => {
    const config = loadConfig({ UZI_API_URL: "http://api.invalid", UZI_WORKER_TOKEN: "t", UZI_DATA_DIR: fx.dataDir });
    assert.equal(config.dataDir, fx.dataDir);
    const statfsPaths: string[] = [];
    const guard = new DataVolumeGuard({
      dataDir: config.dataDir,
      stat: layoutStat(config.dataDir),
      statfs: (p) => { statfsPaths.push(p); return FULL; },
    });
    // The production GitCache decides where the Docker-lane runner clones live.
    const git = new GitCache(config.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await git.ensureClone(fx.originPath);
    const worktree = git.runnerClonePath(bare, "issue-3153");
    const runHome = path.join(config.dataDir, "agent-home", "run-3153");
    assert.equal(path.relative(path.join(config.dataDir, "runner"), worktree).startsWith(".."), false,
      `the production runner clone path ${worktree} lies under <dataDir>/runner`);

    const stat = layoutStat(config.dataDir);
    // Fixture sanity checks (the fake stat maps every path under dataDir to VOLUME_DEV by construction); not production pins.
    assert.equal((await stat(config.dataDir)).dev, (await stat(path.join(config.dataDir, "runner"))).dev);
    assert.equal((await stat(worktree)).dev, VOLUME_DEV);

    // The verdict is data_volume_full, i.e. the device-id guard did not short-circuit, and the
    // only statfs target is the data directory itself (not <dataDir>/runner, not an ancestor).
    assert.equal(await guard.currentOccupancy(worktree, runHome), "data_volume_full");
    assert.deepEqual(statfsPaths, [config.dataDir]);

    // The guard is sensitive to the input: a worktree off the volume short-circuits.
    assert.equal(await guard.currentOccupancy(path.join(path.dirname(fx.dataDir), "elsewhere", "wt"), runHome), "not_disk_full");
  });

  it("main.ts hands config.dataDir to the guard, the git cache and the stats volume sample", () => {
    const main = fs.readFileSync(fileURLToPath(new URL("../src/main.ts", import.meta.url)), "utf8");
    assert.match(main, /new DataVolumeGuard\(\{\s*dataDir: config\.dataDir,/);
    assert.match(main, /new GitCache\(config\.dataDir,/);
    assert.match(main, /volumeTotalBytes: \(\) => sampleVolume\(config\.dataDir\)\?\.bytesTotal/);
  });

  it("an unknown or failed device read reports unknown, never not_disk_full (admission semantics are not changed here)", async () => {
    const guard = new DataVolumeGuard({
      dataDir: fx.dataDir,
      stat: async () => { throw new Error("EIO"); },
      statfs: () => FULL,
    });
    assert.equal(await guard.currentOccupancy(path.join(fx.dataDir, "runner", "r", "k"), path.join(fx.dataDir, "agent-home", "x")), "unknown");
  });
});

