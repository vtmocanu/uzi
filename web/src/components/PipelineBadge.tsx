// PipelineBadge renders a card/repo/board CI status pill (PRD #6): a link to the
// GitLab pipeline, tinted by the collapsed tone, pulsing while the pipeline is
// still running, with the exact status + sync staleness on hover. The tone/label
// taxonomy is the pure, tested pipelineBadge() mapping; this component is only the
// presentation (an anchor wrapping the shared Badge).

import type { PipelineStatus } from "../lib/api";
import { pipelineBadge, pipelineTitle } from "../lib/pipelineBadge";
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
