import type { ClaimResponse } from "./protocol.js";

export const TASK_CHECKPOINT_COORDINATES_CAPABILITY = "task_checkpoint_coordinates_v1";

export interface CheckpointCoordinates {
  readonly version: 1;
  readonly work_branch: string;
  readonly checkpoint_key: string;
  readonly run_id: string;
  readonly user_id: string;
  readonly repo_id: string;
  readonly kind: "issue" | "self_improve" | "task";
  readonly issue_iid: number | null;
  readonly provenance: readonly string[];
}

const uuid = (v: unknown): v is string =>
  typeof v === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(v);

/** Consume the server's authorization; SQL, not this parser, proves graph eligibility.
 * The outer claim has no independent owner ID: user_id is checked for syntax only.
 * A present descriptor is the server's capability acknowledgement. */
export function checkpointCoordinatesForClaim(claim: ClaimResponse): CheckpointCoordinates | undefined {
  const value = claim.checkpoint_coordinates;
  const kind = claim.kind ?? "issue";
  if (value === undefined) {
    // Legacy tasks never published checkpoints. A named anchor or unresolved
    // write cannot be downgraded to branch-only execution by an older API.
    if (kind === "task" && (claim.checkpoint_tip != null ||
        claim.checkpoint_attested_source != null || claim.checkpoint_write_unresolved === true))
      throw new Error("task checkpoint recovery requires authorized coordinates");
    if (kind === "issue" && Number.isSafeInteger(claim.issue_iid) && claim.issue_iid! > 0)
      return legacy("agent/issue-" + claim.issue_iid);
    if (kind === "self_improve") return legacy("uzi/self-improve/" + claim.run_id);
    return undefined;
  }
  const fail = (): never => { throw new Error("invalid authorized checkpoint coordinates"); };
  if (!value || typeof value !== "object" || Array.isArray(value)) return fail();
  const p = value as Record<string, unknown>;
  if (p.version !== 1 || !uuid(p.run_id) || p.run_id !== claim.run_id ||
      !uuid(p.user_id) || !uuid(p.repo_id) || p.repo_id !== claim.repo.id ||
      p.kind !== kind || !["issue", "self_improve", "task"].includes(kind) ||
      p.issue_iid !== (claim.issue_iid ?? null) ||
      (p.issue_iid !== null && (!Number.isSafeInteger(p.issue_iid) || (p.issue_iid as number) <= 0)) ||
      !Array.isArray(p.provenance) || p.provenance.length < 1 || p.provenance.length > 64 ||
      !p.provenance.every(uuid) || new Set(p.provenance).size !== p.provenance.length ||
      p.provenance[0] !== claim.run_id) return fail();
  const branch = kind === "task" ? "uzi/task/" + p.provenance.at(-1)
    : kind === "issue" ? "agent/issue-" + claim.issue_iid : "uzi/self-improve/" + claim.run_id;
  const key = kind === "task" ? "uzi/task/" + claim.run_id : branch;
  if (p.work_branch !== branch || p.checkpoint_key !== key ||
      (claim.branch != null && claim.branch !== branch) ||
      (kind === "task" && claim.branch !== branch) ||
      (kind !== "task" && p.provenance.length !== 1)) return fail();
  return Object.freeze({ ...p, provenance: Object.freeze([...p.provenance]) }) as unknown as CheckpointCoordinates;

  function legacy(branch: string): CheckpointCoordinates {
    return Object.freeze({
      version: 1, work_branch: branch, checkpoint_key: branch, run_id: claim.run_id,
      user_id: "", repo_id: claim.repo.id, kind: kind as "issue" | "self_improve",
      issue_iid: claim.issue_iid ?? null, provenance: Object.freeze([claim.run_id]),
    });
  }
}
