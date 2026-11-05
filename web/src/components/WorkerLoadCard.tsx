// Per-worker resource load (PRD #49): a compact "cpu · mem" line per worker that has
// reported a sample, so the factory floor shows live pressure at a glance. Hidden until at
// least one worker reports (no empty card churn). Shared by both Overview pages.

import { Link } from "react-router-dom";
import type { Worker } from "../lib/api";
import { WorkerStatLine, hasStats } from "./WorkerStats";
import { WorkerCordonBadge } from "./WorkerCordonBadge";
import { WorkerRunBadge } from "./WorkerRunBadge";
import { Badge, Card, SectionTitle } from "./ui";

export function WorkerLoadCard({ workers }: { workers: Worker[] }) {
  if (!workers.some(hasStats)) return null;
  return (
    <Card>
      <div className="flex items-center justify-between">
        <SectionTitle>Worker load</SectionTitle>
        <Link to="/workers" className="text-xs font-medium text-brand hover:text-brand-hover">
          Workers →
        </Link>
      </div>
      <ul className="mt-2 divide-y divide-edge">
        {workers
          .filter(hasStats)
          .map((w) => (
            <li key={w.id} className="flex items-center justify-between gap-3 py-2">
              <div className="flex min-w-0 items-center gap-2">
                <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
                  {w.status}
                </Badge>
                <WorkerCordonBadge worker={w} />
                <WorkerRunBadge worker={w} hideIdleCrossChecks />
                <span className="truncate text-sm font-medium text-fg">{w.name}</span>
              </div>
              <WorkerStatLine worker={w} />
            </li>
          ))}
      </ul>
    </Card>
  );
}
