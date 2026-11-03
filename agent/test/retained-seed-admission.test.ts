import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { DataVolumeGuard } from "../src/disk-full.js";
import { GitCache } from "../src/git.js";
import {
  RETAINED_SEED_ADMISSION_POLICY,
  decideRetainedSeedAdmission,
  type RetainedSeedAdmissionPolicy,
} from "../src/retained-seed-admission.js";
import type { StatfsSample } from "../src/stats.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3865: the retained-seed admission arithmetic, its bounded footprint measurement and the
// device-attributed statfs it samples. The differential check against DiskGovernor lives in
// cache-cap.test.ts.

const GIB = 1024 ** 3;
const BSIZE = 4096;

/** A statfs of `blocks` x 4 KiB with `used` blocks in use and (optionally) an inode pair. */
function statfs(blocks: number, used: number, inodes?: { files: number; used: number }): StatfsSample {
  return {
    bsize: BSIZE,
    blocks,
    bfree: blocks - used,
    bavail: blocks - used,
    ...(inodes ? { files: inodes.files, ffree: inodes.files - inodes.used } : {}),
  };
}

/** A 100 GiB volume in 4 KiB blocks, so a 1 GiB estimate is exactly 1% of it. */
const BLOCKS = (100 * GIB) / BSIZE;
const usedBlocks = (fraction: number): number => Math.round(BLOCKS * fraction);
const COMPLETE = (bytes: number, inodes = 0) => ({ bytes, inodes, complete: true });

describe("decideRetainedSeedAdmission", () => {
  it("admits when the projection stays strictly below the threshold (bytes only)", () => {
    // 50% used; 1 GiB x 1.25 + 1% of 100 GiB = 2.25 GiB -> 52.25%.
    const d = decideRetainedSeedAdmission({ raw: statfs(BLOCKS, usedBlocks(0.5)), threshold: 0.87, estimate: COMPLETE(GIB) });
    assert.equal(d.kind, "admit");
    assert.ok(d.kind === "admit" && Math.abs(d.projected - 0.5225) < 1e-9);
    assert.ok(d.kind === "admit" && d.estimateComplete && d.current === 0.5 && d.threshold === 0.87);
  });

  it("refuses on current pressure at the threshold, before consulting the estimate", () => {
    const at = decideRetainedSeedAdmission({ raw: statfs(1000, 870), threshold: 0.87, estimate: COMPLETE(0) });
    assert.equal(at.kind, "refuse_current");
    const below = decideRetainedSeedAdmission({ raw: statfs(1000, 869), threshold: 0.87, estimate: COMPLETE(0) });
    assert.notEqual(below.kind, "refuse_current");
    const over = decideRetainedSeedAdmission({ raw: statfs(1000, 990), threshold: 0.87, estimate: undefined });
    assert.equal(over.kind, "refuse_current");
  });

  it("a projection exactly at the threshold refuses and one block below admits", () => {
    // Zero-margin, zero-allowance policy: projected = (used + estimate) / total with exact integers.
    const policy: RetainedSeedAdmissionPolicy = { ...RETAINED_SEED_ADMISSION_POLICY, marginRatio: 0, homeAllowanceFraction: 0 };
    const exact = decideRetainedSeedAdmission({ raw: statfs(1000, 500), threshold: 0.75, estimate: COMPLETE(250 * BSIZE), policy });
    assert.equal(exact.kind, "refuse_projected");
    assert.ok(exact.kind === "refuse_projected" && exact.projected === 0.75);
    const below = decideRetainedSeedAdmission({ raw: statfs(1000, 500), threshold: 0.75, estimate: COMPLETE(249 * BSIZE), policy });
    assert.equal(below.kind, "admit");
  });

  it("applies custom margins and home allowance", () => {
    const raw = statfs(1000, 500);
    const estimate = COMPLETE(100 * BSIZE);
    const policy = (marginRatio: number, homeAllowanceFraction: number): RetainedSeedAdmissionPolicy =>
      ({ ...RETAINED_SEED_ADMISSION_POLICY, marginRatio, homeAllowanceFraction });
    // 500 + 100 x 2 + 50 = 750 -> 0.75
    const d = decideRetainedSeedAdmission({ raw, threshold: 0.8, estimate, policy: policy(1, 0.05) });
    assert.ok(d.kind === "admit" && Math.abs(d.projected - 0.75) < 1e-9);
    assert.equal(decideRetainedSeedAdmission({ raw, threshold: 0.75, estimate, policy: policy(1, 0.05) }).kind, "refuse_projected");
    assert.equal(decideRetainedSeedAdmission({ raw, threshold: 0.6, estimate, policy: policy(0, 0) }).kind, "refuse_projected");
  });

  it("projects the inode pair on its own and takes the larger fraction", () => {
    // Bytes at 10% with a tiny estimate; inodes at 80% of 1M with a 100k-inode estimate: inodes win.
    const raw = statfs(BLOCKS, usedBlocks(0.1), { files: 1_000_000, used: 800_000 });
    const d = decideRetainedSeedAdmission({ raw, threshold: 0.9, estimate: COMPLETE(GIB, 100_000) });
    // inodes: (800k + 100k x 1.25 + 1% of 1M) / 1M = 0.935
    assert.equal(d.kind, "refuse_projected");
    assert.ok(d.kind === "refuse_projected" && Math.abs(d.projected - 0.935) < 1e-9);
  });

  it("is driven by the inode pair when it is the larger one, whatever the byte estimate", () => {
    const raw = statfs(BLOCKS, usedBlocks(0.1), { files: 1000, used: 900 });
    const d = decideRetainedSeedAdmission({ raw, threshold: 0.95, estimate: COMPLETE(0, 10) });
    // inodes: (900 + 10 x 1.25 + 10) / 1000 = 0.9225
    assert.ok(d.kind === "admit" && Math.abs(d.projected - 0.9225) < 1e-9);
  });

  it("an unusable byte pair leaves the heartbeat's sample unknown, inode fields included", () => {
    const raw: StatfsSample = { bsize: BSIZE, blocks: Number.NaN, bfree: 0, bavail: 0, files: 1000, ffree: 100 };
    assert.deepEqual(decideRetainedSeedAdmission({ raw, threshold: 0.95, estimate: COMPLETE(0, 10) }), { kind: "unknown" });
  });

  it("an invalid or absent inode pair falls back to bytes", () => {
    for (const inodes of [{ files: 0, used: 0 }, undefined]) {
      const d = decideRetainedSeedAdmission({ raw: statfs(1000, 500, inodes), threshold: 0.9, estimate: COMPLETE(0) });
      // allowance only: (500 + 0 + 10) / 1000
      assert.ok(d.kind === "admit" && Math.abs(d.projected - 0.51) < 1e-9, JSON.stringify(inodes));
    }
  });

  it("a bytes-valid sample with files 0 above the threshold refuses current", () => {
    assert.equal(decideRetainedSeedAdmission({ raw: statfs(1000, 900, { files: 0, used: 0 }), threshold: 0.87, estimate: COMPLETE(0) }).kind, "refuse_current");
  });

  it("an incomplete estimate counts the larger of the partial reading and the fallback", () => {
    const raw = statfs(BLOCKS, usedBlocks(0.5));
    // Fallback bytes = max(1 GiB, 2% of 100 GiB) = 2 GiB; partial 1 GiB loses to it:
    // (50 + 2 x 1.25 + 1) / 100.
    const small = decideRetainedSeedAdmission({ raw, threshold: 0.9, estimate: { bytes: GIB, inodes: 0, complete: false } });
    assert.ok(small.kind === "admit" && !small.estimateComplete && Math.abs(small.projected - 0.535) < 1e-9);
    // A partial reading above the fallback is kept: (50 + 10 x 1.25 + 1) / 100.
    const big = decideRetainedSeedAdmission({ raw, threshold: 0.9, estimate: { bytes: 10 * GIB, inodes: 0, complete: false } });
    assert.ok(big.kind === "admit" && Math.abs(big.projected - 0.6350) < 1e-9);
  });

  it("an undefined estimate uses the fallback, never zero", () => {
    const raw = statfs(BLOCKS, usedBlocks(0.5));
    const d = decideRetainedSeedAdmission({ raw, threshold: 0.9, estimate: undefined });
    assert.ok(d.kind === "admit" && !d.estimateComplete && Math.abs(d.projected - 0.535) < 1e-9);
    // The fallback floors apply on a small volume: 1 GiB of a 4 GiB volume at 50%.
    const tiny = decideRetainedSeedAdmission({ raw: statfs((4 * GIB) / BSIZE, (2 * GIB) / BSIZE), threshold: 0.8, estimate: undefined });
    // (2 + 1 x 1.25 + 0.04) / 4 = 0.8225
    assert.ok(tiny.kind === "refuse_projected" && Math.abs(tiny.projected - 0.8225) < 1e-9);
  });

  it("uses the inode fallback when the inode estimate is incomplete", () => {
    // 1M inodes at 50%: fallback = max(100k, 2% = 20k) = 100k -> (500k + 125k + 10k) / 1M = 0.635.
    const raw = statfs(BLOCKS, usedBlocks(0.1), { files: 1_000_000, used: 500_000 });
    const d = decideRetainedSeedAdmission({ raw, threshold: 0.9, estimate: { bytes: 0, inodes: 5, complete: false } });
    assert.ok(d.kind === "admit" && Math.abs(d.projected - 0.635) < 1e-9);
  });

  it("an unusable sample is unknown", () => {
    assert.deepEqual(decideRetainedSeedAdmission({ raw: statfs(0, 0), threshold: 0.87, estimate: undefined }), { kind: "unknown" });
    assert.deepEqual(
      decideRetainedSeedAdmission({ raw: { bsize: Number.NaN, blocks: 10, bfree: 5, bavail: 5 }, threshold: 0.87, estimate: undefined }),
      { kind: "unknown" },
    );
  });
});

describe("measureRetainedSeedFootprint", () => {
  const roots: string[] = [];
  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  function tree(): string {
    const root = makeScratchRoot("uzi-seed-footprint-");
    roots.push(root);
    const clone = path.join(root, "clone");
    fs.mkdirSync(path.join(clone, ".git", "objects", "pack"), { recursive: true });
    fs.mkdirSync(path.join(clone, "node_modules", "dep"), { recursive: true });
    fs.writeFileSync(path.join(clone, ".git", "HEAD"), "ref: refs/heads/main\n");
    fs.writeFileSync(path.join(clone, ".git", "objects", "pack", "big.pack"), Buffer.alloc(256 * 1024, 1));
    fs.writeFileSync(path.join(clone, "node_modules", "dep", "index.js"), Buffer.alloc(32 * 1024, 2));
    fs.writeFileSync(path.join(clone, "README.md"), "hi\n");
    return clone;
  }

  const cache = (): GitCache => new GitCache(makeScratchRoot("uzi-seed-data-"), nullLogger(), undefined, testGitCacheOptions());
  const far = (): number => Date.now() + 60_000;

  it("leaves .git/objects out unless the successor is self-contained", async () => {
    const clone = tree();
    const g = cache();
    const without = await g.measureRetainedSeedFootprint(clone, { includeGitObjects: false, deadline: far() });
    const withObjects = await g.measureRetainedSeedFootprint(clone, { includeGitObjects: true, deadline: far() });
    assert.equal(without.complete, true);
    assert.equal(withObjects.complete, true);
    // Omitted: objects' children (pack dir, big.pack). The objects directory itself is still counted.
    assert.equal(withObjects.inodes - without.inodes, 2);
    assert.ok(withObjects.bytes - without.bytes >= 256 * 1024, `delta ${withObjects.bytes - without.bytes}`);
    assert.ok(without.bytes >= 32 * 1024, "node_modules is part of the estimate");
  });

  it("a small entry cap is incomplete with the bytes seen so far", async () => {
    const m = await cache().measureRetainedSeedFootprint(tree(), { includeGitObjects: true, deadline: far(), caps: { maxEntries: 2 } });
    assert.equal(m.complete, false);
    assert.ok(m.bytes > 0 && m.inodes > 0);
  });

  it("a small depth cap is incomplete with the bytes seen so far", async () => {
    const m = await cache().measureRetainedSeedFootprint(tree(), { includeGitObjects: true, deadline: far(), caps: { maxDepth: 1 } });
    assert.equal(m.complete, false);
    assert.ok(m.bytes > 0);
  });

  it("an expired deadline is incomplete, a missing root is incomplete with nothing counted", async () => {
    const g = cache();
    const late = await g.measureRetainedSeedFootprint(tree(), { includeGitObjects: true, deadline: 0 });
    assert.equal(late.complete, false);
    const missing = await g.measureRetainedSeedFootprint(path.join(makeScratchRoot("uzi-seed-none-"), "absent"), {
      includeGitObjects: true,
      deadline: far(),
    });
    assert.deepEqual(missing, { bytes: 0, inodes: 0, complete: false });
  });

  it("does not swallow a recovery interruption", async () => {
    const clone = tree();
    const g = cache();
    const ac = new AbortController();
    await assert.rejects(
      g.withRecoveryOperation(ac.signal, Infinity, () =>
        g.measureRetainedSeedFootprint(clone, {
          includeGitObjects: true,
          deadline: far(),
          now: () => {
            ac.abort(new Error("stop"));
            return Date.now();
          },
        })),
    );
  });
});

describe("DataVolumeGuard.attributedStatfs", () => {
  const raw = statfs(1000, 500, { files: 100, used: 10 });
  const guard = (stat: (p: string) => Promise<{ dev: number }>, sample: () => StatfsSample = () => raw): DataVolumeGuard =>
    new DataVolumeGuard({ dataDir: "/data", stat, statfs: sample });

  it("returns the raw sample, unfiltered, for a destination on the data volume", async () => {
    const unfiltered: StatfsSample = { bsize: BSIZE, blocks: 1000, bfree: 100, bavail: 100, files: 0, ffree: 0 };
    assert.deepEqual(await guard(async () => ({ dev: 7 }), () => unfiltered).attributedStatfs("/data/x"), { kind: "sample", raw: unfiltered });
  });

  it("another device is other_volume without a statfs", async () => {
    let sampled = false;
    const g = guard(async (p) => ({ dev: p === "/data" ? 7 : 8 }), () => { sampled = true; return raw; });
    assert.deepEqual(await g.attributedStatfs("/elsewhere"), { kind: "other_volume" });
    assert.equal(sampled, false);
  });

  it("a stat failure, a bad device id or a statfs failure is unknown", async () => {
    const boom = async (): Promise<{ dev: number }> => { throw new Error("ENOENT"); };
    assert.deepEqual(await guard(boom).attributedStatfs("/data/x"), { kind: "unknown" });
    assert.deepEqual(await guard(async () => ({ dev: Number.NaN })).attributedStatfs("/data/x"), { kind: "unknown" });
    assert.deepEqual(await guard(async () => ({ dev: -1 })).attributedStatfs("/data/x"), { kind: "unknown" });
    const throws = (): StatfsSample => { throw new Error("EIO"); };
    assert.deepEqual(await guard(async () => ({ dev: 7 }), throws).attributedStatfs("/data/x"), { kind: "unknown" });
  });
});
