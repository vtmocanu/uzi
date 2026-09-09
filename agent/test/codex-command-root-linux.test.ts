import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";

import {
  COMMAND_CAPTURE_KILLED_CODE,
  MAX_COMMAND_CAPTURE_BYTES,
  makeDefaultSpawnCommand,
} from "../src/codex/codex-executor.js";
import { launchCodexEffectRoot } from "../src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

const ENABLED = process.platform === "linux" && process.env.UZI_R2_COMMAND_ROOT_LINUX === "1";

describe("Codex supervised command root (Linux packaged profile)", { skip: ENABLED ? false : "opt-in packaged Linux profile" }, () => {
  it("drops retained worker controller caps before a worker-PAT supervisor launch", async () => {
    const root = `/data/runner/cdr-r2-worker-${process.pid}`;
    await fs.mkdir(root, { recursive: true, mode: 0o2770 });
    await fs.chmod(root, 0o2770);
    const handle = await launchCodexEffectRoot({
      identity: "worker_pat",
      command: "/bin/true",
      args: [],
      cwd: root,
      env: { PATH: "/usr/bin:/bin", LANG: "C" },
      supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
    });
    assert.equal(handle.started.uid, 10001);
    assert.equal(handle.started.liveCapsZero, true, "SETUID/SETGID did not cross into the supervisor");
    assert.equal((await handle.waitChild()).code, 0);
    assert.equal((await handle.dispose(2000)).clean, true);
  });

  it("runs uid 10003 under per-run Landlock confinement and cap-kills/reaps an oversized command", async () => {
    assert.equal(process.env.UZI_UID_SPLIT, "1", "the real root-start entrypoint established the uid split");
    const root = `/data/runner/cdr-r2-${process.pid}`;
    const worktree = `${root}/worktree`;
    const sibling = `/data/runner/cdr-r2-sibling-${process.pid}`;
    await fs.mkdir(worktree, { recursive: true, mode: 0o2770 });
    await fs.chmod(root, 0o2770);
    await fs.chmod(worktree, 0o2770);
    await fs.mkdir(sibling, { recursive: true, mode: 0o2770 });
    await fs.writeFile(`${sibling}/canary`, "must stay hidden\n");
    const tmpBefore = (await fs.readdir("/tmp")).filter((name) => name.startsWith("uzi-codex-command-")).sort();

    const registry = new ExecutionRegistry(newLocalExecutionEpoch(71));
    const spawnCommand = makeDefaultSpawnCommand(
      registry,
      (spec) => launchCodexEffectRoot(spec),
      5000,
      worktree,
      { PATH: "/usr/bin:/bin", LANG: "C", HOME: "/tmp", TMPDIR: "/tmp" },
    );

    const isolation = await spawnCommand(
      [
        "/bin/sh", "-c",
        `ls /data/runner >/dev/null 2>&1 && printf sibling-enumerable; cat ${sibling}/canary >/dev/null 2>&1 && printf sibling-readable; cat /run/secrets/worker_token >/dev/null 2>&1 && printf token-readable; printf %s "$TMPDIR" > tmpdir-proof; printf isolated > proof`,
      ],
      { cwd: worktree },
    );
    assert.equal(isolation.code, 0, `sandbox command failed: ${isolation.stderr}`);
    assert.equal(isolation.stdout, "", "sibling runs cannot be enumerated/read and the worker token cannot be read");
    assert.equal(await fs.readFile(`${worktree}/proof`, "utf8"), "isolated");
    assert.match(
      await fs.readFile(`${worktree}/tmpdir-proof`, "utf8"),
      /^\/tmp\/uzi-codex-command-/,
      "the command observed its provisioned private TMPDIR",
    );

    const bytesToEmit = 8 * 1024 * 1024;
    const capped = await spawnCommand(
      ["/bin/sh", "-c", `head -c ${bytesToEmit} /dev/zero`],
      { cwd: worktree },
    );
    assert.equal(capped.code, COMMAND_CAPTURE_KILLED_CODE);
    assert.equal(Buffer.byteLength(capped.stdout), MAX_COMMAND_CAPTURE_BYTES);
    assert.equal(registry.rootCount(), 2, "both command roots were registered before the reap proof");
    assert.equal(registry.hasLiveCommandRoot(), false, "cap-kill returned only after ECHILD+__WALL reap");
    const tmpAfter = (await fs.readdir("/tmp")).filter((name) => name.startsWith("uzi-codex-command-")).sort();
    assert.deepEqual(tmpAfter, tmpBefore, "normal and SIGKILL paths leave no per-command tmp directory");
  });
});
