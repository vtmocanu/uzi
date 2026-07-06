// PipelineBadge renders a card/repo/board CI status pill (PRD #6): a link to the
// GitLab pipeline, tinted by the collapsed tone, pulsing while the pipeline is
// still running, with the exact status + sync staleness on hover. The tone/label
// taxonomy is the pure, tested pipelineBadge() mapping; this component is only the
// presentation (an anchor wrapping the shared Badge).

import type { PipelineStatus } from "../lib/api";
import { pipelineBadge, pipelineTitle, pipelineTone } from "../lib/pipelineBadge";
import { Badge } from "./ui";

export function PipelineBadge({ pipeline }: { pipeline: PipelineStatus }) {
  const { label, tone, pulse } = pipelineBadge(pipeline.status);
  const title = pipelineTitle(pipeline.status, pipeline.synced_at, Date.now());
  return (
    <a
      href={pipeline.web_url}
      target="_blank"
      rel="noreferrer"
      title={title}
      aria-label={`CI ${label}`}
      className="inline-flex rounded-md transition-[filter] hover:brightness-110 focus:outline-none focus-visible:ring-2 focus-visible:ring-brand/50"
    >
      <Badge tone={tone} dot pulse={pulse}>
        CI {label}
      </Badge>
    </a>
  );
}

/**
 * FixCiButton renders the "Fix CI" affordance (PRD #6) — but ONLY for a failed
 * pipeline; it returns null otherwise, so callers can drop it in unconditionally.
 * Clicking queues a plan-gated ci_fix run for the pipeline's ref (the caller wires
 * onClick to api.createCIFixRun). Precondition failures (an active fix, the branch
 * in use, no worker/token) surface as the server's 409/error, not a disabled state,
 * since the client cannot know them up front.
 */
export function FixCiButton({
  pipeline,
  busy,
  onClick,
}: {
  pipeline: PipelineStatus;
  busy: boolean;
  onClick: () => void;
}) {
  if (pipelineTone(pipeline.status) !== "failed") return null;
  return (
    <button
      type="button"
      disabled={busy}
      draggable={false}
      onClick={onClick}
      title="Queue a plan-gated agent run to diagnose and fix this failed pipeline"
      className="inline-flex items-center rounded-md border border-danger/40 bg-danger/10 px-1.5 py-0.5 text-[11px] font-medium text-danger transition-colors hover:bg-danger/20 disabled:opacity-50"
    >
      {busy ? "Starting…" : "Fix CI"}
    </button>
  );
}
