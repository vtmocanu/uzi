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
});
