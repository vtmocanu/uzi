import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { recordingLogger } from "./helpers.js";
import { GitCache, gitEnv } from "../src/git.js";

// Primary directive (auditor): the bot PAT must never be readable in the git
// process table. Passing it via `git -c` would put it on argv (world-readable
// /proc/<pid>/cmdline); we pass it via env-scoped config instead. These tests
// pin that guarantee down: argv and logs are asserted PAT-free against the real
// spawn, and gitEnv is checked structurally.

// A deliberately low-entropy, obviously-fake marker so secret scanners don't
// flag the very fixture we use to prove non-leakage.
const FAKE_PAT = "uzi-fixture-pat-marker-do-not-scan";

let fx: Fixture;

beforeEach(() => {
  fx = makeFixture();
});

afterEach(() => fx.cleanup());

describe("gitEnv", () => {
  it("carries the PAT only inside a GIT_CONFIG_VALUE, as a PRIVATE-TOKEN header", () => {
    const env = gitEnv(FAKE_PAT);
    const holders = Object.entries(env).filter(([, v]) => typeof v === "string" && v.includes(FAKE_PAT));
    // Exactly one env var holds the secret, and it is the extraHeader value.
    assert.strictEqual(holders.length, 1);
    const [key, value] = holders[0]!;
    assert.match(key, /^GIT_CONFIG_VALUE_\d+$/);
    assert.strictEqual(value, `PRIVATE-TOKEN: ${FAKE_PAT}`);
    // The corresponding key entry names http.extraHeader.
    const idx = key.slice("GIT_CONFIG_VALUE_".length);
    assert.strictEqual(env[`GIT_CONFIG_KEY_${idx}`], "http.extraHeader");
  });

  it("adds nothing secret-bearing when no PAT is supplied", () => {
    const env = gitEnv();
    assert.ok(!Object.values(env).some((v) => typeof v === "string" && v.includes("extraHeader")));
  });
});

describe("secret flow through real git spawns", () => {
  it("never leaks the PAT into git argv or logs during clone/fetch", async () => {
    // Shim `git` on PATH: record every invocation's argv, then exec the real git.
    const realGit = execFileSync("bash", ["-lc", "command -v git"], { encoding: "utf8" }).trim();
    const shimDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-gitshim-"));
    const argvLog = path.join(shimDir, "argv.log");
    const shim = path.join(shimDir, "git");
    fs.writeFileSync(
      shim,
      `#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> ${JSON.stringify(argvLog)}\nexec ${JSON.stringify(realGit)} "$@"\n`,
    );
    fs.chmodSync(shim, 0o755);

    const { logger, lines } = recordingLogger();
    const git = new GitCache(fx.dataDir, logger);

    const oldPath = process.env.PATH ?? "";
    process.env.PATH = shimDir + path.delimiter + oldPath;
    try {
      // ensureClone runs `git clone --bare` then `git fetch` with the PAT env.
      await git.ensureClone(fx.originPath, FAKE_PAT);
    } finally {
      process.env.PATH = oldPath; // restore PATH only; read the log before cleanup
    }

    const recordedArgv = fs.readFileSync(argvLog, "utf8");
    fs.rmSync(shimDir, { recursive: true, force: true });

    assert.ok(recordedArgv.length > 0, "the shim should have captured git invocations");
    assert.ok(recordedArgv.includes("clone"), "clone should have run through the shim");
    assert.ok(!recordedArgv.includes(FAKE_PAT), "PAT must not appear in any git argv");
    assert.ok(!JSON.stringify(lines).includes(FAKE_PAT), "PAT must not appear in any log line");
  });
});
