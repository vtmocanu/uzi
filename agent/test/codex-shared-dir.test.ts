// Issue #1492 — regression for the create-only group repair in `ensureCodexSharedDirectory`.
//
// On a non-root worker start (hosted k8s, runAsUser:10001, PRD #58 single-uid), the `/data`
// PVC gets `fsGroup:10001`, so the setgid `agent-home` is group-owned `worker` (10001) and
// every freshly-created per-run home and advice root inherits gid `worker`. Before the fix
// `ensureCodexSharedDirectory` threw `Codex shared data directory has an unexpected owner or
// group` on `before.gid !== RUNNER_UID`, before any Codex/OpenAI call. The fix chgrps a
// FRESHLY-created, verified-worker-owned dir to RUNNER_UID (the worker owns it and is a
// supplementary member of `runner`), while leaving EEXISTing directories validation-only
// (the deliberate check-don't-repair stance) and keeping the O_NOFOLLOW symlink guard.
//
// Group A drives the helper directly via its `expect` test-injection seam using the test
// process's OWN uid and two of its OWN groups, so it runs under any CI uid. Group B drives
// the two production call sites (run-home via `prepareCodexRunHome`, advice roots via
// `makeProductionLaunchAdviceRoot`) with the REAL WORKER_UID/RUNNER_UID constants, and is
// gated on running as WORKER_UID with WORKER+RUNNER group membership (true in the uzi worker
// gate and the real image; skipped elsewhere).

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import {
  ensureCodexSharedDirectory,
  prepareCodexRunHome,
  makeProductionLaunchAdviceRoot,
} from "../src/codex/codex-executor.js";
import { WORKER_UID, RUNNER_UID } from "../src/runner-uid.js";
import { CodexUnsupportedProfileError } from "../src/codex/launcher.js";
import type { CodexAdviceLaunchSpec } from "../src/codex/codex-advice-harness.js";

const MODE_MASK = 0o7777;

async function statOf(p: string): Promise<{ uid: number; gid: number; mode: number }> {
  const st = await fs.lstat(p);
  return { uid: st.uid, gid: st.gid, mode: st.mode & MODE_MASK };
}

/** The (dev, ino) identity of `p` without following a final symlink, used to prove the
 *  destructive recovery RECREATED the live path (inode changed) rather than adopting it. */
async function inodeOf(p: string): Promise<{ dev: number; ino: number }> {
  const st = await fs.lstat(p);
  return { dev: st.dev, ino: st.ino };
}

/** A fresh, canonical (realpath'd) temp directory the caller owns; cleaned up by the caller. */
async function freshTmp(): Promise<string> {
  return fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "codex-shared-dir-")));
}

/** Make `dir` a setgid dir group-owned by `gid` (mode 2770): the hosted-k8s setgid-parent
 *  shape a fresh child inherits its group from. Requires the process to be a member of `gid`. */
async function makeSetgidParent(dir: string, uid: number, gid: number): Promise<void> {
  await fs.mkdir(dir, { mode: 0o2770 });
  await fs.chown(dir, uid, gid);
  await fs.chmod(dir, 0o2770);
}

// ─── Group A: portable, any uid ─────────────────────────────────────────────────
const uidA = typeof process.getuid === "function" ? process.getuid() : undefined;
// Distinct groups the process is a member of, so it can chgrp a self-owned dir to either.
const memberGroups = typeof process.getgroups === "function" ? [...new Set(process.getgroups())] : [];
const GROUP_A_SKIP =
  uidA === undefined || memberGroups.length < 2
    ? "requires a POSIX uid and >= 2 distinct supplementary groups"
    : false;

describe("Issue #1492: ensureCodexSharedDirectory create-only group repair (portable seam)", { skip: GROUP_A_SKIP }, () => {
  const uid = uidA as number;
  const gA = memberGroups[0]!;
  const gB = memberGroups[1]!;

  it("repairs a FRESH dir under a setgid gid-A parent to gid B (create-only chgrp)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA); // setgid, group A: a fresh child inherits gid A
      const child = path.join(parent, "run-1");
      await ensureCodexSharedDirectory(child, { uid, gid: gB });
      const st = await statOf(child);
      assert.equal(st.uid, uid, "owner is preserved");
      assert.equal(st.gid, gB, "the freshly-created dir was chgrp'd to the expected gid");
      assert.equal(st.mode, 0o2770, "mode is 2770 (setgid + rwxrwx---)");
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("is a no-op pass when a FRESH dir already inherits the expected gid", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gB); // setgid, group B: child already inherits gid B
      const child = path.join(parent, "run-1");
      await ensureCodexSharedDirectory(child, { uid, gid: gB });
      const st = await statOf(child);
      assert.equal(st.uid, uid);
      assert.equal(st.gid, gB, "the correct-gid dir is left as-is");
      assert.equal(st.mode, 0o2770);
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("REJECTS a PRE-EXISTING wrong-gid dir (never repairs an EEXISTing directory)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA);
      const child = path.join(parent, "run-1");
      // Create the child OUTSIDE the helper so this call does not "create" it: it inherits
      // gid A from the setgid parent and must be REJECTED, not repaired.
      await fs.mkdir(child, { mode: 0o2770 });
      await assert.rejects(
        ensureCodexSharedDirectory(child, { uid, gid: gB }),
        /unexpected owner or group/,
        "a pre-existing wrong-gid dir is rejected, preserving the check-don't-repair stance",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("REJECTS a final-component symlink (O_NOFOLLOW)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gB);
      const real = path.join(parent, "real");
      await fs.mkdir(real, { mode: 0o2770 });
      const link = path.join(parent, "link");
      await fs.symlink(real, link);
      await assert.rejects(
        ensureCodexSharedDirectory(link, { uid, gid: gB }),
        "a final-component symlink is refused by the O_NOFOLLOW open",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  // ─── issue #1495 m2: opt-in destructive recovery of a pre-existing worker-group parent ───

  it("opt-in RECREATES a pre-existing worker-owned wrong-gid dir (removal+recreation, not chgrp)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA); // setgid, group A: a fresh child inherits gid A
      const child = path.join(parent, "codex-advice-data");
      // Create the child OUTSIDE the helper so this call does NOT "create" it: it inherits gid A
      // (the "wrong" disposable gid) and owner = test uid, i.e. the pre-rc.2 worker:worker parent.
      await fs.mkdir(child, { mode: 0o2770 });
      const originalInode = await inodeOf(child);

      await ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA });

      const st = await statOf(child);
      assert.equal(st.uid, uid, "owner is the worker");
      assert.equal(st.gid, gB, "the recreated dir is grouped the expected gid (repaired after fresh create)");
      assert.equal(st.mode, 0o2770, "mode is 2770 (setgid + rwxrwx---)");
      const newInode = await inodeOf(child);
      assert.notEqual(
        `${newInode.dev}:${newInode.ino}`,
        `${originalInode.dev}:${originalInode.ino}`,
        "the LIVE-path inode CHANGED — the original was renamed out and a fresh dir created, never chgrp'd/adopted",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("opt-in NEVER adopts pre-existing content (a marker file does not survive into the live path)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA);
      const child = path.join(parent, "codex-advice-cwd");
      await fs.mkdir(child, { mode: 0o2770 });
      const marker = path.join(child, "leftover.txt");
      await fs.writeFile(marker, "pre-existing content that must NOT be adopted");

      await ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA });

      // The content was renamed OUT with the old inode and discarded; the LIVE path is fresh.
      // (Removal of the tombstone is best-effort, so we assert only that the marker is absent
      // from the LIVE path, not that the tombstone is gone.)
      await assert.rejects(
        fs.access(marker),
        "the pre-existing marker is absent from the recreated live path (content never adopted)",
      );
      const entries = await fs.readdir(child);
      assert.equal(entries.length, 0, "the recreated live path is empty (fresh dir, no adopted content)");
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("opt-in cleanup roots the quarantine under <dataDir>, leaving no tomb/quarantine in the runner-writable parent (issue #1495 TOCTOU)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home"); // the runner-group-writable parent under uid-split
      await makeSetgidParent(parent, uid, gA);
      const child = path.join(parent, "codex-advice-data");
      await fs.mkdir(child, { mode: 0o2770 });

      await ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA });

      // The transient tombstone must NOT be recursively removed in the runner-writable parent: it
      // is relocated into a 0700 quarantine rooted at <dataDir> (here `base`) and removed there.
      const parentEntries = await fs.readdir(parent);
      assert.deepEqual(
        parentEntries.filter((e) => e.includes(".uzi-tomb-")),
        [],
        "no tombstone left in the runner-writable parent (agent-home)",
      );
      // On the happy path the private quarantine subtree is fully removed from <dataDir>.
      const dataRootEntries = await fs.readdir(base);
      assert.deepEqual(
        dataRootEntries.filter((e) => e.includes(".uzi-quarantine-")),
        [],
        "the private quarantine under <dataDir> is removed on the happy path",
      );
      assert.equal((await statOf(child)).gid, gB, "the recreated live path is grouped the expected gid");
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("opt-in still REJECTS a dir whose gid is NOT the recoverGid (recovery does not fire)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA); // child inherits gid A
      const child = path.join(parent, "codex-advice-data");
      await fs.mkdir(child, { mode: 0o2770 });
      // recoverGid gB, but the pre-existing dir is gid A ⇒ gA !== recoverGid ⇒ recovery is NOT
      // triggered, and the strict gid check rejects the EEXISTing mismatch as before.
      await assert.rejects(
        ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gB }),
        /unexpected owner or group/,
        "an EEXISTing dir whose gid is not the recoverGid is rejected, not recovered",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("opt-in still REJECTS a dir whose OWNER is not expect.uid (recovery does not fire, dir left in place)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA); // child inherits gid A, owner = this test uid
      const child = path.join(parent, "codex-advice-data");
      await fs.mkdir(child, { mode: 0o2770 });
      const originalInode = await inodeOf(child);
      // expect.uid is a DIFFERENT uid than the dir's actual owner (this process). The recovery
      // guard requires `before.uid === expect.uid`, so it must NOT fire on a foreign-owned dir —
      // the deliberate "never adopt a dir we don't own" stance — and the strict owner check rejects.
      const foreignUid = uid + 1;
      await assert.rejects(
        ensureCodexSharedDirectory(child, { uid: foreignUid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA }),
        /unexpected owner or group/,
        "an EEXISTing dir owned by a uid other than expect.uid is rejected, not recovered",
      );
      // Discriminating assertion: recovery must NOT have renamed/recreated it — the ORIGINAL inode
      // is still at the live path (a buggy recovery-fires-on-wrong-owner would change it).
      const afterInode = await inodeOf(child);
      assert.equal(
        `${afterInode.dev}:${afterInode.ino}`,
        `${originalInode.dev}:${originalInode.ino}`,
        "the foreign-owned dir was left in place (never renamed out), proving recovery did not fire",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("opt-in still REJECTS a final-component symlink (O_NOFOLLOW)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA);
      const real = path.join(parent, "real");
      await fs.mkdir(real, { mode: 0o2770 });
      const link = path.join(parent, "codex-advice-data");
      await fs.symlink(real, link);
      await assert.rejects(
        ensureCodexSharedDirectory(link, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA }),
        "a final-component symlink is refused by the O_NOFOLLOW open even under the opt-in",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("concurrent opt-in recovery is serialized and safe (two callers, one live child, no tombstone leak)", async () => {
    const base = await freshTmp();
    try {
      const parent = path.join(base, "agent-home");
      await makeSetgidParent(parent, uid, gA);
      const child = path.join(parent, "codex-advice-data");
      await fs.mkdir(child, { mode: 0o2770 }); // pre-existing worker:gA, the "wrong" gid

      // Two overlapping opt-in calls on the SAME path. A NON-serialized impl risks the second
      // caller re-renaming the first's freshly-recreated (healthy worker:gB) dir out of the live
      // path; per-path serialization makes the second RE-READ state and no-op through to strict
      // validation instead.
      await Promise.all([
        ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA }),
        ensureCodexSharedDirectory(child, { uid, gid: gB }, { recreateDisposableWorkerGroupDir: true, recoverGid: gA }),
      ]);

      const st = await statOf(child);
      assert.equal(st.uid, uid, "owner is the worker");
      assert.equal(st.gid, gB, "final dir is grouped the expected gid");
      assert.equal(st.mode, 0o2770, "final dir mode is 2770");

      const entries = await fs.readdir(parent);
      const liveChildren = entries.filter((e) => e === "codex-advice-data");
      assert.equal(liveChildren.length, 1, "exactly one live child directory named as expected");
      const tombstones = entries.filter((e) => e.includes(".uzi-tomb-"));
      assert.equal(tombstones.length, 0, "no leftover tombstone sibling on the happy path");
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });
});

// ─── Group B: production paths with the REAL constants ───────────────────────────
// Group B's fixtures chgrp a parent group to WORKER_UID (10001), which needs the test
// process to be a member of gid WORKER_UID; and the production helper repairs to RUNNER_UID,
// which needs membership in gid RUNNER_UID. Gate on both plus running AS WORKER_UID.
const GROUP_B_SKIP =
  uidA === WORKER_UID && memberGroups.includes(WORKER_UID) && memberGroups.includes(RUNNER_UID)
    ? false
    : "requires running as WORKER_UID with WORKER_UID + RUNNER_UID group membership";

describe("Issue #1492: production paths repair worker-inherited dirs to RUNNER_UID", { skip: GROUP_B_SKIP }, () => {
  it("run-home: prepareCodexRunHome repairs the per-run home and codex-data to gid RUNNER_UID", async () => {
    const base = await freshTmp();
    try {
      // Simulate the fsGroup:10001 agent-home: setgid, group-owned worker (10001).
      const agentHome = path.join(base, "agent-home");
      await makeSetgidParent(agentHome, WORKER_UID, WORKER_UID);
      const runHome = path.join(agentHome, "run-abc"); // == sdkHomeRoot/<runId>
      await prepareCodexRunHome(runHome);

      const home = await statOf(runHome);
      assert.equal(home.uid, WORKER_UID);
      assert.equal(home.gid, RUNNER_UID, "the per-run home was repaired from gid worker to gid runner");
      assert.equal(home.mode, 0o2770);

      const codexData = await statOf(path.join(runHome, "codex-data"));
      assert.equal(codexData.uid, WORKER_UID);
      assert.equal(codexData.gid, RUNNER_UID, "codex-data is runner-group-owned");
      assert.equal(codexData.mode, 0o2770);
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("run-home: a per-epoch REVALIDATION is a validating no-op, but REJECTS a tampered gid (issue #1495 m1)", async () => {
    const base = await freshTmp();
    try {
      // Simulate the fsGroup:10001 agent-home: setgid, group-owned worker (10001).
      const agentHome = path.join(base, "agent-home");
      await makeSetgidParent(agentHome, WORKER_UID, WORKER_UID);
      const runHome = path.join(agentHome, "run-reval"); // == sdkHomeRoot/<runId>

      // Initialize once (run()'s pre-provisioning INITIALIZATION), then REVALIDATE (each provider
      // epoch's startProviderEpoch re-runs prepareCodexRunHome). The second call hits created=false
      // and VALIDATES the already-correct worker:runner 2770 — a no-op, never a re-adopt.
      await prepareCodexRunHome(runHome);
      await prepareCodexRunHome(runHome);
      const home = await statOf(runHome);
      assert.equal(home.uid, WORKER_UID);
      assert.equal(home.gid, RUNNER_UID, "revalidation leaves the per-run home worker:runner");
      assert.equal(home.mode, 0o2770);
      const codexData = await statOf(path.join(runHome, "codex-data"));
      assert.equal(codexData.gid, RUNNER_UID, "revalidation leaves codex-data runner-group-owned");

      // Tamper: chgrp the per-run home to the WRONG gid (worker). The next REVALIDATION must NOT
      // adopt it — the create-only repair fires ONLY on a dir this call just created (created=false
      // here), so the deliberate check-don't-repair stance rejects a tampered, EEXISTing dir.
      await fs.chown(runHome, WORKER_UID, WORKER_UID);
      await assert.rejects(
        prepareCodexRunHome(runHome),
        /unexpected owner or group/,
        "a tampered (wrong-gid) per-run home is REJECTED by revalidation, never silently adopted",
      );
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("advice roots: makeProductionLaunchAdviceRoot repairs codex-advice-data/-cwd to gid RUNNER_UID", async () => {
    const base = await freshTmp();
    try {
      // homeRoot is the worker's shared SDK home (sdkHomeRoot). Simulate it as a setgid dir
      // group-owned worker (10001) and NOT pre-repaired, so the advice children genuinely
      // inherit gid worker and the repair is exercised.
      const homeRoot = path.join(base, "agent-home");
      await makeSetgidParent(homeRoot, WORKER_UID, WORKER_UID);

      const seam = makeProductionLaunchAdviceRoot(homeRoot, "subscription");
      // A type-valid spec whose provider.envKey is INTENTIONALLY invalid. This does not rely
      // on UZI_UID_SPLIT being unset: the seam creates + repairs the advice dirs BEFORE
      // launchCodexRoot, and the invalid envKey forces an early launch failure in BOTH modes —
      // single-uid throws CodexUnsupportedProfileError, split fails validateLaunchContract on
      // the envKey allowlist BEFORE uid resolution, tree creation or spawn.
      const spec: CodexAdviceLaunchSpec = {
        kind: "advice",
        label: "judge",
        provider: { name: "openai", baseUrl: "https://api.openai.com/v1", envKey: "invalid-env-key", model: "gpt-x" },
        model: "gpt-x",
      };

      let outcome: "resolved" | Error;
      try {
        const handle = await seam(spec);
        await handle.dispose().catch(() => undefined); // guard: never leave a real root live
        outcome = "resolved";
      } catch (e) {
        outcome = e instanceof Error ? e : new Error(String(e));
      }
      assert.notEqual(outcome, "resolved", "the advice seam must NOT resolve to a real provider launch");
      const err = outcome as Error;
      assert.ok(
        err instanceof CodexUnsupportedProfileError || /provider envKey is invalid or reserved/.test(err.message),
        `unexpected advice-launch failure class: ${err.message}`,
      );

      for (const name of ["codex-advice-data", "codex-advice-cwd"]) {
        const st = await statOf(path.join(homeRoot, name));
        assert.equal(st.uid, WORKER_UID, `${name} owner is worker`);
        assert.equal(st.gid, RUNNER_UID, `${name} was repaired from gid worker to gid runner`);
        assert.equal(st.mode, 0o2770, `${name} mode is 2770`);
      }
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("advice roots: PRE-EXISTING worker:worker parents are RECOVERED (renamed out + recreated worker:runner) — issue #1495 m2", async () => {
    const base = await freshTmp();
    try {
      // homeRoot is the worker's shared SDK home (sdkHomeRoot), setgid group-owned worker (10001).
      const homeRoot = path.join(base, "agent-home");
      await makeSetgidParent(homeRoot, WORKER_UID, WORKER_UID);

      // Pre-create the two advice PARENTS BEFORE the seam runs, so they EEXIST as worker:worker
      // (gid inherited from the setgid worker homeRoot) — the pre-rc.2 parents that survive a
      // worker roll on the RWO PVC. The seam must RECOVER them, not throw.
      const dataParent = path.join(homeRoot, "codex-advice-data");
      const cwdParent = path.join(homeRoot, "codex-advice-cwd");
      await fs.mkdir(dataParent, { mode: 0o2770 });
      await fs.mkdir(cwdParent, { mode: 0o2770 });
      const originalInodes = {
        "codex-advice-data": await inodeOf(dataParent),
        "codex-advice-cwd": await inodeOf(cwdParent),
      } as const;
      // Confirm the pre-condition: both are worker:worker (the "wrong" disposable gid).
      for (const name of ["codex-advice-data", "codex-advice-cwd"]) {
        const st = await statOf(path.join(homeRoot, name));
        assert.equal(st.gid, WORKER_UID, `${name} starts group-owned worker (pre-rc.2 shape)`);
      }

      const seam = makeProductionLaunchAdviceRoot(homeRoot, "subscription");
      // Same invalid-envKey spec as the advice repair test: the seam creates/recovers the advice
      // parents BEFORE launchCodexRoot, and the invalid envKey forces an early launch failure in
      // BOTH modes (single-uid CodexUnsupportedProfileError, split fails the envKey allowlist).
      const spec: CodexAdviceLaunchSpec = {
        kind: "advice",
        label: "judge",
        provider: { name: "openai", baseUrl: "https://api.openai.com/v1", envKey: "invalid-env-key", model: "gpt-x" },
        model: "gpt-x",
      };

      let outcome: "resolved" | Error;
      try {
        const handle = await seam(spec);
        await handle.dispose().catch(() => undefined); // guard: never leave a real root live
        outcome = "resolved";
      } catch (e) {
        outcome = e instanceof Error ? e : new Error(String(e));
      }
      assert.notEqual(outcome, "resolved", "the advice seam must NOT resolve to a real provider launch");
      const err = outcome as Error;
      assert.ok(
        err instanceof CodexUnsupportedProfileError || /provider envKey is invalid or reserved/.test(err.message),
        `unexpected advice-launch failure class: ${err.message}`,
      );

      for (const name of ["codex-advice-data", "codex-advice-cwd"] as const) {
        const p = path.join(homeRoot, name);
        const st = await statOf(p);
        assert.equal(st.uid, WORKER_UID, `${name} owner is worker`);
        assert.equal(st.gid, RUNNER_UID, `${name} was recovered from gid worker to gid runner`);
        assert.equal(st.mode, 0o2770, `${name} mode is 2770`);
        const newInode = await inodeOf(p);
        assert.notEqual(
          `${newInode.dev}:${newInode.ino}`,
          `${originalInodes[name].dev}:${originalInodes[name].ino}`,
          `${name} LIVE-path inode CHANGED — the pre-existing parent was renamed out and recreated fresh`,
        );
      }
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });

  it("run-home: a PRE-EXISTING worker:worker run HOME stays REJECTED (no opt-in; protects resume state) — issue #1495 m2", async () => {
    const base = await freshTmp();
    try {
      // Simulate the fsGroup:10001 agent-home: setgid, group-owned worker (10001).
      const agentHome = path.join(base, "agent-home");
      await makeSetgidParent(agentHome, WORKER_UID, WORKER_UID);
      const runHome = path.join(agentHome, "run-preexisting"); // == sdkHomeRoot/<runId>
      // Pre-create the run HOME as worker:worker (gid inherited), as if left by a prior worker.
      // Unlike the two disposable advice parents, prepareCodexRunHome does NOT opt into recovery
      // (the run HOME holds codex-session-store resume state), so it MUST reject, never recreate.
      await fs.mkdir(runHome, { mode: 0o2770 });
      await assert.rejects(
        prepareCodexRunHome(runHome),
        /unexpected owner or group/,
        "a pre-existing wrong-gid run HOME is REJECTED, never destructively recovered",
      );
      // The tamper variant (revalidation after a chgrp) is additionally covered by the m1
      // revalidation test above.
    } finally {
      await fs.rm(base, { recursive: true, force: true });
    }
  });
});
