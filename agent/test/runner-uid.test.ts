import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  killRunnerGroup,
  runnerCommand,
  runnerPath,
  runnerTmpdir,
  setprivRunnerArgs,
  uidSplitActive,
} from "../src/runner-uid.js";

// PRD #51 M4 — the worker→runner uid boundary primitive. The split is gated on
// UZI_UID_SPLIT=1 (exported ONLY by the A1 root-started entrypoint); absent = the #58
// single-uid start, where every primitive is a passthrough (no setpriv, direct signal).

const SPLIT_VARS = ["UZI_UID_SPLIT", "UZI_RUNNER_PATH", "UZI_RUNNER_TMPDIR"] as const;

describe("runner-uid: split detection", () => {
  const saved: Record<string, string | undefined> = {};
  beforeEach(() => {
    for (const k of SPLIT_VARS) saved[k] = process.env[k];
    for (const k of SPLIT_VARS) delete process.env[k];
  });
  afterEach(() => {
    for (const k of SPLIT_VARS) {
      if (saved[k] === undefined) delete process.env[k];
      else process.env[k] = saved[k];
    }
  });

  it("uidSplitActive is true ONLY when UZI_UID_SPLIT=1", () => {
    assert.equal(uidSplitActive(), false);
    assert.equal(uidSplitActive({ UZI_UID_SPLIT: "0" }), false);
    assert.equal(uidSplitActive({ UZI_UID_SPLIT: "true" }), false);
    assert.equal(uidSplitActive({ UZI_UID_SPLIT: "1" }), true);
  });

  it("runnerCommand is a PASSTHROUGH single-uid (no setpriv)", () => {
    const { command, args } = runnerCommand("git", ["clone", "x", "y"]);
    assert.equal(command, "git");
    assert.deepEqual(args, ["clone", "x", "y"]);
  });

  it("runnerCommand wraps in setpriv-to-runner under the split", () => {
    process.env.UZI_UID_SPLIT = "1";
    const { command, args } = runnerCommand("git", ["clone", "x", "y"]);
    assert.equal(command, "/bin/setpriv");
    // setpriv args, then `--`, then the real command + its args verbatim.
    assert.deepEqual(args, [...setprivRunnerArgs(), "git", "clone", "x", "y"]);
    // The wrapper reuids to `runner` and CLEARS the inheritable + ambient cap sets — the
    // load-bearing bit (a plain reuid leaves ambient CAP_SETUID intact).
    const s = args.join(" ");
    assert.match(s, /--reuid runner/);
    assert.match(s, /--regid runner/);
    assert.match(s, /--init-groups/);
    assert.match(s, /--inh-caps -all/);
    assert.match(s, /--ambient-caps -all/);
    // The target is separated by `--` so it is never re-parsed as a setpriv flag.
    assert.ok(args.indexOf("--") < args.indexOf("git"), "the command must follow the -- separator");
  });

  it("runnerPath/runnerTmpdir prefer the runner-scoped vars, else the ambient ones", () => {
    assert.equal(runnerPath({ PATH: "/usr/bin" }), "/usr/bin");
    assert.equal(runnerPath({ PATH: "/usr/bin", UZI_RUNNER_PATH: "/nix/bin:/usr/bin" }), "/nix/bin:/usr/bin");
    assert.equal(runnerTmpdir({ TMPDIR: "/tmp/uzi-worker" }), "/tmp/uzi-worker");
    assert.equal(runnerTmpdir({ TMPDIR: "/tmp/uzi-worker", UZI_RUNNER_TMPDIR: "/tmp/uzi-runner" }), "/tmp/uzi-runner");
  });

  it("killRunnerGroup is a no-op for an undefined/invalid pid", () => {
    assert.equal(killRunnerGroup(undefined), false);
    assert.equal(killRunnerGroup(0), false);
    assert.equal(killRunnerGroup(-5), false);
  });
});
