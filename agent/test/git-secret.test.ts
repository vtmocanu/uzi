import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { recordingLogger } from "./helpers.js";
import { GitCache, gitEnv, httpScopeForUrl } from "../src/git.js";

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
  // Locate the extraHeader (idx, value) that carries the Basic credential.
  function authHeader(env: NodeJS.ProcessEnv): { idx: string; value: string } {
    const holder = Object.entries(env).find(
      ([k, v]) => /^GIT_CONFIG_VALUE_\d+$/.test(k) && typeof v === "string" && v.startsWith("Authorization: Basic "),
    );
    assert.ok(holder, "an Authorization: Basic extraHeader value is present");
    return { idx: holder![0].slice("GIT_CONFIG_VALUE_".length), value: holder![1] as string };
  }
  const decodeBasic = (value: string): string =>
    Buffer.from(value.slice("Authorization: Basic ".length), "base64").toString("utf8");

  it("carries the PAT as HTTP Basic (base64 user:pat) in a GIT_CONFIG_VALUE extraHeader", () => {
    const env = gitEnv(FAKE_PAT);
    // The raw PAT is base64-encoded, so it never appears literally anywhere in env.
    assert.ok(
      !Object.values(env).some((v) => typeof v === "string" && v.includes(FAKE_PAT)),
      "the raw PAT must not appear literally (it is base64-encoded)",
    );
    const { idx, value } = authHeader(env);
    assert.strictEqual(env[`GIT_CONFIG_KEY_${idx}`], "http.extraHeader");
    // git-over-HTTPS auth is Basic, NOT PRIVATE-TOKEN (that is REST-only): the PAT
    // is the password and the default username is the conventional "oauth2".
    assert.strictEqual(decodeBasic(value), `oauth2:${FAKE_PAT}`);
  });

  it("uses the supplied bot username as the Basic user", () => {
    const env = gitEnv(FAKE_PAT, undefined, "uzi-bot");
    assert.strictEqual(decodeBasic(authHeader(env).value), `uzi-bot:${FAKE_PAT}`);
  });

  it("adds nothing secret-bearing when no PAT is supplied", () => {
    const env = gitEnv();
    assert.ok(!Object.values(env).some((v) => typeof v === "string" && v.includes("extraHeader")));
  });

  it("host-scopes the header and pins followRedirects off when a scope is given (item 9)", () => {
    const scope = httpScopeForUrl("https://gitlab.example.com/org/repo.git");
    assert.strictEqual(scope, "https://gitlab.example.com/");
    const env = gitEnv(FAKE_PAT, scope, "uzi-bot");
    // The header key is scoped to the repo host, not the global http.extraHeader.
    const { idx, value } = authHeader(env);
    assert.strictEqual(env[`GIT_CONFIG_KEY_${idx}`], `http.${scope}.extraHeader`);
    assert.strictEqual(decodeBasic(value), `uzi-bot:${FAKE_PAT}`);
    // followRedirects is pinned false on the same scope so a redirect can't replay the credential.
    const keys = Object.entries(env).filter(([k]) => /^GIT_CONFIG_KEY_\d+$/.test(k)).map(([, v]) => v);
    assert.ok(keys.includes(`http.${scope}.followRedirects`));
  });

  it("has no http scope for a local/scp-style URL", () => {
    assert.strictEqual(httpScopeForUrl("/tmp/fixture/origin"), undefined);
    assert.strictEqual(httpScopeForUrl("git@gitlab.example.com:org/repo.git"), undefined);
  });
});

describe("pushBranch secret flow", () => {
  it("pushes the branch to origin without the PAT ever touching git argv", async () => {
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
    // Clone + create the branch (without the shim, to keep the log focused on push).
    const bare = await git.ensureClone(fx.originPath);
    await git.createOrAttachWorktree(bare, 7);

    const oldPath = process.env.PATH ?? "";
    process.env.PATH = shimDir + path.delimiter + oldPath;
    try {
      await git.pushBranch(bare, "agent/issue-7", FAKE_PAT, fx.originPath);
    } finally {
      process.env.PATH = oldPath;
    }

    const recordedArgv = fs.readFileSync(argvLog, "utf8");
    fs.rmSync(shimDir, { recursive: true, force: true });
    assert.ok(recordedArgv.includes("push"), "push should have run through the shim");
    assert.ok(!recordedArgv.includes(FAKE_PAT), "PAT must not appear in any git argv");
    assert.ok(!JSON.stringify(lines).includes(FAKE_PAT), "PAT must not appear in any log line");

    // The branch really landed on origin.
    const log = execFileSync("git", ["-C", fx.originPath, "log", "--oneline", "agent/issue-7"], { encoding: "utf8" });
    assert.ok(log.length > 0);
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
