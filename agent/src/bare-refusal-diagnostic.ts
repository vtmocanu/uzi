// issue #4066 M4: classify the stored footprint of an unresolved bare mutation for the `bare_slot_refusal`
// diagnostic. The footprint is `"<op>: <ref>"` as authority-runtime's #beginBare builds it; every op below passes
// exactly one ref. Anything that does not match one op's namespaces exactly (a digest of an oversized footprint,
// several refs, a malformed run id or generation) is `unparsed`. Nothing here ever returns raw footprint text.

export const BARE_DELETE_OPS = [
  "recovery-pin-delete-ref",
  "recovery-pin-archive-delete-ref",
  "recovery-cleanup-delete-ref",
  "owed-reconcile-delete-ref",
] as const;

export type BareDeleteOp = (typeof BARE_DELETE_OPS)[number];

export interface BareFootprintClass {
  deletionClass: BareDeleteOp | "unparsed";
  /** A validated run id, or "unknown". */
  subjectRun: string;
  /** A safe non-negative integer, or "unknown" (always unknown for `refs/uzi-owed/`). */
  generation: number | "unknown";
}

const UNPARSED: BareFootprintClass = { deletionClass: "unparsed", subjectRun: "unknown", generation: "unknown" };

const RUN_ID = "[A-Za-z0-9_-]{1,128}";
const PIN = new RegExp(`^refs/uzi-recovery-pin/(${RUN_ID})/([0-9]+)$`);
const COVERAGE = new RegExp(`^refs/uzi-coverage/(${RUN_ID})/([0-9]+)/[^/\\s,]+$`);
const OWED = new RegExp(`^refs/uzi-owed/(${RUN_ID})/[0-9a-f]{40}$`);

const NAMESPACES: Record<BareDeleteOp, readonly RegExp[]> = {
  "recovery-pin-delete-ref": [PIN],
  "recovery-pin-archive-delete-ref": [PIN],
  "recovery-cleanup-delete-ref": [PIN, COVERAGE, OWED],
  "owed-reconcile-delete-ref": [OWED],
};

export function classifyBareFootprint(footprint: string): BareFootprintClass {
  const sep = footprint.indexOf(": ");
  if (sep <= 0) return UNPARSED;
  const op = footprint.slice(0, sep);
  if (!(BARE_DELETE_OPS as readonly string[]).includes(op)) return UNPARSED;
  const ref = footprint.slice(sep + 2);
  for (const pattern of NAMESPACES[op as BareDeleteOp]) {
    const match = pattern.exec(ref);
    if (!match) continue;
    const run = match[1]!;
    const raw = match[2];
    if (raw === undefined) return { deletionClass: op as BareDeleteOp, subjectRun: run, generation: "unknown" };
    const generation = Number(raw);
    if (!Number.isSafeInteger(generation)) return UNPARSED;
    return { deletionClass: op as BareDeleteOp, subjectRun: run, generation };
  }
  return UNPARSED;
}
