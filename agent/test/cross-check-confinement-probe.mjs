// Trusted, fixed filesystem operations; never execute a screened candidate payload.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

export const filesystemProbe = String.raw`
const fs = require("node:fs");
const [checkout, state, sibling] = process.argv.slice(1);
const attempt = (fn) => { try { fn(); return "allowed"; } catch (e) { return e.code; } };
const result = {
 devNullWrite: attempt(() => { const fd = fs.openSync("/dev/null", "w"); fs.writeSync(fd, "probe"); fs.closeSync(fd); }),
 sharedDeviceRead: attempt(() => fs.readdirSync("/dev/shm")),
 checkoutRead: attempt(() => fs.readFileSync(checkout + "/read.txt")),
 checkoutWrite: attempt(() => fs.writeFileSync(checkout + "/write.txt", "probe")),
 // /etc/passwd is a non-executing file in the cross-check system read grants.
 systemRead: attempt(() => fs.readFileSync(process.execPath)),
 systemWrite: attempt(() => { const fd = fs.openSync("/etc/passwd", "r+"); fs.closeSync(fd); }),
 toolchainRead: attempt(() => fs.readFileSync("/opt/uzi-toolchain/bin/go")),
 toolchainWrite: attempt(() => { const fd = fs.openSync("/opt/uzi-toolchain/bin/go", "r+"); fs.closeSync(fd); }),
 siblingRead: attempt(() => fs.readFileSync(sibling + "/secret.txt")),
 siblingWrite: attempt(() => fs.writeFileSync(sibling + "/write.txt", "probe")),
 symlinkRead: attempt(() => fs.readFileSync(checkout + "/escape")),
 symlinkWrite: attempt(() => fs.writeFileSync(checkout + "/escape", "probe")),
};
for (const dir of ["home", "codex", "codex/sessions", "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"]) {
 result[dir + ":write"] = attempt(() => fs.writeFileSync(state + "/" + dir + "/probe.txt", "private"));
 result[dir + ":read"] = attempt(() => fs.readFileSync(state + "/" + dir + "/probe.txt"));
}
result.authWrite = attempt(() => fs.writeFileSync(state + "/codex/auth.json", JSON.stringify({ synthetic: true })));
result.authRead = attempt(() => fs.readFileSync(state + "/codex/auth.json"));
result.configRead = attempt(() => fs.readFileSync(state + "/codex/config.toml"));
result.configWrite = attempt(() => { const fd = fs.openSync(state + "/codex/config.toml", "r+"); fs.closeSync(fd); });
console.log(JSON.stringify(result));
`;

export function assertIsolation(result) {
 for (const key of ["devNullWrite", "checkoutRead", "systemRead", "toolchainRead", "authRead", "authWrite", "configRead", "configWrite"]) {
  assert.equal(result[key], "allowed", key);
 }
 for (const dir of ["home", "codex", "codex/sessions", "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"]) {
  for (const op of ["read", "write"]) assert.equal(result[dir + ":" + op], "allowed", dir + ":" + op);
 }
 for (const key of ["sharedDeviceRead", "checkoutWrite", "systemWrite", "toolchainWrite", "siblingRead", "siblingWrite", "symlinkRead", "symlinkWrite"]) {
  assert.equal(result[key], "EACCES", key);
 }
}

export function makeProbeFixture(base) {
 const checkout = path.join(base, "checkout");
 const state = path.join(base, "private");
 const sibling = path.join(base, "sibling");
 for (const dir of [checkout, state, sibling]) { fs.mkdirSync(dir, { mode: 0o700 }); fs.chmodSync(dir, 0o700); }
 for (const dir of ["home", "codex/sessions", "xdg-config", "xdg-cache", "xdg-data", "xdg-state", "tmp"]) {
  fs.mkdirSync(path.join(state, dir), { recursive: true, mode: 0o700 });
 }
 fs.writeFileSync(path.join(state, "codex/config.toml"), "# synthetic fixture\n");
 fs.writeFileSync(path.join(checkout, "read.txt"), "checkout");
 fs.writeFileSync(path.join(sibling, "secret.txt"), "synthetic sibling");
 fs.symlinkSync(path.join(sibling, "secret.txt"), path.join(checkout, "escape"));
 return { checkout, state, sibling };
}

function directProof() {
 const sandbox = process.env.UZI_CROSS_CHECK_SANDBOX;
 if (process.platform !== "linux" || !sandbox || !fs.existsSync(sandbox)) {
  console.error("NOT RUN: Linux and a freshly root-target-built helper are required");
  process.exitCode = 2; return;
 }
 const scratch = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../.uzi/scratch");
 fs.mkdirSync(scratch, { recursive: true });
 const base = fs.mkdtempSync(path.join(scratch, "confinement-"));
 try {
  const { checkout, state, sibling } = makeProbeFixture(base);
  const run = (args) => spawnSync(sandbox,
   [...args, "--", process.execPath, "--eval", filesystemProbe, checkout, state, sibling],
   { cwd: checkout, env: { HOME: path.join(state, "home"), TMPDIR: path.join(state, "tmp"), PATH: "/usr/bin:/bin", LANG: "C" },
    encoding: "utf8", timeout: 10000, maxBuffer: 65536 });
  const controlTmp = path.join(base, "control-tmp");
  fs.mkdirSync(controlTmp, { mode: 0o700 }); fs.chmodSync(controlTmp, 0o700);
  const control = run(["--root", checkout, "--tmp", controlTmp, "--cwd", checkout, "--mode", "off"]);
  assert.equal(control.status, 0, control.stderr);
  const unconfined = JSON.parse(control.stdout);
  assert.equal(unconfined.systemWrite, "allowed", "systemWrite control must allow the same r+ open without confinement");
  assert.equal(unconfined.checkoutWrite, "allowed");
  assert.equal(unconfined.siblingRead, "allowed");
  assert.equal(unconfined.siblingWrite, "allowed");
  assert.throws(() => assertIsolation(unconfined), "same isolation assertion must reject the unconfined control");
  console.log("CONTROL: same direct filesystem isolation assertion rejected");
  const result = run(["--cross-check", "--root", checkout, "--state", state, "--cwd", checkout]);
  if (result.status !== 0) {
   console.error("NOT RUN: required confinement refused before process proof: " + result.stderr.trim());
   process.exitCode = 2; return;
  }
  console.log("DIRECT PROCESS: " + result.stdout.trim());
  assertIsolation(JSON.parse(result.stdout));
  console.log("PASS: direct helper filesystem enforcement including private subtrees and symlink denial");
 } finally { fs.rmSync(base, { recursive: true, force: true }); }
}
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) directProof();
