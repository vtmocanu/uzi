import { decodeCodeCrossCheckDispositions, type CodeCrossCheckDispositionBatch, type CodeCrossCheckRecord, type CodeCrossCheckStatus } from "./code-cross-check-contract.js";

export const CODE_DISPOSITIONS_TOOL = "mcp__uzi__report_cross_check_dispositions";
export interface CodeCrossCheckToolAccess {
  readonly generation: number;
  readonly codeCrossCheckRepairActive: () => boolean;
  readonly codeCrossCheckRepair: () => Promise<CodeCrossCheckRecord | undefined>;
  readonly reportCrossCheckDispositions: (batch: CodeCrossCheckDispositionBatch) => Promise<CodeCrossCheckStatus>;
}

/** The worker supplies identity; model arguments supply only unchanged finding IDs and reasons. */
export async function authorizeCodeDispositions(access: CodeCrossCheckToolAccess | undefined, args: unknown): Promise<CodeCrossCheckDispositionBatch> {
  if (!access || !access.codeCrossCheckRepairActive() || !Number.isSafeInteger(access.generation) || access.generation < 1) throw new Error("code repair is not active");
  if (!args || typeof args !== "object" || Array.isArray(args) || Object.keys(args).length !== 1 || !("dispositions" in args)) throw new Error("invalid dispositions");
  const row = await access.codeCrossCheckRepair();
  if (!access.codeCrossCheckRepairActive() || !row || row.stage !== "code" || row.round !== 1 || row.candidate_generation !== access.generation || row.interrupted_at !== null
    || row.outcome !== "completed" || typeof row.head_commit !== "string" || !/^[0-9a-f]{40}$/.test(row.head_commit)
    || typeof row.base_commit !== "string" || !/^[0-9a-f]{40}$/.test(row.base_commit) || row.findings.length === 0) throw new Error("code repair is not active");
  const batch = decodeCodeCrossCheckDispositions(args.dispositions, row.findings);
  if (batch.some(d => d.disposition === "not_reported")) throw new Error("invalid model disposition");
  return batch as CodeCrossCheckDispositionBatch;
}

export async function reportCodeDispositions(access: CodeCrossCheckToolAccess, args: unknown): Promise<void> {
  const batch = await authorizeCodeDispositions(access, args);
  if (!access.codeCrossCheckRepairActive()) throw new Error("code repair is not active");
  await access.reportCrossCheckDispositions(batch);
}
