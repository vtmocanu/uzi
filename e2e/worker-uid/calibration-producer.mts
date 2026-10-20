// Trusted command primary: fixed file protocol, no model-supplied source or load settings.
import { Worker } from "node:worker_threads";
import { open, rename, writeFile, statfs } from "node:fs/promises";
import path from "node:path";
import { performance } from "node:perf_hooks";

export function validateControl(value: unknown, previous: number, rounds: number): number {
  if (!Number.isInteger(previous) || previous < 0 || !value || typeof value !== "object" || Object.keys(value).length !== 1
    || !("step" in value) || value.step !== previous + 1 || previous >= rounds
    || ![16, 32].includes(rounds)) throw new Error("invalid fixed producer control");
  return value.step as number;
}
export function validatePaths(control: string, diskDir: string, shmemDir: string) {
  const index = /^\/proof\/\.uzi\/scratch\/control-([01])$/.exec(control)?.[1];
  if (index === undefined || diskDir !== "/calibration-disk/tree-" + index
    || shmemDir !== "/proof/.uzi/scratch/tree-" + index) throw new Error("invalid owned producer paths");
}
export async function readControl(control: string): Promise<unknown> {
  const file = await open(control, "r");
  try {
    const bytes = Buffer.alloc(65); // 64 protocol bytes plus one overflow sentinel.
    let used = 0;
    while (used < bytes.length) {
      const result = await file.read(bytes, used, bytes.length - used, null);
      if (!result.bytesRead) break;
      used += result.bytesRead;
    }
    if (used > 64) throw new Error("oversized producer control");
    return JSON.parse(new TextDecoder("utf8", { fatal: true }).decode(bytes.subarray(0, used)));
  } finally { await file.close(); }
}
// One owned writer publishes each ACK; readers see the previous or next complete JSON.
export async function publishAck(control: string, step: number, beforeRename?: () => Promise<void>) {
  const ack = control + ".ack", temporary = ack + ".next";
  await writeFile(temporary, JSON.stringify({ step }));
  await beforeRename?.();
  await rename(temporary, ack);
}
export async function runProducer(control: string, diskDir: string, shmemDir: string, rounds: number) {
  validatePaths(control, diskDir, shmemDir);
  if (![16, 32].includes(rounds)) throw new Error("invalid producer rounds");
  if ((await statfs(diskDir)).type === 0x01021994 || (await statfs(shmemDir)).type !== 0x01021994) {
    throw new Error("disk/tmpfs prerequisite");
  }
  const worker = new Worker(new URL("../../agent/test/fixtures/worker-memory-load.cjs", import.meta.url), {
    workerData: { diskDir, shmemDir, rounds, chunkBytes: 1024 * 1024 },
    resourceLimits: { maxOldGenerationSizeMb: 16, maxYoungGenerationSizeMb: 4, stackSizeMb: 1 },
  });
  let failure: Error | undefined, pending: (() => void) | undefined;
  worker.on("error", (error) => { failure = error; pending?.(); });
  worker.on("exit", (code) => { failure = new Error("unexpected load worker exit " + code); pending?.(); });
  worker.on("message", (message) => {
    if (message !== "step" || !pending) failure = new Error("unexpected producer reply");
    pending?.();
  });
  const deadline = performance.now() + 60000;
  await writeFile(control + ".ready", "ready");
  let previous = 0;
  try {
    // One file check per 10ms, <=6000 attempts / 60s. Any fault ends this tree.
    for (let attempt = 0; attempt < 6000 && performance.now() < deadline; attempt++) {
      if (failure) throw failure;
      let value: unknown;
      try { value = await readControl(control); }
      catch (error) { if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error; }
      if (value !== undefined) {
        const step = (value as { step?: unknown } | null)?.step;
        if (step === previous) {
          validateControl(value, previous - 1, rounds); // Repeated exact request is idle, never another allocation.
        } else {
          const next = validateControl(value, previous, rounds);
          await new Promise<void>((resolve, reject) => {
            const timer = setTimeout(() => { pending = undefined; reject(new Error("producer step deadline")); }, 5000);
            pending = () => { clearTimeout(timer); pending = undefined; if (failure) reject(failure); else resolve(); };
            worker.postMessage("step");
          });
          previous = next;
          await publishAck(control, previous);
        }
      }
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    throw new Error("producer lifetime expired");
  } finally { await worker.terminate(); }
}
if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(new URL(import.meta.url).pathname)) {
  const [, , control, disk, shmem, rounds] = process.argv;
  if (!control || !disk || !shmem) throw new Error("trusted producer arguments required");
  await runProducer(control, disk, shmem, Number(rounds));
}
