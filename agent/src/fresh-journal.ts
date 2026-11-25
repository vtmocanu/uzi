// issue #4628: the fresh-resume destination journal, a leaf module so git.ts and fresh-resume.ts can both name it.
//
// One value per branch in the worker bare's `config --local`, key `uzi-freshresume.<branch>.journal`. Canonical JSON in a fixed
// field order that carries no absolute path (the pod-epoch scratch readers absorb every absolute path they find in a `uzi-*`
// value, so a path here would widen their protected set). A reader accepts only a value that re-serializes to itself.

import { ATTEMPT_ID_RE } from "./attempt-path.js";

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const SHA40_RE = /^[0-9a-f]{40}$/;

/** `seeding`: the seed (L2) may have written; `ready`: the receipt commit (L4) landed. */
export type FreshJournalStage = "seeding" | "ready";

export interface FreshJournal {
  /** The transfer id (`run_fresh_resumes.id`). */
  frid: string;
  /** The destination attempt A. */
  attemptId: string;
  /** G', the claim generation the transfer was claimed at. */
  generation: number;
  /** The server-named evidence sha. */
  sha: string;
  stage: FreshJournalStage;
}

export function freshJournalKey(branch: string): string {
  return `uzi-freshresume.${branch}.journal`;
}

/** The branch a `uzi-freshresume.<branch>.journal` key names, or undefined for any other key. */
export function freshJournalKeyBranch(key: string): string | undefined {
  const m = /^uzi-freshresume\.(.+)\.journal$/.exec(key);
  return m ? m[1] : undefined;
}

export function freshJournalJson(journal: FreshJournal): string {
  return JSON.stringify({
    frid: journal.frid, attemptId: journal.attemptId, generation: journal.generation, sha: journal.sha, stage: journal.stage,
  });
}

/** Strict parse; throws on anything but the canonical shape. */
export function parseFreshJournal(value: string): FreshJournal {
  let parsed: unknown;
  try { parsed = JSON.parse(value); } catch { throw new Error("fresh journal is not JSON"); }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) throw new Error("fresh journal is not an object");
  const o = parsed as Record<string, unknown>;
  if (Object.keys(o).join(",") !== "frid,attemptId,generation,sha,stage") throw new Error("fresh journal fields");
  if (typeof o.frid !== "string" || !UUID_RE.test(o.frid)) throw new Error("fresh journal frid");
  if (typeof o.attemptId !== "string" || !ATTEMPT_ID_RE.test(o.attemptId)) throw new Error("fresh journal attempt id");
  if (typeof o.generation !== "number" || !Number.isSafeInteger(o.generation) || o.generation <= 0) throw new Error("fresh journal generation");
  if (typeof o.sha !== "string" || !SHA40_RE.test(o.sha)) throw new Error("fresh journal sha");
  if (o.stage !== "seeding" && o.stage !== "ready") throw new Error("fresh journal stage");
  const journal: FreshJournal = { frid: o.frid, attemptId: o.attemptId, generation: o.generation, sha: o.sha, stage: o.stage };
  if (freshJournalJson(journal) !== value) throw new Error("fresh journal is not canonical");
  return journal;
}
