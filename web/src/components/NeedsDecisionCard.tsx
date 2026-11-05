// The Overview preview's "Needs your decision" card (PRD #3878 M2): the viewer's own plan
// reviews, questions, owner-action parks and open pull requests, each with one verb and its
// age. Self-hiding when empty. The caller passes only the viewer's own items.

import { useId } from "react";
import { Link } from "react-router-dom";
import { preferForgeUrl } from "../lib/api";
import { maskRepoPath } from "../lib/demoMask";
import { mrAbbrev } from "../lib/forgeNoun";
import { mergeRequestUrl, projectWebUrlFromIssue } from "../lib/forgeUrls";
import { decisionCount, type DecisionItem, type HeldDecisionItem, type RunDecisionItem } from "../lib/overview";
import { activityAge } from "../lib/runActivity";
import { stripUnsafeChars } from "../lib/safeText";
import { HELD_WORK_HREF } from "./HeldWorkCard";
import { RunIssueRef } from "./RunIssueRef";
import { SectionTitle } from "./ui";

// The secondary small Button look, on a real anchor: a Button inside a link would nest
// interactive elements.
const ACTION_CLASS =
  "inline-flex h-7 shrink-0 select-none items-center justify-center gap-1 rounded-lg border border-edge bg-raised px-2.5 text-xs font-medium text-fg transition-colors hover:border-edge-strong hover:bg-raised/70";

// A compact decision age that rolls into days (activityAge stops at hours: "678h 0m").
function decisionAge(since: string, now: number): string {
  const ms = now - Date.parse(since);
  if (Number.isFinite(ms) && ms >= 3_600_000) {
    const h = Math.floor(ms / 3_600_000);
    return h < 24 ? `${h}h` : `${Math.floor(h / 24)}d`;
  }
  return activityAge(since, now);
}

function summary(items: DecisionItem[]): string {
  const prs = items.filter((i) => i.kind === "pr").length;
  const decisions = decisionCount(items);
  return [
    decisions > 0 ? `${decisions} ${decisions === 1 ? "decision" : "decisions"}` : null,
    prs > 0 ? `${prs} PR ${prs === 1 ? "review" : "reviews"}` : null,
  ]
    .filter((s): s is string => s !== null)
    .join(", ");
}

// The held-work decisions as one row. The title names the run only when the aggregate's single
// decision is identified in the hold list; several decisions read as a count, and anything the
// list cannot name stays generic.
function HeldDecisionRow({ item, now }: { item: HeldDecisionItem; now: number }) {
  const titleId = useId();
  const age = item.since ? decisionAge(item.since, now) : "";
  const shortId = item.runId ? stripUnsafeChars(item.runId).slice(0, 8) : "";
  return (
    <li className="flex items-center gap-3 py-2.5">
      <div className="min-w-0 flex-1">
        <p id={titleId} className="text-sm font-medium text-fg">
          {item.count > 1 ? (
            `${item.count} held-work holds need a decision`
          ) : shortId ? (
            <>
              Held work from run <span className="font-mono">{shortId}</span> needs a decision
            </>
          ) : (
            "Held work needs a decision"
          )}
        </p>
        <p className="flex flex-wrap items-center gap-x-2 text-xs text-faint">
          <span>Custody</span>
          {age && <span>waiting {age}</span>}
        </p>
      </div>
      <Link to={HELD_WORK_HREF} aria-describedby={titleId} className={ACTION_CLASS}>
        {item.verb}
      </Link>
    </li>
  );
}

function DecisionRow({ item, now, demo }: { item: RunDecisionItem; now: number; demo: boolean }) {
  const { run } = item;
  const age = decisionAge(item.since, now);
  // The row's title describes its action link, so several "Review plan" links are told apart
  // without changing the link's name. The external link also points at a hidden new-tab note.
  const titleId = useId();
  const newTabId = useId();
  // Same derivation as RunsList's MR chip: the persisted forge URL when it is https, else
  // rebuilt from the run's issue URL; a malformed value yields null and falls back to the run.
  const prHref =
    item.kind === "pr"
      ? preferForgeUrl(
          run.mr_web_url,
          run.mr_iid != null && run.issue_web_url
            ? mergeRequestUrl(projectWebUrlFromIssue(run.issue_web_url), run.mr_iid, run.forge_type)
            : null,
        )
      : null;
  return (
    <li className="flex items-center gap-3 py-2.5">
      <div className="min-w-0 flex-1">
        <p id={titleId} className="truncate text-sm font-medium text-fg">
          {stripUnsafeChars(run.issue_title)}
        </p>
        <p className="flex flex-wrap items-center gap-x-2 text-xs text-faint">
          <span>
            {run.repo_path ? <>{maskRepoPath(run.repo_path, demo)} </> : null}
            <RunIssueRef
              issueIid={run.issue_iid}
              issueWebUrl={run.issue_web_url}
              kind={run.kind}
              forgeType={run.forge_type}
            />
          </span>
          {item.kind === "pr" && <span>{mrAbbrev(run.forge_type)} state as last seen by uzi</span>}
          {age && <span>waiting {age}</span>}
        </p>
      </div>
      {prHref ? (
        <>
          <a
            href={prHref}
            target="_blank"
            rel="noopener noreferrer"
            aria-describedby={`${titleId} ${newTabId}`}
            className={ACTION_CLASS}
          >
            {item.verb}
          </a>
          <span id={newTabId} className="sr-only">
            (opens in a new tab)
          </span>
        </>
      ) : (
        <Link to={`/runs/${run.id}`} aria-describedby={titleId} className={ACTION_CLASS}>
          {item.verb}
        </Link>
      )}
    </li>
  );
}

export function NeedsDecisionCard({ items, now, demo }: { items: DecisionItem[]; now: number; demo: boolean }) {
  if (items.length === 0) return null;
  // Card (components/ui) has no attribute passthrough, so this repeats its classes to carry
  // data-attention, which only the Shadow theme styles.
  return (
    <section
      aria-label="Needs your decision"
      data-attention=""
      className="rounded-xl border border-edge bg-surface p-5"
    >
      <div className="flex items-center justify-between">
        <SectionTitle>Needs your decision</SectionTitle>
        <span className="text-xs text-faint">{summary(items)}</span>
      </div>
      <ul className="mt-2 divide-y divide-edge">
        {items.map((item) =>
          item.kind === "held" ? (
            <HeldDecisionRow key="held" item={item} now={now} />
          ) : (
            <DecisionRow key={`${item.kind}:${item.run.id}`} item={item} now={now} demo={demo} />
          ),
        )}
      </ul>
    </section>
  );
}
