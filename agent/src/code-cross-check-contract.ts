import type { ClaimPlanCrossCheck, ClaimCodeCrossCheck } from "./protocol.js";

export interface CodeFinding {
  id: string;
  severity: "critical" | "major" | "minor";
  path: string;
  line: number;
  title: string;
  detail: string;
}
export interface CodeCrossCheckDisposition {
  finding_id: string;
  disposition: "addressed" | "declined" | "not_reported";
  reason: string;
}
export type CodeCrossCheckDispositionBatch = Array<Omit<CodeCrossCheckDisposition, "disposition"> & {
  disposition: "addressed" | "declined";
}>;
export interface CodeCrossCheckRecord {
  stage: "code";
  round: 1;
  candidate_generation: number;
  base_commit: string | null;
  head_commit: string | null;
  candidate_digest: string;
  checker_run_id: string | null;
  checker_harness: string | null;
  checker_model: string | null;
  checker_effort: string | null;
  outcome: "pending" | "completed" | "failed";
  reason_class: string | null;
  findings: CodeFinding[];
  interrupted_at: string | null;
  // Optional for existing M1 worker fixtures; decoded legacy statuses normalize to null.
  dispositions?: CodeCrossCheckDisposition[] | null;
  finalized_at?: string | null;
  deadline_at: string;
}
export type CodeCrossCheckStatus = CodeCrossCheckRecord | { stage: "code"; result: "no_row" };

export interface CodeSnapshotCleanup {
  protocol: "code_snapshot_cleanup_v1";
  lead_run_id: string;
  head_commit: string | null;
  outcome: "pending" | "completed" | "failed";
  lead_status: string;
  owned_by_worker: boolean;
  checker_run_id: string;
  checker_claim_generation: number;
  checker_status: string;
}

export function decodeCodeSnapshotCleanup(v: unknown): CodeSnapshotCleanup {
  const uuid = (s: unknown) => typeof s === "string" && s.length === 36 &&
    /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(s);
  const states = ["queued", "claimed", "running", "awaiting_approval", "awaiting_input",
    "awaiting_followup", "limit_wait", "recovery_wait", "paused", "completed", "failed", "cancelled"];
  if (!record(v) || Object.keys(v).sort().join(",") !==
    "checker_claim_generation,checker_run_id,checker_status,head_commit,lead_run_id,lead_status,outcome,owned_by_worker,protocol" ||
    v.protocol !== "code_snapshot_cleanup_v1" || !uuid(v.lead_run_id) || !uuid(v.checker_run_id) ||
    (v.head_commit !== null && (typeof v.head_commit !== "string" || v.head_commit.length !== 40 || !/^[a-f0-9]{40}$/.test(v.head_commit))) ||
    !["pending", "completed", "failed"].includes(v.outcome as string) ||
    !states.includes(v.lead_status as string) || !states.includes(v.checker_status as string) ||
    typeof v.owned_by_worker !== "boolean" || !Number.isSafeInteger(v.checker_claim_generation) ||
    (v.checker_claim_generation as number) <= 0) throw new Error("invalid code snapshot cleanup metadata");
  return v as unknown as CodeSnapshotCleanup;
}

function record(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
function utf8(v: unknown): v is string {
  return typeof v === "string" && Buffer.from(v, "utf8").toString("utf8") === v;
}
/** Stage-discriminated wire validation shared by the production claim client and runner. */
export function decodeCrossCheckClaim(v: unknown): ClaimPlanCrossCheck | ClaimCodeCrossCheck {
  const invalid = (): never => { throw new Error("invalid cross-check claim"); };
  const bounded = (s: unknown, cap: number) => utf8(s) && Buffer.byteLength(s) <= cap;
  if (!record(v) || !["plan", "code"].includes(v.stage as string)
    || !bounded(v.lead_run_id, 128) || !/^[A-Za-z0-9_-]+$/.test(v.lead_run_id as string)
    || typeof v.base_commit !== "string" || !/^[a-f0-9]{40}$/.test(v.base_commit)
    || !bounded(v.candidate_digest, 128)
    || typeof v.deadline_at !== "string" || !Number.isFinite(Date.parse(v.deadline_at))
    || !bounded(v.plan_md, 256 * 1024)) return invalid();
  if (v.stage === "code") {
    if (!/^[A-Za-z0-9_-]+(?![\s\S])/.test(v.lead_run_id as string) || v.base_commit.length !== 40 || (v.candidate_digest as string).length !== 64 || v.round !== 1 || !/^[a-f0-9]{64}$/.test(v.candidate_digest as string) || typeof v.head_commit !== "string" || v.head_commit.length !== 40 || !/^[a-f0-9]{40}$/.test(v.head_commit)
      || v.code_context === undefined || Buffer.byteLength(JSON.stringify(v.code_context)) > 512 * 1024
      || (v.guidance_snapshot !== undefined && !bounded(v.guidance_snapshot, 8192))) return invalid();
    return { ...v, guidance_snapshot: v.guidance_snapshot ?? "" } as unknown as ClaimCodeCrossCheck;
  }
  if (!Number.isInteger(v.round) || (v.round as number) < 1 || (v.round as number) > 5
    || !v.plan_md || !bounded(v.planning_diff, 512 * 1024)
    || !Array.isArray(v.milestones) || v.milestones.length > 64
    || Buffer.byteLength(JSON.stringify(v.milestones)) > 256 * 1024
    || !["required_capabilities", "required_tools"].every((key) =>
      Array.isArray(v[key]) && v[key].length <= 64 && v[key].every((s: unknown) => bounded(s, 8192)))
    || !bounded(v.size_class, 128)) return invalid();
  return v as unknown as ClaimPlanCrossCheck;
}

const codeFindingID = (v: unknown): v is string =>
  typeof v === "string" && /^[A-Za-z0-9_-]{1,64}(?![\s\S])/.test(v);

export function decodeCodeCrossCheckDispositions(v: unknown, findings?: CodeFinding[]): CodeCrossCheckDisposition[] {
  const invalid = (): never => { throw new Error("invalid code cross-check dispositions"); };
  if (!Array.isArray(v) || v.length > 20) return invalid();
  const known = findings === undefined ? undefined : new Set(findings.map(f => f.id));
  const seen = new Set<string>();
  for (const d of v) {
    if (!record(d) || Object.keys(d).sort().join(",") !== "disposition,finding_id,reason"
      || !codeFindingID(d.finding_id) || seen.has(d.finding_id)
      || (known !== undefined && !known.has(d.finding_id)) || !utf8(d.reason)
      || Buffer.byteLength(d.reason) > 1024
      || (d.disposition === "not_reported"
        ? known === undefined || d.reason !== ""
        : !["addressed", "declined"].includes(d.disposition as string) || /^\p{White_Space}*$/u.test(d.reason))) return invalid();
    seen.add(d.finding_id);
  }
  return v as CodeCrossCheckDisposition[];
}

export function decodeCodeFindings(v: unknown): CodeFinding[] {
  const invalid = (): never => { throw new Error("invalid code cross-check findings"); };
  if (!Array.isArray(v) || v.length > 20 || Buffer.byteLength(JSON.stringify(v)) > 32768) return invalid();
  const seen = new Set<string>();
  for (const f of v) {
    if (!record(f) || !codeFindingID(f.id) || seen.has(f.id)
      || !["critical", "major", "minor"].includes(f.severity as string)
      || !utf8(f.path) || !utf8(f.title) || !utf8(f.detail)
      || !Number.isSafeInteger(f.line) || (f.line as number) < 0 || (f.line as number) > 2147483647
      || Object.keys(f).sort().join(",") !== "detail,id,line,path,severity,title"
      || Buffer.byteLength(JSON.stringify(f)) > 2048) return invalid();
    seen.add(f.id);
  }
  return v as CodeFinding[];
}
export function decodeCodeCrossCheckStatus(v: unknown): CodeCrossCheckStatus {
  const invalid = (): never => { throw new Error("invalid code cross-check response"); };
  if (!record(v) || v.stage !== "code") return invalid();
  if (v.result === "no_row") {
    if (Object.keys(v).sort().join(",") !== "result,stage") return invalid();
    return { stage: "code", result: "no_row" };
  }
  const sha = (s: unknown) => typeof s === "string" && s.length === 40 && /^[a-f0-9]{40}$/.test(s);
  const date = (s: unknown) => typeof s === "string" && Number.isFinite(Date.parse(s));
  if ((v.reason_class === "interrupted" && !date(v.interrupted_at)) || v.round !== 1 || !Number.isSafeInteger(v.candidate_generation) || (v.candidate_generation as number) < 1
    || !["pending", "completed", "failed"].includes(v.outcome as string) || !date(v.deadline_at)
    || (v.interrupted_at !== null && !date(v.interrupted_at)) || (v.finalized_at !== undefined && v.finalized_at !== null && !date(v.finalized_at))
    || !["checker_run_id", "checker_harness", "checker_model", "checker_effort", "reason_class"].every(
      (k) => v[k] === null || utf8(v[k]))
    || !utf8(v.candidate_digest)) return invalid();
  if (!(sha(v.base_commit) && sha(v.head_commit))) {
    if (v.base_commit !== null || v.head_commit !== null || v.outcome !== "failed"
      || !["snapshot_failed", "worker_unsupported"].includes(v.reason_class as string)) return invalid();
  } else if ((v.candidate_digest as string).length !== 64 || !/^[a-f0-9]{64}$/.test(v.candidate_digest as string)) return invalid();
  const findings = decodeCodeFindings(v.findings);
  if (v.outcome !== "completed" && findings.length !== 0) return invalid();
  if (v.outcome === "completed" && v.reason_class !== null && v.reason_class !== "interrupted") return invalid();
  // M1 statuses predate dispositions. They cannot establish a finalized repair pass.
  const finalized_at = v.dispositions === undefined ? null : v.finalized_at ?? null;
  let dispositions: CodeCrossCheckDisposition[] | null = null;
  if (v.dispositions !== undefined && v.dispositions !== null) {
    dispositions = decodeCodeCrossCheckDispositions(v.dispositions, findings);
    if (v.interrupted_at !== null || v.outcome !== "completed" || finalized_at === null
      || dispositions.length !== findings.length) return invalid();
  } else if (v.dispositions === null && finalized_at !== null && v.interrupted_at === null) return invalid();
  // WorkerCodeCrossCheckStatus hides superseded findings and dispositions.
  if (v.interrupted_at !== null && findings.length !== 0) return invalid();
  return { ...v, findings, dispositions, finalized_at } as unknown as CodeCrossCheckRecord;
}
