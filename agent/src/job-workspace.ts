// The per-run job workspace (PRD #1908, Decision 5): a scratch directory tree the repo-less `job`
// run kind works in, at `<worker data root>/jobs/<run_id>`. It lives OUTSIDE the clone
// (`runner/`) and attempt roots, is created at claim with mode 0700, and is removed at terminal.
//
//   <jobsRoot>/<run_id>/            0700, the whole tree removed at terminal
//     home/                         the SDK HOME (transcripts); NOT inside the agent's file jail
//     work/                         the SDK cwd and the path-guard root
//       inputs/NN-<name>            the caller's named input documents
//
// Under the PRD #51 uid split (UZI_UID_SPLIT=1) the SDK CLI runs as the `runner` uid while this
// worker process creates the tree, so 0700 would lock the CLI out of its own HOME, cwd and
// inputs. There the jobs root is a runner-group carve-out (entrypoint.sh: worker:runner 3775,
// like agent-home), the run root/inputs are group-traversable/readable (2750) and home/work are
// group-writable (2770), inputs files 0640. Never world-accessible; single-uid stays 0700/0600.
//
// The caller's input names are server-validated, but this module re-validates defensively: a name
// is a single path segment (no separator, no `..`, no leading dot) and each file is created with
// O_CREAT|O_EXCL|O_NOFOLLOW, then its realpath is checked to be inside the workspace, so a name
// can never write outside it or through a planted symlink.

import { constants as fsc, promises as fs } from "node:fs";
import path from "node:path";

import type { Logger } from "./log.js";
import { rmHomeTree } from "./rmtree.js";
import { uidSplitActive } from "./runner-uid.js";
import { errMessage, RUN_ID_RE } from "./util.js";

/** The server's input-name shape (api job_inputs validation), mirrored client-side. */
const INPUT_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;

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

/** The file name an input is written under: `NN-<name>` where NN is the 1-based ordinal padded to
 *  two digits. Throws JobInputError for any name that is not one plain path segment. */
export function jobInputFileName(index: number, name: string): string {
  if (typeof name !== "string" || !INPUT_NAME_RE.test(name) || name.includes("..")) {
    throw new JobInputError("job input name is not a plain file name");
  }
  return `${String(index + 1).padStart(2, "0")}-${name}`;
}

/** Directory/file modes for the current uid layout (`split` = the PRD #51 worker/runner split). */
function modes(split: boolean): { root: number; home: number; work: number; inputs: number; file: number } {
  return split
    ? { root: 0o2750, home: 0o2770, work: 0o2770, inputs: 0o2750, file: 0o640 }
    : { root: 0o700, home: 0o700, work: 0o700, inputs: 0o700, file: 0o600 };
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

/** Create `<jobsRoot>/<runId>` (0700; group-runner modes under the uid split) with its home/work/inputs subtree. A leftover tree for the
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
