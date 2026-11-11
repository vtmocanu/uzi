// The Overview preview's "Needs your decision" card (PRD #3878 M2, regrouped by #4041): the
// viewer's own plan reviews, questions, owner-action parks and open pull requests, held work
// first and PRs last. Several items of one kind (and every open PR) collapse into one group row
// that previews three and expands in place, each item keeping its own action. Self-hiding when
// empty. The caller passes only the viewer's own items.

import { Fragment, useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { preferForgeUrl, type RunSummaryItem } from "../lib/api";
import { maskRepoPath } from "../lib/demoMask";
import { mrAbbrev, mrRefSymbol } from "../lib/forgeNoun";
import { mergeRequestUrl, projectWebUrlFromIssue } from "../lib/forgeUrls";
import {
  GROUP_BUTTON,
  GROUP_PREVIEW,
  decisionCount,
  decisionRows,
  groupTitle,
  planDetail,
  type DecisionItem,
  type DecisionKind,
  type HeldDecisionItem,
  type RunDecisionItem,
} from "../lib/overview";
import { activityAge } from "../lib/runActivity";
import { stripUnsafeChars } from "../lib/safeText";
import { HELD_WORK_HREF } from "./HeldWorkCard";
import { RunIssueRef } from "./RunIssueRef";
import { SectionTitle } from "./ui";

// The secondary small Button look, on a real anchor: a Button inside a link would nest
// interactive elements. The group toggle reuses it on a real <button>.
const ACTION_CLASS =
  "inline-flex h-7 shrink-0 select-none items-center justify-center gap-1 rounded-lg border border-edge bg-raised px-2.5 text-xs font-medium text-fg transition-colors hover:border-edge-strong hover:bg-raised/70";

// The per-item action inside an expanded group: a small trailing text link, so each item
// stays on one line ("ref title · age · Action") even in a narrow column.
const ITEM_ACTION_CLASS =
  "shrink-0 text-[11px] font-medium text-fg underline decoration-edge-strong underline-offset-2 transition-colors hover:decoration-fg";

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

// Every repo path the card renders goes through here: maskRepoPath only rewrites the
// namespace in demo mode, so the bidi/control strip is applied on top of it.
function repoLabel(run: RunSummaryItem, demo: boolean): string {
  return stripUnsafeChars(maskRepoPath(run.repo_path, demo));
}

// Same derivation as RunsList's MR chip: the persisted forge URL when it is https, else
// rebuilt from the run's issue URL; a malformed value yields null and falls back to the run.
function prHrefOf(run: RunSummaryItem): string | null {
  return preferForgeUrl(
    run.mr_web_url,
    run.mr_iid != null && run.issue_web_url
      ? mergeRequestUrl(projectWebUrlFromIssue(run.issue_web_url), run.mr_iid, run.forge_type)
      : null,
  );
}

// ---- Kind icons (ported from prds/mockups/4041-needs-decision-after.html) ----

type IconKind = "held" | DecisionKind;

export function KindIcon({ kind }: { kind: IconKind }) {
  const common = {
    viewBox: "0 0 24 24",
    fill: "none",
    stroke: "currentColor",
    strokeWidth: 1.8,
    className: "size-[17px]",
    "aria-hidden": true,
    focusable: false,
  } as const;
  switch (kind) {
    case "plan":
      return (
        <svg {...common} strokeLinecap="round">
          <path d="M6 4v16M18 4v16M6 9h12M6 15h12" />
        </svg>
      );
    case "question":
      return (
        <svg {...common} strokeLinecap="round" strokeLinejoin="round">
          <path d="M4 5h16v11H9l-5 4z" />
          <path d="M10 9a2 2 0 1 1 3 1.7c-.6.4-1 .8-1 1.5M12 14.2v.1" />
        </svg>
      );
    case "pr":
      return (
        <svg {...common} strokeLinecap="round">
          <circle cx="6" cy="5" r="2" />
          <circle cx="6" cy="19" r="2" />
          <circle cx="18" cy="19" r="2" />
          <path d="M6 7v10M18 17V9a3 3 0 0 0-3-3h-4m2-2-2 2 2 2" />
        </svg>
      );
    case "held":
    case "park":
      return (
        <svg {...common} strokeLinejoin="round">
          <path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6z" />
        </svg>
      );
  }
}

// Decisions wear the warn tone; open PRs (nothing is blocked on them) wear ok.
function KindTile({ kind }: { kind: IconKind }) {
  const tone = kind === "pr" ? "border-ok/50 bg-ok/10 text-ok" : "border-warn/50 bg-warn/10 text-warn";
  return (
    <span
      aria-hidden="true"
      data-kind-tile={kind}
      className={`grid size-[30px] shrink-0 place-items-center rounded-[9px] border-[1.5px] md:size-[34px] ${tone}`}
    >
      <KindIcon kind={kind} />
    </span>
  );
}

// A meta line whose parts read "A · B" to assistive tech and in the DOM.
function Meta({ parts }: { parts: ReactNode[] }) {
  return (
    <p data-meta="" className="mt-0.5 text-xs text-faint">
      {parts.map((part, i) => (
        <Fragment key={i}>
          {i > 0 ? " · " : null}
          {part}
        </Fragment>
      ))}
    </p>
  );
}

function Age({ age }: { age: string }) {
  return <span className="text-warn">waiting {age}</span>;
}

// One top-level row: tile, body, action, top-aligned so an expanding body never moves the
// action. Below md the action drops under the body. actionFirst puts the action before the
// body in DOM (focus) order while the grid keeps it in the same place: a group's toggle then
// precedes the list it reveals.
function RowShell({
  kind,
  border,
  children,
  action,
  actionFirst = false,
}: {
  kind: IconKind;
  border: string;
  children: ReactNode;
  action: ReactNode;
  actionFirst?: boolean;
}) {
  const body = (
    <div key="body" className="col-start-2 row-start-1 min-w-0">
      {children}
    </div>
  );
  const act = (
    <div
      key="action"
      className="col-start-2 row-start-2 justify-self-start md:col-start-3 md:row-start-1 md:justify-self-auto"
    >
      {action}
    </div>
  );
  return (
    <li
      data-decision-row=""
      className={`grid min-w-0 grid-cols-[30px_minmax(0,1fr)] items-start gap-x-3.5 gap-y-2 py-3.5 md:grid-cols-[34px_minmax(0,1fr)_auto] md:gap-y-0 ${border}`}
    >
      <KindTile kind={kind} />
      {actionFirst ? [act, body] : [body, act]}
    </li>
  );
}

// The held-work decisions as one row. The title names the run only when the aggregate's single
// decision is identified in the hold list; several decisions read as a count, and anything the
// list cannot name stays generic.
function HeldDecisionRow({ item, now, border }: { item: HeldDecisionItem; now: number; border: string }) {
  const titleId = useId();
  const age = item.since ? decisionAge(item.since, now) : "";
  const shortId = item.runId ? stripUnsafeChars(item.runId).slice(0, 8) : "";
  return (
    <RowShell
      kind="held"
      border={border}
      action={
        <Link to={HELD_WORK_HREF} aria-describedby={titleId} className={ACTION_CLASS}>
          {item.verb}
        </Link>
      }
    >
      <p id={titleId} className="text-sm font-medium text-fg">
        {item.count > 1 ? (
          `Held work needs ${item.count} decisions`
        ) : shortId ? (
          <>
            Held work from run <span className="font-mono">{shortId}</span> needs a decision
          </>
        ) : (
          "Held work needs a decision"
        )}
      </p>
      {/* The oldest hold's state names a single hold; several holds stay generic. */}
      <Meta
        parts={[
          <span key="label">{item.count === 1 && item.label ? item.label : "Custody"}</span>,
          ...(age ? [<Age key="age" age={age} />] : []),
        ]}
      />
    </RowShell>
  );
}

function IssueLabel({ run }: { run: RunSummaryItem }) {
  return (
    <>
      <RunIssueRef issueIid={run.issue_iid} issueWebUrl={run.issue_web_url} kind={run.kind} forgeType={run.forge_type} />{" "}
      {stripUnsafeChars(run.issue_title)}
    </>
  );
}

// A lone plan, question or park: "<repo> #7 Title", the repo muted and omitted when empty.
function SingleRow({ item, now, demo, border }: { item: RunDecisionItem; now: number; demo: boolean; border: string }) {
  const { run } = item;
  const age = decisionAge(item.since, now);
  // The row's title describes its action link, so several "Review plan" links are told apart
  // without changing the link's name.
  const titleId = useId();
  // PRD #4040: the pending question's server excerpt, untrusted model text. ONE stripped value
  // feeds both the visible line and the title tooltip (a separate render sink).
  const safeExcerpt = item.excerpt ? stripUnsafeChars(item.excerpt) : "";
  const parts: ReactNode[] = [];
  if (item.kind === "plan") parts.push(<span key="plan">{planDetail(run)}</span>);
  if (age) parts.push(<Age key="age" age={age} />);
  return (
    <RowShell
      kind={item.kind}
      border={border}
      action={
        <Link to={`/runs/${run.id}`} aria-describedby={titleId} className={ACTION_CLASS}>
          {item.verb}
        </Link>
      }
    >
      <p id={titleId} className="truncate text-sm font-medium text-fg">
        {run.repo_path && (
          <>
            <span className="font-normal text-muted">{repoLabel(run, demo)}</span>{" "}
          </>
        )}
        <IssueLabel run={run} />
      </p>
      {safeExcerpt && (
        <p data-excerpt="" title={safeExcerpt} className="mt-1 truncate text-[13px] italic text-muted">
          “{safeExcerpt}”
        </p>
      )}
      {parts.length > 0 && <Meta parts={parts} />}
    </RowShell>
  );
}

// A PR's identity inside a group: "<repo> #12" to the forge in a new tab, else to the run.
function PrRef({ run, demo }: { run: RunSummaryItem; demo: boolean }) {
  const newTabId = useId();
  const href = prHrefOf(run);
  const repo = run.repo_path ? repoLabel(run, demo) : "";
  const num = run.mr_iid != null ? `${mrRefSymbol(run.forge_type)}${run.mr_iid}` : mrAbbrev(run.forge_type);
  // Only the repo truncates (full path on hover); the number never shrinks, so a long repo path
  // cannot hide which PR this is. The space rides in the number span (whitespace-pre, a flex
  // container drops a bare whitespace node), keeping the text content "<repo> <sigil><iid>".
  // The link's name is pinned to the same string: name-from-content trims each child's text,
  // which would otherwise drop the space ("<repo>#12").
  const name = repo ? `${repo} ${num}` : num;
  const text = repo ? (
    <>
      <span data-pr-repo="" title={repo} className="min-w-0 truncate">
        {repo}
      </span>
      <span data-pr-num="" className="shrink-0 whitespace-pre">{` ${num}`}</span>
    </>
  ) : (
    <span data-pr-num="">{num}</span>
  );
  const cls = "inline-flex min-w-0 max-w-full align-top font-mono text-fg hover:underline";
  if (!href) {
    return (
      <Link to={`/runs/${run.id}`} aria-label={name} className={cls}>
        {text}
      </Link>
    );
  }
  return (
    <>
      <a
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        aria-describedby={newTabId}
        aria-label={name}
        className={cls}
      >
        {text}
      </a>
      <span id={newTabId} className="sr-only">
        (opens in a new tab)
      </span>
    </>
  );
}

function GroupItem({ item, now, demo, open }: { item: RunDecisionItem; now: number; demo: boolean; open: boolean }) {
  const { run } = item;
  const labelId = useId();
  const newTabId = useId();
  const age = decisionAge(item.since, now);
  const label =
    item.kind === "pr" ? (
      <>
        <PrRef run={run} demo={demo} /> <span id={labelId}>{stripUnsafeChars(run.issue_title)}</span>
      </>
    ) : (
      <span id={labelId}>
        <IssueLabel run={run} />
      </span>
    );
  if (!open) {
    // relative: the PR ref's absolutely positioned sr-only note stays inside the clipped row.
    return <li className="relative min-w-0 truncate">{label}</li>;
  }
  const href = item.kind === "pr" ? prHrefOf(run) : null;
  // PRD #4040: an awaiting_input item quotes its pending question on a second line, only once
  // the group is expanded. ONE stripped value feeds both the text and the title tooltip.
  const safeExcerpt = item.excerpt ? stripUnsafeChars(item.excerpt) : "";
  // One line: the ref keeps its width up to a cap (a long repo path truncates), the title takes
  // and truncates the rest, the age and action never shrink.
  const refCls = "relative max-w-[40%] shrink-0 truncate";
  return (
    <li className="relative min-w-0 py-0.5">
      <div className="flex min-w-0 items-center gap-x-2">
        {item.kind === "pr" ? (
          <>
            <span className={refCls}>
              <PrRef run={run} demo={demo} />
            </span>
            <span id={labelId} title={stripUnsafeChars(run.issue_title)} className="min-w-0 flex-1 truncate">
              {stripUnsafeChars(run.issue_title)}
            </span>
          </>
        ) : (
          <span id={labelId} className="flex min-w-0 flex-1 items-center gap-x-1">
            <span className={refCls}>
              <RunIssueRef issueIid={run.issue_iid} issueWebUrl={run.issue_web_url} kind={run.kind} forgeType={run.forge_type} />
            </span>{" "}
            <span title={stripUnsafeChars(run.issue_title)} className="min-w-0 truncate">
              {stripUnsafeChars(run.issue_title)}
            </span>
          </span>
        )}
        {age && (
          <span className="shrink-0 text-warn">
            <span className="sr-only">waiting </span>
            {age}
          </span>
        )}
        {href ? (
          <>
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              aria-describedby={`${labelId} ${newTabId}`}
              className={ITEM_ACTION_CLASS}
            >
              {item.verb}
            </a>
            <span id={newTabId} className="sr-only">
              (opens in a new tab)
            </span>
          </>
        ) : (
          <Link to={`/runs/${run.id}`} aria-describedby={labelId} className={ITEM_ACTION_CLASS}>
            {item.verb}
          </Link>
        )}
      </div>
      {safeExcerpt && (
        <p data-excerpt="" title={safeExcerpt} className="truncate italic">
          “{safeExcerpt}”
        </p>
      )}
    </li>
  );
}

// Several items of one kind (any number of open PRs) as one row that expands in place.
function GroupRow({
  kind,
  items,
  now,
  demo,
  border,
}: {
  kind: DecisionKind;
  items: RunDecisionItem[];
  now: number;
  demo: boolean;
  border: string;
}) {
  const [open, setOpen] = useState(false);
  const titleId = useId();
  const listId = useId();
  const shown = open ? items : items.slice(0, GROUP_PREVIEW);
  const hidden = items.length - shown.length;
  const oldest = decisionAge(items[0].since, now);
  // One cached-state note for the whole group: the forge's own noun when every PR shares it.
  // Compared by the noun shown, so a GitLab PR and a forge-less one (both "MR") agree.
  const nouns = new Set(items.map((i) => mrAbbrev(i.run.forge_type)));
  const noun = nouns.size === 1 ? [...nouns][0] : "PR";
  return (
    <RowShell
      kind={kind}
      border={border}
      actionFirst
      action={
        <button
          type="button"
          aria-expanded={open}
          aria-controls={listId}
          aria-describedby={titleId}
          onClick={() => setOpen((o) => !o)}
          className={`${ACTION_CLASS} cursor-pointer`}
        >
          {open ? "Show fewer" : GROUP_BUTTON[kind]}
        </button>
      }
    >
      <p id={titleId} className="text-sm font-medium text-fg">
        {groupTitle(kind, items.length)}
      </p>
      <ul id={listId} className="mt-1 grid min-w-0 gap-0.5 text-xs text-muted">
        {shown.map((item) => (
          <GroupItem key={item.run.id} item={item} now={now} demo={demo} open={open} />
        ))}
        {hidden > 0 && (
          <li className="text-faint">
            +{hidden} more{oldest ? ` · oldest waiting ${oldest}` : ""}
          </li>
        )}
      </ul>
      {kind === "pr" && <p className="mt-1 text-xs text-faint">{noun} state as last seen by uzi</p>}
    </RowShell>
  );
}

export function NeedsDecisionCard({ items, now, demo }: { items: DecisionItem[]; now: number; demo: boolean }) {
  if (items.length === 0) return null;
  const rows = decisionRows(items);
  const n = rows.length;
  // Two columns on wide screens only while the rows fit side by side (2 to 4); a lone row and
  // five rows stay one column, and a phone always stacks.
  const twoCol = n >= 2 && n <= 4;
  // Separators: none under the last row; in two columns none under either column's last row,
  // but the stacked (narrow) layout needs the one between the last two back.
  const border = (i: number) =>
    i === n - 1 ? "" : twoCol && i === n - 2 ? "border-b border-edge md:border-b-0" : "border-b border-edge";
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
      <ul data-decision-rows="" className={`mt-2 grid min-w-0 grid-cols-1 ${twoCol ? "md:grid-cols-2 md:gap-x-10" : ""}`}>
        {rows.map((row, i) =>
          row.type === "held" ? (
            <HeldDecisionRow key="held" item={row.item} now={now} border={border(i)} />
          ) : row.type === "single" ? (
            <SingleRow key={`${row.item.kind}:${row.item.run.id}`} item={row.item} now={now} demo={demo} border={border(i)} />
          ) : (
            <GroupRow key={`group:${row.kind}`} kind={row.kind} items={row.items} now={now} demo={demo} border={border(i)} />
          ),
        )}
      </ul>
    </section>
  );
}
