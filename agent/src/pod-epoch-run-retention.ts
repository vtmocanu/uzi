// Issue #4420 m2: the read-only "is this run's local state ended?" classifier behind the startup scratch phase's
// per-run TMPDIR leaves (`run-<uuid>`, see run-tmpdir.ts). Safety principle: a read only ADDS keeps. A leaf is
// deletable only when EVERY source below was observed and positively ended; an error, cap, malformed value, symlink
// or special file, or an unknown shape is `unknown` (a keep), and no disposition alone ever means "ended".
//
// This module holds the pure judgements and the dataDir file readers; GitCache.classifyEpochScratchRunRetention
// (git.ts) owns the bare-config, ref, custody and retained-group sources, and pod-epoch-reclaim.ts owns the outbox
// source (it needs the live Outbox). Source tokens are a fixed vocabulary: never a path, run id or controller text.
import { constants as fsc } from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import type { SlotSnapshot } from "./completion-authority.js";
import { holdingNamePrefix } from "./restore-journal.js";
import { parseRunDirName } from "./util.js";

export type RunRetention = { state: "ended" } | { state: "active" | "unknown"; source: string };

export const ENDED: RunRetention = { state: "ended" };
export const activeRetention = (source: string): RunRetention => ({ state: "active", source });
export const unknownRetention = (source: string): RunRetention => ({ state: "unknown", source });

/** The six kept reasons the run-leaf wiring adds to the scratch verdict. */
export const RUN_KEPT_REASONS = ["run_active", "run_retained", "run_unknown", "run_unreadable", "run_local_active", "run_local_unknown"] as const;

const SOURCE_TOKEN_RE = /^[a-z0-9_]{1,48}$/;

/** A malformed result of a classifier is unknown; a source that is not a fixed-vocabulary token is replaced. */
export function normalizeRetention(value: unknown): RunRetention {
  if (typeof value !== "object" || value === null) return unknownRetention("classifier");
  const r = value as { state?: unknown; source?: unknown };
  if (r.state === "ended") return ENDED;
  if (r.state !== "active" && r.state !== "unknown") return unknownRetention("classifier");
  return { state: r.state, source: typeof r.source === "string" && SOURCE_TOKEN_RE.test(r.source) ? r.source : "unspecified" };
}

export const isRunUuid = (id: string): boolean => /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(id);

// ---- retained-group evidence ---------------------------------------------------------------------------------

/** The slice of a RetainedSourceEvidence the judgement reads (structural, so this module needs no git.ts import). */
export interface RetainedEvidenceShape {
  kind: string;
  ledgerState: string;
  presence: string;
  sources: ReadonlyArray<{ presence: string; live: boolean }>;
  disposition?: { kind: string } | undefined;
  epochSettlement?: { basis: string; disposition: { kind: string } } | undefined;
}

/**
 * ONE retained source of this run. `ended` only for the verified settled shape: an attempt source whose ledger is
 * `retired`, whose every part is positively missing and not live, with a strict-plain epoch settlement of
 * `local_discard`. A disposition alone (even `local_discard`) never yields ended.
 */
export function judgeRetainedEvidence(s: RetainedEvidenceShape): RunRetention {
  const partsMissing = Array.isArray(s.sources) && s.sources.length > 0 && s.sources.every(p => p.presence === "missing" && p.live === false);
  if (s.kind === "attempt" && s.ledgerState === "retired" && s.presence === "missing" && partsMissing &&
      s.epochSettlement?.basis === "strict_plain" && s.epochSettlement.disposition.kind === "local_discard") return ENDED;
  const kinds = [s.disposition?.kind, s.epochSettlement?.disposition.kind];
  if (s.kind === "protected" || kinds.includes("local_protected_discard")) return activeRetention("protected_source");
  if (kinds.includes("owner_hold_discard")) return activeRetention("owner_hold");
  if (kinds.includes("resolve_local_record")) return activeRetention("local_record");
  if (kinds.includes("refused")) return activeRetention("retained_refused");
  if (s.ledgerState === "live" || s.ledgerState === "abandoned" || s.ledgerState === "reclaimed") return activeRetention(`ledger_${s.ledgerState}`);
  if (s.presence !== "missing" || !partsMissing) return activeRetention("attempt_footprint");
  return activeRetention("retained_source");
}

// ---- authority slot ------------------------------------------------------------------------------------------

/**
 * One read-only authority slot (peekAuthoritySlot) of a ledger entry whose footprint is ALREADY verified missing; this
 * never stands in for that check. Absent (observed) is ended; an authenticated `ineligible` or `retired` slot with no
 * pending footprint, retiring record or publication is ended; any other state or any non-null field is a live
 * authority record (active); a malformed slot is unknown.
 */
export function judgeAuthoritySlot(slot: Pick<SlotSnapshot, "state" | "pending" | "retiring" | "publication"> | null): RunRetention {
  if (slot === null) return ENDED;
  if (typeof slot !== "object" || typeof slot.state !== "string") return unknownRetention("authority_slot");
  for (const field of [slot.pending, slot.retiring, slot.publication]) {
    if (field !== null && (typeof field !== "object" || field === undefined)) return unknownRetention("authority_slot");
  }
  if ((slot.state === "ineligible" || slot.state === "retired") &&
      slot.pending === null && slot.retiring === null && slot.publication === null) return ENDED;
  return activeRetention("authority_slot");
}

// ---- dataDir records -----------------------------------------------------------------------------------------

type Probe = "absent" | "present" | "unknown";

async function probe(file: string, interrupt: () => void): Promise<Probe> {
  interrupt();
  try { await fsp.lstat(file); return "present"; }
  catch (error) {
    interrupt();
    return (error as NodeJS.ErrnoException).code === "ENOENT" ? "absent" : "unknown";
  }
}

/** A bounded no-follow regular-file read: absent (ENOENT), the text, or unknown. */
async function readBounded(file: string, max: number, interrupt: () => void): Promise<{ text: string } | "absent" | "unknown"> {
  interrupt();
  let handle: fsp.FileHandle;
  try { handle = await fsp.open(file, fsc.O_RDONLY | fsc.O_NOFOLLOW | fsc.O_NONBLOCK); }
  catch (error) { interrupt(); return (error as NodeJS.ErrnoException).code === "ENOENT" ? "absent" : "unknown"; }
  try {
    const st = await handle.stat();
    if (!st.isFile() || st.size > max) return "unknown";
    const buffer = Buffer.alloc(st.size + 1);
    let length = 0;
    for (;;) {
      const { bytesRead } = await handle.read(buffer, length, buffer.length - length, length);
      if (!bytesRead) break;
      length += bytesRead;
      if (length > st.size) return "unknown";
    }
    if (length !== st.size) return "unknown";
    return { text: new TextDecoder("utf-8", { fatal: true }).decode(buffer.subarray(0, length)) };
  } catch { interrupt(); return "unknown"; }
  finally { await handle.close().catch(() => undefined); }
}

const CREDENTIAL_STATES = new Set(["sending", "ambiguous", "clean_route_absent", "settled"]);

/** `credential-switch-ops/<runId>.json` (credential-switch-op-ledger.ts): any `sending`/`ambiguous` op may have
 *  committed server-side, so it is active; all `settled`/`clean_route_absent` is ended; anything else is unknown. */
export function classifyCredentialSwitchText(text: string): RunRetention {
  let parsed: unknown;
  try { parsed = JSON.parse(text); } catch { return unknownRetention("credential_switch"); }
  const ops = (parsed as { ops?: unknown } | null)?.ops;
  if (typeof ops !== "object" || ops === null || Array.isArray(ops)) return unknownRetention("credential_switch");
  let result: RunRetention = ENDED;
  for (const entry of Object.values(ops)) {
    const state = (entry as { state?: unknown } | null)?.state;
    if (typeof state !== "string" || !CREDENTIAL_STATES.has(state)) return unknownRetention("credential_switch");
    if (state === "sending" || state === "ambiguous") result = activeRetention("credential_switch");
  }
  return result;
}

/** `recovery-maintenance/complete.json`: a cycle of this run that is not `exhausted` may still do work. */
export function classifyMaintenanceTable(text: string, id: string): RunRetention {
  let parsed: unknown;
  try { parsed = JSON.parse(text); } catch { return unknownRetention("recovery_maintenance"); }
  const cycles = (parsed as { cycles?: unknown } | null)?.cycles;
  if (!Array.isArray(cycles)) return unknownRetention("recovery_maintenance");
  let result: RunRetention = ENDED;
  for (const cycle of cycles) {
    const c = cycle as { identity?: { run?: unknown }; exhausted?: unknown } | null;
    const run = c?.identity?.run;
    if (typeof run !== "string" || typeof c?.exhausted !== "boolean") return unknownRetention("recovery_maintenance");
    if (run.toLowerCase() === id && c.exhausted !== true) result = activeRetention("recovery_maintenance");
  }
  return result;
}

const MAX_RECORD_BYTES = 1024 * 1024;
const MAX_TABLE_BYTES = 16 * 1024 * 1024;
const MAX_QUARANTINE_ENTRIES = 4096;

/**
 * The run's records directly under the worker data dir. lstat-based: ENOENT is observed-absent (ended for that
 * source); any other error, a symlink where a directory belongs, an oversized file or a listing over its cap is
 * unknown. `id` is the lowercase run id.
 */
export async function classifyRunDataDirRecords(
  dataDir: string, id: string, opts: { interrupt: () => void; maxDirectoryEntries: number },
): Promise<RunRetention> {
  const { interrupt } = opts;
  const present = async (relative: string, source: string): Promise<RunRetention | undefined> => {
    const state = await probe(path.join(dataDir, relative), interrupt);
    return state === "present" ? activeRetention(source) : state === "unknown" ? unknownRetention(source) : undefined;
  };
  // 1. The run HOME: `agent-home/<runId>` or an independent restore's `<runId>.g<N>`.
  const home = await present(path.join("agent-home", id), "run_home");
  if (home) return home;
  const homes = path.join(dataDir, "agent-home");
  const homesState = await probe(homes, interrupt);
  if (homesState === "unknown") return unknownRetention("run_home");
  if (homesState === "present") {
    let dir: Awaited<ReturnType<typeof fsp.opendir>>;
    try { dir = await fsp.opendir(homes, { bufferSize: 1 }); } catch { interrupt(); return unknownRetention("run_home"); }
    try {
      let seen = 0;
      for (;;) {
        interrupt();
        const entry = await dir.read();
        if (!entry) break;
        if (++seen > opts.maxDirectoryEntries) return unknownRetention("run_home");
        if (parseRunDirName(entry.name)?.runId.toLowerCase() === id) return activeRetention("run_home");
      }
    } catch { interrupt(); return unknownRetention("run_home"); }
    finally { await dir.close().catch(() => undefined); }
  }
  const retainedHome = await present(path.join("retained-homes", `${id}.json`), "retained_home");
  if (retainedHome) return retainedHome;
  // 2. The credential-switch write-ahead ledger.
  const ops = await readBounded(path.join(dataDir, "credential-switch-ops", `${id}.json`), MAX_RECORD_BYTES, interrupt);
  if (ops === "unknown") return unknownRetention("credential_switch");
  if (ops !== "absent") { const r = classifyCredentialSwitchText(ops.text); if (r.state !== "ended") return r; }
  // 3. A quarantined (holding) runner directory named for the run.
  const quarantine = path.join(dataDir, "runner-quarantine");
  const quarantineState = await probe(quarantine, interrupt);
  if (quarantineState === "unknown") return unknownRetention("runner_quarantine");
  if (quarantineState === "present") {
    try {
      const st = await fsp.lstat(quarantine);
      if (!st.isDirectory() || st.isSymbolicLink()) return unknownRetention("runner_quarantine");
    } catch { interrupt(); return unknownRetention("runner_quarantine"); }
    const prefix = holdingNamePrefix(id).toLowerCase();
    let dir: Awaited<ReturnType<typeof fsp.opendir>>;
    try { dir = await fsp.opendir(quarantine, { bufferSize: 1 }); } catch { interrupt(); return unknownRetention("runner_quarantine"); }
    try {
      let seen = 0;
      for (;;) {
        interrupt();
        const entry = await dir.read();
        if (!entry) break;
        if (++seen > MAX_QUARANTINE_ENTRIES) return unknownRetention("runner_quarantine");
        if (entry.name.toLowerCase().startsWith(prefix)) return activeRetention("runner_quarantine");
      }
    } catch { interrupt(); return unknownRetention("runner_quarantine"); }
    finally { await dir.close().catch(() => undefined); }
  }
  // 4. Terminal preservation and the quarantine archive (its contract has no ended predicate).
  const terminal = await present(path.join("terminal-preservation", id), "terminal_preservation");
  if (terminal) return terminal;
  const archive = await present(path.join("recovery-archive", id), "quarantine_archive");
  if (archive) return archive;
  // 5. The recovery-maintenance table: complete.json and initialized.json are written as a pair.
  const root = path.join(dataDir, "recovery-maintenance");
  const table = await readBounded(path.join(root, "complete.json"), MAX_TABLE_BYTES, interrupt);
  const marker = await readBounded(path.join(root, "initialized.json"), MAX_TABLE_BYTES, interrupt);
  if (table === "unknown" || marker === "unknown") return unknownRetention("recovery_maintenance");
  if (table === "absent" && marker === "absent") return ENDED;
  if (table === "absent" || marker === "absent") return unknownRetention("recovery_maintenance");
  try { JSON.parse(marker.text); } catch { return unknownRetention("recovery_maintenance"); }
  return classifyMaintenanceTable(table.text, id);
}
