import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { createHash, randomUUID } from "node:crypto";

import {
  createJobWorkspace,
  openJobWorkspace,
  jobInputFileName,
  JobInputError,
  reapStaleJobWorkspaces,
  removeJobWorkspace,
  writeJobInputFile,
  writeJobInputs,
  JobFileIntegrityError,
  JOB_INPUT_CEILINGS,
  checkJobInputManifest,
  resolveJobInputCaps,
  validateJobInputFileSpec,
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

  it("under the uid split the tree is group accessible (files 0640), never world, and work opens only after the inputs are written", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID(), true);
    const mode = async (p: string) => (await fsp.stat(p)).mode & 0o7777;
    assert.strictEqual(await mode(ws.root), 0o2750);
    assert.strictEqual(await mode(ws.home), 0o2770);
    assert.strictEqual(await mode(ws.work), 0o2750, "work is not group-writable while inputs are written");
    assert.strictEqual(await mode(ws.inputsDir), 0o2750);
    const [rel] = await writeJobInputs(ws, [{ name: "doc.md", content: "x" }], true);
    assert.strictEqual(await mode(path.join(ws.work, rel!)), 0o640);
    assert.strictEqual(await mode(ws.work), 0o2750, "writing inputs leaves work closed");
    await openJobWorkspace(ws, true);
    assert.strictEqual(await mode(ws.work), 0o2770, "work is widened just before the session");
  });

  it("creates work/outputs for the agent: 2770 under the uid split (group-writable while work is still closed), 0700 otherwise (PRD #1909 M4)", async () => {
    const mode = async (p: string) => (await fsp.stat(p)).mode & 0o7777;
    const split = await createJobWorkspace(await tmpRoot(), randomUUID(), true);
    assert.strictEqual(split.outputsDir, path.join(split.work, "outputs"));
    assert.strictEqual(await mode(split.outputsDir), 0o2770);
    assert.strictEqual(await mode(split.work), 0o2750);
    const single = await createJobWorkspace(await tmpRoot(), randomUUID(), false);
    assert.strictEqual(await mode(single.outputsDir), 0o700);
    assert.deepStrictEqual(await fsp.readdir(single.outputsDir), []);
  });

  it("non-split modes stay 0700/0600 for the worker's own uid", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID(), false);
    assert.strictEqual((await fsp.stat(ws.home)).mode & 0o7777, 0o700);
    const [rel] = await writeJobInputs(ws, [{ name: "doc.md", content: "x" }], false);
    assert.strictEqual((await fsp.stat(path.join(ws.work, rel!))).mode & 0o7777, 0o600);
  });

  it("refuses a symlinked jobs root (create throws, the reaper skips it and never follows)", async () => {
    const jobsRoot = await tmpRoot();
    const victim = path.join(path.dirname(jobsRoot), "victim");
    await fsp.mkdir(victim);
    const stale = path.join(victim, randomUUID());
    await fsp.mkdir(stale);
    await fsp.symlink(victim, jobsRoot);
    await assert.rejects(() => createJobWorkspace(jobsRoot, randomUUID()), /symlink or not a directory/);
    assert.strictEqual(await reapStaleJobWorkspaces(jobsRoot, nullLogger()), 0);
    assert.ok((await fsp.stat(stale)).isDirectory(), "the symlink target was not reaped");
  });
});

describe("uploaded input files (PRD #1909 M3)", () => {
  const bytes = Buffer.from("%PDF-1.7 pretend pdf bytes");
  const digest = createHash("sha256").update(bytes).digest("hex");
  const spec = { name: `${digest}.pdf`, size: bytes.length, sha256: digest };
  async function* chunks(...parts: Buffer[]): AsyncGenerator<Buffer> {
    for (const c of parts) yield c;
  }
  const inputsLeft = async (ws: { inputsDir: string }): Promise<string[]> => (await fsp.readdir(ws.inputsDir)).sort();

  it("streams, verifies and publishes inputs/<storage name> read-only, next to inline inputs", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const inline = await writeJobInputs(ws, [{ name: "doc.md", content: "hi" }]);
    const rel = await writeJobInputFile(ws, spec, chunks(bytes.subarray(0, 5), bytes.subarray(5)));
    assert.strictEqual(rel, `inputs/${digest}.pdf`);
    assert.deepStrictEqual(await inputsLeft(ws), ["01-doc.md", `${digest}.pdf`], "no partial left, no name collision with inline inputs");
    assert.deepStrictEqual(inline, ["inputs/01-doc.md"]);
    const target = path.join(ws.inputsDir, `${digest}.pdf`);
    assert.ok((await fsp.readFile(target)).equals(bytes));
    assert.strictEqual((await fsp.stat(target)).mode & 0o777, 0o400, "read-only single-uid mode");
  });

  it("uses the read-only variant of the uid-split file mode", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID(), true).catch(() => undefined);
    if (!ws) return; // the setgid modes need privileges some CI sandboxes lack
    await writeJobInputFile(ws, spec, chunks(bytes), true);
    assert.strictEqual((await fsp.stat(path.join(ws.inputsDir, spec.name))).mode & 0o777, 0o440);
  });

  it("rejects a digest mismatch and leaves nothing under inputs/", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const tampered = Buffer.from(bytes);
    tampered[3] = tampered[3]! ^ 0xff;
    await assert.rejects(() => writeJobInputFile(ws, spec, chunks(tampered)), JobFileIntegrityError);
    assert.deepStrictEqual(await inputsLeft(ws), []);
  });

  it("rejects more bytes than declared, at once, and fewer bytes than declared", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    let pulled = 0;
    async function* endless(): AsyncGenerator<Buffer> {
      for (;;) {
        pulled++;
        yield Buffer.alloc(bytes.length, 1);
      }
    }
    await assert.rejects(() => writeJobInputFile(ws, spec, endless()), /larger than its declared size/);
    assert.ok(pulled <= 2, "an oversize body is aborted after the first excess chunk, not drained");
    await assert.rejects(() => writeJobInputFile(ws, spec, chunks(bytes.subarray(0, bytes.length - 1))), /shorter than its declared size/);
    assert.deepStrictEqual(await inputsLeft(ws), []);
  });

  it("rejects a stream error mid-body (a torn connection) and cleans up", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    async function* torn(): AsyncGenerator<Buffer> {
      yield bytes.subarray(0, 4);
      throw new Error("terminated");
    }
    await assert.rejects(() => writeJobInputFile(ws, spec, torn()), (err: unknown) => err instanceof JobFileIntegrityError && /terminated/.test(err.message));
    assert.deepStrictEqual(await inputsLeft(ws), []);
  });

  it("a body that ends early is an integrity failure; a local write error is not relabelled as one", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    await assert.rejects(() => writeJobInputFile(ws, spec, chunks(bytes.subarray(0, 4))), JobFileIntegrityError);
    // A null chunk makes our own length read throw: not a stream error, so not an integrity error.
    async function* bad(): AsyncGenerator<unknown> {
      yield null;
    }
    await assert.rejects(
      () => writeJobInputFile(ws, spec, bad() as AsyncIterable<Uint8Array>),
      (err: unknown) => !(err instanceof JobFileIntegrityError),
    );
    assert.deepStrictEqual(await inputsLeft(ws), []);
  });

  it("refuses a declared size over the fixed per-file ceiling before creating any file", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const big = { ...spec, size: JOB_INPUT_CEILINGS.fileBytes + 1 };
    await assert.rejects(() => writeJobInputFile(ws, big, chunks(bytes)), /over the worker's per-file limit/);
    const huge = { ...spec, size: Number.MAX_SAFE_INTEGER };
    await assert.rejects(() => writeJobInputFile(ws, huge, chunks(bytes)), JobInputError);
    assert.deepStrictEqual(await inputsLeft(ws), []);
    assert.doesNotThrow(() => validateJobInputFileSpec(spec));
  });

  for (const bad of [
    "../etc/passwd",
    "01-doc.md",
    `${"a".repeat(63)}.pdf`,
    `${"A".repeat(64)}.pdf`,
    `${"a".repeat(64)}.html`,
    `${"a".repeat(64)}.pdf/x`,
    `${"a".repeat(64)}.exe`,
    "",
  ]) {
    it(`rejects the storage name ${JSON.stringify(bad)}`, async () => {
      const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
      await assert.rejects(() => writeJobInputFile(ws, { name: bad, size: 3, sha256: "a".repeat(64) }, chunks(Buffer.from("abc"))), JobInputError);
      assert.deepStrictEqual(await inputsLeft(ws), []);
    });
  }

  it("rejects a digest that is not the storage name's, and a non-positive size", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    await assert.rejects(() => writeJobInputFile(ws, { ...spec, sha256: "b".repeat(64) }, chunks(bytes)), JobInputError);
    await assert.rejects(() => writeJobInputFile(ws, { ...spec, size: 0 }, chunks(bytes)), JobInputError);
  });

  it("refuses a symlink planted at the partial path (O_EXCL|O_NOFOLLOW) without writing through it", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const victim = path.join(path.dirname(ws.root), "victim.txt");
    await fsp.writeFile(victim, "untouched");
    await fsp.symlink(victim, path.join(ws.inputsDir, ".partial-abc123"));
    await assert.rejects(() => writeJobInputFile(ws, spec, chunks(bytes), false, "abc123"), { code: "EEXIST" });
    assert.strictEqual(await fsp.readFile(victim, "utf8"), "untouched");
  });

  it("refuses a symlink planted at the final target and never follows or replaces it", async () => {
    const ws = await createJobWorkspace(await tmpRoot(), randomUUID());
    const victim = path.join(path.dirname(ws.root), "victim.txt");
    await fsp.writeFile(victim, "untouched");
    await fsp.symlink(victim, path.join(ws.inputsDir, spec.name));
    await assert.rejects(() => writeJobInputFile(ws, spec, chunks(bytes)), JobInputError);
    assert.strictEqual(await fsp.readFile(victim, "utf8"), "untouched");
    assert.ok((await fsp.lstat(path.join(ws.inputsDir, spec.name))).isSymbolicLink());
    assert.deepStrictEqual(await inputsLeft(ws), [spec.name], "the partial is removed");
  });
});

describe("job input caps (PRD #1909 D1, worker-side)", () => {
  const f = (name: string, size: number) => ({ name, size });
  it("uses the fixed ceilings when the claim carries no caps, and clamps any cap above them", () => {
    assert.deepStrictEqual(resolveJobInputCaps(undefined), { fileBytes: JOB_INPUT_CEILINGS.fileBytes, files: JOB_INPUT_CEILINGS.files, totalBytes: JOB_INPUT_CEILINGS.totalBytes });
    assert.deepStrictEqual(resolveJobInputCaps({ job_input_file_max_bytes: 5, job_inputs_max_files: 2, job_inputs_max_bytes: 9 }), { fileBytes: 5, files: 2, totalBytes: 9 });
    const hostile = resolveJobInputCaps({ job_input_file_max_bytes: Number.MAX_SAFE_INTEGER, job_inputs_max_files: 1e9, job_inputs_max_bytes: Number.MAX_SAFE_INTEGER });
    assert.deepStrictEqual(hostile, { fileBytes: JOB_INPUT_CEILINGS.fileBytes, files: JOB_INPUT_CEILINGS.files, totalBytes: JOB_INPUT_CEILINGS.totalBytes });
    assert.deepStrictEqual(resolveJobInputCaps({ job_input_file_max_bytes: -1, job_inputs_max_files: 0, job_inputs_max_bytes: 1.5 }), resolveJobInputCaps(undefined));
  });

  it("refuses too many entries, an oversize file and an oversize total, each with its own reason", () => {
    const caps = { fileBytes: 10, files: 2, totalBytes: 15 };
    assert.doesNotThrow(() => checkJobInputManifest([f("a", 10), f("b", 5)], caps));
    assert.throws(() => checkJobInputManifest([f("a", 1), f("b", 1), f("c", 1)], caps), /3 input files.*limit of 2/);
    assert.throws(() => checkJobInputManifest([f("a", 11)], caps), /per-file limit of 10/);
    assert.throws(() => checkJobInputManifest([f("a", 10), f("b", 6)], caps), /total 16 bytes.*15/);
    // A repeated storage name is fetched once, so it counts once towards the total.
    assert.doesNotThrow(() => checkJobInputManifest([f("a", 10), f("a", 10)], { ...caps, totalBytes: 10 }));
  });
});
