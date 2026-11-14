// Run as `node --import tsx test/fixtures/dind-builder-prune-exit-runner.ts <stateFile>` from agent/.
// It drives the builder leg against the `escape` child fixture (a descendant that escapes the
// process group and holds the output pipes), aborts the attempt, and then returns from main
// WITHOUT any explicit exit. A worker with an unsettled child must still be able to exit by
// itself, so the test asserts this process ends on its own.

import { existsSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { DindBuilderPruneController } from "../../src/dind-builder-prune.js";
import { DindPruneGate, dockerExecEnvironment } from "../../src/dind-prune.js";

const stateFile = process.argv[2]!;
const childScript = fileURLToPath(new URL("./dind-builder-prune-child.mjs", import.meta.url));

let time = 1_700_000_000_000;
const lines: string[] = [];
const log = {
  info: (m: string) => void lines.push(m),
  warn: (m: string) => void lines.push(m),
  error: (m: string) => void lines.push(m),
  debug: () => {},
};

const controller = new DindBuilderPruneController({
  dockerHost: "tcp://dind:2375",
  workerName: "runner",
  gate: new DindPruneGate(),
  log: log as never,
  readSample: () => ({ epochS: Math.floor(time / 1000), bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 }),
  now: () => time,
  pluginPath: process.execPath,
  spawnFile: process.execPath,
  spawnArgvPrefix: [childScript, "escape", stateFile],
  pruneTimeoutMs: 60_000,
  settleTimeoutMs: 200,
});

const abort = new AbortController();
time += 30_000;
await controller.tick(abort.signal);
time += 30_000;
const attempt = controller.tick(abort.signal);
// Abort once the child has started its escaped descendant.
const poll = setInterval(() => {
  if (existsSync(stateFile)) {
    clearInterval(poll);
    abort.abort();
  }
}, 20);
const outcome = await attempt;
console.log(JSON.stringify({ outcome, unsettled: lines.some((l) => l.startsWith("dind-builder-prune-unsettled")) }));
rmSync(dockerExecEnvironment("tcp://dind:2375").DOCKER_CONFIG!, { recursive: true, force: true });
// No process.exit: returning from main must be enough.
