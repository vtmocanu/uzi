// CIWatchDetail — a ci_watch schedule's detail panel (PRD #2865), opened from its row on
// the Schedules tab.
//
// It answers the three questions an owner has about a CI repair job: what it watches
// (the workflow selector, or every workflow in the listed window of newest executions), why
// it waits for plan approval (the auto-approve note), and where each watched workflow
// stands (the per-workflow episode state). The list DTO carries no episodes, so the panel
// reads the schedule's detail GET for the first page and follows next_cursor on "Load more".
//
// Every workflow key and failure signature is forge-derived text: it is rendered as React
// text children only, never as markup and never into an attribute.

import { stripUnsafeChars } from "../lib/safeText";
import { useEffect, useState } from "react";
import { api, type CIEpisodeDTO, type Schedule } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { CI_WATCH_TARGET_NAME } from "../lib/scheduleList";
import { CIWatchApproveNote } from "./ScheduleModal";
import { formatStamp } from "./LastRun";
import { Alert, Badge, Button, type BadgeTone } from "./ui";

// The window is the one bounded page a fire reads (schedsvc ciListLimit; Forgejo caps at 50).
const CI_ALL_WORKFLOWS = "All workflows in the listed window (newest 100 executions; 50 on Forgejo)";

type Loaded = { episodes: CIEpisodeDTO[]; cursor: string | null };

export function CIWatchDetail({ s, panelId }: { s: Schedule; panelId: string }) {
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [loadError, setLoadError] = useState("");
  const [moreBusy, setMoreBusy] = useState(false);
  const [moreError, setMoreError] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let alive = true;
    setLoadError("");
    api
      .getSchedule(s.id)
      .then((detail) => {
        if (!alive) return;
        setLoaded({
          episodes: detail.ci_episodes?.episodes ?? [],
          cursor: detail.ci_episodes?.next_cursor ?? null,
        });
      })
      .catch((err) => {
        if (alive) setLoadError(errorMessage(err, "Could not load the CI watch state"));
      });
    return () => {
      alive = false;
    };
  }, [s.id, attempt]);

  const loadMore = async () => {
    if (!loaded?.cursor || moreBusy) return;
    setMoreBusy(true);
    setMoreError("");
    try {
      const page = await api.listScheduleCIEpisodes(s.id, loaded.cursor);
      setLoaded((cur) => ({
        episodes: [...(cur?.episodes ?? []), ...page.episodes],
        cursor: page.next_cursor,
      }));
    } catch (err) {
      setMoreError(errorMessage(err, "Could not load more workflows"));
    } finally {
      setMoreBusy(false);
    }
  };

  const workflows = s.ci_workflows ?? [];
  const headingId = `${panelId}-heading`;

  return (
    <section aria-labelledby={headingId} className="space-y-4 rounded-lg border border-edge bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <h3 id={headingId} className="text-sm font-semibold text-fg">
          {CI_WATCH_TARGET_NAME}
        </h3>
        <span className="text-[12px] text-muted">
          Plan approval: {s.auto_approve ? "automatic" : "you review each plan"}
        </span>
      </div>

      <CIWatchApproveNote />

      <div>
        <h4 className="text-[12px] font-medium text-muted">Watched workflows</h4>
        {workflows.length === 0 ? (
          <p className="mt-1 text-[13px] text-fg">{CI_ALL_WORKFLOWS}</p>
        ) : (
          <ul className="mt-1.5 flex flex-wrap gap-1.5" aria-label="Watched workflows">
            {workflows.map((w) => (
              <li
                key={w}
                className="break-all rounded-md border border-edge bg-raised px-2 py-0.5 font-mono text-[12px] text-fg"
              >
                {stripUnsafeChars(w)}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div>
        <h4 className="text-[12px] font-medium text-muted">Artifact selectors</h4>
        {(s.ci_artifacts?.length ?? 0) === 0 ? (
          <p className="mt-1 text-[13px] text-fg">None — no artifacts fetched</p>
        ) : (
          <ul className="mt-1.5 flex min-w-0 flex-wrap gap-1.5" aria-label="Artifact selectors">
            {s.ci_artifacts!.map((selector) => (
              <li key={selector} className="min-w-0 break-all rounded-md border border-edge bg-raised px-2 py-0.5 font-mono text-[12px] text-fg">
                {stripUnsafeChars(selector)}
              </li>
            ))}
          </ul>
        )}
        <p className="mt-1 text-[11px] text-faint">Fetching is GitHub-only; unsupported providers remain logs-only.</p>
      </div>

      <div>
        <h4 className="text-[12px] font-medium text-muted">Workflow state</h4>
        {loadError ? (
          <div className="mt-2 space-y-2">
            <Alert message={loadError} />
            <Button variant="secondary" size="sm" onClick={() => setAttempt((n) => n + 1)}>
              Try again
            </Button>
          </div>
        ) : loaded === null ? (
          <p className="mt-1 text-[13px] text-faint" role="status">
            Loading workflow state…
          </p>
        ) : loaded.episodes.length === 0 ? (
          <p className="mt-1 text-[13px] text-faint">
            No failed run seen yet. A workflow appears here the first time it fails on the watched branch.
          </p>
        ) : (
          <EpisodeTable episodes={loaded.episodes} />
        )}
        {loaded?.cursor && (
          <div className="mt-3 flex flex-wrap items-center gap-3">
            <Button variant="secondary" size="sm" onClick={loadMore} aria-disabled={moreBusy || undefined}>
              {moreBusy ? "Loading…" : "Load more"}
            </Button>
            {moreError && <Alert message={moreError} />}
          </div>
        )}
      </div>
    </section>
  );
}

// The episode table: one row per watched workflow. Below sm the run-id and updated
// columns fold into the workflow cell, so a phone shows no horizontal scroll.
function EpisodeTable({ episodes }: { episodes: CIEpisodeDTO[] }) {
  return (
    <table className="mt-2 w-full table-fixed border-collapse text-left text-[12.5px]">
      <thead>
        <tr className="border-b border-edge text-[11px] text-faint">
          <th scope="col" className="w-[44%] py-1.5 pr-3 font-medium sm:w-[34%]">Workflow</th>
          <th scope="col" className="py-1.5 pr-3 font-medium">State</th>
          <th scope="col" className="w-16 py-1.5 pr-2 text-right font-medium sm:w-20 sm:pr-3">Attempts</th>
          <th scope="col" className="hidden py-1.5 font-medium sm:table-cell">Updated</th>
        </tr>
      </thead>
      <tbody>
        {episodes.map((e) => {
          const state = episodeState(e);
          return (
            <tr key={e.workflow_key} className="border-b border-edge/60 align-top last:border-b-0">
              <td className="py-2 pr-3">
                <div className="break-all font-mono text-fg">{stripUnsafeChars(e.workflow_key)}</div>
                {e.last_signature && (
                  <div className="mt-0.5 break-words text-[11.5px] text-muted">{stripUnsafeChars(e.last_signature)}</div>
                )}
                <div className="mt-0.5 text-[11px] text-faint sm:hidden">{formatStamp(e.updated_at)}</div>
              </td>
              <td className="py-2 pr-3">
                <Badge tone={state.tone} dot wrap>
                  {state.label}
                </Badge>
                {e.observed_run_id != null && (
                  <div className="mt-1 text-[11px] text-faint">
                    latest run <span className="font-mono tabular-nums">{e.observed_run_id}</span>
                  </div>
                )}
              </td>
              <td className="py-2 pr-3 text-right font-mono tabular-nums text-fg">{e.attempts}</td>
              <td className="hidden py-2 text-muted sm:table-cell">{formatStamp(e.updated_at)}</td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

// episodeState names where one workflow stands. The server creates a row only on a red
// observation, inside a repair start (attempts+1, watermark = the failed run) or a halt
// (halted_reason set, signature and attempts kept); a later fresh green resets it
// (attempts 0, signature and halt cleared). Any other shape gets a neutral fallback. A halt needs the owner, so it reads first.
function episodeState(e: CIEpisodeDTO): { label: string; tone: BadgeTone } {
  if (e.halted_reason === "attempt_cap") return { label: "Stopped: attempt limit reached", tone: "warning" };
  if (e.halted_reason === "no_progress") return { label: "Stopped: no progress", tone: "warning" };
  if (e.halted_reason) return { label: "Stopped", tone: "warning" };
  if (e.attempts > 0)
    return { label: e.attempts === 1 ? "Repair attempted" : `Repair attempted (${e.attempts} times)`, tone: "info" };
  if (e.last_signature == null) return { label: "Passing again", tone: "ok" };
  return { label: "Failure seen", tone: "neutral" };
}
