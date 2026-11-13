// The compact list of passing checks in the Overview health popover (issue #4281).
// This is a view over the server's check registry, not a second health calculation.
import type { HealthCheck } from "../lib/api";
import { stripUnsafeChars } from "../lib/safeText";

const AREAS = [
  "Workers",
  "Queue",
  "Platform",
  "Forge and Slack",
  "Schedules",
  "Recovery",
  "Releases and pricing",
  "Other",
] as const;
type Area = (typeof AREAS)[number];

function areaFor(id: string): Area {
  if (id.startsWith("fleet.")) return "Workers";
  if (id.startsWith("queue.")) return "Queue";
  if (id.startsWith("controller.") || id === "db" || id.startsWith("db.") || id === "loops" || id.startsWith("maintenance.")) return "Platform";
  if (id.startsWith("forge.") || id.startsWith("board.") || id.startsWith("slack.")) return "Forge and Slack";
  if (id.startsWith("schedules.")) return "Schedules";
  if (id.startsWith("custody.") || id.startsWith("recovery.")) return "Recovery";
  if (id.startsWith("release.") || id.startsWith("agentsource.") || id.startsWith("pricing.")) return "Releases and pricing";
  return "Other";
}

export function HealthPassingChecks({ checks }: { checks: HealthCheck[] }) {
  const groups = new Map<Area, HealthCheck[]>();
  for (const check of checks) {
    if (check.severity !== "ok") continue;
    const area = areaFor(check.id);
    const items = groups.get(area);
    if (items) items.push(check);
    else groups.set(area, [check]);
  }
  return (
    <div data-health-passing-grid className="grid grid-cols-1 gap-x-7 gap-y-3 sm:grid-cols-2 xl:grid-cols-4">
      {AREAS.map((area) => {
        const items = groups.get(area);
        if (!items?.length) return null;
        return (
          <section key={area} aria-label={area} className="min-w-0">
            <h4 className="mb-1 text-[11px] font-medium uppercase tracking-[0.06em] text-faint">{area}</h4>
            <ul className="space-y-0.5">
              {items.map((check) => {
                const title = stripUnsafeChars(check.title);
                const summary = stripUnsafeChars(check.summary);
                return (
                  <li key={check.id} className="flex min-w-0 items-baseline gap-2">
                    <span aria-hidden="true" className="h-1.5 w-1.5 shrink-0 rounded-full bg-ok" />
                    <span
                      data-passing-check
                      tabIndex={0}
                      title={summary}
                      aria-label={summary ? `${title}: ${summary}` : title}
                      className="min-w-0 rounded-sm text-xs leading-relaxed text-fg focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
                      style={{ overflowWrap: "anywhere" }}
                    >
                      {title}
                    </span>
                  </li>
                );
              })}
            </ul>
          </section>
        );
      })}
    </div>
  );
}
