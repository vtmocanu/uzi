import { MaintenanceBudget } from "./maintenance-budget.js";
import type { CheckpointRange, GitCache } from "./git.js";
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
  /** Task checkpoint evidence belongs to this exact run key, never merely its branch. */
  checkpointKey?: string;
  extraFloors: string[];
  /** Frozen from a provider-bound run: the held range MUST be scanned for the run's provider key.
   *  A caller with no claim (so no key forms) cannot scan and fails closed instead. Absent for a vendor run. */
  providerKeyScan?: true;
}
export type HeldGit = Pick<GitCache, "resolveCheckpointRange" | "secretScanCheckpointRange" |
  "ancestry" | "rangeTouchesWorkflows" | "changedFilesNoRenames" | "checkpointPack">;

export function validFrozenHeldPolicy(value: unknown, owner?: { kind: string; runId: string }): value is FrozenHeldPolicy {
  if (!value || typeof value !== "object") return false;
  const p = value as FrozenHeldPolicy;
  const sha = (s: unknown) => typeof s === "string" && /^[0-9a-f]{40}$/.test(s);
  const strings = (s: unknown): s is string[] => Array.isArray(s) && s.every(x => typeof x === "string");
  return p.version === 1 && typeof p.blocked === "boolean" && typeof p.everKnown === "boolean" &&
    (p.cleanTip === undefined || sha(p.cleanTip)) && Array.isArray(p.flaggedCommits) &&
    p.flaggedCommits.length <= 256 && p.flaggedCommits.every(sha) &&
    (p.ciGlobs === null || strings(p.ciGlobs)) &&
    (p.confirmedTip === undefined || sha(p.confirmedTip)) &&
    (p.checkpointKey === undefined || (typeof p.checkpointKey === "string" &&
      /^uzi\/task\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(p.checkpointKey))) &&
    Array.isArray(p.extraFloors) && p.extraFloors.every(sha) &&
    (p.providerKeyScan === undefined || p.providerKeyScan === true) &&
    (!owner || ((p.checkpointKey === undefined ||
      (owner.kind === "task" && p.checkpointKey === `uzi/task/${owner.runId}`)) &&
      (owner.kind !== "task" || p.confirmedTip === undefined || p.checkpointKey !== undefined)));
}

type FrozenHeldInput = {
  git: HeldGit; barePath: string; branch: string; sourceSha: string; policy: FrozenHeldPolicy;
  deadlineMs: number; maxBytes?: number; defaultBranch?: string;
  /** The encoded forms of the run's provider key; `[]` for a vendor run or a caller with no claim. */
  literals: readonly string[];
  onScan?: (scan: Awaited<ReturnType<HeldGit["secretScanCheckpointRange"]>>) => void;
  remediationHold?: () => Promise<boolean>;
  ciHold?: () => Promise<"clear" | "blocked" | "unknown">;
};

/** Optional digest and per-commit CI methods, required only on the pinned path. */
export interface TerminalDigestGit {
  terminalScanDigest?(barePath: string, range: CheckpointRange, opts: { deadlineMs: number }): Promise<string | null>;
  terminalTransportDigest?(pack: Buffer, opts: { deadlineMs: number }): Promise<string | null>;
  changedCiPathsPerCommit?(barePath: string, range: CheckpointRange, targetSha: string,
    opts: { deadlineMs: number; flag?: (paths: string[]) => boolean }): Promise<Array<{ commit: string; paths: string[] }> | null>;
}

interface OverRangeOpts {
  perCommitCi?: boolean; ciTarget?: string;
  wantDigests?: boolean; expectDigests?: { scan: string; transport: string };
}
type OverRangeResult = { pack: Buffer; digests?: { scan: string; transport: string } } | { reason: string };

/** One range flows through scan, guards and pack. The pack cap bounds buffered bytes;
 * GitCache bounds each child. No retry occurs, and a failed guard blocks publication. */
export async function prepareFrozenHeld(input: FrozenHeldInput): Promise<{ pack: Buffer } | { reason: string }> {
  const run = async () => {
    const range = await resolveFrozenRange(input);
    if (!range) return { reason: "range_unresolved" };
    return prepareOverRange(input, range, {}) as Promise<{ pack: Buffer } | { reason: string }>;
  };
  if (MaintenanceBudget.current()) return run();
  // Issue #4140: this clock bounds preparation only. A producer scope never becomes the
  // enclosing attempt's budget, so the held POST that follows keeps its own allowance.
  return MaintenanceBudget.runProducer(run, { durationMs: input.deadlineMs });
}

/** Pinned variant: one budget around optional resolution plus preparation over a supplied range. */
export async function prepareFrozenHeldPinned(
  input: FrozenHeldInput & { perCommitCi?: boolean; ciTarget?: string; expectDigests?: { scan: string; transport: string } },
  range: CheckpointRange | "resolve",
): Promise<{ pack: Buffer; range: CheckpointRange; digests: { scan: string; transport: string } } | { reason: string; range?: CheckpointRange }> {
  const run = async () => {
    // A supplied range comes only from a worker-persisted record of this same function; its tip must be the source.
    if (range !== "resolve" && range.tipSha !== input.sourceSha) return { reason: "range_tip_mismatch" };
    const r = range === "resolve" ? await resolveFrozenRange(input) : range;
    if (!r) return { reason: "range_unresolved" };
    const out = await prepareOverRange(input, r, {
      perCommitCi: input.perCommitCi, ciTarget: input.ciTarget,
      wantDigests: true, expectDigests: input.expectDigests,
    });
    if ("reason" in out) return { reason: out.reason, range: r };
    if (!out.digests) return { reason: "digest_unavailable", range: r };
    return { pack: out.pack, range: r, digests: out.digests };
  };
  if (MaintenanceBudget.current()) return run();
  return MaintenanceBudget.runProducer(run, { durationMs: input.deadlineMs });
}

async function resolveFrozenRange(input: FrozenHeldInput): Promise<CheckpointRange | null | undefined> {
  const budget = MaintenanceBudget.current();
  budget?.check();
  const range = await input.git.resolveCheckpointRange(input.barePath, input.branch, {
    tipSha: input.sourceSha, confirmedTip: input.policy.confirmedTip, extraFloors: input.policy.extraFloors,
    defaultBranch: input.defaultBranch,
  });
  budget?.check();
  return range;
}

async function prepareOverRange(input: FrozenHeldInput, range: CheckpointRange, opts: OverRangeOpts): Promise<OverRangeResult> {
  const { git, barePath, branch, sourceSha, policy } = input;
  const dg = git as HeldGit & TerminalDigestGit;
  const budget = MaintenanceBudget.current();
  const check = () => budget?.check();
  const deadlineMs = () => {
    check();
    return Math.floor(Math.min(input.deadlineMs, budget?.remainingMs() ?? input.deadlineMs));
  };
  // A provider-bound policy with no key forms to scan for cannot prove the range clean: the work stays local.
  if (policy.providerKeyScan === true && input.literals.length === 0) return { reason: "scan_untrusted" };
  const scan = await git.secretScanCheckpointRange(barePath, range, { deadlineMs: deadlineMs(), literals: input.literals });
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
    // A null path list is an unknown (a git failure), not a verdict: it keeps the held refusal but is
    // a distinct reason so the same-pass closure grant never reads it as a stated refusal.
    if (paths === null) return { reason: "ci_config_unknown" };
    if (flagCIConfigPathsRaw(paths, policy.ciGlobs).length) return { reason: "ci_config_guard" };
    if (opts.perCommitCi) {
      if (!opts.ciTarget || !dg.changedCiPathsPerCommit) return { reason: "ci_config_unknown" };
      const globs = policy.ciGlobs;
      const per = await dg.changedCiPathsPerCommit(barePath, range, opts.ciTarget, {
        deadlineMs: deadlineMs(), flag: (paths) => flagCIConfigPathsRaw(paths, globs).length > 0,
      });
      check();
      if (per === null) return { reason: "ci_config_unknown" };
      for (const c of per) if (flagCIConfigPathsRaw(c.paths, policy.ciGlobs).length) return { reason: "ci_config_guard" };
    }
  }
  check();
  const ci = await input.ciHold?.();
  check();
  if (ci === "unknown") return { reason: "ci_config_unknown" };
  if (ci === "blocked") return { reason: "ci_config_guard" };
  let scanDigest = "";
  if (opts.wantDigests) {
    if (!dg.terminalScanDigest || !dg.terminalTransportDigest) return { reason: "digest_unavailable" };
    const d = await dg.terminalScanDigest(barePath, range, { deadlineMs: deadlineMs() });
    check();
    if (d === null) return { reason: "digest_unavailable" };
    if (opts.expectDigests && opts.expectDigests.scan !== d) return { reason: "digest_mismatch" };
    scanDigest = d;
  }
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
  const pack = Buffer.concat(chunks);
  if (!opts.wantDigests) return { pack };
  const transport = await dg.terminalTransportDigest!(pack, { deadlineMs: deadlineMs() });
  check();
  if (transport === null) return { reason: "digest_unavailable" };
  if (opts.expectDigests && opts.expectDigests.transport !== transport) return { reason: "digest_mismatch" };
  return { pack, digests: { scan: scanDigest, transport } };
}
