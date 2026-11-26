// The per-run marker that proves a run was handed variables on this worker (PRD 4416 M2).
//
// A publish that has no live flight asks the api which secret versions the run was delivered
// (run-env-scan.ts resolveScanNeedles). When the api answers with a generic 404 (the route is absent, or a
// proxy answered), nothing the api said can show whether the run held secrets. The marker is the local
// evidence: it is written, durably, before any variable is handed to an executor, so its absence after a
// restart means this worker never delivered variables for the run. Markers are a few bytes each and are
// never removed.

import * as fsp from "node:fs/promises";
import * as path from "node:path";

/** The filesystem steps of a durable marker write, separable so a test can observe their order. */
export interface SeenOps {
  mkdirp(dir: string): Promise<void>;
  /** Creates `file` exclusively, writes `data`, fsyncs the file and closes it. */
  writeSynced(file: string, data: string): Promise<void>;
  rename(from: string, to: string): Promise<void>;
  /** Removes `file`; a missing file is not an error. */
  remove(file: string): Promise<void>;
  syncDir(dir: string): Promise<void>;
}

export const realSeenOps: SeenOps = {
  async mkdirp(dir) { await fsp.mkdir(dir, { recursive: true, mode: 0o700 }); },
  async writeSynced(file, data) {
    const fh = await fsp.open(file, "wx", 0o600);
    try {
      await fh.writeFile(data);
      await fh.sync();
    } finally {
      await fh.close();
    }
  },
  rename: (from, to) => fsp.rename(from, to),
  remove: (file) => fsp.rm(file, { force: true }),
  async syncDir(dir) {
    const fh = await fsp.open(dir, "r");
    try { await fh.sync(); } finally { await fh.close(); }
  },
};

const SAFE_RUN_ID = /^[A-Za-z0-9_-]{1,128}$/;

function markerPath(root: string, runId: string): string {
  if (!SAFE_RUN_ID.test(runId)) throw new Error("run id is not a safe marker name");
  return path.join(root, `${runId}.seen`);
}

/** Records that `runId` is about to receive variables. Resolves only after the marker's bytes and its
 *  directory entry are durable: temp file, fsync of the file, rename, fsync of the directory. Throws on any
 *  failure; the caller must then not hand the run any variable. Idempotent. */
export async function markRunEnvSeen(root: string, runId: string, ops: SeenOps = realSeenOps): Promise<void> {
  const final = markerPath(root, runId);
  const tmp = `${final}.tmp-${process.pid}-${Date.now().toString(36)}`;
  await ops.mkdirp(root);
  await ops.syncDir(path.dirname(root));
  try {
    await ops.writeSynced(tmp, "1\n");
    await ops.rename(tmp, final);
  } catch (e) {
    await ops.remove(tmp).catch(() => undefined);
    throw e;
  }
  await ops.syncDir(root);
}

/** Whether a marker exists for `runId`. `"unknown"` for any read error other than a missing file. */
export async function runEnvSeen(root: string, runId: string): Promise<boolean | "unknown"> {
  let file: string;
  try {
    file = markerPath(root, runId);
  } catch {
    return "unknown";
  }
  try {
    await fsp.stat(file);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "ENOENT" ? false : "unknown";
  }
}
