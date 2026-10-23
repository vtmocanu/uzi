import { createHash } from "node:crypto";
import { constants } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";

export const MANIFEST_MAX_BYTES = 1024 * 1024;
export const HELPER_MAX_BYTES = 64 * 1024 * 1024;
const SOURCE_LIMITS = {
  fileBytes: 4 * 1024 * 1024,
  totalBytes: 64 * 1024 * 1024,
  entries: 8192,
  depth: 64,
};

function checkLimit(value: number, ceiling: number): void {
  if (!Number.isSafeInteger(value) || value < 0 || value > ceiling) {
    throw new Error("invalid input limit");
  }
}

export async function readRegularFile(file: string, maxBytes: number): Promise<Buffer> {
  checkLimit(maxBytes, HELPER_MAX_BYTES);
  const before = await fs.lstat(file);
  if (!before.isFile()) throw new Error(`input must be a regular file: ${file}`);
  if (before.size > maxBytes) throw new Error(`input byte limit exceeded: ${file}`);
  // NOFOLLOW rejects a swapped-in link; NONBLOCK prevents a swapped-in FIFO
  // from blocking open. Inspect the opened descriptor before allocating/reading.
  const handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const stat = await handle.stat();
    if (!stat.isFile()) throw new Error(`input must be a regular file: ${file}`);
    if (stat.size > maxBytes) throw new Error(`input byte limit exceeded: ${file}`);
    const buffer = Buffer.alloc(maxBytes + 1);
    let used = 0;
    // At most maxBytes + 1 successful byte reads, in chunks <= 64 KiB.
    // One overflow byte distinguishes exact-cap EOF from growth; errors stop.
    for (;;) {
      const { bytesRead } = await handle.read(buffer, used, Math.min(64 * 1024, buffer.length - used), null);
      if (bytesRead === 0) return buffer.subarray(0, used);
      used += bytesRead;
      if (used > maxBytes) throw new Error(`input byte limit exceeded: ${file}`);
    }
  } finally {
    await handle.close();
  }
}

export async function sourceId(root: string, overrides: Partial<typeof SOURCE_LIMITS> = {}): Promise<string> {
  const limits = { ...SOURCE_LIMITS, ...overrides };
  for (const key of Object.keys(SOURCE_LIMITS) as (keyof typeof SOURCE_LIMITS)[]) {
    checkLimit(limits[key], SOURCE_LIMITS[key]);
  }
  const hash = createHash("sha256");
  let entries = 0;
  let totalBytes = 0;
  // Serial traversal stops on the first failure, bounded by entries and depth.
  async function walk(dir: string, depth: number): Promise<void> {
    if (depth > limits.depth) throw new Error("source depth limit exceeded");
    if (!(await fs.lstat(dir)).isDirectory()) throw new Error(`source must be a directory: ${dir}`);
    const names: string[] = [];
    const directory = await fs.opendir(dir);
    try {
      // Count before collecting: even one enormous directory cannot allocate
      // an unbounded readdir array. Sorting preserves the historical source ID.
      for (;;) {
        const entry = await directory.read();
        if (entry === null) break;
        if (++entries > limits.entries) throw new Error("source entry limit exceeded");
        names.push(entry.name);
      }
    } finally {
      await directory.close();
    }
    for (const name of names.sort()) {
      const file = path.join(dir, name);
      const stat = await fs.lstat(file);
      if (stat.isDirectory()) await walk(file, depth + 1);
      else {
        const content = await readRegularFile(file, Math.min(limits.fileBytes, limits.totalBytes - totalBytes));
        totalBytes += content.length;
        hash.update(path.relative(root, file));
        hash.update("\0");
        hash.update(content);
        hash.update("\0");
      }
    }
  }
  await walk(root, 0);
  return hash.digest("hex");
}
