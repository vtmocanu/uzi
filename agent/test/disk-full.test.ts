import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  DataVolumeGuard,
  DISK_FULL_MIN_FREE_BYTES,
  DISK_FULL_MIN_FREE_INODES,
  hasDiskFullSignal,
  volumeBelowFloor,
} from "../src/disk-full.js";
import { sampleVolume, type StatfsSample } from "../src/stats.js";
import { DiskPressureController } from "../src/disk-reclaim.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// PRD #1809 M5 (D6): the data-volume disk-full classifier, its statfs sample, the awaitable
// reclaim hook, and the outbox reserve's cause logging. Seams only: a fake statfs, a fake stat
// for the device-id attribution, never a real full disk.

const GIB = 1024 ** 3;
function volume(availBytes: number, opts: { files?: number; ffree?: number } = {}): StatfsSample {
  const bsize = 4096;
  return {
    bsize,
    blocks: (25 * GIB) / bsize,
    bfree: availBytes / bsize,
    bavail: availBytes / bsize,
    files: opts.files ?? 1_600_000,
    ffree: opts.ffree ?? 1_500_000,
  };
}
const FULL = volume(1024 * 1024);
const ROOMY = volume(10 * GIB);
const enospc = () => Object.assign(new Error("ENOSPC: no space left on device, write"), { code: "ENOSPC" });

function withDataDir(fn: (dataDir: string) => Promise<void>): () => Promise<void> {
  return async () => {
    const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-diskfull-"));
    try {
      await fn(dataDir);
    } finally {
      fs.rmSync(dataDir, { recursive: true, force: true });
    }
  };
}

describe("hasDiskFullSignal (PRD #1809 D6)", () => {
  it("recognises Node ENOSPC and EDQUOT codes", () => {
    assert.equal(hasDiskFullSignal(enospc()), true);
    assert.equal(hasDiskFullSignal(Object.assign(new Error("write failed"), { code: "EDQUOT" })), true);
  });

  it("recognises git's diagnostics in the message, the captured stderr and the cause chain", () => {
    assert.equal(hasDiskFullSignal(new Error("fatal: unable to write loose object file: No space left on device")), true);
    assert.equal(hasDiskFullSignal(new Error("error: unable to write sha1 filename .git/objects/ab/cd")), true);
    assert.equal(hasDiskFullSignal(Object.assign(new Error("git failed"), { stderr: "Disk quota exceeded" })), true);
    assert.equal(hasDiskFullSignal(new Error("wrapped", { cause: enospc() })), true);
  });

  it("does not read unrelated failures as a signal", () => {
    assert.equal(hasDiskFullSignal(new Error("fatal: Authentication failed for 'https://x/y.git/'")), false);
    assert.equal(hasDiskFullSignal(Object.assign(new Error("denied"), { code: "EACCES" })), false);
    assert.equal(hasDiskFullSignal(undefined), false);
  });
});

describe("volumeBelowFloor (PRD #1809 D6)", () => {
  it("bytes below max(256 MiB, 1%) are full; above are not", () => {
    const total = 25 * GIB;
    const at = (bytesAvailable: number) => volumeBelowFloor({ bytesAvailable, bytesTotal: total, inodesTotal: 1e6, inodesFree: 9e5 });
    assert.equal(at(DISK_FULL_MIN_FREE_BYTES - 1), true);
    assert.equal(at(DISK_FULL_MIN_FREE_BYTES + 1), false);
    // On a 1 TiB volume 1% (10.24 GiB) outranks 256 MiB.
    assert.equal(volumeBelowFloor({ bytesAvailable: 5 * GIB, bytesTotal: 1024 * GIB, inodesTotal: 1e6, inodesFree: 9e5 }), true);
  });

  it("inode exhaustion classifies like bytes (bytes fine, ffree near 0)", () => {
    assert.equal(volumeBelowFloor({ bytesAvailable: 10 * GIB, bytesTotal: 25 * GIB, inodesTotal: 1e6, inodesFree: 3 }), true);
    assert.equal(
      volumeBelowFloor({ bytesAvailable: 10 * GIB, bytesTotal: 25 * GIB, inodesTotal: 50_000, inodesFree: DISK_FULL_MIN_FREE_INODES - 1 }),
      true,
    );
  });

  it("a filesystem without inode accounting (files 0) or a zero size is unknown", () => {
    assert.equal(volumeBelowFloor({ bytesAvailable: 0, bytesTotal: 25 * GIB, inodesTotal: 0, inodesFree: 0 }), undefined);
    assert.equal(volumeBelowFloor({ bytesAvailable: 0, bytesTotal: 0, inodesTotal: 10, inodesFree: 0 }), undefined);
  });
});

describe("sampleVolume (PRD #1809 D6, stats.ts)", () => {
  it("keeps the inode counts from statfs", () => {
    assert.deepEqual(sampleVolume("/data", () => volume(GIB, { files: 100, ffree: 7 })), {
      bytesAvailable: GIB,
      bytesTotal: 25 * GIB,
      inodesTotal: 100,
      inodesFree: 7,
    });
  });

  it("a statfs failure or a malformed sample is undefined", () => {
    assert.equal(
      sampleVolume("/data", () => {
        throw new Error("EIO");
      }),
      undefined,
    );
    assert.equal(sampleVolume("/data", () => ({ ...ROOMY, bavail: Number.NaN })), undefined);
  });

  it("reads a real filesystem's inode counts through the default seam", () => {
    const s = sampleVolume(os.tmpdir());
    assert.ok(s, "statfs of the tmp dir succeeds");
    assert.ok(s.bytesTotal > 0);
    assert.ok(Number.isFinite(s.inodesTotal));
  });
});

describe("DataVolumeGuard.classify (PRD #1809 D6)", () => {
  it(
    "a signal on the data volume below the floor is data_volume_full; a roomy volume is not",
    withDataDir(async (dataDir) => {
      const dest = path.join(dataDir, "repos", "x.git");
      assert.equal(await new DataVolumeGuard({ dataDir, statfs: () => FULL }).classify(enospc(), dest), "data_volume_full");
      assert.equal(await new DataVolumeGuard({ dataDir, statfs: () => ROOMY }).classify(enospc(), dest), "not_disk_full");
    }),
  );

  it(
    "attributes a destination that does not exist yet through its nearest existing ancestor",
    withDataDir(async (dataDir) => {
      const guard = new DataVolumeGuard({ dataDir, statfs: () => FULL });
      assert.equal(await guard.classify(enospc(), path.join(dataDir, "not", "yet", "created.git")), "data_volume_full");
    }),
  );

  it(
    "ENOSPC on another mount (a different st_dev) is not_disk_full, without sampling the volume",
    withDataDir(async (dataDir) => {
      let sampled = 0;
      const guard = new DataVolumeGuard({
        dataDir,
        statfs: () => {
          sampled += 1;
          return FULL;
        },
        stat: async (p) => ({ dev: path.resolve(p) === path.resolve(dataDir) ? 1 : 2 }),
      });
      assert.equal(await guard.classify(enospc(), "/elsewhere/scratch/file"), "not_disk_full");
      assert.equal(sampled, 0);
    }),
  );

  it(
    "no signal is not_disk_full; a statfs failure, a data dir that cannot be stat'd or no inode accounting is unknown",
    withDataDir(async (dataDir) => {
      const dest = path.join(dataDir, "repos");
      assert.equal(await new DataVolumeGuard({ dataDir, statfs: () => FULL }).classify(new Error("boom"), dest), "not_disk_full");
      const failing = new DataVolumeGuard({
        dataDir,
        statfs: () => {
          throw new Error("EIO");
        },
      });
      assert.equal(await failing.classify(enospc(), dest), "unknown");
      assert.equal(await new DataVolumeGuard({ dataDir: path.join(dataDir, "missing"), statfs: () => FULL }).classify(enospc(), dest), "unknown");
      const noInodes = new DataVolumeGuard({ dataDir, statfs: () => volume(0, { files: 0, ffree: 0 }) });
      assert.equal(await noInodes.classify(enospc(), dest), "unknown");
      // A bytes-only sample (no files/ffree) reads as no inode accounting too.
      const bytesOnly = new DataVolumeGuard({ dataDir, statfs: () => ({ bsize: 4096, blocks: 100, bfree: 0, bavail: 0 }) });
      assert.equal(await bytesOnly.classify(enospc(), dest), "unknown");
    }),
  );

  it("preflight samples the data volume; reclaim awaits the hook and swallows its failure", async () => {
    assert.equal(new DataVolumeGuard({ dataDir: "/data", statfs: () => FULL }).preflight(), "data_volume_full");
    assert.equal(new DataVolumeGuard({ dataDir: "/data", statfs: () => ROOMY }).preflight(), "not_disk_full");
    let done = false;
    await new DataVolumeGuard({
      dataDir: "/data",
      reclaim: async () => {
        await new Promise((r) => setTimeout(r, 5));
        done = true;
      },
    }).reclaim();
    assert.equal(done, true, "reclaim() waits for the pass");
    await new DataVolumeGuard({
      dataDir: "/data",
      reclaim: async () => {
        throw new Error("pass blew up");
      },
    }).reclaim();
    await new DataVolumeGuard({ dataDir: "/data" }).reclaim(); // reclaim off: resolves at once
  });
});

describe("DiskPressureController.reclaimNow (PRD #1809 D6)", () => {
  it("awaits a pass in flight, then runs a fresh one; resolves at once with the reclaim off", async () => {
    let passes = 0;
    let release!: () => void;
    const c = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: () => undefined,
      intervalMs: 60_000,
      admission: false,
      admissionMaxWaitMs: 60_000,
      log: nullLogger(),
      reclaim: async () => {
        passes++;
        if (passes === 1) await new Promise<void>((r) => (release = r));
      },
    });
    const first = c.requestReclaim("periodic");
    assert.ok(first);
    const now = c.reclaimNow();
    await new Promise((r) => setTimeout(r, 5));
    assert.equal(passes, 1, "the in-flight pass is awaited before a fresh one starts");
    release();
    await now;
    assert.equal(passes, 2, "a fresh pass ran after the in-flight one");

    const off = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: () => undefined,
      intervalMs: 60_000,
      admission: false,
      admissionMaxWaitMs: 60_000,
      log: nullLogger(),
    });
    await off.reclaimNow();
  });
});

describe("Outbox reserve on a full data volume (PRD #1809 D6)", () => {
  it(
    "a reserve that cannot be (re)allocated logs the data_volume_full cause the classifier names",
    withDataDir(async (dataDir) => {
      const root = path.join(dataDir, "outbox");
      // A directory where the reserve file goes makes the atomic grow's rename fail.
      fs.mkdirSync(path.join(root, ".reserve", "x"), { recursive: true });
      const { logger, lines } = recordingLogger();
      const seen: string[] = [];
      const o = new Outbox({
        root,
        log: logger,
        runMaxBytes: 1 << 20,
        maxBytes: 1 << 22,
        retentionMs: 60_000,
        classifyWriteFailure: async (_err, destination) => {
          seen.push(destination);
          return "data_volume_full";
        },
      });
      await o.init();
      const warn = (lines as Array<Record<string, unknown>>).find((l) => String(l.msg).includes("could not (pre)allocate reserve"));
      assert.ok(warn, "the reserve failure is logged");
      assert.equal(warn.cause, "data_volume_full");
      assert.deepEqual(seen, [root], "the outbox root is the destination");
    }),
  );
});
