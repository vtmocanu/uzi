import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";

import {
  createJobWorkspace,
  jobInputFileName,
  JobInputError,
  reapStaleJobWorkspaces,
  removeJobWorkspace,
  writeJobInputs,
} from "../src/job-workspace.js";
import { nullLogger } from "./helpers.js";

const roots: string[] = [];
async function tmpRoot(): Promise<string> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-jobws-"));
  roots.push(d);
  return path.join(d, "jobs");
}
afterEach(async () => {
  while (roots.length) await fsp.rm(roots.pop()!, { recursive: true, force: true });
});

describe("job workspace (PRD #1908 M4)", () => {
  it("creates <jobsRoot>/<run_id> with mode 0700 and the home/work/inputs subtree", async () => {
    const jobsRoot = await tmpRoot();
    const runId = randomUUID();
    const ws = await createJobWorkspace(jobsRoot, runId);
    assert.strictEqual(ws.root, path.join(jobsRoot, runId));
    assert.strictEqual((await fsp.stat(ws.root)).mode & 0o777, 0o700);
    assert.ok((await fsp.stat(ws.inputsDir)).isDirectory());
    assert.ok((await fsp.stat(ws.home)).isDirectory());
    // The SDK HOME is a sibling of the file jail, never inside it.
    assert.ok(!ws.home.startsWith(ws.work + path.sep));
  });

  it("refuses a run id that is not a UUID (no path traversal through the run id)", async () => {
    const jobsRoot = await tmpRoot();
    await assert.rejects(() => createJobWorkspace(jobsRoot, "../escape"), /UUID/);
  });

  it("writes inputs to inputs/NN-<name> with the content", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const files = await writeJobInputs(ws, [
      { name: "doc.md", content: "hello" },
      { name: "b_2.txt", content: "world" },
    ]);
    assert.deepStrictEqual(files, ["inputs/01-doc.md", "inputs/02-b_2.txt"]);
    assert.strictEqual(await fsp.readFile(path.join(ws.work, "inputs/01-doc.md"), "utf8"), "hello");
    assert.strictEqual(await fsp.readFile(path.join(ws.work, "inputs/02-b_2.txt"), "utf8"), "world");
  });

  for (const bad of ["../x", "/etc/x", "a/b", "..", ".hidden", "a..b", "", "x".repeat(101), "a\\b", "a\0b"]) {
    it(`refuses the input name ${JSON.stringify(bad.length > 20 ? bad.slice(0, 10) + "..." : bad)}`, async () => {
      assert.throws(() => jobInputFileName(0, bad), JobInputError);
      const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
      await assert.rejects(() => writeJobInputs(ws, [{ name: bad, content: "x" }]), JobInputError);
      assert.deepStrictEqual(await fsp.readdir(ws.inputsDir), [], "nothing was written for a refused name");
    });
  }

  it("does not write through a pre-planted symlink (O_EXCL|O_NOFOLLOW)", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const outside = path.join(path.dirname(ws.root), "outside.txt");
    await fsp.writeFile(outside, "original");
    await fsp.symlink(outside, path.join(ws.inputsDir, "01-doc.md"));
    await assert.rejects(() => writeJobInputs(ws, [{ name: "doc.md", content: "pwned" }]));
    assert.strictEqual(await fsp.readFile(outside, "utf8"), "original");
  });

  it("removeJobWorkspace deletes the whole tree", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    await writeJobInputs(ws, [{ name: "doc.md", content: "x" }]);
    await removeJobWorkspace(ws, nullLogger());
    await assert.rejects(() => fsp.stat(ws.root), { code: "ENOENT" });
  });

  it("the startup reaper removes run-id-named leftovers and leaves other entries alone", async () => {
    const jobsRoot = await tmpRoot();
    const a = await createJobWorkspace(jobsRoot, randomUUID());
    const b = await createJobWorkspace(jobsRoot, randomUUID());
    await writeJobInputs(a, [{ name: "doc.md", content: "x" }]);
    await fsp.mkdir(path.join(jobsRoot, "not-a-run"));
    const target = path.join(path.dirname(jobsRoot), "keep");
    await fsp.mkdir(target);
    await fsp.symlink(target, path.join(jobsRoot, randomUUID()));
    const removed = await reapStaleJobWorkspaces(jobsRoot, nullLogger());
    assert.strictEqual(removed, 3, "two workspaces and the symlink entry");
    await assert.rejects(() => fsp.stat(a.root), { code: "ENOENT" });
    await assert.rejects(() => fsp.stat(b.root), { code: "ENOENT" });
    assert.deepStrictEqual(await fsp.readdir(jobsRoot), ["not-a-run"]);
    assert.ok((await fsp.stat(target)).isDirectory(), "a symlink entry is unlinked, never followed");
  });

  it("the reaper is a no-op when the jobs root does not exist", async () => {
    const jobsRoot = await tmpRoot();
    assert.strictEqual(await reapStaleJobWorkspaces(jobsRoot, nullLogger()), 0);
  });
});
