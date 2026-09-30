// The per-run job workspace (PRD #1908, Decision 5): a scratch directory tree the repo-less `job`
// run kind works in, at `<worker data root>/jobs/<run_id>`. It lives OUTSIDE the clone
// (`runner/`) and attempt roots, is created at claim with mode 0700 (single-uid) or the setgid codex-session group modes below
// (uid split), and is removed at terminal.
//
//   <jobsRoot>/<run_id>/            0700 (uid split: 2750), the whole tree removed at terminal
//     home/                         the SDK HOME (transcripts); NOT inside the agent's file jail
//     work/                         the SDK cwd and the path-guard root
//       inputs/NN-<name>            the caller's named input documents
//       inputs/<sha256>.<ext>       an uploaded input file (PRD #1909), read-only, content-named
//
// Under the PRD #51 uid split (UZI_UID_SPLIT=1) the SDK CLI runs as the `runner` uid while this
// worker process creates the tree, so 0700 would lock the CLI out of its own HOME, cwd and
// inputs. There the jobs root is a `codex-session` group carve-out (entrypoint.sh: worker:codex-session
// 3710 (group traverse only: the worker alone creates/lists entries); gid 10004 holds only worker + runner, NOT `runner`, whose members include the Codex
// command shell uid 10003), and setgid makes every child inherit that group. The run root/inputs
// are group-traversable/readable (2750) and home is group-writable (2770), inputs files 0640.
// `work` is created 2750 and only widened to 2770 by openJobWorkspace AFTER the inputs are
// written, so a group member cannot swap `work/inputs` for a symlink while the worker writes.
// The per-run tree is never world-accessible; single-uid stays 0700/0600.
//
// The caller's input names are server-validated, but this module re-validates defensively: a name
// is a single path segment (no separator, no `..`, no leading dot) and each file is created with
// O_CREAT|O_EXCL|O_NOFOLLOW, then its realpath is checked to be inside the workspace, so a name
// can never write outside it or through a planted symlink.

import { createHash, randomBytes } from "node:crypto";
import { constants as fsc, promises as fs } from "node:fs";
import path from "node:path";

import type { Logger } from "./log.js";
import { rmHomeTree } from "./rmtree.js";
import { uidSplitActive } from "./runner-uid.js";
import { errMessage, RUN_ID_RE } from "./util.js";

/** The server's input-name shape (api job_inputs validation), mirrored client-side. */
const INPUT_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;

/** The storage name of an uploaded input file (PRD #1909 D6): `<sha256>.<ext>` with the extension
 *  from the api's input allowlist (workersvc jobFileExt minus the output-only html). It can never
 *  collide with an inline `NN-<name>` input: that shape has a dash at index 2. */
const STORAGE_NAME_RE = /^[0-9a-f]{64}\.(pdf|txt|md|csv|json|docx|xlsx|png|jpg)$/;
const SHA256_RE = /^[0-9a-f]{64}$/;

/** The workspace paths of one job run. */
export interface JobWorkspace {
  /** `<jobsRoot>/<run_id>`: removed as one tree. */
  root: string;
  /** The SDK HOME, inside `root` but outside `work`. */
  home: string;
  /** The SDK cwd and the path-guard root. */
  work: string;
  /** `work/inputs`. */
  inputsDir: string;
}

/** Refusal of an unsafe input name or a write that escaped the workspace. */
export class JobInputError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "JobInputError";
  }
}

/** A downloaded input file's bytes failed verification (wrong size, digest mismatch or a truncated
 *  stream). The runner reports it as an integrity failure, distinct from a transport failure. */
export class JobFileIntegrityError extends JobInputError {
  constructor(message: string) {
    super(message);
    this.name = "JobFileIntegrityError";
  }
}

/** The file name an input is written under: `NN-<name>` where NN is the 1-based ordinal padded to
 *  two digits. Throws JobInputError for any name that is not one plain path segment. */
export function jobInputFileName(index: number, name: string): string {
  if (typeof name !== "string" || !INPUT_NAME_RE.test(name) || name.includes("..")) {
    throw new JobInputError("job input name is not a plain file name");
  }
  return `${String(index + 1).padStart(2, "0")}-${name}`;
}

/** Directory/file modes for the current uid layout (`split` = the PRD #51 worker/runner split). */
function modes(split: boolean): { root: number; home: number; work: number; workOpen: number; inputs: number; file: number } {
  return split
    ? { root: 0o2750, home: 0o2770, work: 0o2750, workOpen: 0o2770, inputs: 0o2750, file: 0o640 }
    : { root: 0o700, home: 0o700, work: 0o700, workOpen: 0o700, inputs: 0o700, file: 0o600 };
}

/** Refuse a jobs root that is a symlink or not a directory (an attacker-planted link would
 *  redirect the reaper's rm and the workspace creation). A missing root is fine. */
async function assertRealJobsRoot(jobsRoot: string): Promise<void> {
  let st;
  try {
    st = await fs.lstat(jobsRoot);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return;
    throw err;
  }
  if (st.isSymbolicLink() || !st.isDirectory()) {
    throw new Error("job workspace root is a symlink or not a directory");
  }
}

/** Create `<jobsRoot>/<runId>` (0700 single-uid; under the uid split the setgid codex-session modes: root 2750, home 2770, work 2750 then 2770, inputs 2750, files 0640) with its home/work/inputs subtree. A leftover tree for the
 *  same run id (a requeue on this worker after a hard kill) is removed first. */
export async function createJobWorkspace(
  jobsRoot: string,
  runId: string,
  split: boolean = uidSplitActive(),
): Promise<JobWorkspace> {
  if (!RUN_ID_RE.test(runId)) throw new Error("job run id is not a UUID");
  if (!path.isAbsolute(jobsRoot)) throw new Error("job workspace root must be absolute");
  await assertRealJobsRoot(jobsRoot);
  await fs.mkdir(jobsRoot, { recursive: true, mode: 0o700 });
  const m = modes(split);
  const root = path.join(jobsRoot, runId);
  await fs.rm(root, { recursive: true, force: true }).catch(() => undefined);
  await fs.mkdir(root, { mode: m.root });
  const home = path.join(root, "home");
  const work = path.join(root, "work");
  const inputsDir = path.join(work, "inputs");
  await fs.mkdir(home, { mode: m.home });
  await fs.mkdir(work, { mode: m.work });
  await fs.mkdir(inputsDir, { mode: m.inputs });
  // mkdir's mode is masked by the umask and drops setgid: set the final modes explicitly.
  await fs.chmod(root, m.root);
  await fs.chmod(home, m.home);
  await fs.chmod(work, m.work);
  await fs.chmod(inputsDir, m.inputs);
  return { root, home, work, inputsDir };
}

/** Write each input to `inputs/NN-<name>` and return the relative paths (`inputs/NN-<name>`) in
 *  order. Refuses (JobInputError) an unsafe name or a file whose realpath leaves the workspace. */
export async function writeJobInputs(
  ws: JobWorkspace,
  inputs: ReadonlyArray<{ name: string; content: string }>,
  split: boolean = uidSplitActive(),
): Promise<string[]> {
  const workReal = await fs.realpath(ws.work);
  const written: string[] = [];
  for (const [i, input] of inputs.entries()) {
    const file = jobInputFileName(i, input.name);
    const target = path.join(ws.inputsDir, file);
    const handle = await fs.open(
      target,
      fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW,
      modes(split).file,
    );
    try {
      await handle.chmod(modes(split).file); // umask-independent
      await handle.writeFile(input.content, "utf8");
    } finally {
      await handle.close();
    }
    const real = await fs.realpath(target);
    if (real !== path.join(workReal, "inputs", file)) {
      await fs.rm(target, { force: true }).catch(() => undefined);
      throw new JobInputError("job input resolved outside the job workspace");
    }
    written.push(`inputs/${file}`);
  }
  return written;
}

/** One uploaded input file of the claim manifest, as the writer needs it. */
export interface JobInputFileSpec {
  /** The storage name `<sha256>.<ext>`. */
  name: string;
  /** The declared plaintext size in bytes. */
  size: number;
  /** The declared lowercase hex SHA-256. */
  sha256: string;
}

/** Stream an uploaded input file into `inputs/<storage name>` and return its workspace-relative
 *  path. The bytes go to `inputs/.partial-<rand>` (O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW, the
 *  workspace file mode), are hashed as they are written and counted against the declared size
 *  (more bytes than declared aborts at once; fewer, or a digest that is not the declared one, is a
 *  JobFileIntegrityError). Only a verified file reaches its final name, published with link(2) so an
 *  existing entry at the target (a planted symlink included) is refused rather than replaced, and
 *  is then made read-only. Any failure removes the partial: nothing unverified is left under
 *  `inputs/`. `body` is consumed to its end; a stream error is rethrown as is. */
export async function writeJobInputFile(
  ws: JobWorkspace,
  file: JobInputFileSpec,
  body: AsyncIterable<Uint8Array>,
  split: boolean = uidSplitActive(),
  partialSuffix: string = randomBytes(8).toString("hex"),
): Promise<string> {
  if (typeof file.name !== "string" || !STORAGE_NAME_RE.test(file.name)) {
    throw new JobInputError("job input file name is not a storage name");
  }
  if (typeof file.sha256 !== "string" || !SHA256_RE.test(file.sha256) || !file.name.startsWith(file.sha256)) {
    throw new JobInputError("job input file digest does not match its storage name");
  }
  if (!Number.isSafeInteger(file.size) || file.size <= 0) {
    throw new JobInputError("job input file size is not a positive integer");
  }
  if (!/^[0-9a-f]{1,32}$/.test(partialSuffix)) throw new JobInputError("job input partial name is invalid");
  const workReal = await fs.realpath(ws.work);
  const inputsReal = path.join(workReal, "inputs");
  const partial = path.join(ws.inputsDir, `.partial-${partialSuffix}`);
  const target = path.join(ws.inputsDir, file.name);
  const m = modes(split);
  const handle = await fs.open(partial, fsc.O_WRONLY | fsc.O_CREAT | fsc.O_EXCL | fsc.O_NOFOLLOW, m.file);
  let closed = false;
  try {
    await handle.chmod(m.file); // umask-independent
    const hash = createHash("sha256");
    let total = 0;
    for await (const chunk of body) {
      total += chunk.length;
      if (total > file.size) throw new JobFileIntegrityError("job input file is larger than its declared size");
      hash.update(chunk);
      let off = 0;
      while (off < chunk.length) {
        const { bytesWritten } = await handle.write(chunk, off, chunk.length - off);
        off += bytesWritten;
      }
    }
    if (total !== file.size) throw new JobFileIntegrityError("job input file is shorter than its declared size");
    if (hash.digest("hex") !== file.sha256) throw new JobFileIntegrityError("job input file does not match its declared digest");
    await handle.chmod(m.file & ~0o222); // read-only from here on
    await handle.close();
    closed = true;
    if ((await fs.realpath(partial)) !== path.join(inputsReal, path.basename(partial))) {
      throw new JobInputError("job input resolved outside the job workspace");
    }
    try {
      await fs.link(partial, target); // EEXIST for anything already at the target: never overwrites
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "EEXIST") throw new JobInputError("job input file name is already taken");
      throw err;
    }
    await fs.unlink(partial);
    if ((await fs.realpath(target)) !== path.join(inputsReal, file.name)) {
      await fs.rm(target, { force: true }).catch(() => undefined);
      throw new JobInputError("job input resolved outside the job workspace");
    }
    return `inputs/${file.name}`;
  } catch (err) {
    if (!closed) await handle.close().catch(() => undefined);
    await fs.rm(partial, { force: true }).catch(() => undefined);
    throw err;
  }
}

/** Widen `work` to its session mode (group-writable under the uid split). Call it after the inputs
 *  are written and immediately before the SDK session starts; until then `work` is not group-writable. */
export async function openJobWorkspace(ws: JobWorkspace, split: boolean = uidSplitActive()): Promise<void> {
  await fs.chmod(ws.work, modes(split).workOpen);
}

/** Remove a job workspace tree. Best-effort by contract: a cleanup failure is logged and never
 *  fails or reclassifies the run. */
export async function removeJobWorkspace(ws: JobWorkspace, log: Logger): Promise<void> {
  await rmHomeTree(ws.root).catch((err) =>
    log.warn("job workspace cleanup failed", { workspace: ws.root, error: errMessage(err) }),
  );
}

/** The startup reaper: remove every `<jobsRoot>/<uuid>` left by a worker that was killed before
 *  its `finally` ran. Call it BEFORE the worker claims anything, so no entry belongs to a live run.
 *  Entries that are not run-id-named are left alone; a symlink entry is unlinked (never followed).
 *  Never throws. Returns the number of entries removed. */
export async function reapStaleJobWorkspaces(jobsRoot: string, log: Logger): Promise<number> {
  let names: string[];
  try {
    await assertRealJobsRoot(jobsRoot);
    names = await fs.readdir(jobsRoot);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "ENOENT") {
      log.warn("job workspace reaper skipped the jobs root", { error: errMessage(err) });
    }
    return 0;
  }
  let removed = 0;
  for (const name of names) {
    if (!RUN_ID_RE.test(name)) continue;
    const entry = path.join(jobsRoot, name);
    try {
      const st = await fs.lstat(entry);
      if (st.isSymbolicLink() || !st.isDirectory()) await fs.rm(entry, { force: true });
      else await rmHomeTree(entry);
      removed++;
    } catch (err) {
      log.warn("job workspace reaper could not remove a stale workspace", { workspace: entry, error: errMessage(err) });
    }
  }
  if (removed > 0) log.info("reaped stale job workspaces", { removed });
  return removed;
}
