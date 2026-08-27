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

  it("issue #120: the agent PATH resolves `agent-browser` to the SHIM in BOTH entrypoint modes", () => {
    // Two dirs in the worker image hold an `agent-browser` entry, and only one injects
    // --no-sandbox: the PRD #87 shim at /usr/local/bin, and the real npm CLI at
    // /app/node_modules/.bin (which the Dockerfiles never put on the image PATH).
    const SHIM_DIR = "/usr/local/bin";
    const REAL_CLI_DIR = "/app/node_modules/.bin";
    /** The dir a bare `agent-browser` resolves to — first PATH entry holding the name. */
    const resolvesFrom = (p: string | undefined): string | undefined =>
      (p ?? "").split(":").find((d) => d === SHIM_DIR || d === REAL_CLI_DIR);

    // The image PATH the entrypoint sees (it runs BEFORE the CMD), live worker order.
    const IMAGE = [
      "/opt/uzi-toolchain/bin",
      "/nix/var/nix/profiles/default/bin",
      "/usr/local/sbin",
      SHIM_DIR,
      "/usr/sbin",
      "/usr/bin",
      "/sbin",
      "/bin",
    ].join(":");
    // What the CMD (`npm run start`) turns that into inside the worker process: npm's
    // run-script prepends exactly these three entries (node-gyp-bin is its fingerprint).
    const NPM_MUTATED = [
      REAL_CLI_DIR,
      "/node_modules/.bin",
      "/app/node_modules/@npmcli/run-script/lib/node-gyp-bin",
      IMAGE,
    ].join(":");

    // THE REGRESSION (pre-#120 non-root/k8s branch): UZI_RUNNER_PATH unset ⇒ runnerPath()
    // falls back to the npm-mutated PATH ⇒ the real CLI wins ⇒ no --no-sandbox ⇒ SUID abort.
    assert.equal(
      resolvesFrom(runnerPath({ PATH: NPM_MUTATED })),
      REAL_CLI_DIR,
      "sanity: without the entrypoint pin the npm-injected dir shadows the shim",
    );

    // FIXED: the entrypoint pins UZI_RUNNER_PATH to the image PATH on the non-root branch
    // too, so the shim wins even though the worker's OWN PATH is still npm-mutated.
    assert.equal(
      resolvesFrom(runnerPath({ PATH: NPM_MUTATED, UZI_RUNNER_PATH: IMAGE })),
      SHIM_DIR,
      "the pinned runner PATH must resolve agent-browser to the shim",
    );

    // COMPOSE/A1 was always correct (worker PATH stripped, IMAGE_PATH handed over) — the
    // fix must not disturb it.
    assert.equal(
      resolvesFrom(runnerPath({ PATH: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", UZI_RUNNER_PATH: IMAGE })),
      SHIM_DIR,
      "the A1/compose mode must keep resolving to the shim",
    );
  });

  it("killRunnerGroup is a no-op for an undefined/invalid pid", () => {
    assert.equal(killRunnerGroup(undefined), false);
    assert.equal(killRunnerGroup(0), false);
    assert.equal(killRunnerGroup(-5), false);
  });
});
