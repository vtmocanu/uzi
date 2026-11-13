// Stand-in for the docker client in dind-builder-prune tests. The controller's test seam runs
// `<node> <this file> <mode> <stateFile> <frozen builder prune argv...>`.
//
// Modes:
//   exit0              prints "Total:\t1.081MB" and exits 0.
//   daemon-error       "Error response from daemon: boom" on stderr, exit 1.
//   daemon-conflict    a daemon "already running" error on stderr, exit 1.
//   buildx-missing     the docker/cli buildx-missing text on stderr, exit 1.
//   plain-fail         a non-daemon failure on stderr, exit 1.
//   env                writes its environment as JSON to <stateFile>, exits 0.
//   sleep              starts an in-group grandchild sharing stdout/stderr, then sleeps forever.
//   daemon-then-sleep  as sleep, after printing daemon error text on stderr.
//   escape             starts a DETACHED grandchild (own session) that inherits stdout/stderr and
//                      sleeps forever, then sleeps itself: killing this process group leaves a
//                      pipe holder alive, so `close` never fires.
//
//   overflow           writes 1 MiB of stdout in chunks, then sleeps forever (tests pass a small cap).
//
// Every run appends a line to <stateFile>.spawns; sleep/escape modes write
// {"child":pid,"grandchild":pid} to <stateFile> once the grandchild exists.

import { spawn } from "node:child_process";
import { appendFileSync, writeFileSync } from "node:fs";

const [mode, stateFile] = process.argv.slice(2);
if (stateFile) appendFileSync(`${stateFile}.spawns`, `${mode}\n`);

const forever = () => setInterval(() => {}, 1000);
const holder = (detached) =>
  spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { detached, stdio: ["ignore", "inherit", "inherit"] });

switch (mode) {
  case "exit0":
    process.stdout.write("Deleted build cache records\nTotal:\t1.081MB\n");
    break;
  case "daemon-error":
    process.stderr.write("Error response from daemon: boom\n");
    process.exitCode = 1;
    break;
  case "daemon-conflict":
    process.stderr.write("Error response from daemon: a prune operation is already running\n");
    process.exitCode = 1;
    break;
  case "buildx-missing":
    process.stderr.write("ERROR: BuildKit is enabled but the buildx component is missing or broken.\n");
    process.exitCode = 1;
    break;
  case "plain-fail":
    process.stderr.write("error during connect: connection lost\n");
    process.exitCode = 1;
    break;
  case "env":
    writeFileSync(stateFile, JSON.stringify(process.env));
    break;
  case "sleep":
  case "daemon-then-sleep":
  case "escape": {
    if (mode === "daemon-then-sleep") process.stderr.write("Error response from daemon: boom\n");
    const grandchild = holder(mode === "escape");
    writeFileSync(stateFile, JSON.stringify({ child: process.pid, grandchild: grandchild.pid }));
    forever();
    break;
  }
  case "overflow": {
    writeFileSync(stateFile, JSON.stringify({ child: process.pid }));
    const chunk = Buffer.alloc(64 * 1024, 120);
    for (let i = 0; i < 16; i++) process.stdout.write(chunk);
    forever();
    break;
  }
  default:
    process.stderr.write(`unknown mode ${mode}\n`);
    process.exitCode = 2;
}
