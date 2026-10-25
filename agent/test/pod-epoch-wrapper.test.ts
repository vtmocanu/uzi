import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";
import { commandWrapper } from "../../e2e/worker-uid/pod-epoch-command-wrapper.mjs";

function quote(value: string): string { return "'" + value.replaceAll("'", "'\\''") + "'"; }
function fixture(t: { after(fn: () => void): void }) {
  const root = fs.mkdtempSync(fileURLToPath(new URL("../../.uzi/scratch/wrapper-", import.meta.url)));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const state = path.join(root, "state"), log = path.join(root, "events"), called = path.join(root, "called");
  fs.writeFileSync(state, "correct");
  const check = `if(require("node:fs").readFileSync(${JSON.stringify(state)},"utf8")!=="correct")process.exit(9)`;
  function run(body: string, restore = false) {
    const binary = path.join(root, "command"), wrapper = path.join(root, "wrapper");
    fs.writeFileSync(binary, "#!/bin/sh\n" + body, { mode: 0o755 });
    fs.writeFileSync(wrapper, commandWrapper({ binary, symbol: "TEST", check, log, node: process.execPath }), { mode: 0o755 });
    const script = (restore ? `trap ${quote("printf correct > " + quote(state))} EXIT\n` : "") + quote(wrapper);
    return spawnSync("/bin/sh", ["-c", script], { encoding: "utf8", timeout: 5000 });
  }
  return { state, log, called, run };
}

test("metadata wrapper refuses a transient mismatch even when caller restores state", (t) => {
  const f = fixture(t);
  const result = f.run("printf bad > " + quote(f.state), true);
  assert.equal(fs.readFileSync(f.state, "utf8"), "correct", "caller restores final posture");
  assert.notEqual(result.status, 0, "post-operation mismatch must fail despite correct final posture");
  assert.match(fs.readFileSync(f.log, "utf8"), /POSTURE_VIOLATION/);
});
test("metadata wrapper refuses the operation when its initial posture is invalid", (t) => {
  const f = fixture(t);
  fs.writeFileSync(f.state, "bad");
  const result = f.run("touch " + quote(f.called));
  assert.notEqual(result.status, 0);
  assert.equal(fs.existsSync(f.called), false);
});
test("metadata wrapper preserves command failures with valid root posture", (t) => {
  const f = fixture(t);
  assert.equal(f.run("exit 7").status, 7);
});
