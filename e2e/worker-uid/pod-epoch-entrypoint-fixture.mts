// Transport only baked path constants, as entrypoint-migration and runner-shared do.
// Mutation commands remain real image binaries; wrappers record before exec.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createInterface } from "node:readline";
import { commandWrapper } from "./pod-epoch-command-wrapper.mjs";

const entrypoint = fs.readFileSync(new URL("../../agent/templates/entrypoint.sh", import.meta.url), "utf8");
const helperSource = fs.readFileSync(new URL("../../agent/templates/pod-epoch-init.cjs", import.meta.url), "utf8");
const rootPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
export function entrypointFixture(data: string) {
  const root = fs.mkdtempSync("/tmp/epoch-entrypoint-");
  fs.chmodSync(root, 0o755);
  const app = path.join(root, "app"), home = path.join(root, "home");
  const bootstrap = path.join(root, "bootstrap"), nix = path.join(root, "nix");
  for (const directory of [app, home, bootstrap, nix]) {
    fs.mkdirSync(directory, { mode: directory === bootstrap ? 0o700 : 0o755 });
    if (directory === home || directory === bootstrap) fs.chownSync(directory, 10001, 10001);
  }
  const log = path.join(root, "operations");
  fs.writeFileSync(log, "", { mode: 0o666 }); fs.chmodSync(log, 0o666);
  const helper = path.join(root, "helper.cjs");
  assert.ok(helperSource.includes("const fs = require('node:fs');"), "classification injection applies exactly");
  fs.writeFileSync(helper, helperSource.replace("const fs = require('node:fs');",
    "const realFs = require('node:fs'); const fs = { ...realFs, statfsSync: (...a) => ({ ...realFs.statfsSync(...a), type: 0xef53 }) };")
    .replace("else resolve({ code, output });", "else { if (code === 200) { console.error('EPOCH_LOCK_CONFLICT'); if (process.argv[2] === 'hold') console.error('EPOCH_HOLD_LOCK_CONFLICT'); } resolve({ code, output }); }")
    .replace("const runtime = createRuntime({ root });",
      "if (command === 'hold') console.error('EPOCH_HOLD_REQUEST'); const runtime = createRuntime({ root });"));
  // Actual npm -> tsx -> worker static import graph. Only main's body is
  // instrumented after the permitted /proc snapshot, before any epoch operation.
  fs.cpSync(new URL("../../agent/src/", import.meta.url), path.join(app, "src"), { recursive: true });
  fs.mkdirSync(path.join(app, "templates"));
  fs.writeFileSync(path.join(app, "templates/pod-epoch-init.cjs"), helperSource);
  fs.symlinkSync("/app/node_modules", path.join(app, "node_modules"));
  fs.writeFileSync(path.join(app, "package.json"), JSON.stringify({
    type: "module", scripts: { start: "tsx src/main.ts" },
  }));
  const mainFile = path.join(app, "src/main.ts");
  const originalMain = fs.readFileSync(mainFile, "utf8");
  const marker = "  recordMaintenanceBootstrap(snapshotMaintenanceBootstrap());";
  assert.equal(originalMain.split(marker).length, 2, "main probe applies once");
  const probe = `
const assert = (await import('node:assert/strict')).default;
const fs = (await import('node:fs')).default;
const os = (await import('node:os')).default;
assert.equal(process.getuid(), 10001);
assert.ok(!fs.readdirSync('/proc/self/fd').some(fd => { try { return fs.readlinkSync('/proc/self/fd/' + fd) === ${JSON.stringify(data + '/.uzi-epoch.lock')}; } catch { return false; } }), 'no shell epoch descriptor crosses exec; Node may reuse FD9');
assert.equal(process.env.TMPDIR, ${JSON.stringify(bootstrap)}, 'pre-main TMPDIR is container-local');
assert.equal(os.tmpdir(), ${JSON.stringify(bootstrap)});
assert.equal(process.env.npm_config_cache, ${JSON.stringify(bootstrap)});
assert.equal(process.env.TSX_DISABLE_CACHE, '1');
if (process.env.NODE_EXTRA_CA_CERTS) {
  const crypto = await import('node:crypto'), tls = await import('node:tls');
  const expected = new crypto.X509Certificate(fs.readFileSync(process.env.NODE_EXTRA_CA_CERTS));
  assert.ok(tls.getCACertificates('extra').some(pem => new crypto.X509Certificate(pem).fingerprint256 === expected.fingerprint256), 'Node trust store loads the controller private CA');
  fs.appendFileSync(${JSON.stringify(log)}, 'CA_TRUST ' + process.env.NODE_EXTRA_CA_CERTS + '\\n');
}
const made = fs.mkdtempSync(os.tmpdir() + '/dependency-');
fs.appendFileSync(${JSON.stringify(log)}, 'BOOTSTRAP ' + made + '\\n');
console.log('EPOCH_NPM_READY');
return;
`;
  fs.writeFileSync(mainFile, originalMain.replace(marker, marker + '\n' + probe));
  let script = entrypoint.replace("DATA_DIR=/data", "DATA_DIR=" + data)
    .replace("NIX_DIR=/nix", "NIX_DIR=" + nix)
    .replace("EPOCH_HELPER=/usr/local/lib/uzi/pod-epoch-init.cjs", "EPOCH_HELPER=" + helper)
    .replace("EPOCH_BOOTSTRAP=/tmp/uzi-epoch-bootstrap", "EPOCH_BOOTSTRAP=" + bootstrap)
    .replaceAll("/home/worker", home).replaceAll("/app", app)
    // Pause after shell FD9 closes so the parent can take EX across the
    // complete npm/tsx/import gap. This adds observation only in this copy.
    .replace(/exec 9>&-\n( +)set --/g,
      'exec 9>&-\n$1echo EPOCH_INIT_DONE >&2\n$1IFS= read -r epoch_continue\n$1set --')
    .replace("CODEX_CMD_CACHE_DIR=/var/cache/uzi-codex-cmd", "CODEX_CMD_CACHE_DIR=" + root + "/cmd-cache");
  const initialRoot = fs.lstatSync(data);
  const checkRoot = `const fs=require("node:fs");const s=fs.lstatSync(${JSON.stringify(data)});
if(s.ino!==${initialRoot.ino}||s.uid!==${initialRoot.uid}||s.gid!==${initialRoot.gid}||(s.mode&4095)!==${initialRoot.mode & 0o7777})throw Error("data-root posture changed during initializer");`;
  for (const [symbol, binary] of [["CHOWN", "/bin/chown"], ["CHMOD", "/bin/chmod"],
    ["MKDIR", "/bin/mkdir"], ["RM", "/bin/rm"]] as const) {
    const wrapper = path.join(root, symbol.toLowerCase());
    fs.writeFileSync(wrapper, commandWrapper({ binary, symbol, check: checkRoot, log }), { mode: 0o755 });
    script = script.replace(symbol + "=" + binary, symbol + "=" + wrapper);
  }
  const file = path.join(root, "entrypoint.sh");
  fs.writeFileSync(file, script, { mode: 0o755 });
  const env = { PATH: rootPath, HOME: home, TMPDIR: data + "/runner/.tmp",
    UZI_EPOCH_LOCK: "1", UZI_POD_UID: "22222222-2222-4222-8222-222222222222", UZI_WORKER_TOKEN_FILE: data + "/token" };
  return { root, log, home, app, bootstrap,
    reject(extra: Record<string, string> = {}, args = ["npm", "run", "start"], cwd = app) {
      return spawnSync("/bin/sh", [file, ...args], { env: { ...env, ...extra }, cwd,
        encoding: "utf8", timeout: 8000 });
    },
    initScript(seed: boolean) {
      const golden = fs.readFileSync(new URL(seed
        ? "../../controller/internal/kube/testdata/pod-epoch-seed.sh"
        : "../../controller/internal/kube/testdata/runner-shared-init.sh", import.meta.url), "utf8");
      const source = path.join(root, "seed-source"), target = path.join(root, "seed-target");
      for (const d of [source, target]) if (!fs.existsSync(d)) fs.mkdirSync(d);
      fs.chownSync(target, 10001, 10001);
      fs.writeFileSync(path.join(source, "seed-positive-control"), "seeded");
      const marker = path.join(root, "toolchain"), layout = path.join(root, "layout");
      fs.writeFileSync(marker, "epoch-proof-toolchain");
      fs.writeFileSync(layout, "runner-shared-v1");
      const transformed = golden.replaceAll("/data-seed", data)
        .replaceAll("/usr/local/lib/uzi/pod-epoch-init.cjs", helper)
        .replaceAll("/etc/uzi-runner-layout", layout)
        .replaceAll("/etc/uzi-toolchain-profile", marker)
        .replaceAll("/nix-seed", target).replace("cd /nix\n", "cd " + source + "\n");
      const rendered = path.join(root, seed ? "seed.sh" : "runner-init.sh");
      fs.writeFileSync(rendered, transformed, { mode: 0o755 });
      return rendered;
    },
    start(single = false, script = file, extra: Record<string, string> = {}) {
      const argv = ["/bin/sh", script, "npm", "run", "start"];
      const command = single ? "/bin/setpriv" : argv.shift()!;
      const args = single ? ["--reuid", "10001", "--regid", "10001", "--clear-groups",
        "--bounding-set", "-all", "--inh-caps", "-all", "--ambient-caps", "-all", "--", ...argv] : argv;
      const child = spawn(command, args, { env: { ...env, ...extra }, cwd: app, stdio: ["pipe", "pipe", "pipe"], detached: true });
      let output = "";
      const waiters = new Map<string, () => void>();
      let failure = "";
      const closed = new Promise<number | null>((resolve, reject) => {
        child.once("error", reject); child.once("close", resolve);
      });
      const lines = createInterface({ input: child.stderr });
      lines.on("line", line => {
        failure += line + "\n"; output += line + "\n";
        for (const [needle, ready] of waiters) if (output.includes(needle)) ready();
      });
      child.stdout.on("data", chunk => {
        output += chunk;
        for (const [needle, ready] of waiters) if (output.includes(needle)) ready();
      });
      return { child, closed,
        async wait(needle: string) {
          if (output.includes(needle)) return;
          await new Promise<void>((resolve, reject) => {
            const timer = setTimeout(() => reject(new Error("entrypoint readiness deadline: " + needle + "\n" + failure)), 8000);
            waiters.set(needle, () => { clearTimeout(timer); waiters.delete(needle); resolve(); });
            closed.then(code => {
              clearTimeout(timer); waiters.delete(needle);
              if (output.includes(needle)) resolve();
              else reject(new Error("entrypoint exited " + code + ": " + failure));
            }, reject);
          });
        },
        async join() {
          let timer: ReturnType<typeof setTimeout> | undefined;
          try {
            const code = await Promise.race([closed, new Promise<never>((_, reject) => {
              timer = setTimeout(() => {
                try { process.kill(-child.pid!, "SIGKILL"); } catch { /* Already exited. */ }
                reject(new Error("owned bootstrap join deadline: " + failure));
              }, 10000);
            })]);
            assert.equal(code, 0, failure);
          } catch (error) {
            // SIGKILL is only a request. Join the owned process/stdio before
            // returning a failed proof; uncertainty stays a failure.
            let drain: ReturnType<typeof setTimeout> | undefined;
            await Promise.race([closed, new Promise<never>((_, reject) => {
              drain = setTimeout(() => reject(new Error("owned bootstrap did not settle")), 5000);
            })]).finally(() => clearTimeout(drain));
            throw error;
          } finally { clearTimeout(timer); }
        },
      };
    },
    close() {
      const reclaim = spawnSync("/bin/chown", ["-R", "-h", "0:0", root]);
      assert.equal(reclaim.status, 0);
      fs.rmSync(root, { recursive: true });
    },
  };
}
