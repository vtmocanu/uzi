// issue #2213 — the additive, credential-free archival capture of a quarantined run's committed work.
//
// A run that fails because the worker is quarantined (an unattributable runner-uid process may read
// credentials) cannot run the ordinary recovery settle: that path starts runner-clone git (which can
// run clone-configured code), deletes recovery pins, retires journal records and releases custody on
// an `available` capture. So the ordinary path stays skipped, and this capture instead keeps a
// verified local copy of the work that is ALREADY in the worker bare:
//
//   1. resolve the committed head H from the bare tracking ref `refs/uzi-runner/<branch>`;
//   2. anchor H at the create-only archive ref `refs/uzi-archive/<runId>/g<generation>`;
//   3. bundle H from the bare (self-contained, no forge tip, the recovery size cap) into a temp file
//      in a worker-owned 0700 directory;
//   4. read the temp file ONCE into memory (bounded by the size cap), require its sha256 and size to
//      equal the producer's, then verify those bytes (fed over stdin, never re-read from a path) by
//      unbundling into a fresh temporary bare repository;
//   5. re-read the archive ref (must still be H), write exactly the verified bytes to a fresh
//      exclusive temp and publish `g<generation>.bundle` (0600) and its manifest by rename; the
//      reported sha256 and size are the in-memory digest of those same bytes.
//
// It reads only the worker bare and runs only credential-free worker-uid git. It never runs git in
// the runner clone, spawns no provider, makes no reserve/upload/release client call, never deletes a
// recovery pin and never touches the journal or the custody hold. Its outcome never changes the run's
// typed failure: every error becomes an outcome. Everything is bounded by `deadlineMs`; a capture
// that overruns it reports `incomplete` and publishes nothing afterwards.

import { createHash, randomBytes } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { RECOVERY_MAX_BUNDLE_BYTES, RecoveryBundleTooLargeError, type RecoveryBundleResult } from "./git.js";
import { sanitizeForLog } from "./run-quiescence.js";
import { errMessage, RUN_ID_RE } from "./util.js";

export type QuarantineArchiveOutcome = "archived" | "incomplete" | "no_source" | "too_large";

export interface QuarantineArchiveResult {
  outcome: QuarantineArchiveOutcome;
  /** The committed head H (40-hex), when one was resolved. */
  head?: string;
  /** Lowercase hex SHA-256 of the archived bundle (outcome `archived` only). */
  sha256?: string;
  /** Bundle size in bytes (outcome `archived` only). */
  size?: number;
  /** Why the capture is not `archived` (sanitized). */
  detail?: string;
}

/** The GitCache surface the capture uses; a test can wrap the real one to inject a failure. */
export interface QuarantineArchiveGit {
  readonly recoveryArchiveRoot: string;
  trackingTip(barePath: string, branch: string): Promise<string | null>;
  createQuarantineArchiveRef(
    barePath: string,
    runId: string,
    generation: number,
    sha: string,
  ): Promise<"created" | "exists" | "conflict" | "absent">;
  quarantineArchiveRefTip(barePath: string, runId: string, generation: number): Promise<string | null>;
  produceRecoveryBundle(
    barePath: string,
    opts: { sourceSha: string; outPath: string; maxBytes?: number },
  ): Promise<RecoveryBundleResult>;
  verifyBundleReproduces(bundle: Buffer, sha: string, scratchRoot: string): Promise<string | undefined>;
}

export interface QuarantineArchiveInput {
  runId: string;
  generation: number;
  barePath: string | undefined;
  branch: string | undefined;
  /** Wall-clock bound on the whole capture (default 120 s). */
  deadlineMs?: number;
  /** Bundle size cap (default RECOVERY_MAX_BUNDLE_BYTES, applied by produceRecoveryBundle). */
  maxBytes?: number;
}

const QUARANTINE_ARCHIVE_DEADLINE_MS = 120_000;

/** Run the capture. Never throws; the outcome is also logged. */
export async function archiveQuarantinedSource(
  git: QuarantineArchiveGit,
  log: Logger,
  input: QuarantineArchiveInput,
): Promise<QuarantineArchiveResult> {
  const state = { cancelled: false };
  const deadlineMs = input.deadlineMs ?? QUARANTINE_ARCHIVE_DEADLINE_MS;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const deadline = new Promise<QuarantineArchiveResult>((resolve) => {
    timer = setTimeout(() => {
      state.cancelled = true;
      resolve({ outcome: "incomplete", detail: `the capture did not finish within ${deadlineMs} ms` });
    }, deadlineMs);
    timer.unref?.();
  });
  let result: QuarantineArchiveResult;
  try {
    result = await Promise.race([capture(git, input, state), deadline]);
  } catch (err) {
    result = { outcome: "incomplete", detail: sanitizeForLog(errMessage(err), 160) };
  } finally {
    if (timer) clearTimeout(timer);
  }
  // From here a late-finishing capture must publish nothing.
  state.cancelled = true;
  log.warn("quarantine archival capture finished", {
    run_id: input.runId,
    generation: input.generation,
    outcome: result.outcome,
    ...(result.head ? { head: result.head } : {}),
    ...(result.sha256 ? { sha256: result.sha256, size: result.size } : {}),
    ...(result.detail ? { detail: result.detail } : {}),
  });
  return result;
}

async function capture(
  git: QuarantineArchiveGit,
  input: QuarantineArchiveInput,
  state: { cancelled: boolean },
): Promise<QuarantineArchiveResult> {
  const { runId, generation, barePath, branch } = input;
  if (!barePath || !branch) return { outcome: "no_source", detail: "the run has no worker bare or branch" };
  if (!RUN_ID_RE.test(runId)) return { outcome: "incomplete", detail: "the run id is not a UUID" };
  const head = await git.trackingTip(barePath, branch);
  if (head === null) return { outcome: "no_source", detail: "no tracking ref in the worker bare" };

  const anchored = await git.createQuarantineArchiveRef(barePath, runId, generation, head);
  if (anchored === "conflict") return { outcome: "incomplete", head, detail: "the archive ref already names another commit" };
  if (anchored === "absent") return { outcome: "incomplete", head, detail: "the committed head is not present in the worker bare" };

  const dir = path.join(git.recoveryArchiveRoot, runId);
  await fs.mkdir(git.recoveryArchiveRoot, { recursive: true, mode: 0o700 });
  await fs.mkdir(dir, { recursive: true, mode: 0o700 });
  await fs.chmod(dir, 0o700);

  const suffix = randomBytes(6).toString("hex");
  const tmpBundle = path.join(dir, `.g${generation}.${suffix}.bundle.tmp`);
  const tmpPublish = path.join(dir, `.g${generation}.${suffix}.publish.tmp`);
  const tmpManifest = path.join(dir, `.g${generation}.${suffix}.manifest.tmp`);
  const finalBundle = path.join(dir, `g${generation}.bundle`);
  const finalManifest = path.join(dir, `g${generation}.manifest.json`);
  const cleanupTemps = async (): Promise<void> => {
    await fs.rm(tmpBundle, { force: true }).catch(() => undefined);
    await fs.rm(tmpPublish, { force: true }).catch(() => undefined);
    await fs.rm(tmpManifest, { force: true }).catch(() => undefined);
  };
  try {
    let produced: RecoveryBundleResult;
    try {
      produced = await git.produceRecoveryBundle(barePath, {
        sourceSha: head,
        outPath: tmpBundle,
        ...(input.maxBytes === undefined ? {} : { maxBytes: input.maxBytes }),
      });
    } catch (err) {
      await cleanupTemps();
      if (err instanceof RecoveryBundleTooLargeError) {
        return { outcome: "too_large", head, detail: sanitizeForLog(err.message, 160) };
      }
      return { outcome: "incomplete", head, detail: sanitizeForLog(`bundle production failed: ${errMessage(err)}`, 160) };
    }
    if (produced.sourceSha !== head) return fail(head, "the produced bundle names another head");

    // Snapshot the produced file ONCE. Digest, verification and publication all use these
    // in-memory bytes: the published bytes are written from the verified snapshot, and a later
    // change to the file is detectable against the reported sha256.
    const cap = Math.min(input.maxBytes ?? RECOVERY_MAX_BUNDLE_BYTES, RECOVERY_MAX_BUNDLE_BYTES);
    if (produced.byteSize > cap) return fail(head, "the produced bundle exceeds the size cap");
    let bytes: Buffer;
    try {
      bytes = await readBounded(tmpBundle, produced.byteSize);
    } catch (err) {
      return fail(head, `could not read the produced bundle: ${errMessage(err)}`);
    }
    // The produced file is no longer trusted for anything; drop it now.
    await fs.rm(tmpBundle, { force: true }).catch(() => undefined);
    const digest = { sha256: createHash("sha256").update(bytes).digest("hex"), size: bytes.length };
    if (digest.sha256 !== produced.checksum || digest.size !== produced.byteSize) {
      return fail(head, "the bundle file changed after it was produced");
    }

    const reason = await git.verifyBundleReproduces(bytes, head, dir);
    if (reason !== undefined) return fail(head, reason);

    // The archive ref must still be H: a force-move during the capture means the anchor no longer
    // guards what was bundled.
    const anchorNow = await git.quarantineArchiveRefTip(barePath, runId, generation);
    if (anchorNow !== head) return fail(head, "the archive ref no longer names the committed head");

    // Every publish step re-checks the deadline: a capture that overruns it reports `incomplete`, so
    // it must also leave no final file behind. The last check runs with nothing awaited between it
    // and the return, so the deadline timer cannot fire after it and before `archived` is reported.
    const deadlinePassed = (): void => {
      if (state.cancelled) throw new Error("the capture deadline passed before publication");
    };
    try {
      deadlinePassed();
      await fs.writeFile(tmpPublish, bytes, { mode: 0o600, flag: "wx" });
      deadlinePassed();
      const manifest = {
        version: 1,
        run_id: runId,
        generation,
        head,
        sha256: digest.sha256,
        size: digest.size,
        archived_at: new Date().toISOString(),
      };
      await fs.writeFile(tmpManifest, `${JSON.stringify(manifest, null, 2)}\n`, { mode: 0o600, flag: "wx" });
      deadlinePassed();
      await fs.rename(tmpPublish, finalBundle);
      deadlinePassed();
      await fs.rename(tmpManifest, finalManifest);
      deadlinePassed();
    } catch (err) {
      // Never leave a bundle that reads as verified without its manifest, nor any final file for a
      // capture that is reported incomplete.
      await fs.rm(finalBundle, { force: true }).catch(() => undefined);
      await fs.rm(finalManifest, { force: true }).catch(() => undefined);
      return fail(head, state.cancelled ? "the capture deadline passed before publication" : `could not publish the archive: ${errMessage(err)}`);
    }
    return { outcome: "archived", head, sha256: digest.sha256, size: digest.size };
  } catch (err) {
    await cleanupTemps();
    return { outcome: "incomplete", head, detail: sanitizeForLog(errMessage(err), 160) };
  }

  async function fail(h: string, why: string): Promise<QuarantineArchiveResult> {
    await cleanupTemps();
    return { outcome: "incomplete", head: h, detail: sanitizeForLog(why, 160) };
  }
}

/** Read `file` into memory, refusing a file that is not exactly `expected` bytes (read at most
 *  expected + 1 so an oversized replacement is never fully buffered). */
async function readBounded(file: string, expected: number): Promise<Buffer> {
  const fh = await fs.open(file, "r");
  try {
    const buf = Buffer.alloc(expected + 1);
    let n = 0;
    for (;;) {
      const { bytesRead } = await fh.read(buf, n, buf.length - n, null);
      if (bytesRead === 0) break;
      n += bytesRead;
      if (n > expected) throw new Error("the bundle file is larger than it was produced");
    }
    return buf.subarray(0, n);
  } finally {
    await fh.close();
  }
}

/** The failure-reason suffix naming an archived capture (H and the bundle sha256): a server-side
 *  integrity anchor a same-uid survivor cannot reach. Empty for every other outcome. */
export function archiveFailureReasonSuffix(r: QuarantineArchiveResult): string {
  if (r.outcome !== "archived" || !r.head || !r.sha256) return "";
  return ` Committed work archived on the worker: head ${r.head}, bundle sha256 ${r.sha256}.`;
}
