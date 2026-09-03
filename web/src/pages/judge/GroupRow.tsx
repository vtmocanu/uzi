import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { type JudgeOccurrence, type JudgeRecommendationGroup, type Repo } from "../../lib/api";
import { recommendationLabel } from "../../lib/judge";
import { rollupLabel, rollupTone, seenInRunsLabel } from "../../lib/judgeBacklog";
import { stripUnsafeChars } from "../../lib/safeText";
import { judgeBadge } from "../../lib/judgeBadge";
import { OccurrenceFileIssue } from "../../components/OccurrenceFileIssue";
import { Badge, Button } from "../../components/ui";
import { ChevronDownIcon, ChevronRightIcon, ExternalLinkIcon } from "../../components/icons";

// GroupRow is one deduped (category, target) row: the category + target header, the "seen
// in N runs" frequency chip, the rollup badge, the group actions, and the occurrence
// expander. rationale_preview and target are UNTRUSTED judge text — rendered as escaped React
// text with whitespace-pre-wrap, NEVER through a markdown renderer or dangerouslySetInnerHTML,
// and passed through stripUnsafeChars first (issue #124): escaping does not touch a bidi
// override, and the api's review-ingest scrub dropped Cc but not Cf until it learned both —
// which leaves every row stored before that fix still carrying them.
export function GroupRow({
  group,
  selected,
  onToggleSelect,
  onDispose,
  repos,
  onFiled,
}: {
  group: JudgeRecommendationGroup;
  selected: boolean;
  onToggleSelect: () => void;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
  repos: Repo[];
  onFiled: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const openCount = group.open_count;

  return (
    <li className="rounded-lg border border-edge bg-raised/40">
      <div className="flex flex-wrap items-start gap-2 px-3 py-2.5">
        <input
          type="checkbox"
          checked={selected}
          onChange={onToggleSelect}
          aria-label={`Select ${recommendationLabel(group.category)} ${stripUnsafeChars(group.target)}`}
          className="mt-1 h-4 w-4 shrink-0 accent-brand"
        />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone="info">{recommendationLabel(group.category)}</Badge>
            {group.target.trim() !== "" && (
              <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs text-fg">
                {stripUnsafeChars(group.target)}
              </code>
            )}
            <span className="text-xs text-faint">{seenInRunsLabel(group.run_count)}</span>
            {group.bucket !== "todo" && <Badge tone={rollupTone(group.bucket)}>{rollupLabel(group.bucket)}</Badge>}
            {openCount > 0 && <span className="text-xs text-faint">{openCount} open</span>}
          </div>
          {group.rationale_preview.trim() !== "" && (
            <p className="mt-1.5 line-clamp-3 whitespace-pre-wrap text-sm text-muted">
              {stripUnsafeChars(group.rationale_preview)}
            </p>
          )}
        </div>
        <div className="flex shrink-0 items-center gap-1.5">
          <GroupDisposeControls disabled={openCount === 0} onDispose={onDispose} />
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            aria-expanded={expanded}
            aria-label={expanded ? "Collapse occurrences" : "Expand occurrences"}
            className="rounded-md p-1 text-faint transition-colors hover:bg-raised hover:text-fg"
          >
            {expanded ? <ChevronDownIcon /> : <ChevronRightIcon />}
          </button>
        </div>
      </div>

      {expanded && (
        <ul className="space-y-2 border-t border-edge px-3 py-2.5">
          {group.occurrences.map((occ) => (
            <OccurrenceRow
              key={`${occ.run_id} ${occ.rec_id}`}
              occ={occ}
              repos={repos}
              onFiled={onFiled}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

// GroupDisposeControls: the per-group Mark done + Dismiss ▾ (Won't do / Not an issue),
// mirroring RunView's per-rec controls but fanning out across the group. Disabled when
// the group has no open member — scope=open would settle nothing.
function GroupDisposeControls({
  disabled,
  onDispose,
}: {
  disabled: boolean;
  onDispose: (status: "done" | "dismissed", reason?: "wont_do" | "not_an_issue") => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!menuOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setMenuOpen(false);
        wrapRef.current?.querySelector<HTMLElement>("button")?.focus();
      }
    };
    const onPointerDown = (e: Event) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setMenuOpen(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [menuOpen]);

  if (disabled) return null;

  return (
    <div className="flex items-center gap-1.5">
      <Button size="sm" variant="secondary" onClick={() => onDispose("done")}>
        Mark done
      </Button>
      <div className="relative" ref={wrapRef}>
        <Button
          size="sm"
          variant="secondary"
          aria-haspopup="menu"
          aria-expanded={menuOpen}
          onClick={() => setMenuOpen((o) => !o)}
        >
          Dismiss ▾
        </Button>
        {menuOpen && (
          <div
            role="menu"
            className="absolute right-0 z-10 mt-1 w-56 rounded-lg border border-edge-strong bg-surface p-1 shadow-lg"
          >
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                setMenuOpen(false);
                onDispose("dismissed", "wont_do");
              }}
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
            >
              Won't do
              <span className="text-xs text-faint">Valid, but not worth acting on</span>
            </button>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                setMenuOpen(false);
                onDispose("dismissed", "not_an_issue");
              }}
              className="flex w-full flex-col gap-0.5 rounded-md px-2.5 py-2 text-left text-sm text-fg transition-colors hover:bg-raised"
            >
              Not an issue
              <span className="text-xs text-faint">False positive — the judge got it wrong</span>
            </button>
          </div>
        )}
      </div>
    </div>
  );
}

// OccurrenceRow is one run's instance inside the expander: the run link, its verdict, the
// per-run triage state, and the per-recommendation File-issue draft. run_title is UNTRUSTED
// (the run's issue_title) — rendered as escaped React text.
function OccurrenceRow({ occ, repos, onFiled }: { occ: JudgeOccurrence; repos: Repo[]; onFiled: () => void }) {
  return (
    <li className="rounded-md border border-edge bg-surface/60 px-2.5 py-2">
      <div className="flex flex-wrap items-center gap-2">
        <Link
          to={`/runs/${occ.run_id}`}
          className="min-w-0 max-w-full truncate text-sm font-medium text-fg underline-offset-2 hover:underline"
        >
          {stripUnsafeChars(occ.run_title) || "Untitled run"}
        </Link>
        <OccurrenceVerdictBadge occ={occ} />
        <OccurrenceBucketChip occ={occ} />
      </div>
      {/* Filing stays per-recommendation via the existing #68 browser draft (Decision 3):
          a filed occurrence renders its link, an open one offers the draft, a settled-but-
          unfiled one (done/dismissed) offers nothing to file. */}
      {occ.filed_issue ? (
        <OccurrenceFileIssue runId={occ.run_id} recId={occ.rec_id} filed={occ.filed_issue} repos={repos} />
      ) : occ.bucket === "todo" ? (
        <OccurrenceFileIssue runId={occ.run_id} recId={occ.rec_id} repos={repos} onFiled={onFiled} />
      ) : null}
    </li>
  );
}

// OccurrenceVerdictBadge renders the run's judge verdict in the ONE grammar the product
// uses for that fact: `⚖ issues · 2`, exactly as /runs renders it (PRD #98 review N8).
//
// It previously rendered `<Badge>{verdictLabel(occ.verdict)}</Badge>` → "Issues found",
// which is a SECOND grammar for the same fact on a different screen. Collapsing the mock's
// two grammars into one was a load-bearing correction earlier in this PRD, and that is
// precisely the regression this reintroduced.
//
// It reuses judgeBadge() rather than re-deriving the label, which is why JudgeBadgeable is a
// structural subset (`{judge_verdict, judge_todo_count}`) instead of taking RunListItem —
// the type was made that shape SO this page could pass an occurrence-shaped object.
//
// judge_todo_count is 0 here on purpose. M4's badge counts a RUN's open recommendations, and
// an occurrence is a single coordinate, not a run — synthesising a count would state a
// number this DTO does not carry. judgeBadge drops the count entirely at 0, so the label is
// the bare `⚖ issues`: the same grammar, minus a claim we cannot make.
function OccurrenceVerdictBadge({ occ }: { occ: JudgeOccurrence }) {
  const badge = judgeBadge({ judge_verdict: occ.verdict, judge_todo_count: 0 });
  // Unreachable while the DTO types verdict as a non-null enum; judgeBadge returns null only
  // for an unjudged run, and an occurrence exists because a review produced it.
  if (!badge) return null;
  // THE TITLE IS OVERRIDDEN, and the label deliberately is not (PRD #98 review N-b).
  //
  // One glyph, two inference rules: on /runs the count is always rendered when > 0, so a
  // bare `⚖ issues` there genuinely means "nothing left to triage" (M4 behaviour (c)). Here
  // it means "no count is carried" — and these badges sit on rows that are by construction
  // still open. The two render byte-identically, so a reader who learned the grammar on
  // /runs would parse this as the opposite of the truth.
  //
  // The fix goes in the title rather than the label because the shared visual grammar is
  // what the fable review fought for, and splitting it again to disambiguate would trade one
  // false inference for the two-grammars problem N8 just removed. The distinguishing claim
  // belongs where the claim actually lives.
  return (
    <Badge tone={badge.tone} title={`This run's judge verdict: ${occ.verdict}. Triage state is the chip beside it.`}>
      {badge.label}
    </Badge>
  );
}

// OccurrenceBucketChip renders one occurrence's triage state from its bucket. The
// occurrence DTO carries no dismiss reason, so a HAND-dismissed occurrence reads a plain
// "Dismissed" — the group-level controls carry the won't-do / not-an-issue distinction.
//
// Both DONE and DISMISSED split by provenance (PRD #98 Decision 6; issue #167). A person's
// "✓ Done" and the M6 issue-close sync's "Done via #IID" are different claims, as are a
// person's "Dismissed" and the system's auto-dismissal of a recommendation naming a
// policy-barred credential-bearing CLI (set_via "denied_cli") — rendering either pair
// identically attributes a system inference to the user. The split is on set_via, which is
// the only thing that carries the difference within a single bucket.
function OccurrenceBucketChip({ occ }: { occ: JudgeOccurrence }) {
  switch (occ.bucket) {
    case "done":
      // An auto-done always has a filed link — the sync fires FROM one closing — but the
      // link is rendered defensively anyway: a filed row deleted after the sync would
      // otherwise print "Done via #undefined". Without an iid the provenance is still
      // stated, just unnamed.
      if (occ.set_via === "issue_close") {
        return (
          <Badge tone="ok" title="Marked done automatically when the filed issue was closed">
            <span aria-hidden="true">✓</span>{" "}
            {occ.filed_issue ? `Done via #${occ.filed_issue.issue_iid}` : "Done via issue close"}
          </Badge>
        );
      }
      return (
        <Badge tone="ok">
          <span aria-hidden="true">✓</span> Done
        </Badge>
      );
    case "dismissed":
      if (occ.set_via === "denied_cli") {
        return (
          <Badge
            tone="neutral"
            title="Automatically dismissed: this recommended a credential-bearing CLI that policy permanently bars (e.g. glab, gh, aws)"
          >
            Dismissed · barred CLI
          </Badge>
        );
      }
      return <Badge tone="neutral">Dismissed</Badge>;
    case "filed":
      return occ.filed_issue && isHttpsUrl(occ.filed_issue.issue_url) ? (
        <a
          href={occ.filed_issue.issue_url}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1 text-xs font-medium text-info underline underline-offset-2 hover:text-info"
        >
          Filed #{occ.filed_issue.issue_iid} <ExternalLinkIcon />
        </a>
      ) : (
        <Badge tone="info">Filed</Badge>
      );
    default:
      return <Badge tone="neutral">To do</Badge>;
  }
}

function isHttpsUrl(u: string): boolean {
  try {
    return new URL(u).protocol === "https:";
  } catch {
    return false;
  }
}
