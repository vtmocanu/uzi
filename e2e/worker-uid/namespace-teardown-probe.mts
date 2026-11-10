// In-container half of the namespace teardown proof (issue #3644, PRD #3831 section 8.1).
//
// namespace-teardown-real.test.mts drives this file with `docker exec` as the worker uid. It runs the
// real agent/src/restore-boundary.ts writer, reader, live-identity reader and evaluateComposeRestore (the
// compose predicate: R1-R6, R8, R9) against the container's real procfs. The R8 inputs are stubbed (bareBusy
// false, bareSlot null, census ok) and the predecessor slot is a cast stub carrying only the fields the
// predicate reads; that is what this kernel proof needs, so the admission instant is the compose predicate
// with stubbed R8 inputs, not a full restore. Each subcommand prints exactly one JSON line.
//   boundary <incarnation>      establishProcessBoundary for that incarnation; prints the outcome and the
//                               wall clock taken right after the record was durable
//   admit <predecessor> <self>  re-reads both records, snapshots the bare repo config, evaluates the compose
//                               restore predicate, snapshots again; the three are one admission instant
//   snapshot                    the bare repo config snapshot alone
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { evaluateComposeRestore, establishProcessBoundary, readBoundaryRecord, readLiveIdentity } from "../../agent/src/restore-boundary.js";
import type { SlotSnapshot } from "../../agent/src/completion-authority.js";

const DATA_DIR = "/data";
const BARE = path.join(DATA_DIR, "bare.git");
const token = process.env.UZI_NSPROOF_TOKEN ?? "";
if (token === "") throw new Error("UZI_NSPROOF_TOKEN is required");

function snapshot() {
  const config = path.join(BARE, "config");
  const bytes = fs.readFileSync(config);
  const st = fs.statSync(config);
  return {
    at: Date.now(),
    sha256: createHash("sha256").update(bytes).digest("hex"),
    ino: st.ino,
    mtimeMs: st.mtimeMs,
    text: bytes.toString("utf8"),
    lockPresent: fs.existsSync(config + ".lock"),
  };
}

const [command, a, b] = process.argv.slice(2);
if (command === "boundary" && a !== undefined) {
  const result = await establishProcessBoundary({ dataDir: DATA_DIR, workerToken: token, incarnation: a });
  console.log(JSON.stringify({ result, writtenAt: Date.now() }));
} else if (command === "admit" && a !== undefined && b !== undefined) {
  const options = { dataDir: DATA_DIR, workerToken: token };
  const before = snapshot();
  const selfRead = await readBoundaryRecord(options, b);
  const live = await readLiveIdentity({});
  // Only the fields the predicate reads are meaningful: the predecessor owns incarnation `a` and is quiet.
  const predecessor = { owner: { worker: a, executor: null }, pending: null, publication: null } as unknown as SlotSnapshot;
  const decision = await evaluateComposeRestore({
    selfIncarnation: b,
    selfRead,
    selfLive: live.ok ? live.identity : undefined,
    predecessor,
    predecessorFenced: false,
    readBoundary: (incarnation) => readBoundaryRecord(options, incarnation),
    bareBusy: false,
    bareSlot: null,
    census: { ok: true, paths: [] },
  });
  const after = snapshot();
  console.log(JSON.stringify({ decision, live, before, after }));
} else if (command === "snapshot") {
  console.log(JSON.stringify(snapshot()));
} else {
  throw new Error("usage: boundary <incarnation> | admit <predecessor> <self> | snapshot");
}
