import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { restoreTreeWritability } from "../src/rmtree.js";
import { RACED_FILES } from "./swap-racer.js";

export async function residualTestFixture(body: (root: string, victim: string) => Promise<void>) {
  const root = await fs.mkdtemp(path.join(process.platform === "linux" ? "/tmp" : os.tmpdir(), "cdr-residual-"));
  const victim = path.join(root, "outside");
  await fs.mkdir(victim);
  try { await body(root, victim); }
  finally {
    await restoreTreeWritability(root);
    await fs.rm(root, { recursive: true, force: true });
  }
}

export async function assertOutsideFiles(victim: string, swaps: number) {
  assert.ok(swaps > 0, "positive intermediate swaps prove the race happened");
  const names = (await fs.readdir(victim)).sort();
  console.log(JSON.stringify({ swaps, outsideRemaining: names.length, outsideLost: RACED_FILES - names.length }));
  assert.deepEqual(names, Array.from({ length: RACED_FILES }, (_, i) => `f${i}`).sort());
  for (const name of names) assert.equal(await fs.readFile(path.join(victim, name), "utf8"), "keep\n");
}

