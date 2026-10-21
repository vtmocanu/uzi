// Issue #3153: boot-time check of the worker's TMPDIR when it lives under `<dataDir>/runner`.
//
// On the Docker lane `<dataDir>/runner` is a subPath of the PERSISTENT data volume (it used to
// be an emptyDir) and the controller points TMPDIR below it (`/data/runner/.tmp`, created by the
// `runner-init` init container). That directory is the root of every temp path the worker, the
// runner and the provisioning children use, so before anything touches it the worker confirms it
// is what it should be:
//   * every component from the runner root down to TMPDIR is a REAL directory (lstat; a symlink or
//     a non-directory is refused, nothing is followed, deleted or re-owned);
//   * an absent TMPDIR is created (single level, never `mkdir -p`) and re-checked;
//   * when TMPDIR is STRICTLY below the runner root it is owned by this process's uid and
//     traversable/writable by it (the controller / entrypoint make it worker-owned in both uid
//     modes). When TMPDIR IS the runner root (a controller that predates #3153, where the root is
//     the emptyDir mountpoint) only the real-directory check applies.
// Outside `<dataDir>/runner` (compose, plain-lane k8s, unset TMPDIR) this is a no-op.
//
// This is defence in depth: `runner-init` and the entrypoint normally establish all of it.
import fs from "node:fs";
import path from "node:path";

/** Thrown when the TMPDIR under the runner root cannot be used; the worker must not start. */
export class RunnerTmpdirError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "RunnerTmpdirError";
  }
}

export interface EnsureRunnerTmpdirOptions {
  /** The worker data dir (config.dataDir). */
  dataDir: string;
  env?: NodeJS.ProcessEnv;
  /** The uid the directory must be owned by; defaults to the process uid (skipped when unavailable). */
  uid?: number;
}

export interface EnsureRunnerTmpdirResult {
  /** True when TMPDIR is inside the runner root and was checked. */
  checked: boolean;
  /** True when this call created the directory. */
  created: boolean;
  path?: string;
}

function lstatOrUndefined(p: string): fs.Stats | undefined {
  try {
    return fs.lstatSync(p);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return undefined;
    throw new RunnerTmpdirError(`cannot inspect ${p}: ${(err as Error).message}`);
  }
}

function requireRealDir(p: string): fs.Stats {
  const st = lstatOrUndefined(p);
  if (!st) throw new RunnerTmpdirError(`${p} does not exist`);
  if (st.isSymbolicLink()) throw new RunnerTmpdirError(`${p} is a symlink`);
  if (!st.isDirectory()) throw new RunnerTmpdirError(`${p} is not a directory`);
  return st;
}

export function ensureRunnerTmpdir(opts: EnsureRunnerTmpdirOptions): EnsureRunnerTmpdirResult {
  const env = opts.env ?? process.env;
  const raw = env.TMPDIR?.trim();
  if (!raw) return { checked: false, created: false };
  const tmp = path.resolve(raw);
  const root = path.join(path.resolve(opts.dataDir), "runner");
  const rel = path.relative(root, tmp);
  if (rel === ".." || rel.startsWith(`..${path.sep}`) || path.isAbsolute(rel)) {
    return { checked: false, created: false };
  }
  const prefix = "runner tmpdir refused to start: ";
  try {
    // Complete path: the runner root and every intermediate directory down to (not including) TMPDIR.
    requireRealDir(root);
    const parts = rel === "" ? [] : rel.split(path.sep);
    let cur = root;
    for (const part of parts.slice(0, -1)) {
      cur = path.join(cur, part);
      requireRealDir(cur);
    }
    let created = false;
    if (rel !== "" && !lstatOrUndefined(tmp)) {
      try {
        // Non-recursive: the parent was just verified as a real directory.
        fs.mkdirSync(tmp, { mode: 0o700 });
        created = true;
      } catch (err) {
        if ((err as NodeJS.ErrnoException).code !== "EEXIST") {
          throw new RunnerTmpdirError(`cannot create ${tmp}: ${(err as Error).message}`);
        }
      }
    }
    const st = requireRealDir(tmp);
    if (rel !== "") {
      const uid = opts.uid ?? process.getuid?.();
      if (uid !== undefined && st.uid !== uid) {
        throw new RunnerTmpdirError(`${tmp} is owned by uid ${st.uid}, expected ${uid}`);
      }
      try {
        fs.accessSync(tmp, fs.constants.W_OK | fs.constants.X_OK);
      } catch {
        throw new RunnerTmpdirError(`${tmp} is not writable by this process`);
      }
    }
    return { checked: true, created, path: tmp };
  } catch (err) {
    if (err instanceof RunnerTmpdirError) throw new RunnerTmpdirError(prefix + err.message);
    throw err;
  }
}
