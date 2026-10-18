import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { WorkerMemoryReader } from "../src/worker-memory-reader.js";

const files = () => ({
  "/fake/proc": "0::/\n",
  "/fake/memory.current": "1000\n",
  "/fake/memory.max": "2000\n",
  "/fake/memory.stat": "anon 600\nshmem 10\nslab_unreclaimable 20\nunevictable 30\ninactive_file 900\n",
  "/fake/memory.pressure": "some avg10=1.25 avg60=0.00 avg300=0.00 total=100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=20\n",
});
function fixture(overrides: Record<string, string | undefined> = {}) {
  const data: Record<string, string | undefined> = { ...files(), ...overrides };
  const reads: { file: string; budget: number }[] = [];
  let clock = 10;
  const reader = new WorkerMemoryReader({
    procCgroupPath: "/fake/proc", cgroupRoot: "/fake", now: () => clock++,
    readBytes: (file, budget) => {
      reads.push({ file, budget });
      if (data[file] === undefined) throw new Error("missing");
      return Buffer.from(data[file]).subarray(0, budget + 1);
    },
  });
  return { reader, reads };
}
function unavailable(overrides: Record<string, string | undefined>, reason: string) {
  const result = fixture(overrides).reader.sample();
  assert.equal(result.available, false);
  if (!result.available) assert.equal(result.reason, reason);
}

test("total accounting, complete PSI, identity and monotonic age metadata", () => {
  const { reader, reads } = fixture();
  const result = reader.sample();
  assert.equal(result.available, true);
  if (!result.available) return;
  assert.equal(result.currentBytes, 1000);
  assert.equal(result.limitBytes, 2000);
  assert.deepEqual(result.stat, { anon: 600, shmem: 10, slab_unreclaimable: 20, unevictable: 30 });
  assert.deepEqual(result.pressure.some, { avg10: 1.25, avg60: 0, avg300: 0, total: 100 });
  assert.equal(result.pressure.full.total, 20);
  assert.equal(result.sampleId, 1);
  assert.equal(result.startedAtMs, 10);
  assert.equal(result.completedAtMs, 11);
  assert.equal(reader.sample().sampleId, 2);
  assert.deepEqual(reads.slice(0, 5).map((r) => r.file), Object.keys(files()));
});
test("root must be exactly one unified 0::/ entry, with no fallback reads", () => {
  for (const membership of ["0::/host\n", "1:memory:/\n", "0::/\n0::/\n", "0:://\n", " 0::/\n"]) {
    const { reader, reads } = fixture({ "/fake/proc": membership });
    const result = reader.sample();
    assert.equal(result.available, false);
    if (!result.available) assert.equal(result.reason, "unverified");
    assert.equal(reads.length, 1);
  }
  assert.equal(fixture({ "/fake/proc": "1:cpu:/\n0::/\n" }).reader.sample().available, true);
});
test("absent files, unlimited and invalid numeric scalar fields", () => {
  for (const file of Object.keys(files())) unavailable({ [file]: undefined }, "absent");
  unavailable({ "/fake/memory.max": "max\n" }, "unlimited");
  unavailable({ "/fake/memory.max": "0" }, "malformed");
  for (const field of ["memory.max", "memory.current"]) {
    for (const value of ["", "-1", "1.5", "1e3", "NaN", "Infinity", "9007199254740992", "1 2"]) {
      unavailable({ ["/fake/" + field]: value }, "malformed");
    }
    assert.equal(fixture({ ["/fake/" + field]: "9007199254740991" }).reader.sample().available, true);
  }
  assert.equal(fixture({ "/fake/memory.current": "0" }).reader.sample().available, true);
});
test("each stat field is required, unique and a nonnegative safe integer", () => {
  for (const key of ["anon", "shmem", "slab_unreclaimable", "unevictable"]) {
    const original = files()["/fake/memory.stat"];
    unavailable({ "/fake/memory.stat": original.replace(new RegExp(key + " [0-9]+\\n"), "") }, "malformed");
    unavailable({ "/fake/memory.stat": original + key + " 0\n" }, "malformed");
    for (const value of ["-1", "1e3", "9007199254740992"]) {
      unavailable({ "/fake/memory.stat": original.replace(new RegExp(key + " [0-9]+"), key + " " + value) }, "malformed");
    }
  }
});
test("invalid, incomplete or duplicated PSI invalidates the entire sample", () => {
  const original = files()["/fake/memory.pressure"];
  for (const raw of [
    "", original.split("\n")[0], original + original,
    original.replace("avg10=1.25", "avg10=NaN"),
    original.replace("avg10=1.25", "avg10=-1"),
    original.replace("avg10=1.25", "avg10=101"),
    original.replace("avg10=1.25", "avg10=1e2"),
    original.replace("avg60=0.00", "avg10=0.00"),
    original.replace("total=100", "total=9007199254740992"),
    original.replace("total=100", "total=-1"),
    original.replace("total=100", "total=1.5"),
  ]) unavailable({ "/fake/memory.pressure": raw }, "malformed");
});
test("aggregate budget shrinks and oversized input aborts before parsing", () => {
  unavailable({ "/fake/proc": "x".repeat(65537) }, "oversized");
  const { reader, reads } = fixture({ "/fake/memory.stat": "x".repeat(65536) });
  const result = reader.sample();
  assert.equal(result.available, false);
  if (!result.available) assert.equal(result.reason, "oversized");
  assert.equal(reads[0]!.budget, 65536);
  assert.equal(reads[1]!.budget, 65531);
  assert.equal(reads.length, 4);
});
test("production bounded read seam works on fake proc/cgroup files only", () => {
  const dir = fs.mkdtempSync(path.resolve("../.uzi/scratch/memory-reader-"));
  try {
    for (const [file, raw] of Object.entries(files())) fs.writeFileSync(path.join(dir, path.basename(file)), raw);
    const reader = new WorkerMemoryReader({ cgroupRoot: dir, procCgroupPath: path.join(dir, "proc") });
    assert.equal(reader.sample().available, true);
    fs.writeFileSync(path.join(dir, "memory.stat"), "x".repeat(65537));
    const result = reader.sample();
    assert.equal(result.available, false);
    if (!result.available) assert.equal(result.reason, "oversized");
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("invalid UTF-8 is unavailable before scalar parsing", () => {
  const reader = new WorkerMemoryReader({
    cgroupRoot: "/fake", procCgroupPath: "/fake/proc",
    readBytes: () => Uint8Array.from([0xc3, 0x28]),
  });
  assert.deepEqual(reader.sample().available, false);
  const result = reader.sample();
  if (!result.available) assert.equal(result.reason, "malformed");
});

test("production reads retry short reads within the aggregate allocation budget", (t) => {
  const dir = fs.mkdtempSync(path.resolve("../.uzi/scratch/memory-short-"));
  const original = fs.readSync;
  const reads: { allocation: number; offset: number; length: number; count: number }[] = [];
  try {
    for (const [file, raw] of Object.entries(files())) fs.writeFileSync(path.join(dir, path.basename(file)), raw);
    t.mock.method(fs, "readSync", (fd: number, buffer: NodeJS.ArrayBufferView, offset: number, length: number, position: number | null) => {
      const count = original(fd, buffer, offset, Math.min(length, 3), position);
      reads.push({ allocation: buffer.byteLength, offset, length, count });
      return count;
    });
    const reader = new WorkerMemoryReader({ cgroupRoot: dir, procCgroupPath: path.join(dir, "proc") });
    assert.equal(reader.sample().available, true);
    assert.ok(reads.some((read) => read.offset > 0 && read.count > 0));
    assert.ok(reads.every((read) => read.allocation <= 65537 && read.length === read.allocation - read.offset));
    reads.length = 0;
    fs.writeFileSync(path.join(dir, "memory.stat"), "x".repeat(100000));
    const result = reader.sample();
    assert.equal(result.available, false);
    if (!result.available) assert.equal(result.reason, "oversized");
    assert.equal(reads.reduce((sum, read) => sum + read.count, 0), 65537);
    const allocations = reads.filter((read) => read.offset === 0).map((read) => read.allocation);
    assert.deepEqual(allocations, [65537, 65532, 65527, 65522]);
  } finally {
    t.mock.restoreAll();
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
