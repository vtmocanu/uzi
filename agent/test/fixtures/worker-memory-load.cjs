// Fixed bounded load fixture; receives no commands or source from the model.
// Two parent-owned producer threads retain charges until terminated by their handle.
const fs = require("node:fs");
const path = require("node:path");
const { Worker, parentPort, workerData } = require("node:worker_threads");
if (workerData.churn) {
  // Short-lived fixed thread, no nested churn.
  parentPort.postMessage("done");
} else {
  const { diskDir, shmemDir, rounds, chunkBytes } = workerData;
  if (![16, 32].includes(rounds) || chunkBytes !== 1024 * 1024) throw new Error("invalid fixture bounds");
  const retained = [];
  const block = Buffer.alloc(chunkBytes, 1);
  let count = 0;
  parentPort.on("message", async (message) => {
    if (message !== "step" || count >= rounds) throw new Error("invalid fixture step");
    const round = count++;
    retained.push(Buffer.alloc(chunkBytes, 1)); // touched anonymous memory
    for (const [directory, name, sync] of [
      [diskDir, "dirty", false], [diskDir, "cache", true], [shmemDir, "shmem", false],
    ]) {
      const fd = fs.openSync(path.join(directory, name), "a+");
      try {
        // A short write retries only while making progress, <=chunkBytes calls.
        let written = 0;
        while (written < block.length) {
          const count = fs.writeSync(fd, block, written, block.length - written);
          if (count <= 0) throw new Error("fixture write made no progress");
          written += count;
        }
        if (sync) {
          fs.fsyncSync(fd);
          let read = 0;
          while (read < block.length) {
            const count = fs.readSync(fd, block, read, block.length - read, round * chunkBytes + read);
            if (count <= 0) throw new Error("fixture cache read made no progress");
            read += count;
          }
        }
      } finally { fs.closeSync(fd); }
    }
    // At most four sequential short-lived threads per producer. Each of <=32
    // rounds opens/closes three files, fsyncs once, and performs <=chunkBytes
    // writes per file and <=chunkBytes cache reads (short IO must make progress).
    if (round < 4) {
      const child = new Worker(__filename, {
        workerData: { churn: true },
        resourceLimits: { maxOldGenerationSizeMb: 8, maxYoungGenerationSizeMb: 2, stackSizeMb: 1 },
      });
      await new Promise((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code) => code === 0 ? resolve() : reject(new Error("fixture thread failed")));
      });
    }
    parentPort.postMessage("step");
  });
}
