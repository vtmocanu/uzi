import { MaintenanceBudget } from "./maintenance-budget.js";
import type { GitCache } from "./git.js";
import { flagCIConfigPathsRaw } from "./ci-config-guard.js";

/** Authenticated at freeze. No paths or finding text are needed to retain a remediation hold. */
export interface FrozenHeldPolicy {
  version: 1;
  blocked: boolean;
  everKnown: boolean;
  cleanTip?: string;
  flaggedCommits: string[];
  ciGlobs: string[] | null;
  confirmedTip?: string;
  extraFloors: string[];
}
export type HeldGit = Pick<GitCache, "resolveCheckpointRange" | "secretScanCheckpointRange" |
  "ancestry" | "rangeTouchesWorkflows" | "changedFilesNoRenames" | "checkpointPack">;

export function validFrozenHeldPolicy(value: unknown): value is FrozenHeldPolicy {
  if (!value || typeof value !== "object") return false;
  const p = value as FrozenHeldPolicy;
  const sha = (s: unknown) => typeof s === "string" && /^[0-9a-f]{40}$/.test(s);
  const strings = (s: unknown): s is string[] => Array.isArray(s) && s.every(x => typeof x === "string");
  return p.version === 1 && typeof p.blocked === "boolean" && typeof p.everKnown === "boolean" &&
    (p.cleanTip === undefined || sha(p.cleanTip)) && Array.isArray(p.flaggedCommits) &&
    p.flaggedCommits.length <= 256 && p.flaggedCommits.every(sha) &&
    (p.ciGlobs === null || strings(p.ciGlobs)) &&
    (p.confirmedTip === undefined || sha(p.confirmedTip)) &&
    Array.isArray(p.extraFloors) && p.extraFloors.every(sha);
}

/** One range flows through scan, guards and pack. The pack cap bounds buffered bytes;
 * GitCache bounds each child. No retry occurs, and a failed guard blocks publication. */
export async function prepareFrozenHeld(input: {
  git: HeldGit; barePath: string; branch: string; sourceSha: string; policy: FrozenHeldPolicy;
  deadlineMs: number; maxBytes?: number;
  onScan?: (scan: Awaited<ReturnType<HeldGit["secretScanCheckpointRange"]>>) => void;
  remediationHold?: () => Promise<boolean>;
  ciHold?: () => Promise<boolean>;
}): Promise<{ pack: Buffer } | { reason: string }> {
  const { git, barePath, branch, sourceSha, policy } = input;
  const budget = MaintenanceBudget.current();
  const check = () => budget?.check();
  const deadlineMs = () => {
    check();
    return Math.floor(Math.min(input.deadlineMs, budget?.remainingMs() ?? input.deadlineMs));
  };
  check();
  const range = await git.resolveCheckpointRange(barePath, branch, {
    tipSha: sourceSha, confirmedTip: policy.confirmedTip, extraFloors: policy.extraFloors,
  });
  check();
  if (!range) return { reason: "range_unresolved" };
  const scan = await git.secretScanCheckpointRange(barePath, range, { deadlineMs: deadlineMs() });
  check();
  input.onScan?.(scan);
  check();
  if (scan.findings.length) return { reason: "secret_found" };
  if (!scan.trusted) return { reason: "scan_untrusted" };
  if (policy.blocked || (policy.everKnown && sourceSha !== policy.cleanTip)) return { reason: "secret_remediation_pending" };
  for (const sha of policy.flaggedCommits) {
    check();
    const ancestry = await git.ancestry(barePath, sha, sourceSha);
    check();
    if (ancestry !== "divergent") return { reason: "secret_remediation_pending" };
  }
  check();
  const remediation = await input.remediationHold?.();
  check();
  if (remediation) return { reason: "secret_remediation_pending" };
  const workflows = await git.rangeTouchesWorkflows(barePath, range, { deadlineMs: deadlineMs() });
  check();
  if (workflows !== "clean") return { reason: workflows === "touches" ? "workflow_change" : "workflow_check_unknown" };
  if (policy.ciGlobs !== null) {
    const paths = await git.changedFilesNoRenames(barePath, sourceSha);
    check();
    if (paths === null || flagCIConfigPathsRaw(paths, policy.ciGlobs).length) return { reason: "ci_config_guard" };
  }
  check();
  const ci = await input.ciHold?.();
  check();
  if (ci) return { reason: "ci_config_guard" };
  const packed = await git.checkpointPack(barePath, branch, undefined, range, undefined, sourceSha);
  if (!packed) { check(); return { reason: "pack_tip_mismatch" }; }
  const chunks: Buffer[] = [];
  let size = 0, over = false;
  let code: number | null = null;
  let stopped = false;
  const abort = () => {
    if (stopped) return;
    stopped = true;
    packed.cancel?.();
    packed.pack.destroy();
  };
  budget?.signal.addEventListener("abort", abort, { once: true });
  try {
    check();
    if (packed.tipOid === sourceSha) {
      for await (const chunk of packed.pack) {
        check();
        const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array);
        size += buf.length;
        if (size > (input.maxBytes ?? 64 * 1024 * 1024)) { over = true; break; }
        chunks.push(buf);
      }
    }
  } finally {
    if (!packed.pack.readableEnded) {
      abort();
    }
    code = await packed.exited;
    budget?.signal.removeEventListener("abort", abort);
    check();
  }
  if (packed.tipOid !== sourceSha) return { reason: "pack_tip_mismatch" };
  if (over) return { reason: "pack_too_large" };
  if (code !== 0) return { reason: "pack_failed" };
  return { pack: Buffer.concat(chunks) };
}
