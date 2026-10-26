import { createHash } from "node:crypto";
import { decodeCodeCoverage, type CodeCoverage } from "./code-cross-check-coverage.js";
import { parseSemanticCandidate, SEMANTIC_ENVELOPE_MAX_BYTES } from "./semantic-plan.js";
import type { ClaimPlanCrossCheck, ClaimCodeCrossCheck } from "./protocol.js";

/** Cap on the repository-instructions text carried through a code cross-check; the same 64 KiB
 *  as REPO_INSTRUCTIONS_MAX_BYTES (repo-instructions.ts), restated here so this wire contract
 *  imports no filesystem code. */
const CODE_CROSS_CHECK_INSTRUCTIONS_MAX_BYTES = 64 * 1024;
const sha256Hex = (s: string): string => createHash("sha256").update(s, "utf8").digest("hex");

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
  coverage?: CodeCoverage | null;
  stage: "code";
  round: 1;
  candidate_generation: number;
  base_commit: string | null;
  head_commit: string | null;
  repo_instructions_commit?: string | null;
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
  // Frozen guidance/instruction metadata the server sends on every row. Optional so a row from an
  // older server still decodes; when present each is validated by decodeCodeCrossCheckStatus.
  custom_guidance_applied?: boolean;
  guidance_digest?: string;
  repo_instructions_enabled?: boolean;
  repo_instructions_digest?: string;
}
/** The repository-instructions fields of a code cross-check submit: consent off sends no text. */
export type CodeCrossCheckInstructions =
  | { repo_instructions_enabled: false }
  | { repo_instructions_enabled: true; repo_instructions_text: string };
/** The no-row reply carries the live repository-instructions consent (WorkerCodeCrossCheckStatus). */
type CodeCrossCheckNoRow = { stage: "code"; result: "no_row"; repo_instructions_enabled: boolean };
export type CodeCrossCheckStatus = CodeCrossCheckRecord | CodeCrossCheckNoRow;

/**
 * The server's answer for snapshot cleanup. `row_present: false` is a valid "no code row"
 * answer and `child_present: false` on a present row a valid "no child" answer; both are
 * explicit so a worker can tell them from a failed lookup (which never decodes).
 * `lead_claim_generation` is cross_checks.lead_claim_generation: the lead claim generation
 * the snapshot was pinned at (0 only when there is no row). `checker_claim_generation` is 0
 * for a child that was never claimed.
 */
export interface CodeSnapshotCleanup {
  protocol: "code_snapshot_cleanup_v1";
  lead_run_id: string;
  lead_status: string;
  owned_by_worker: boolean;
  row_present: boolean;
  lead_claim_generation: number;
  head_commit: string | null;
  outcome: "pending" | "completed" | "failed" | null;
  child_present: boolean;
  checker_run_id: string | null;
  checker_claim_generation: number;
  checker_status: string | null;
}

export function decodeCodeSnapshotCleanup(v: unknown): CodeSnapshotCleanup {
  const invalid = (): never => { throw new Error("invalid code snapshot cleanup metadata"); };
  const uuid = (s: unknown) => typeof s === "string" && s.length === 36 &&
    /^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(s);
  const states = ["queued", "claimed", "running", "awaiting_approval", "awaiting_input",
    "awaiting_followup", "limit_wait", "recovery_wait", "paused", "completed", "failed", "cancelled"];
  const generation = (n: unknown) => Number.isSafeInteger(n) && (n as number) >= 0;
  if (!record(v) || Object.keys(v).sort().join(",") !==
    "checker_claim_generation,checker_run_id,checker_status,child_present,head_commit,lead_claim_generation,lead_run_id,lead_status,outcome,owned_by_worker,protocol,row_present" ||
    v.protocol !== "code_snapshot_cleanup_v1" || !uuid(v.lead_run_id) || !states.includes(v.lead_status as string) ||
    typeof v.owned_by_worker !== "boolean" || typeof v.row_present !== "boolean" || typeof v.child_present !== "boolean" ||
    !generation(v.lead_claim_generation) || !generation(v.checker_claim_generation)) return invalid();
  if (!v.row_present) {
    // A valid no-row answer carries nothing else: no head, outcome, generation or child.
    if (v.lead_claim_generation !== 0 || v.head_commit !== null || v.outcome !== null || v.child_present ||
        v.checker_run_id !== null || v.checker_claim_generation !== 0 || v.checker_status !== null) return invalid();
    return v as unknown as CodeSnapshotCleanup;
  }
  if ((v.lead_claim_generation as number) <= 0 ||
      (v.head_commit !== null && (typeof v.head_commit !== "string" || !/^[a-f0-9]{40}$/.test(v.head_commit))) ||
      !["pending", "completed", "failed"].includes(v.outcome as string)) return invalid();
  if (!v.child_present) {
    if (v.checker_run_id !== null || v.checker_claim_generation !== 0 || v.checker_status !== null) return invalid();
  } else if (!uuid(v.checker_run_id) || !states.includes(v.checker_status as string)) return invalid();
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
  if ("semantic_identity" in v) return invalid();
  if (v.candidate_identity_version !== undefined || v.semantic_candidate !== undefined) {
    if (v.stage !== "plan" || v.candidate_identity_version !== "semantic_v1" ||
        Buffer.byteLength(JSON.stringify(v.semantic_candidate) ?? "") > SEMANTIC_ENVELOPE_MAX_BYTES) return invalid();
    try {
      if (parseSemanticCandidate(v.semantic_candidate).digest !== v.candidate_digest) return invalid();
    } catch { return invalid(); }
  }
  if (v.stage === "code") {
    if (v.coverage_supported !== undefined && typeof v.coverage_supported !== "boolean") return invalid();
    if (v.repo_instructions_commit !== undefined && (typeof v.repo_instructions_commit !== "string"
      || v.repo_instructions_commit.length !== 40 || !/^[a-f0-9]{40}$/.test(v.repo_instructions_commit))) return invalid();
    if (!/^[A-Za-z0-9_-]+(?![\s\S])/.test(v.lead_run_id as string) || v.base_commit.length !== 40 || (v.candidate_digest as string).length !== 64 || v.round !== 1 || !/^[a-f0-9]{64}$/.test(v.candidate_digest as string) || typeof v.head_commit !== "string" || v.head_commit.length !== 40 || !/^[a-f0-9]{40}$/.test(v.head_commit)
      || v.code_context === undefined || Buffer.byteLength(JSON.stringify(v.code_context)) > 512 * 1024
      || (v.guidance_snapshot !== undefined && !bounded(v.guidance_snapshot, 8192))) return invalid();
    // Frozen owner guidance and repository instructions (PRD #2170). The server omits empty
    // values, so an absent text hashes as "". A digest that disagrees with the text it names, or
    // instruction text while instructions are disabled, means the claim was altered in transit.
    const guidance = (v.guidance_snapshot ?? "") as string;
    const instructions = v.repo_instructions_text ?? "";
    const digestOK = (digest: unknown, text: string) =>
      digest === undefined ? text === "" : typeof digest === "string" && /^[a-f0-9]{64}$/.test(digest) && digest === sha256Hex(text);
    if ((v.repo_instructions_enabled !== undefined && typeof v.repo_instructions_enabled !== "boolean")
      || (v.custom_guidance_applied !== undefined && typeof v.custom_guidance_applied !== "boolean")
      || !bounded(instructions, CODE_CROSS_CHECK_INSTRUCTIONS_MAX_BYTES)
      || (instructions !== "" && v.repo_instructions_enabled !== true)
      || !digestOK(v.guidance_digest, guidance) || !digestOK(v.repo_instructions_digest, instructions as string)
      || (v.custom_guidance_applied !== undefined && v.custom_guidance_applied !== (guidance !== ""))) return invalid();
    return { ...v, guidance_snapshot: guidance } as unknown as ClaimCodeCrossCheck;
  }
  if ("repo_instructions_commit" in v || "coverage_supported" in v) return invalid();
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
    if (Object.keys(v).sort().join(",") !== "repo_instructions_enabled,result,stage"
      || typeof v.repo_instructions_enabled !== "boolean") return invalid();
    return { stage: "code", result: "no_row", repo_instructions_enabled: v.repo_instructions_enabled };
  }
  const sha = (s: unknown) => typeof s === "string" && s.length === 40 && /^[a-f0-9]{40}$/.test(s);
  if (v.repo_instructions_commit !== undefined && v.repo_instructions_commit !== null && !sha(v.repo_instructions_commit)) return invalid();
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
  const hex64 = (x: unknown) => typeof x === "string" && /^[a-f0-9]{64}$/.test(x);
  if ((v.custom_guidance_applied !== undefined && typeof v.custom_guidance_applied !== "boolean")
    || (v.repo_instructions_enabled !== undefined && typeof v.repo_instructions_enabled !== "boolean")
    || (v.guidance_digest !== undefined && !hex64(v.guidance_digest))
    || (v.repo_instructions_digest !== undefined && !hex64(v.repo_instructions_digest))) return invalid();
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
  return { ...v, findings, dispositions, finalized_at, coverage: decodeCodeCoverage(v.coverage) } as unknown as CodeCrossCheckRecord;
}
