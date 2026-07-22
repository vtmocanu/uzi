import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { toolchainPreflight, REQUIRED_TOOLS } from "../src/toolchain-preflight.js";

// PRD #92 M3 — the boot toolchain preflight is a pure function: it resolves the five
// baked tools (python3/go/gcc/pip/openssl) against runnerPath(env) (UZI_RUNNER_PATH || PATH) and
// asserts the stable `/opt/uzi-toolchain` handle dereferences. These tests are hermetic:
// a temp bin dir holds executable stubs, and the stable-path arg is overridden to a temp
// symlink so the suite never depends on the host having `/opt/uzi-toolchain`.

let tmp: string;
let binDir: string;
let stableTarget: string;
let stableLink: string;
let danglingLink: string;

function writeStub(dir: string, name: string): void {
  const p = path.join(dir, name);
  fs.writeFileSync(p, "#!/bin/sh\nexit 0\n");
  fs.chmodSync(p, 0o755);
}

before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-preflight-"));
  binDir = path.join(tmp, "bin");
  fs.mkdirSync(binDir);
  for (const tool of REQUIRED_TOOLS) writeStub(binDir, tool);

  // A resolvable stable handle: a symlink → a real dir (mirrors /opt/uzi-toolchain → store).
  stableTarget = path.join(tmp, "toolchain-store");
  fs.mkdirSync(stableTarget);
  stableLink = path.join(tmp, "uzi-toolchain");
  fs.symlinkSync(stableTarget, stableLink);

  // A DANGLING handle (the stranded-PVC signature): the symlink target is absent.
  danglingLink = path.join(tmp, "uzi-toolchain-dangling");
  fs.symlinkSync(path.join(tmp, "does-not-exist"), danglingLink);
});

after(() => {
  fs.rmSync(tmp, { recursive: true, force: true });
});

describe("toolchainPreflight (PRD #92 M3)", () => {
  it("ok when all four tools resolve on UZI_RUNNER_PATH and the stable path dereferences", () => {
    const res = toolchainPreflight({ UZI_RUNNER_PATH: `/nonexistent:${binDir}` }, stableLink);
    assert.deepEqual(res.missing, []);
    assert.equal(res.ok, true);
  });

  it("reports a missing tool (and only that tool) when it is absent from the runner PATH", () => {
    // A bin dir missing `go` only.
    const partial = path.join(tmp, "partial-bin");
    fs.mkdirSync(partial);
    for (const tool of REQUIRED_TOOLS) if (tool !== "go") writeStub(partial, tool);

    const res = toolchainPreflight({ UZI_RUNNER_PATH: partial }, stableLink);
    assert.equal(res.ok, false);
    assert.deepEqual(res.missing, ["go"]);
  });

  it("reports the stable-path sentinel when `/opt/uzi-toolchain` does not resolve (stale store)", () => {
    const res = toolchainPreflight({ UZI_RUNNER_PATH: binDir }, danglingLink);
    assert.equal(res.ok, false);
    assert.deepEqual(res.missing, [danglingLink], "only the unresolvable stable path is missing");
  });

  it("fails with both a missing tool and the stale stable path", () => {
    const res = toolchainPreflight({ UZI_RUNNER_PATH: "/nonexistent" }, danglingLink);
    assert.equal(res.ok, false);
    assert.deepEqual(res.missing, [...REQUIRED_TOOLS, danglingLink]);
  });

  it("prefers UZI_RUNNER_PATH over PATH (the runner PATH incl. /nix, not the stripped worker PATH)", () => {
    // The worker's own PATH is empty of tools; the runner PATH carries them. A check
    // that read PATH would false-fail — this proves it reads UZI_RUNNER_PATH.
    const res = toolchainPreflight({ UZI_RUNNER_PATH: binDir, PATH: "/nonexistent" }, stableLink);
    assert.equal(res.ok, true, "resolves against UZI_RUNNER_PATH, not the stripped PATH");

    // And falls back to PATH when UZI_RUNNER_PATH is unset (#58 single-uid start).
    const fallback = toolchainPreflight({ PATH: binDir }, stableLink);
    assert.equal(fallback.ok, true, "falls back to PATH when UZI_RUNNER_PATH is unset");
  });

  it("does not require a tool to be executable via a non-executable file of the same name", () => {
    // A same-named but non-executable file must NOT satisfy the check.
    const nonExec = path.join(tmp, "nonexec-bin");
    fs.mkdirSync(nonExec);
    for (const tool of REQUIRED_TOOLS) {
      const p = path.join(nonExec, tool);
      fs.writeFileSync(p, "not a program\n");
      fs.chmodSync(p, 0o644);
    }
    const res = toolchainPreflight({ UZI_RUNNER_PATH: nonExec }, stableLink);
    assert.equal(res.ok, false);
    assert.deepEqual(res.missing, [...REQUIRED_TOOLS]);
  });
});
