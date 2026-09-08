// The Findings backlog (PRD #333 M7, D7/D8; rebuilt onto Judge's skeleton in PRD #1183 M4): the
// per-repo, coordinate-deduped list of off-task bugs workers flagged mid-run, which the user
// triages the way the judge backlog is triaged — file (turn into a real forge issue on their own
// connection) or dismiss with a reason.
//
// It now renders Judge's skeleton on the shared triage components: a stats-driven summary strip and
// five counted tabs (To triage / Filed / Done / Dismissed / All) whose counts come from the
// canonical GET /findings/stats aggregate (never a tally of the rows on screen), select-all +
// per-row checkboxes + a Dismiss-only MultiSelectBar, an UndoToast after a dismiss, and each row's
// shared TriageActions / TriageStateChip plus an evidence + run-links expander.
//
// KEY ROUTING (PRD #1183 M3, critical): file keys on the EVIDENCE id (finding_id); dismiss (single
// row AND bulk) and undo key on the DISPOSITION id (disposition_id) via the bulk endpoint, so a
// dismiss/undo pair is symmetric and a dismissed coordinate whose evidence cascaded away can still
// be undone.
//
// Two rules copied from the judge/proposal surfaces:
//   - last_title / repo_path / location / evidence_preview / run_title are AGENT-authored,
//     untrusted: rendered as escaped JSX text through stripUnsafeChars, never Markdown (issue #124);
//     any that reach a title=/aria-label= attribute are stripped there too (.claude/rules/web.md).
//   - file/dismiss act on the coordinate's ids; a null finding_id (evidence cascaded away with a
//     deleted run, D12) is a display-only, non-actionable row.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  api,
  ApiError,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type Repo,
  type TriageCounts,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { formatAgo } from "../lib/rateLimits";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { Alert, cx, EmptyState, ListSkeleton, PageHeader } from "../components/ui";
import { BugIcon, ChevronDownIcon, ChevronRightIcon, XIcon } from "../components/icons";
import { TriageSummary } from "./RunView";
import { useSetFindingsOpen } from "../components/FindingsOpenContext";
import { TriageActions } from "../components/triage/TriageActions";
import { TriageStateChip } from "../components/triage/TriageStateChip";
import { IssueDraftCard } from "../components/triage/IssueDraftCard";
import { findingState } from "../components/triage/triageCopy";
import { SelectAllCheckbox } from "../components/triage/SelectAllCheckbox";
import { MultiSelectBar } from "./findings/MultiSelectBar";
import { UndoToast } from "./judge/UndoToast";

// The five rungs the page's tabs expose (PRD #1183 M4). `to_file` is the default: what still needs
// triaging. `all` is now a real tab — a parity call with Judge that reverses PRD #333's
// "triage queue, not an archive browser" choice (the API already accepted `all`).
const FINDING_BUCKETS = ["to_file", "filed", "done", "dismissed", "all"] as const;
type FindingsTab = (typeof FINDING_BUCKETS)[number];

function isTab(v: string | null): v is FindingsTab {
  return v !== null && (FINDING_BUCKETS as readonly string[]).includes(v);
}

// "To triage" for the open bucket — the one vocabulary shared with Judge (PRD #1183), NOT the old
// "To file".
const TAB_LABEL: Record<FindingsTab, string> = {
  to_file: "To triage",
  filed: "Filed",
  done: "Done",
  dismissed: "Dismissed",
  all: "All",
};

// tabCount reads the canonical per-status tally straight from GET /findings/stats — never a tally of
// the (bucket/repo/run-filtered) rows on screen (D7/D8, the same rule Judge follows). `all` reads
// the raw total so its count matches the All list even through a transient filing window.
function tabCount(stats: TriageCounts, b: FindingsTab): number {
  switch (b) {
    case "to_file":
      return stats.todo;
    case "filed":
      return stats.filed;
    case "done":
      return stats.done;
    case "dismissed":
      return stats.dismissed;
    case "all":
      return stats.total;
  }
}

// seenInRunsLabel renders the occurrence count, or "" below 2 (D12 + PRD #1183): "seen in 1 run" is
// noise and "seen in 0 runs" reads wrong on a filed/dismissed row that outlived its evidence, so the
// count is shown only from 2 up.
function seenInRunsLabel(n: number): string {
  if (n < 2) return "";
  return `seen in ${n} runs`;
}

// UNDO_CONCURRENCY bounds the parallel DELETEs an Undo issues (lifted from Judge.tsx). Undo
// re-expands, one request per coordinate, a dismiss the bulk endpoint collapsed into a single
// statement; firing them all at once from the browser is exactly the amplification the one-statement
// write exists to prevent, moved to the client. Small enough to stay polite, large enough that a
// realistic undo is not perceptibly serial.
const UNDO_CONCURRENCY = 6;

// A pending Undo: the message and the disposition ids to reopen. Kept as a plain string[] here (the
// shared UndoToast's Toast.undo is JudgeSettledMember[], adapted at the render site below).
type FindingsToast = { message: string; undo: string[] };

export function Findings() {
  const demo = useDemoMode();
  const [searchParams, setSearchParams] = useSearchParams();
  const runAnchor = searchParams.get("run") ?? "";
  const repoFilter = searchParams.get("repo") ?? "";
  const bucketParam = searchParams.get("bucket");
  const bucket: FindingsTab = isTab(bucketParam) ? bucketParam : "to_file";

  const [backlog, setBacklog] = useState<IncidentalFindingBacklog | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [actionErr, setActionErr] = useState("");
  // Per-coordinate created-with-warning note from the just-clicked File (the forge issue WAS created
  // but its local disposition could not settle): kept so the filed row can surface the warning
  // inline, mirroring what the CLI prints. Keyed by finding_id, cleared on reload.
  const [filedWarnings, setFiledWarnings] = useState<Record<string, string>>({});
  // Coordinates that came back 409 (already filed/dismissed from elsewhere): best-effort, the backlog
  // is the source of truth, so the row shows a friendly note and a reload reconciles.
  const [resolvedIds, setResolvedIds] = useState<Set<string>>(new Set());
  // The checkbox selection is keyed on DISPOSITION ids (what bulk dismiss and undo act on).
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [toast, setToast] = useState<FindingsToast | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Publishes the canonical open count to the nav badge (PRD #1183 M4, the BLK-BADGE pattern). A
  // no-op when this page is mounted outside an AppShell (every unit test does that), which is why the
  // badge-moves regression test mounts the two TOGETHER.
  const setFindingsOpen = useSetFindingsOpen();

  // The canonical per-status tally: repo-scoped, run-ignored (the server does not read ?run= here),
  // so the summary strip, the tabs and the nav badge are ONE number per repo scope. Publishing
  // stats.todo on every fetch (mount, repo change, and every reloadStats() after a mutation) is what
  // moves the badge without a navigation. Best-effort: a failed fetch leaves the strip/tabs hidden.
  const { data: statsData, reload: reloadStats } = useAsyncData(
    async ({ isCurrent }) => {
      const stats = await api.getFindingsStats(repoFilter || undefined);
      if (isCurrent()) setFindingsOpen(stats.todo);
      return stats;
    },
    [repoFilter],
    { mapError: () => "" },
  );
  const stats = statsData;

  // backlog is ALSO written by the in-place patch on file/dismiss/undo, so it stays local and is set
  // as a side effect here rather than bundled into the hook's data. skeleton:"always" reproduces the
  // old load's setLoading(true) on every call including the 409 handlers' reload.
  const {
    loading,
    error,
    reload: load,
  } = useAsyncData(
    async ({ isCurrent }) => {
      const rows = await api.listFindings(bucket, repoFilter || undefined, runAnchor || undefined);
      if (isCurrent()) setBacklog(rows);
    },
    [bucket, repoFilter, runAnchor],
    { skeleton: "always", fallback: "Failed to load the findings backlog" },
  );

  // These resets are tied to a deps change (bucket/repo/run filter), NOT to a reload(): the 409
  // File/Dismiss handlers call load() without clearing them, so they must stay in their own effect
  // keyed on the same deps as the load. Selection is dropped too — a stale checkbox from another tab
  // must never drive a bulk action.
  useEffect(() => {
    setFiledWarnings({});
    setResolvedIds(new Set());
    setSelected(new Set());
  }, [bucket, repoFilter, runAnchor]);

  // The repo scope selector's options. Best-effort (a failure leaves All-repos as the only option,
  // and the backlog still renders); the selector defaults to All-repos-grouped.
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        const { repos } = await api.listRepos();
        if (alive) setRepos(repos);
      } catch {
        /* selector stays at All repos */
      }
    })();
    return () => {
      alive = false;
    };
  }, []);

  // Clear any pending toast timer on unmount.
  useEffect(
    () => () => {
      if (toastTimer.current) clearTimeout(toastTimer.current);
    },
    [],
  );

  const showToast = useCallback((t: FindingsToast) => {
    if (toastTimer.current) clearTimeout(toastTimer.current);
    setToast(t);
    toastTimer.current = setTimeout(() => setToast(null), 9000);
  }, []);

  const setBucket = (b: FindingsTab) => {
    const next = new URLSearchParams(searchParams);
    next.set("bucket", b);
    setSearchParams(next, { replace: true });
  };

  const setRepoFilter = (repoId: string) => {
    const next = new URLSearchParams(searchParams);
    if (repoId) next.set("repo", repoId);
    else next.delete("repo");
    setSearchParams(next, { replace: true });
  };

  const clearRunAnchor = () => {
    const next = new URLSearchParams(searchParams);
    next.delete("run");
    next.set("bucket", bucket);
    setSearchParams(next, { replace: true });
  };

  // patchByFinding flips a coordinate's fields in place keyed by finding_id (the file path), so the
  // acted row re-renders at its new rollup rather than vanishing (the judge's chosen behaviour).
  const patchByFinding = useCallback((findingID: string, patch: Partial<IncidentalFinding>) => {
    setBacklog((prev) =>
      prev
        ? { ...prev, findings: prev.findings.map((f) => (f.finding_id === findingID ? { ...f, ...patch } : f)) }
        : prev,
    );
  }, []);

  // patchByDisposition flips a coordinate's fields in place keyed by disposition_id (the dismiss/undo
  // path — a dismissed coordinate may have no finding_id to key by).
  const patchByDisposition = useCallback((dispositionID: string, patch: Partial<IncidentalFinding>) => {
    setBacklog((prev) =>
      prev
        ? { ...prev, findings: prev.findings.map((f) => (f.disposition_id === dispositionID ? { ...f, ...patch } : f)) }
        : prev,
    );
  }, []);

  const fileFinding = useCallback(
    async (findingID: string, body: { title: string; description: string; labels: string[] }) => {
      setActionErr("");
      try {
        const res = await api.fileFinding(findingID, body);
        // Patch the row to its filed rollup with the DTO's link fields; a non-empty warning is
        // created-with-warning (the issue exists), kept beside the filed row.
        patchByFinding(findingID, {
          status: "filed",
          filed_issue_iid: res.issue.iid,
          filed_issue_url: res.issue.web_url,
        });
        setFiledWarnings((m) => ({ ...m, [findingID]: res.warning ?? "" }));
        // Filing moves a coordinate to_file → filed, so the tabs and the nav badge change.
        reloadStats();
      } catch (e) {
        // A stale File on an already-resolved coordinate is the 409 (the guarded claim): show the
        // friendly "already filed" note and reload so the list reconciles. Swallowed (not rethrown)
        // so the draft card closes; any other error rethrows so the card keeps the edits + shows it.
        if (e instanceof ApiError && e.status === 409) {
          setResolvedIds((s) => new Set(s).add(findingID));
          load();
          return;
        }
        throw e;
      }
    },
    [patchByFinding, load, reloadStats],
  );

  // dismiss routes BOTH a single-row dismiss and a multi-select dismiss through the bulk endpoint
  // (keyed on disposition_id), so Undo (also disposition_id) is symmetric with either. The response
  // re-reads exactly the rows that moved; patch each in place, publish the fresh stats, and offer an
  // Undo over the settled ids.
  const dismiss = useCallback(
    async (dispositionIDs: string[], reason: "wont_do" | "not_an_issue") => {
      if (dispositionIDs.length === 0) return;
      setActionErr("");
      try {
        const res = await api.dismissFindings(dispositionIDs, reason);
        const settled: string[] = [];
        for (const row of res.findings) {
          if (!row.disposition_id) continue;
          settled.push(row.disposition_id);
          patchByDisposition(row.disposition_id, {
            status: row.status,
            dismiss_reason: row.dismiss_reason,
            resolved_at: row.resolved_at,
          });
        }
        reloadStats();
        setSelected(new Set());
        showToast({
          message:
            res.updated === 0
              ? "Nothing to update — those findings were already resolved."
              : `${res.updated} ${res.updated === 1 ? "finding" : "findings"} dismissed.`,
          undo: settled,
        });
      } catch (e) {
        setActionErr(errorMessage(e, "Could not dismiss the finding"));
      }
    },
    [patchByDisposition, reloadStats, showToast],
  );

  const undo = useCallback(async () => {
    const ids = toast?.undo ?? [];
    setToast(null);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    if (ids.length === 0) return;
    setActionErr("");
    // Reopen each dismissed coordinate at BOUNDED concurrency, one DELETE per disposition id — the
    // same queue+workers shape Judge uses so a large undo cannot fan out unbounded from the browser,
    // and a partial failure is reported honestly rather than masked by a first-rejection Promise.all.
    let failed = 0;
    const queue = [...ids];
    const workers = Array.from({ length: Math.min(UNDO_CONCURRENCY, queue.length) }, async () => {
      for (let id = queue.shift(); id !== undefined; id = queue.shift()) {
        try {
          const row = await api.undoDismissFinding(id);
          patchByDisposition(id, { status: row.status, dismiss_reason: undefined, resolved_at: row.resolved_at });
        } catch {
          failed += 1;
        }
      }
    });
    await Promise.all(workers);
    if (failed > 0) {
      setActionErr(
        failed === ids.length
          ? "Could not undo — nothing was reopened."
          : `Partly undone: ${ids.length - failed} of ${ids.length} reopened, ${failed} failed. Re-check the affected findings.`,
      );
    }
    reloadStats();
  }, [toast, patchByDisposition, reloadStats]);

  // In single-repo view the repo chip on each row is redundant (D3), so rows are flat; in the default
  // All-repos view rows are grouped under a repo_path header.
  const singleRepo = repoFilter !== "";
  const groups = useMemo(() => groupByRepo(backlog?.findings ?? []), [backlog]);

  // Select-all + bulk dismiss act only on OPEN coordinates carrying a disposition id (the only rows
  // the bulk endpoint can move). The rest of the list is not selectable.
  const selectableIds = useMemo(
    () =>
      (backlog?.findings ?? [])
        .filter((f) => f.status === "open" && !!f.disposition_id)
        .map((f) => f.disposition_id as string),
    [backlog],
  );
  const allSelected = selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));
  const someSelected = selectableIds.some((id) => selected.has(id));
  const toggleSelectAll = (checked: boolean) => setSelected(checked ? new Set(selectableIds) : new Set());
  const toggleSelect = (id: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // Per-row handlers bound to the row's ids. File keys on finding_id (the evidence id); dismiss keys
  // on disposition_id and routes through the bulk endpoint, so Undo (also disposition_id) is
  // symmetric with a single-row dismiss.
  const rowProps = (f: IncidentalFinding) => ({
    finding: f,
    selectable: f.status === "open" && !!f.disposition_id,
    selected: f.disposition_id ? selected.has(f.disposition_id) : false,
    onToggleSelect: () => f.disposition_id && toggleSelect(f.disposition_id),
    repoLabel: stripUnsafeChars(maskRepoPath(f.repo_path, demo)),
    warning: f.finding_id ? filedWarnings[f.finding_id] : undefined,
    resolved: f.finding_id ? resolvedIds.has(f.finding_id) : false,
    onFile: (body: { title: string; description: string; labels: string[] }) =>
      f.finding_id ? fileFinding(f.finding_id, body) : Promise.resolve(),
    onDismiss: (reason: "wont_do" | "not_an_issue") => {
      if (f.disposition_id) dismiss([f.disposition_id], reason);
    },
  });

  return (
    <div className="space-y-6 pb-24">
      <PageHeader
        titleNode={
          <h1 className="flex items-center gap-2 text-xl font-semibold tracking-tight">
            <span className="text-info" aria-hidden="true">
              <BugIcon />
            </span>
            Findings
          </h1>
        }
        description="Off-task bugs your workers flagged mid-run, deduped per repo. File one as a real issue or dismiss it."
      />

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      {runAnchor && (
        <div className="flex flex-wrap items-center gap-2 rounded-lg border border-info/30 bg-info/[0.06] px-3 py-2 text-sm">
          <span className="text-muted">Filtered to one run's findings (from a notification).</span>
          <button
            type="button"
            onClick={clearRunAnchor}
            className="inline-flex min-h-[24px] items-center gap-1 rounded-md px-2 py-1 text-xs font-medium text-muted transition-colors hover:bg-raised hover:text-fg"
          >
            <XIcon /> Clear filter
          </button>
        </div>
      )}

      {/* The canonical summary strip (PRD #1183 M4): counts from GET /findings/stats, scoped by the
          repo select, ignoring the run anchor. */}
      {stats && stats.total > 0 && <TriageSummary triage={stats} title="Findings · all your repos" />}

      {/* Repo scope selector — defaults to All repos (grouped). */}
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor="findings-repo" className="text-xs font-semibold uppercase tracking-wide text-muted">
          Repo
        </label>
        <select
          id="findings-repo"
          value={repoFilter}
          onChange={(e) => setRepoFilter(e.target.value)}
          className="rounded-md border border-edge bg-surface px-2 py-1 text-sm text-fg"
        >
          <option value="">All repos</option>
          {repos.map((r) => (
            <option key={r.id} value={r.id}>
              {maskRepoPath(r.path_with_namespace, demo)}
            </option>
          ))}
        </select>
      </div>

      {/* Five counted tabs (PRD #1183 M4), counts straight from the canonical stats aggregate, brand
          active underline — mirroring the Judge bucket tabs. The strip renders UNCONDITIONALLY: it is
          the only affordance that calls setBucket, so a failed stats fetch must not strand the user in
          the current bucket. Only the per-bucket count is canonical-stats-only and is simply absent
          when the stats fetch failed. */}
      <div role="tablist" aria-label="Findings bucket" className="flex flex-wrap gap-1 border-b border-edge">
        {FINDING_BUCKETS.map((b) => {
          const active = b === bucket;
          return (
            <button
              key={b}
              role="tab"
              aria-selected={active}
              onClick={() => setBucket(b)}
              className={cx(
                "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                active
                  ? "border-brand text-fg"
                  : "border-transparent text-faint hover:border-edge-strong hover:text-muted",
              )}
            >
              {TAB_LABEL[b]}
              {stats && <span className="ml-1.5 tabular-nums text-faint">{tabCount(stats, b)}</span>}
            </button>
          );
        })}
      </div>

      {loading && <ListSkeleton rows={4} />}

      {!loading && backlog && (
        backlog.findings.length === 0 ? (
          <EmptyState
            icon={<BugIcon />}
            title={runAnchor ? "No findings for this run" : `Nothing under ${TAB_LABEL[bucket]}`}
            description="Switch buckets or clear a filter to see findings in another state."
          />
        ) : (
          <>
            {selectableIds.length > 0 && (
              <div className="flex items-center px-1">
                <SelectAllCheckbox
                  checked={allSelected}
                  indeterminate={someSelected && !allSelected}
                  onChange={toggleSelectAll}
                  label={`Select all ${selectableIds.length} shown`}
                />
              </div>
            )}
            {singleRepo ? (
              <ul className="space-y-2">
                {backlog.findings.map((f) => (
                  <FindingRow key={rowKey(f)} {...rowProps(f)} />
                ))}
              </ul>
            ) : (
              <div className="space-y-5">
                {groups.map((g) => (
                  <section key={g.repo_id} className="space-y-2">
                    <h2 className="font-mono text-xs font-semibold text-muted">
                      {stripUnsafeChars(maskRepoPath(g.repo_path, demo))}
                    </h2>
                    <ul className="space-y-2">
                      {g.findings.map((f) => (
                        <FindingRow key={rowKey(f)} {...rowProps(f)} />
                      ))}
                    </ul>
                  </section>
                ))}
              </div>
            )}
          </>
        )
      )}

      {selected.size > 0 && (
        <MultiSelectBar
          count={selected.size}
          onClear={() => setSelected(new Set())}
          onDismiss={(reason) => dismiss([...selected], reason)}
        />
      )}

      {toast && (
        <UndoToast
          // UndoToast is shared with Judge; it reads only toast.message and toast.undo.length. Its
          // Toast.undo is typed JudgeSettledMember[], so adapt our disposition-id list to that shape
          // and length — the Undo handler above reads our own string[], not this.
          toast={{ message: toast.message, undo: toast.undo.map((id) => ({ run_id: id, rec_id: id })) }}
          onUndo={undo}
          onDismiss={() => setToast(null)}
        />
      )}
    </div>
  );
}

// rowKey keys a row by coordinate — disposition_id (always present) first, else finding_id, else
// repo+location (belt and braces for a legacy row missing both).
function rowKey(f: IncidentalFinding): string {
  return f.disposition_id ?? f.finding_id ?? `${f.repo_id}:${f.location}`;
}

interface RepoGroup {
  repo_id: string;
  repo_path: string;
  findings: IncidentalFinding[];
}

// groupByRepo groups findings under their repo, preserving the server's row order within each group
// and the first-seen repo order across groups.
function groupByRepo(findings: IncidentalFinding[]): RepoGroup[] {
  const out: RepoGroup[] = [];
  const byId = new Map<string, RepoGroup>();
  for (const f of findings) {
    let g = byId.get(f.repo_id);
    if (!g) {
      g = { repo_id: f.repo_id, repo_path: f.repo_path, findings: [] };
      byId.set(f.repo_id, g);
      out.push(g);
    }
    g.findings.push(f);
  }
  return out;
}

// FindingRow is one coordinate on the shared triage row: the inert last_title, its location,
// "seen in N runs" (from 2 up), the shared TriageStateChip, a chevron expander showing the newest
// evidence and one link per run it was seen in, and the shared TriageActions (File issue · Dismiss ▾,
// no Mark done — a finding's done comes only from its issue closing). A null finding_id row is
// display-only (no actions); its evidence has cascaded away. A non-empty `warning` is the
// created-with-warning note, surfaced inline beneath the row, mirroring the CLI.
function FindingRow({
  finding,
  selectable,
  selected,
  onToggleSelect,
  repoLabel,
  warning,
  resolved,
  onFile,
  onDismiss,
}: {
  finding: IncidentalFinding;
  selectable: boolean;
  selected: boolean;
  onToggleSelect: () => void;
  repoLabel: string;
  warning?: string;
  resolved: boolean;
  onFile: (body: { title: string; description: string; labels: string[] }) => Promise<void>;
  onDismiss: (reason: "wont_do" | "not_an_issue") => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [filing, setFiling] = useState(false);
  const actionable = !!finding.finding_id && finding.status === "open" && !resolved;
  const seen = seenInRunsLabel(finding.seen_in_runs);
  const occurrences = finding.occurrences ?? [];
  const hasEvidence = (finding.evidence_preview?.trim() ?? "") !== "" || occurrences.length > 0;
  // A 409'd stale row shows the neutral "already resolved" note in place of the chip.
  const showResolved = resolved && finding.status === "open";
  // findingState wants the reason as the closed union; the wire types dismiss_reason as a bare
  // string, so narrow it here (an unknown reason falls to the bare "Dismissed").
  const dismissReason =
    finding.dismiss_reason === "not_an_issue" || finding.dismiss_reason === "wont_do"
      ? finding.dismiss_reason
      : undefined;
  const state = findingState({
    status: finding.status,
    dismiss_reason: dismissReason,
    set_via: finding.set_via,
    filed_issue_iid: finding.filed_issue_iid,
    filed_issue_url: finding.filed_issue_url,
  });

  return (
    <li className="rounded-lg border border-edge bg-raised/40">
      <div className="flex flex-wrap items-start gap-2 px-3 py-2.5">
        {selectable && (
          <input
            type="checkbox"
            checked={selected}
            onChange={onToggleSelect}
            // last_title is agent-authored — stripped here because it reaches an aria-label attribute
            // (.claude/rules/web.md), not only where it renders as text below.
            aria-label={`Select ${stripUnsafeChars(finding.last_title) || "finding"}`}
            className="mt-1 h-4 w-4 shrink-0 accent-brand"
          />
        )}
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-fg">{stripUnsafeChars(finding.last_title) || "Untitled finding"}</p>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs">
            <code className="max-w-full break-all rounded bg-raised px-1.5 py-0.5 font-mono text-faint">
              {stripUnsafeChars(finding.location)}
            </code>
            {seen && <span className="text-faint">{seen}</span>}
            {showResolved ? (
              <span className="rounded bg-raised px-1.5 py-0.5 text-faint">already resolved</span>
            ) : (
              <TriageStateChip {...state} />
            )}
          </div>
          {finding.status === "filed" && warning && (
            // warning is a server-authored constant (created-with-warning), not model text.
            <p className="mt-1 text-xs text-muted">· {warning}</p>
          )}
        </div>

        <div className="flex shrink-0 items-center gap-1.5">
          {actionable && !filing && (
            <TriageActions onFile={() => setFiling(true)} onDismiss={onDismiss} dismissCopy="finding" />
          )}
          {hasEvidence && (
            <button
              type="button"
              onClick={() => setExpanded((v) => !v)}
              aria-expanded={expanded}
              aria-label={expanded ? "Collapse evidence" : "Expand evidence"}
              className="rounded-md p-1 text-faint transition-colors hover:bg-raised hover:text-fg"
            >
              {expanded ? <ChevronDownIcon /> : <ChevronRightIcon />}
            </button>
          )}
        </div>
      </div>

      {/* Filing opens the shared IssueDraftCard in FIXED-REPO mode (the finding draft carries no repo;
          the coordinate fixes it server-side). Create posts the human's edits to fileFinding(finding_id).
          A successful create closes the card; a non-409 error keeps it open with the edits intact. */}
      {actionable && filing && finding.finding_id && (
        <div className="border-t border-edge px-3 py-2.5">
          <IssueDraftCard
            fixedRepoLabel={repoLabel}
            loadDraft={async () => {
              const draft = await api.findingIssueDraft(finding.finding_id!);
              return {
                title: draft.title,
                description: draft.description,
                labels: draft.labels,
                provenance: draft.provenance,
              };
            }}
            onCreate={async (values) => {
              await onFile({
                title: values.title,
                description: values.description,
                labels: values.labels,
              });
              setFiling(false);
            }}
            onCancel={() => setFiling(false)}
          />
        </div>
      )}

      {expanded && hasEvidence && (
        <div className="space-y-2 border-t border-edge px-3 py-2.5">
          {finding.evidence_preview?.trim() && (
            // evidence_preview is UNTRUSTED worker text — escaped React text, never Markdown, stripped
            // (issue #124), like the judge's rationale_preview.
            <p className="whitespace-pre-wrap text-sm text-muted">{stripUnsafeChars(finding.evidence_preview)}</p>
          )}
          {occurrences.length > 0 && (
            <ul className="space-y-1.5">
              {occurrences.map((occ) => (
                <li key={`${occ.run_id} ${occ.reported_at}`} className="flex flex-wrap items-center gap-2 text-xs">
                  <Link
                    to={`/runs/${occ.run_id}`}
                    // run_title is agent-adjacent — stripped both where it renders and in the title
                    // attribute (.claude/rules/web.md).
                    title={stripUnsafeChars(occ.run_title)}
                    className="min-w-0 max-w-full truncate font-medium text-fg underline-offset-2 hover:underline"
                  >
                    {stripUnsafeChars(occ.run_title) || "Untitled run"}
                  </Link>
                  <span className="text-faint">{formatAgo(occ.reported_at)}</span>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
    </li>
  );
}
