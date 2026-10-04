import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { spawnSync, type SpawnSyncReturns } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { launchCodexRoot, type CodexLaunchSpec, type LauncherDeps, type ProvisionSpawnSync } from "../src/codex/launcher.js";
import { SESSION_SEED_ENTRYPOINT, runSessionSeedCli, sessionSeedInvocation } from "../src/codex/session-seed-cli.js";
import { CODEX_SESSION_GID, setprivRunnerArgs } from "../src/runner-uid.js";

const ARGV = ["node", "session-seed-cli.ts", "/stage/sessions", "/dest/sessions"];

async function run(
  seed: (a: string, b: string) => Promise<{ files: number }>,
  argv: readonly string[] = ARGV,
): Promise<{ code: number; lines: string[] }> {
  const lines: string[] = [];
  const code = await runSessionSeedCli(argv, { seed: seed as never, stderr: (l) => lines.push(l) });
  return { code, lines };
}

describe("runSessionSeedCli exit codes and stderr reason", () => {
  it("a caught seed error exits 2 with only the error code on stderr", async () => {
    const marker = "UNIQUE-MARKER-9f3a";
    const err = Object.assign(new Error(`${marker} cannot open /secret/path/sessions/abc.jsonl`), { code: "EACCES" });
    const { code, lines } = await run(async () => { throw err; });
    assert.equal(code, 2);
    assert.deepEqual(lines, ["session seed failed: EACCES"]);
    assert.ok(!lines.join("").includes(marker));
    assert.ok(!lines.join("").includes("/secret/path"));
  });

  it("falls back to the error name when there is no code", async () => {
    const { lines } = await run(async () => { throw new TypeError("nope /x/y"); });
    assert.deepEqual(lines, ["session seed failed: TypeError"]);
  });

  it("sanitizes an attacker-shaped code to [A-Za-z0-9_] and 64 chars", async () => {
    const weird = await run(async () => { throw Object.assign(new Error("m"), { code: "E\u001b[2J\nX" }); });
    assert.deepEqual(weird.lines, ["session seed failed: E2JX"]);
    const long = await run(async () => { throw Object.assign(new Error("m"), { code: "A".repeat(200) }); });
    assert.deepEqual(long.lines, [`session seed failed: ${"A".repeat(64)}`]);
  });

  it("zero copied artifacts exits 2 with a reason line", async () => {
    const { code, lines } = await run(async () => ({ files: 0 }));
    assert.equal(code, 2);
    assert.deepEqual(lines, ["session seed: no artifacts copied"]);
  });

  it("wrong argc exits 2 with a reason line and never seeds", async () => {
    let called = false;
    const { code, lines } = await run(async () => { called = true; return { files: 1 }; }, ARGV.slice(0, 3));
    assert.equal(code, 2);
    assert.deepEqual(lines, ["session seed: invalid arguments"]);
    assert.equal(called, false);
  });

  it("success exits 0 and writes nothing", async () => {
    const { code, lines } = await run(async () => ({ files: 3 }));
    assert.equal(code, 0);
    assert.deepEqual(lines, []);
  });
});

describe("session seed failure outside runSessionSeedCli reaches the launcher error", () => {
  it("a module-load throw exits 1 and its stderr is published by launchCodexRoot", async () => {
    const agentRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
    const appRoot = await fs.mkdtemp(path.join(os.tmpdir(), "seed-app-"));
    let tmpDir = "";
    const probe = "SEED-PROBE-module-load-throw-71c2";
    try {
      tmpDir = await fs.mkdtemp(path.join(os.tmpdir(), "seed-tmp-"));
      await fs.symlink(path.join(agentRoot, "node_modules"), path.join(appRoot, "node_modules"));
      await fs.mkdir(path.join(appRoot, "src", "codex"), { recursive: true });
      await fs.writeFile(path.join(appRoot, "src", "codex", "session-seed-cli.ts"), `throw new Error(${JSON.stringify(probe)});\n`);

      const inv = sessionSeedInvocation(appRoot, "/stage/sessions", "/dest/sessions");
      // Same spawn shape as the launcher's seed step (no cwd there), minus setpriv.
      const real = spawnSync(inv.command, inv.args, {
        env: { PATH: process.env.PATH, HOME: tmpDir, TMPDIR: tmpDir },
        stdio: ["ignore", "ignore", "pipe"],
        timeout: 30_000, // a sync spawn cannot be interrupted by --test-timeout
      });
      assert.equal(real.status, 1, `stderr: ${String(real.stderr)}`);
      assert.ok(String(real.stderr).includes(probe));

      const root = "/data/run/root-seed";
      const codexHome = path.join(root, "codex");
      const sessions = path.join(codexHome, "sessions");
      let sharedSessionPosture = false;
      const provisionSpawnSync: ProvisionSpawnSync = (command, args) => {
        if (args.includes(SESSION_SEED_ENTRYPOINT)) return real as SpawnSyncReturns<Buffer>;
        let stdout = Buffer.alloc(0);
        const prefixLength = command === "/bin/setpriv" ? setprivRunnerArgs().length : 0;
        const executable = prefixLength === 0 ? command : args[prefixLength];
        const shellArgs = prefixLength === 0 ? args : args.slice(prefixLength + 1);
        if (executable === "/bin/sh" && shellArgs[1] === 'stat -c "%u:%g:%a" -- "$1"') {
          const requestedPath = shellArgs[3];
          assert.equal(typeof requestedPath, "string");
          assert.ok([
            root, codexHome, sessions, path.join(root, "home"), path.join(root, "tmp"),
            ...["config", "cache", "data", "state"].map((dir) => path.join(root, `xdg-${dir}`)),
          ].includes(requestedPath!));
          const shared = sharedSessionPosture && [root, codexHome, sessions].includes(requestedPath!);
          const mode = shared ? (requestedPath === sessions ? "2750" : "710") : "700";
          stdout = Buffer.from(`10002:${shared ? CODEX_SESSION_GID : 10002}:${mode}\n`);
        } else if (executable === "/bin/sh") {
          if (shellArgs[1]?.includes("chgrp")) sharedSessionPosture = true;
          else if (shellArgs[1]?.includes("mkdir -m 700")) sharedSessionPosture = false;
        }
        return { pid: 1, output: [], stdout, stderr: Buffer.alloc(0), status: 0, signal: null } as SpawnSyncReturns<Buffer>;
      };
      const spec: CodexLaunchSpec = {
        ownedDataRoot: root,
        provider: { name: "uzi-codex", baseUrl: "http://127.0.0.1:9/v1", envKey: "CODEX_PROVIDER_KEY", credentialValue: "dummy-key" },
        model: "gpt-5-codex",
        codexBin: "/opt/uzi-codex/0.159.3/bin/codex",
        supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
        kind: "provider",
        childArgv: ["app-server"],
        cwd: "/work/repo",
        useAppServerAuth: true,
        authMode: "subscription",
        seedSession: true,
      };
      const deps: LauncherDeps = {
        env: { UZI_UID_SPLIT: "1" },
        resolveRunnerUid: () => 10002,
        assertNoUnexpectedSystemConfig: () => { /* no /etc/codex in unit tests */ },
        makeRunnerTrees: undefined,
        provisionSpawnSync,
        redactDiagnostic: (s) => s,
        spawnSupervisor: () => { throw new Error("supervisor must not spawn after a seed failure"); },
      };
      await assert.rejects(launchCodexRoot(spec, deps), (e: unknown) => {
        const msg = (e as Error).message;
        assert.match(msg, /runner-owned session seed failed \(exit 1\)/);
        assert.ok(msg.includes(probe), msg);
        return true;
      });
    } finally {
      await fs.rm(appRoot, { recursive: true, force: true });
      if (tmpDir !== "") await fs.rm(tmpDir, { recursive: true, force: true });
    }
  });
});
