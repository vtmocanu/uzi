import path from "node:path";
import type { OperatorSourceEvidence } from "../../src/recovery.js";

// issue #3885: injected Case B source evidence. The shapes mirror what GitCache.readInventoryCloneHeads (with
// cloneIdentity) and GitCache.inspectRetainedSources (no selector) return for one retained canonical source; a test
// edits the returned object to forge any malformed or hostile variant. Real readers are exercised in the e2e world.

export interface FakeEvidenceOptions {
  /** Where the one retained clone lives (never touched: the evidence is injected). */
  clonePath?: string;
  branch?: string;
  head?: string;
  identity?: string;
}

export function fakeSourceEvidence(runId: string, bare: string, opts: FakeEvidenceOptions = {}): OperatorSourceEvidence {
  const clonePath = opts.clonePath ?? path.join(path.dirname(path.dirname(bare)), "runner", "fixture", "issue-3105");
  const branch = opts.branch ?? "agent/issue-3105";
  const head = opts.head ?? "a".repeat(40);
  const identity = opts.identity ?? "1:42";
  const source = {
    barePath: bare, runId, branch, key: "issue-3105", kind: "canonical", generation: "none", attempt: "canonical", ledgerState: "none",
    journalState: "plain", markerState: "none", presence: "present", descriptor: "d".repeat(64), allocatedBytes: 100, bytesState: "measured",
    sources: [{ clonePath, attempt: "canonical", generation: "none", presence: "present", ledgerState: "none", live: false, allocatedBytes: 100 }],
    // The aggregate view never reports the run's own local records as a blocker; pins are consumers only.
    consumers: ["run_state:not_read", "owed_pins:1"],
    localRecords: [{ kind: "recovery", generation: 7, name: "record.json", resolution: "recovery record owner disposition" }],
    holds: { state: "not_read", ids: [] }, disposition: undefined,
  };
  return {
    e1: { kind: "verified", heads: [head], foreignOwners: [], clones: [{ clonePath, branch, runId, head, identity }] },
    e2: { sources: [source], partial: false, unreadable: 0, partialCauses: {}, sampledAt: "2026-01-01T00:00:00.000Z" },
  } as unknown as OperatorSourceEvidence;
}
