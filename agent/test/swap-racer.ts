import fs from "node:fs/promises";
import { existsSync } from "node:fs";
import path from "node:path";
import { spawn } from "node:child_process";

/**
 * PRD #1809 M3 (audit fix): the racer a same-uid agent process can run against a deletion
 * in progress. It watches `<root>/d<i>/x` for each i and, as soon as the deletion has
 * started emptying one (some names gone, some left), renames it away and plants a symlink
 * to `victim`, which holds the SAME names. A path-based walk that listed `x` before the swap
 * then unlinks `x/<name>` through the symlink, i.e. inside the victim.
 *
 * {@link seedRacedTree} builds the matching tree and victim; `start` resolves once the
 * racer is running, and `stop` ends it (it also ends by itself after 8 s).
 */
const RACER = `
const fs = require("node:fs");
const [root, victim, stop, n, files] = process.argv.slice(1);
const until = Date.now() + 8000;
const swapped = new Set();
process.stdout.write("ready\\n");
while (!fs.existsSync(stop) && Date.now() < until && swapped.size < Number(n)) {
  for (let i = 0; i < Number(n); i++) {
    if (swapped.has(i)) continue;
    const x = root + "/d" + i + "/x";
    let left;
    try { left = fs.readdirSync(x).length; } catch { continue; }
    if (left > 0 && left < Number(files)) {
      try { fs.renameSync(x, x + ".moved"); fs.symlinkSync(victim, x, "dir"); swapped.add(i); } catch {}
    }
  }
}
`;

/** A host without `/proc/self/fd` (macOS): every pinned walk refuses there by design. */
export const noProcFd = !existsSync("/proc/self/fd");

export const RACED_DIRS = 10;
export const RACED_FILES = 400;

/** `<root>/d<i>/x/f<j>` for the racer to swap, and `victim/f<j>` with the same names. */
export async function seedRacedTree(root: string, victim: string): Promise<void> {
  for (let i = 0; i < RACED_DIRS; i++) {
    const d = path.join(root, `d${i}`, "x");
    await fs.mkdir(d, { recursive: true });
    for (let j = 0; j < RACED_FILES; j++) await fs.writeFile(path.join(d, `f${j}`), "");
  }
  for (let j = 0; j < RACED_FILES; j++) await fs.writeFile(path.join(victim, `f${j}`), "keep\n");
}

/** Start the racer on `root` (stop file under `scratch`); resolves once it is running. */
export async function startSwapRacer(root: string, victim: string, scratch: string): Promise<{ stop: () => Promise<void> }> {
  const stopFile = path.join(scratch, "racer.stop");
  const racer = spawn(process.execPath, ["-e", RACER, root, victim, stopFile, String(RACED_DIRS), String(RACED_FILES)], {
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = new Promise<void>((r) => racer.on("exit", () => r()));
  await new Promise<void>((r) => racer.stdout.once("data", () => r()));
  return {
    stop: async () => {
      await fs.writeFile(stopFile, "");
      await exited;
    },
  };
}
