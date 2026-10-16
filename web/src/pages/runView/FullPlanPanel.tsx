import { useState } from "react";
import { Markdown } from "../../components/Markdown";
import type { RunMessage } from "../../lib/api";

function EarlierSubmittedVersions({ messages }: { messages: RunMessage[] }) {
  const [expanded, setExpanded] = useState(false);
  // Only the already-loaded submissions are considered. Drop the newest submission
  // before validating bodies so a malformed latest payload cannot promote an older one.
  const priorPlans = [...messages]
    .sort((a, b) => a.seq - b.seq)
    .filter((message) => message.kind === "plan")
    .slice(0, -1)
    .flatMap((message) => {
      const payload = message.payload;
      if (!payload || typeof payload !== "object" || Array.isArray(payload)) return [];
      const md = (payload as { plan_md?: unknown }).plan_md;
      return typeof md === "string" && md.trim() !== "" ? [md] : [];
    });

  if (priorPlans.length === 0) return null;
  return (
    <details
      className="rounded-lg border border-edge bg-surface/50"
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary className="cursor-pointer px-3 py-2 text-xs text-muted">
        Earlier submitted versions
      </summary>
      {expanded && (
        <div className="max-h-96 space-y-3 overflow-auto border-t border-edge p-3">
          {priorPlans.map((md, index) => (
            <div key={index} className="border-l-2 border-edge-strong pl-3 text-xs text-muted">
              <Markdown content={md} />
            </div>
          ))}
        </div>
      )}
    </details>
  );
}

// RunView keys this disclosure by run.id; expansion is local and never persisted.
export function FullPlanPanel({ plan, messages }: { plan: string; messages: RunMessage[] }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <details
      className="overflow-hidden rounded-xl border border-edge bg-raised/40"
      onToggle={(event) => setExpanded(event.currentTarget.open)}
    >
      <summary className="cursor-pointer px-4 py-3 text-sm font-semibold text-fg">
        Full plan
      </summary>
      {expanded && (
        <div className="space-y-3 border-t border-edge p-4">
          <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
            <Markdown content={plan} />
          </div>
          <EarlierSubmittedVersions messages={messages} />
        </div>
      )}
    </details>
  );
}
