import { after, describe, it } from "node:test";
import fs from "node:fs";
import { fileURLToPath } from "node:url";

// Required inventory must reject this skip: only the production worker with both distinct
// untrusted identities can make entries the runner uid cannot read.
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== 10001 ||
  process.env.UZI_UID_SPLIT !== "1"
  ? "requires Linux worker UID 10001 with production runner 10002/command 10003 split"
  : false;

describe("stopped-source legacy proof with entries the runner cannot read", { skip: INIT_SKIP }, async () => {
  const scratchPath = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
  fs.mkdirSync(scratchPath, { recursive: true });
  const scratch = fs.realpathSync(scratchPath);
  const scratchBefore = fs.statSync(scratch);
  // Grant only runner-group traversal, not listing; each fixture owns its own permissions and the
  // original scratch metadata is restored.
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
  // Import only after TMPDIR points inside the worktree: fixtures stay in scratch.
  const fixture = await import("./stopped-source-unreadable-uid-fixture.js");

  it("is not covered for a nonignored untracked directory the runner cannot read", async (t) => {
    await fixture.untrackedUnreadable(t);
  });
  it("is not covered for a modified tracked file inside a tracked directory the runner cannot read", async () => {
    await fixture.trackedUnreadable();
  });
  it("is not covered for a nonignored directory private to the command uid", async () => {
    await fixture.commandPrivateDirectory();
  });
  it("is not covered for a runner-unreadable loose ref naming a commit only the clone holds", async () => {
    await fixture.looseRefUnreadable();
  });
  it("stays covered for a stat-dirty tracked file whose content is unchanged", async () => {
    await fixture.statDirtyControl();
  });
  it("stays covered for an unreadable directory matched by a committed gitignore", async () => {
    await fixture.gitignoredControl();
  });
  it("stays covered for an unreadable directory matched by the clone info exclude", async () => {
    await fixture.excludedControl();
  });
  it("retains conservatively when Git warns about a benign deprecated config key", async () => {
    await fixture.deprecatedKeyWarning();
  });
});
