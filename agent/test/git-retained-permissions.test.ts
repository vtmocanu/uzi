import { after, describe, it } from "node:test";
import fs from "node:fs";
import { fileURLToPath } from "node:url";

// Required inventory must reject this skip: only the production worker with both distinct
// untrusted identities and the image supervisor can supply recovery permission evidence.
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== 10001 ||
  process.env.UZI_UID_SPLIT !== "1"
  ? "requires Linux worker UID 10001 with production runner 10002/command 10003 split"
  : false;

describe("retained recovery object permissions across real UIDs", { skip: INIT_SKIP }, async () => {
  const scratchPath = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
  fs.mkdirSync(scratchPath, { recursive: true });
  const scratch = fs.realpathSync(scratchPath);
  const scratchBefore = fs.statSync(scratch);
  // The lane mounts private worker scratch. Grant only runner-group traversal, not listing;
  // each fixture owns its own permissions, and the original scratch metadata is restored.
  fs.chownSync(scratch, -1, 10002);
  fs.chmodSync(scratch, 0o710);
  const oldTmp = process.env.TMPDIR;
  const oldUmask = process.umask(0o002);
  process.env.TMPDIR = scratch;
  after(() => {
    fs.chmodSync(scratch, scratchBefore.mode & 0o7777);
    fs.chownSync(scratch, -1, scratchBefore.gid);
    process.umask(oldUmask);
    if (oldTmp === undefined) delete process.env.TMPDIR;
    else process.env.TMPDIR = oldTmp;
  });
  // Import only after TMPDIR points inside the worktree: harness/proc fixtures also stay here.
  const { installRetainedPermissionsHarness, retainedPermissionsCase } = await import("./retained-permissions-fixture.js");
  installRetainedPermissionsHarness();

  it('recovers retained command-owned loose objects', async () => {
    await retainedPermissionsCase();
  });
  it('recovers retained command-owned packed objects', async () => {
    await retainedPermissionsCase({ packed: true });
  });
  it('normalizes before verifying an already captured restore tip', async () => {
    await retainedPermissionsCase({ resumed: true });
  });
  it('preserves worker-owned hardlinks while recovering command-owned loose objects', async () => {
    await retainedPermissionsCase({ mixed: "worker" });
  });
  it('preserves worker-owned hardlinks while recovering command-owned packed objects', async () => {
    await retainedPermissionsCase({ mixed: "worker", packed: true });
  });
  it('preserves runner-owned hardlinks while recovering command-owned loose objects', async () => {
    await retainedPermissionsCase({ mixed: "runner" });
  });
  it('preserves runner-owned hardlinks while recovering command-owned packed objects', async () => {
    await retainedPermissionsCase({ mixed: "runner", packed: true });
  });
  it('refuses command-owned multiply linked mutation candidates and keeps custody', async () => {
    await retainedPermissionsCase({ linked: true });
  });
  it('keeps source and custody when retained object recovery fails', async () => {
    await retainedPermissionsCase({ corrupt: true });
  });
});
