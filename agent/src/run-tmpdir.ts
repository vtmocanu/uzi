// Issue #4420: a run-owned TMPDIR for a Claude flight. Instead of every flight scattering `tmp.*` /
// `go-link-*` / npm entries directly in the shared runner scratch root, the launch points TMPDIR at
// `<scratch root>/run-<runId>`, so a run's scratch is one unit that is found by name.
//
// This module only creates-or-adopts that one leaf. It never chowns, removes or recurses, adds no
// privilege, and never throws: every failure is a fixed-token `skipped` result and the caller launches
// with today's TMPDIR. The root is pinned through descriptors (pod-epoch-scratch's `pinScratchRoot`);
// the split layout's 0700 runner-owned root is entered only by the fixed runner-uid helper
// (rmtree.ts `RUN_TMPDIR_SCRIPT`), and the worker accepts only its exact receipt followed by a root
// re-pin to the same identity. Like go-work.ts this is scratch, not cross-run isolation: same-uid
// processes of the shared runner uid can still rename a leaf.
import * as fs from "node:fs";
import {
  RUN_SCRATCH_LEAF_RE, pinScratchRoot, repinScratchRoot, scratchLayout,
  type ScratchIo, type ScratchLayout,
} from "./pod-epoch-scratch.js";
import { runTmpdirHelper } from "./rmtree.js";

/** Why no run TMPDIR was provided; a fixed vocabulary, safe to log. */
export type RunTmpdirSkipReason =
  | "layout_refused"
  | "run_id_invalid"
  | "root_unpinned"
  | "root_identity"
  | "helper_failed"
  | "helper_receipt"
  | "root_repin"
  | "leaf_create_failed"
  | "leaf_refused";

export type RunTmpdirPrepared = { dir: string } | { skipped: true; reason: RunTmpdirSkipReason };

/** Test seams only; production callers pass none. */
export interface RunTmpdirTestSeams {
  /** Environment for the exact-layout gate (defaults to process.env). */
  env?: NodeJS.ProcessEnv;
  /** Replaces the layout the gate returned, so a fixture can use a temp root. */
  layout?: ScratchLayout;
  io?: ScratchIo;
  /** Replaces the runner-uid helper launch (split mode). */
  helper?: typeof runTmpdirHelper;
}

const HELPER_TIMEOUT_MS = 15_000;
const SELF_FD = "/proc/self/fd/";
const O_DIR_NOFOLLOW = fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
const RECEIPT_RE = /^UZI-RUN-TMP (created|reused) \d{1,20}:\d{1,20}\n$/;

const skip = (reason: RunTmpdirSkipReason): RunTmpdirPrepared => ({ skipped: true, reason });

/** Creates or adopts `<scratch root>/run-<runId>` (0700, owned by the layout's owner) and returns its
 * path. `runId` must already be a lowercase UUID; it is never lowercased or otherwise repaired. */
export async function prepareRunTmpdir(runId: string, seams: RunTmpdirTestSeams = {}): Promise<RunTmpdirPrepared> {
  try {
    const name = `run-${runId}`;
    if (!RUN_SCRATCH_LEAF_RE.test(name)) return skip("run_id_invalid");
    const layout = seams.layout ?? scratchLayout(seams.env ?? process.env);
    if ("refused" in layout) return skip("layout_refused");
    const io = seams.io ?? fs;
    const pin = pinScratchRoot(layout, io);
    if ("refused" in pin) return skip(pin.refused);
    try {
      if (layout.mode === "split") {
        const run = seams.helper ?? runTmpdirHelper;
        let result: { code: number; stdout: string };
        try { result = await run(layout.root, pin.devIno, name, layout.ownerUid, HELPER_TIMEOUT_MS); }
        catch { return skip("helper_failed"); }
        if (result.code !== 0) return skip(result.code === 3 ? "root_identity" : "helper_failed");
        if (!RECEIPT_RE.test(result.stdout)) return skip("helper_receipt");
        if (!repinScratchRoot(layout, pin.devIno, io)) return skip("root_repin");
      } else {
        const refused = createLeafAsWorker(io, pin.fd, name, layout.ownerUid);
        if (refused) return skip(refused);
        if (!repinScratchRoot(layout, pin.devIno, io)) return skip("root_repin");
      }
    } finally {
      pin.close();
    }
    return { dir: `${layout.root}/${name}` };
  } catch {
    return skip("leaf_create_failed");
  }
}

/** Single-uid mode: the worker performs the helper's steps itself, relative to the pinned root. */
function createLeafAsWorker(io: ScratchIo, rootFd: number, name: string, ownerUid: number): RunTmpdirSkipReason | undefined {
  const leafPath = `${SELF_FD}${rootFd}/${name}`;
  let fresh = false;
  try { io.mkdirSync(leafPath, 0o700); fresh = true; }
  catch (e) { if ((e as NodeJS.ErrnoException).code !== "EEXIST") return "leaf_create_failed"; }
  let fd: number | undefined;
  try {
    try { fd = io.openSync(leafPath, O_DIR_NOFOLLOW); } catch { return "leaf_refused"; }
    let st = io.fstatSync(fd, { bigint: true });
    if (!st.isDirectory() || st.uid !== BigInt(ownerUid)) return "leaf_refused";
    // A new leaf under a setgid root is 2700 until this chmod; an existing leaf is never repaired.
    if (fresh) { io.fchmodSync(fd, 0o700); st = io.fstatSync(fd, { bigint: true }); }
    if ((Number(st.mode) & 0o7777) !== 0o700) return "leaf_refused";
    return undefined;
  } finally {
    if (fd !== undefined) { try { io.closeSync(fd); } catch { /* best effort */ } }
  }
}
