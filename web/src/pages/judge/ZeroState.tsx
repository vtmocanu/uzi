import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type JudgeBacklog } from "../../lib/api";
import { verdictTrend, rollupLabel, rollupTone, seenInRunsLabel } from "../../lib/judgeBacklog";
import { coordKey, recommendationLabel } from "../../lib/judge";
import { stripUnsafeChars } from "../../lib/safeText";
import { Badge, Card, cx, ListSkeleton, SectionTitle } from "../../components/ui";

// ZeroState is the first-class inbox-zero view (Decision 8): to-triage = 0 is the goal, so
// the page is not blank. It fetches the bucket=all snapshot to show the recent-verdict trend
// and the recently Filed / Done groups, and — when the user has not opted into the judge —
// an opt-in card linking Settings. A badge-less nav item most of the week is expected; this
// is what keeps the destination worth opening.
export function ZeroState({ judgeEnabled }: { judgeEnabled: boolean }) {
  const [all, setAll] = useState<JudgeBacklog | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const data = await api.getJudgeBacklog("all");
        if (alive) setAll(data);
      } catch {
        /* the headline still renders without the snapshot */
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  const trend = all ? verdictTrend(all.groups) : null;
  // Settled groups, MOST RECURRENT first — the order the server already sorted by
  // (run_count DESC), not a recency order. The heading below says so. Calling this
  // "Recently handled" was a claim the code never made: nothing here has a timestamp to sort
  // by, and slicing the first 6 of a frequency-sorted list yields the most FREQUENT
  // (PRD #98 review N6).
  const settled = (all?.groups ?? [])
    .filter((g) => g.bucket === "done" || g.bucket === "filed")
    .slice(0, 6);

  return (
    <div className="space-y-4">
      <Card className="space-y-2 border-ok/30 bg-ok/[0.04] p-5 text-center">
        <p className="text-lg font-semibold text-fg">Inbox zero — nothing to triage.</p>
        <p className="text-sm text-muted">
          Every recommendation across your runs is handled. New verdicts will show up here as your runs finish.
        </p>
      </Card>

      {!judgeEnabled && (
        <Card className="flex flex-wrap items-center justify-between gap-3 border-brand/30 bg-brand/[0.05] p-4">
          <div className="min-w-0">
            <p className="text-sm font-medium text-fg">The judge is off for your account.</p>
            <p className="text-sm text-faint">
              Turn it on and uzi reviews each finished run, surfacing recommendations here.
            </p>
          </div>
          <Link
            to="/settings"
            className="shrink-0 rounded-lg bg-brand px-3 py-1.5 text-sm font-medium text-on-brand hover:bg-brand-hover"
          >
            Enable in Settings
          </Link>
        </Card>
      )}

      {loading && <ListSkeleton rows={2} />}

      {trend && trend.total > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Verdicts across your judged runs</SectionTitle>
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
            <VerdictCount label="ideal" n={trend.ideal} tone="ok" />
            <VerdictCount label="ok" n={trend.ok} tone="info" />
            <VerdictCount label="issues" n={trend.issues} tone="warning" />
            <span className="text-faint">across {trend.total} judged {trend.total === 1 ? "run" : "runs"}</span>
          </div>
        </Card>
      )}

      {settled.length > 0 && (
        <Card className="space-y-2 p-4">
          <SectionTitle>Settled — most recurrent first</SectionTitle>
          <ul className="space-y-1.5">
            {settled.map((g) => (
              <li key={coordKey(g.category, g.target)} className="flex flex-wrap items-center gap-2 text-sm">
                <Badge tone={rollupTone(g.bucket)}>{rollupLabel(g.bucket)}</Badge>
                <span className="text-muted">{recommendationLabel(g.category)}</span>
                {g.target.trim() !== "" && (
                  <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">
                    {stripUnsafeChars(g.target)}
                  </code>
                )}
                <span className="text-xs text-faint">{seenInRunsLabel(g.run_count)}</span>
              </li>
            ))}
          </ul>
        </Card>
      )}
    </div>
  );
}

function VerdictCount({ label, n, tone }: { label: string; n: number; tone: "ok" | "info" | "warning" }) {
  const dot = tone === "ok" ? "bg-ok" : tone === "info" ? "bg-info" : "bg-warn";
  return (
    <span className="inline-flex items-baseline gap-1.5">
      <span aria-hidden="true" className={cx("inline-block h-2 w-2 self-center rounded-full", dot)} />
      <b className="text-sm font-semibold tabular-nums text-fg">{n}</b>
      <span className="uppercase tracking-wide text-faint">{label}</span>
    </span>
  );
}
