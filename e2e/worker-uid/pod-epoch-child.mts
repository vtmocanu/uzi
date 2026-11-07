// Image-only owned Node process. Classification is the ONLY fake: tmpfs is not
// reclaimed in production. Every descriptor, flock, fsync and UID here is real.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createRequire } from "node:module";
import { execFileSync, spawn, spawnSync, type ChildProcess } from "node:child_process";
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
const lockFile = path.join(root, ".uzi-epoch.lock");
// Reports which of this process's own descriptors alias the epoch lock inode. The
// image proof lacks CAP_SYS_PTRACE, so the holder must self-report rather than be
// inspected through /proc from the root test process.
const SCAN = `const fs=require('node:fs');
const t=fs.statSync(process.argv[1]);
const holds=fs.readdirSync('/proc/self/fd').filter(fd=>{
  try{const s=fs.statSync('/proc/self/fd/'+fd);return s.ino===t.ino&&s.dev===t.dev;}catch{return false;}});`;
const DETECT = SCAN + "console.log(JSON.stringify(holds));";
// Holds until the release file exists. The root test process cannot signal a worker-uid process.
const HOLD = SCAN + `process.stdout.write(JSON.stringify({pid:process.pid,holds})+'\\n');
setInterval(()=>{if(fs.existsSync(process.argv[2])||!fs.existsSync(process.argv[1]))process.exit(0);},50);`;
// Starts HOLD as a grandchild that inherits the mutation descriptor from this middle process, then exits.
const MIDDLE = `const c=require('node:child_process').spawn(process.execPath,['-e',${JSON.stringify(HOLD)},process.argv[1],process.argv[2]],
  {stdio:['ignore','inherit','ignore',3]});c.unref();`;
function firstLine(child: ChildProcess): Promise<any> {
  return new Promise((resolve, reject) => {
    let buffer = "";
    const timer = setTimeout(() => reject(new Error("holder startup deadline")), 5000);
    child.once("error", reject);
    child.stdout!.on("data", chunk => {
      buffer += chunk;
      const end = buffer.indexOf("\n");
      if (end < 0) return;
      clearTimeout(timer);
      child.stdout!.destroy();
      child.unref();
      resolve(JSON.parse(buffer.slice(0, end)));
    });
  });
}
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
      case "selfkill": process.kill(process.pid, "SIGKILL"); break; // no response, no close
      case "mutator": {
        // The production path: reclaim scope, mutation ticket, explicit inheritStdio.
        const scope = ownership.beginReclaim();
        const ticket = scope.beginMutation();
        const stdio = scope.inheritStdio(["ignore", "pipe", "ignore"]);
        const program = request.shape === "grandchild" ? MIDDLE : HOLD;
        const holder = spawn(process.execPath, ["-e", program, lockFile, request.release], { stdio });
        result = await firstLine(holder);
        if (request.finish) { ticket.settle(); scope.finish(); }
        break;
      }
      case "ordinary": {
        // Ordinary run/SDK-style spawns while a reclaim scope and mutation ticket are live.
        const scope = ownership.beginReclaim();
        const ticket = scope.beginMutation();
        const detect = (options: object) => JSON.parse(spawnSync(process.execPath, ["-e", DETECT, lockFile],
          { encoding: "utf8", timeout: 5000, ...options }).stdout);
        const asynchronous = await new Promise<string[]>((resolve, reject) => {
          const c = spawn(process.execPath, ["-e", DETECT, lockFile]);
          let out = "";
          c.stdout.on("data", chunk => { out += chunk; });
          c.once("error", reject);
          c.once("close", () => resolve(JSON.parse(out)));
        });
        result = {
          default: detect({}),
          inheritedStdio: detect({ stdio: ["inherit", "pipe", "inherit"] }),
          execFile: JSON.parse(execFileSync(process.execPath, ["-e", DETECT, lockFile], { encoding: "utf8", timeout: 5000 })),
          shell: JSON.parse(spawnSync("/bin/sh", ["-c", 'exec "$0" -e "$1" "$2"', process.execPath, DETECT, lockFile],
            { encoding: "utf8", timeout: 5000 }).stdout),
          asynchronous,
          // Positive control: only the explicit scope option passes the descriptor.
          viaScope: detect({ stdio: scope.inheritStdio(["ignore", "pipe", "inherit"]) }),
        };
        ticket.settle(); scope.finish();
        break;
      }
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
