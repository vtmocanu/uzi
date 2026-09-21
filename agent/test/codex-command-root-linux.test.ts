import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import {
  COMMAND_CAPTURE_KILLED_CODE,
  MAX_COMMAND_CAPTURE_BYTES,
  ensureCodexSharedDirectory,
  makeDefaultSpawnCommand,
} from "../src/codex/codex-executor.js";
import { launchCodexEffectRoot } from "../src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

const ENABLED = process.platform === "linux" && process.env.UZI_R2_COMMAND_ROOT_LINUX === "1";

/** Assert the created provider-root ancestor is 3770 — sticky PLUS setgid — owned by
 *  `uid:gid`. PRD #1493 M3 (D7): the sticky bit is what stops uid 10003 (runner-cmd, a
 *  `runner`-group member that does NOT own the entry) from renaming, unlinking or replacing
 *  an active provider root through this group-writable parent — unlink/rename are governed
 *  by the PARENT directory, so a sticky parent denies a non-owner even with group write. */
async function assertStickyProviderAncestor(dir: string, uid: number, gid: number): Promise<void> {
  const st = await fs.lstat(dir);
  assert.equal(st.uid, uid, "provider-root ancestor is owned by the worker identity");
  assert.equal(st.gid, gid, "provider-root ancestor is group-owned by the runner group");
  assert.equal(st.mode & 0o7777, 0o3770, "provider-root ancestor is 3770 (sticky + setgid + rwxrwx---)");
  assert.equal(st.mode & 0o1000, 0o1000, "the STICKY bit is set (non-owner group member cannot rename/unlink entries)");
  assert.equal(st.mode & 0o2000, 0o2000, "the setgid bit is set (children inherit the runner group)");
}

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
      "required",
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

  it("uid 10003 cannot rename/unlink/replace an active provider HOME (sticky 3770 ancestor)", async () => {
    // Expressed via MODE/OWNERSHIP: the gate runs as the worker uid with no live uid-10003
    // process, so we assert the resulting sticky posture rather than driving a real rename.
    const uid = process.getuid!();
    const groups = [...new Set(process.getgroups!())];
    const gA = groups[0]!;
    const gB = groups[1] ?? gA;
    const base = await fs.mkdtemp(path.join(os.tmpdir(), "cdr-sticky-"));
    try {
      const parent = path.join(base, "agent-home");
      await fs.mkdir(parent, { mode: 0o3770 });
      await fs.chown(parent, uid, gA);
      await fs.chmod(parent, 0o3770);
      const providerHome = path.join(parent, "run-xyz"); // == sdkHomeRoot/<runId>, a provider-root ancestor
      await ensureCodexSharedDirectory(providerHome, { uid, gid: gB });
      await assertStickyProviderAncestor(providerHome, uid, gB);
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });
});

// PRD #1493 M3 (part F) — a PORTABLE mode/ownership assertion that runs under the worker
// uid in the normal `task gate:agent` (not behind the opt-in UZI_R2 flag), proving the
// executor-created per-run HOME is 3770 sticky. Kept Linux-only (the sandbox is Linux) and
// gated on >= 2 distinct groups so the expect-seam can chgrp a self-owned dir portably.
const PORTABLE_SKIP =
  process.platform !== "linux"
    ? "sticky-ancestor posture is a Linux concern"
    : typeof process.getuid !== "function" || typeof process.getgroups !== "function"
      ? "requires a POSIX uid/groups"
      : new Set(process.getgroups()).size < 2
        ? "requires >= 2 distinct supplementary groups"
        : false;

describe("Codex per-run HOME is a sticky 3770 provider-root ancestor (portable)", { skip: PORTABLE_SKIP }, () => {
  it("ensureCodexSharedDirectory yields a sticky+setgid 3770 dir a non-owner group member cannot rename/unlink through", async () => {
    const uid = process.getuid!();
    const groups = [...new Set(process.getgroups!())];
    const gA = groups[0]!;
    const gB = groups[1]!;
    const base = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "cdr-portable-sticky-")));
    try {
      const parent = path.join(base, "agent-home");
      await fs.mkdir(parent, { mode: 0o3770 });
      await fs.chown(parent, uid, gA);
      await fs.chmod(parent, 0o3770);
      const providerHome = path.join(parent, "run-1");
      await ensureCodexSharedDirectory(providerHome, { uid, gid: gB });
      await assertStickyProviderAncestor(providerHome, uid, gB);
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });
});
