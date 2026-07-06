// CIFixRunHeader renders a ci_fix run's header extras (PRD #6): a link to the
// failing pipeline — guarded by isHttpsUrl like every forge link, so a non-https
// web_url renders as plain text, never an anchor — and the fix-verdict chip once
// the run has settled. Renders nothing for an issue run. Extracted from RunView so
// the guard + verdict mapping are testable in isolation.

import { isHttpsUrl, type Run } from "../lib/api";
import { fixVerdictChip } from "../lib/fixVerdict";
import { Badge } from "./ui";

export function CIFixRunHeader({ run, terminal }: { run: Run; terminal: boolean }) {
  if (run.kind !== "ci_fix") return null;
  const chip = fixVerdictChip(run.fix_verdict, terminal);
  const pillClass =
    "inline-flex items-center rounded-md border border-danger/40 bg-danger/10 px-2 py-0.5 text-[11px] font-medium text-danger";
  const title = `Failing pipeline${run.pipeline_ref ? ` on ${run.pipeline_ref}` : ""}`;
  return (
    <>
      {run.pipeline_web_url &&
        (isHttpsUrl(run.pipeline_web_url) ? (
          <a
            href={run.pipeline_web_url}
            target="_blank"
            rel="noreferrer"
            title={title}
            className={`${pillClass} transition-colors hover:bg-danger/20`}
          >
            failing pipeline
          </a>
        ) : (
          <span title={title} className={pillClass}>
            failing pipeline
          </span>
        ))}
      {chip && (
        <Badge tone={chip.tone} title={chip.title}>
          {chip.label}
        </Badge>
      )}
    </>
  );
}
