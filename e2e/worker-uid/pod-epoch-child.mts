// Image-only owned Node process. Classification is the ONLY fake: tmpfs is not
// reclaimed in production. Every descriptor, flock, fsync and UID here is real.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createRequire } from "node:module";
import { spawnSync } from "node:child_process";
import { createInterface } from "node:readline";
import { setprivArgsForUid, RUNNER_UID } from "../../agent/src/runner-uid.js";
import { restoreEpochRuntimeTmpdir } from "../../agent/src/pod-epoch-lock.js";

const require = createRequire(import.meta.url);
const { createRuntime } = require("../../agent/templates/pod-epoch-init.cjs");
const root = process.argv[2]!;
const io = { ...fs, statfsSync: (...args: Parameters<typeof fs.statfsSync>) =>
  ({ ...fs.statfsSync(...args), type: 0xef53 }) };
const runtime = createRuntime({ root, io, uid: 10001, gid: 10001 });
let ownership: Awaited<ReturnType<typeof runtime.acquire>> | undefined;
console.log(JSON.stringify({ ready: true, pid: process.pid, uid: process.getuid!(),
  actualFilesystem: Number(fs.statfsSync(root).type), classificationOnly: "ext4" }));
for await (const line of createInterface({ input: process.stdin })) {
  const request = JSON.parse(line);
  try {
    let result: unknown;
    switch (request.op) {
      case "seed": result = await runtime.seed(request.uid); break;
      case "acquire":
        ownership = await runtime.acquire({ firstMode: request.mode, signal: AbortSignal.timeout(5000) });
        result = ownership.mode; break;
      case "consume": result = await ownership.consume(request.uid); break;
      case "matches": result = ownership.matchesEntry(request.current, request.previous); break;
      case "shared": await ownership.reacquireShared(); result = ownership.mode; break;
      case "close": ownership.close(); ownership = undefined; result = "closed"; break;
      case "exit": process.exit(0); break; // intentionally no close: kernel exit release
      case "unrelated": {
        // Default child stdio cannot inherit Node's private epoch descriptor.
        const child = spawnSync(process.execPath, ["-e", `
          const fs = require('node:fs');
          const target = process.argv[1];
          const matches = fs.readdirSync('/proc/self/fd').filter(fd => {
            try { return fs.readlinkSync('/proc/self/fd/' + fd) === target; }
            catch { return false; }
          });
          console.log(JSON.stringify(matches));
        `, path.join(root, ".uzi-epoch.lock")], { encoding: "utf8", timeout: 5000 });
        if (child.status !== 0) throw new Error(child.stderr);
        result = JSON.parse(child.stdout); break;
      }
      case "restore": {
        const expected = "/data/runner/.tmp" + (request.split ? "/uzi-worker" : "");
        process.env.UZI_UID_SPLIT = request.split ? "1" : "0";
        process.env.TMPDIR = "/tmp/uzi-epoch-bootstrap";
        if (request.saved === null) delete process.env.UZI_EPOCH_RUNTIME_TMPDIR;
        else process.env.UZI_EPOCH_RUNTIME_TMPDIR = request.saved ?? expected;
        restoreEpochRuntimeTmpdir(ownership);
        const made = fs.mkdtempSync(path.join(os.tmpdir(), "epoch-worker-"));
        const runnerTmp = request.split ? "/data/runner/.tmp/uzi-runner" : expected;
        const runnerProgram = ["-e",
          "const fs=require('node:fs'),os=require('node:os');console.log(JSON.stringify({tmp:os.tmpdir(),made:fs.mkdtempSync(os.tmpdir()+'/epoch-runner-')}))"];
        const child = spawnSync(request.split ? "/bin/setpriv" : process.execPath,
          request.split ? [...setprivArgsForUid(RUNNER_UID), process.execPath, ...runnerProgram] : runnerProgram,
          { env: { PATH: "/usr/local/bin:/usr/bin:/bin", TMPDIR: runnerTmp }, encoding: "utf8", timeout: 5000 });
        if (child.status !== 0) throw new Error(child.stderr);
        result = { env: process.env.TMPDIR, os: os.tmpdir(), made,
          saved: process.env.UZI_EPOCH_RUNTIME_TMPDIR ?? null, runner: JSON.parse(child.stdout) };
        break;
      }
      default: throw new Error("unknown request");
    }
    console.log(JSON.stringify({ result }));
  } catch (error) {
    console.log(JSON.stringify({ error: String(error) }));
  }
}
