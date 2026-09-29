// Typed run-kind profile table (PRD #983 M4b). Collapses the per-kind if-chains that
// used to live in runner.ts (clone-branch derivation, MR title, MR body) into one
// Record keyed over the RunKind union, so exhaustiveness is enforced by the Record and
// each kind's behaviour reads as one row instead of a scattered comparison. The clone-branch
// and title values were lifted verbatim from the branches they replaced; PRD #1798 (D14) then
// shrank each kind's MR body to the one-line `completionLine` the completion block renders.
//
// RUN_KINDS stays the wire type in protocol.ts (mirrored from the DB runs_kind_check
// constraint); it is imported here, not moved.

import { type RunKind, type ClaimResponse } from "./protocol.js";
import { codeSpan, urlOrCodeSpan } from "./pr-description.js";
import { selfImproveBranch } from "./self-improve.js";

/**
 * The run-kind default (a missing kind becomes `issue`), folded here from its four
 * former call sites.
 *
 * A run whose `kind` is absent is treated as an `issue` run. `kind` is optional on the
 * wire (an older server omits it; ClaimResponse.kind is `RunKind | undefined`), and
 * runner.ts, prompt.ts and sdk-executor.ts all defaulted a missing kind to `"issue"`
 * independently. The AUTHORITATIVE gate is the api's, where runs.kind is NOT NULL; this
 * worker-side default only has to agree with it, and a stricter fail-closed default would
 * silently break every test that omits kind. One function so the four sites cannot drift.
 */
export function resolveRunKind(kind: RunKind | undefined): RunKind {
  return kind ?? "issue";
}

/** The context a kind's `completionLine` renders from: the source branch and, for a task, the
 *  branch it was cut from. The agents line, sections, bridge sentence and footer are rendered
 *  around the line by the completion block (pr-description.ts renderCompletionBlock). */
interface CompletionLineCtx {
  branch: string;
  baseBranch?: string;
}

/**
 * One run kind's behaviour. Every field is OPTIONAL, and an absent field means "take
 * today's issue fall-through" — the issue arm is deliberately NEVER moved into a row, so
 * the issue behaviour stays the single richest fall-through at each caller.
 *
 * - `cloneBranch` — derive the runner clone's branch+slug; `undefined` (or a row with no
 *   `cloneBranch`) falls through to `createOrAttachRunnerClone` on issue_iid.
 * - `mrTitle` — the raw MR title (NO `[partial]` prefix; the caller applies that only to
 *   the trimmed issue title and the issue fallback). `undefined` falls through to
 *   `Work on issue #<iid>`.
 * - `completionLine` — PRD #1798 D14: the kind's ONE-line sentence in the PR's completion block;
 *   `undefined` falls through to the issue arm (the scopeCapped / gates / Closes arm). Branch and
 *   ref interpolations go through codeSpan (closing keywords broken, backticks fenced).
 */
interface RunKindProfile {
  cloneBranch?: (claim: ClaimResponse, runId: string) => { branch: string; slug: string } | undefined;
  mrTitle?: (claim: ClaimResponse) => string | undefined;
  completionLine?: (claim: ClaimResponse, ctx: CompletionLineCtx) => string | undefined;
}

/** The minimal identity needed to derive a run's canonical runner-clone { branch, slug }.
 *  Populated from a live claim (runnerCloneForClaim path) OR from a persisted owner-run
 *  identity (issue #1319 orphan validation), so ONE derivation serves both and cannot drift. */
export interface CloneIdentity {
  kind: RunKind;
  runId: string;
  issueIid?: number | null;
  branch?: string | null;
  pipelineId?: number | null;
  pipelineRef?: string | null;
  defaultBranch?: string | null;
}

/** Derive the canonical runner-clone { branch, slug } for a run, or `undefined` when the
 *  identity is insufficient/malformed (the caller fails closed). This is the SINGLE source
 *  of the per-kind branch/slug rule: RUN_KIND_PROFILES.*.cloneBranch delegates here, and the
 *  #1319 orphan validation feeds it the OWNER's persisted identity, so the two always agree.
 *  Mirrors the pre-#1319 verbatim per-kind logic exactly. */
export function deriveCloneKey(id: CloneIdentity): { branch: string; slug: string } | undefined {
  const slugify = (b: string) => b.replace(/\//g, "-");
  const kind = resolveRunKind(id.kind);
  switch (kind) {
    case "issue":
    case "chat":
    case "judge": {
      if (id.issueIid == null) return undefined;
      return { branch: `agent/issue-${id.issueIid}`, slug: `issue-${id.issueIid}` };
    }
    case "ci_fix": {
      if (id.pipelineRef == null) return undefined; // mirrors the `!claim.pipeline` guard
      const def = id.defaultBranch?.trim();
      if (def && id.pipelineRef === def) {
        if (id.pipelineId == null) return undefined; // cannot reconstruct ci-fix/pipeline-<id>
        const branch = `ci-fix/pipeline-${id.pipelineId}`;
        return { branch, slug: slugify(branch) };
      }
      return { branch: id.pipelineRef, slug: slugify(id.pipelineRef) };
    }
    case "self_improve": {
      const branch = selfImproveBranch(id.runId);
      return { branch, slug: slugify(branch) };
    }
    case "prompt": {
      const branch = `uzi/prompt-${id.runId}`;
      return { branch, slug: slugify(branch) };
    }
    case "task": {
      const branch = id.branch?.trim();
      if (!branch) return undefined;
      return { branch, slug: `task-${id.runId}` };
    }
    case "mr_rework": {
      const branch = id.branch?.trim();
      if (!branch) return undefined;
      return { branch, slug: slugify(branch) };
    }
    case "job":
      // A job is repo-less: there is no clone, so there is no key to derive (PRD #1908).
      return undefined;
    default: {
      const _exhaustive: never = kind;
      return _exhaustive;
    }
  }
}

/**
 * The per-kind profile table. The `Record<RunKind, ...>` makes the union exhaustive: a
 * new run kind added to RUN_KINDS fails to compile until it has a row here. `chat`,
 * `judge` and `issue` carry NO functions — they reproduce today's issue fall-through
 * byte-for-byte should either ever reach the run lane (Decision D8).
 */
export const RUN_KIND_PROFILES: Record<RunKind, RunKindProfile> = {
  issue: {},

  ci_fix: {
    // GUARDED: a ci_fix claim with no pipeline falls through to the issue path
    // (byte-identical defensive path), as the old `kind === "ci_fix" && claim.pipeline`
    // condition did. The branch/slug rule itself lives in deriveCloneKey.
    cloneBranch: (claim) =>
      claim.pipeline
        ? deriveCloneKey({
            kind: "ci_fix",
            runId: claim.run_id,
            pipelineId: claim.pipeline.id,
            pipelineRef: claim.pipeline.ref,
            defaultBranch: claim.repo.default_branch,
          })
        : undefined,
    mrTitle: (claim) =>
      claim.pipeline
        ? `Fix CI: pipeline #${claim.pipeline.id} on ${claim.pipeline.ref}`
        : undefined,
    // The failing pipeline ref and URL (D14), rendered here by the renderer, never by the model. The
    // URL is a bare autolink only when it is plainly an http(s) URL (urlOrCodeSpan), else a code span.
    completionLine: (claim) =>
      claim.pipeline
        ? `CI fix for the failing pipeline on ${codeSpan(claim.pipeline.ref)}: ${urlOrCodeSpan(claim.pipeline.web_url)}`
        : undefined,
  },

  chat: {},

  judge: {},

  self_improve: {
    cloneBranch: (_claim, runId) => deriveCloneKey({ kind: "self_improve", runId }),
    // A self_improve MR references its tracking issue but does NOT `Closes` it — the issue
    // is a stable container reused across cycles (PRD #46 Decision 10). No mrTitle: the
    // issue fallback `Work on issue #<iid>` is the intended empty-title behaviour.
    completionLine: (claim) =>
      `Autonomous self-improvement change (PRD #46), one top improvement per cycle. Tracking issue: #${claim.issue_iid} (a stable container this PR never closes).`,
  },

  prompt: {
    cloneBranch: (_claim, runId) => deriveCloneKey({ kind: "prompt", runId }),
    mrTitle: () => "Scheduled prompt run",
    completionLine: () =>
      "Ad-hoc scheduled prompt run (PRD #241) from a schedule's stored prompt. There is no tracking issue, so this PR closes nothing.",
  },

  task: {
    cloneBranch: (claim) => {
      // A task MUST carry its branch (the destination is never worker-derived); a
      // missing/empty branch is a create-time bug, so fail loudly.
      const key = deriveCloneKey({ kind: "task", runId: claim.run_id, branch: claim.branch });
      if (!key)
        throw new Error("task run claim is missing its branch (uzi/task/<run-id>)");
      return key;
    },
    mrTitle: () => "Handoff task",
    completionLine: (_claim, ctx) =>
      `Handoff task (PRD #400) on ${codeSpan(ctx.branch)}${ctx.baseBranch ? ` (branched from ${codeSpan(ctx.baseBranch)})` : ""}, opened because it was created with \`--mr\`. There is no tracking issue, so this PR closes nothing.`,
  },

  // job (PRD #1908): repo-less. No clone, no git, no MR, no plan gate: it defines no cloneBranch,
  // mrTitle or completionLine, and the worker dispatches it to its own job runner, so this row
  // is never consulted for a job claim. Empty on purpose, like chat and judge.
  job: {},

  mr_rework: {
    cloneBranch: (claim) => {
      // A missing/empty branch is a create-time bug (an mr_rework run must carry its MR
      // branch), so fail loudly rather than fall through to the issue path.
      const key = deriveCloneKey({ kind: "mr_rework", runId: claim.run_id, branch: claim.branch });
      if (!key)
        throw new Error("mr_rework run claim is missing its MR branch (pipeline_ref)");
      return key;
    },
    mrTitle: () => "MR rework",
    completionLine: (_claim, ctx) =>
      `Automated MR rework (PRD #700) addressing review feedback on ${codeSpan(ctx.branch)}. There is no tracking issue, so this PR closes nothing.`,
  },
};
