// The Findings backlog (PRD #333 M7, D7/D8): the per-repo, coordinate-deduped list of off-task
// bugs workers flagged mid-run, which the user triages the way the judge backlog is triaged —
// file (turn into a real forge issue on their own connection) or dismiss with a reason.
//
// It mirrors the Judge page's shape (a bucket segmented control, a run-anchor deep-link from the
// notification, owner-scoped reads) but differs deliberately: dedup lives in the storage
// coordinate (D7), so a row is one (repo, location) coordinate carrying "seen in N runs", and the
// backlog groups/filters by repo because the coordinate is repo-scoped by construction (D3).
//
// Two rules copied from the judge/proposal surfaces:
//   - last_title / repo_path / location are AGENT-authored, untrusted: rendered as escaped JSX
//     text through stripUnsafeChars, never Markdown (issue #124).
//   - the row acts on its `finding_id`; a null finding_id (evidence cascaded away with a deleted
//     run, D12) is a display-only, non-actionable row.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  api,
  ApiError,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type IncidentalFindingBucket,
  type Repo,
} from "../lib/api";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { Alert, Badge, Button, cx, EmptyState, ListSkeleton, PageHeader } from "../components/ui";
import { AlertIcon, XIcon } from "../components/icons";

// The three rungs the page's segmented control exposes (the `all` bucket the API also accepts is
// deliberately not a tab — the backlog is a triage queue, not an archive browser). `to_file` is
// the default: what still needs filing.
const FINDING_BUCKETS = ["to_file", "filed", "dismissed"] as const;
type FindingsTab = (typeof FINDING_BUCKETS)[number];

function isTab(v: string | null): v is FindingsTab {
  return v !== null && (FINDING_BUCKETS as readonly string[]).includes(v);
}

const TAB_LABEL: Record<FindingsTab, string> = {
  to_file: "To file",
  filed: "Filed",
  dismissed: "Dismissed",
};

// seenInRunsLabel renders the occurrence count, or "" for a coordinate whose evidence was all
// cascaded away with deleted runs (seen_in_runs 0): "seen in 0 runs" reads wrong on a
// filed/dismissed row that outlived its evidence, so the count is suppressed there (D12).
function seenInRunsLabel(n: number): string {
  if (n <= 0) return "";
  return `seen in ${n} ${n === 1 ? "run" : "runs"}`;
}

export function Findings() {
  const demo = useDemoMode();
  const [searchParams, setSearchParams] = useSearchParams();
  const runAnchor = searchParams.get("run") ?? "";
  const repoFilter = searchParams.get("repo") ?? "";
  const bucketParam = searchParams.get("bucket");
  const bucket: FindingsTab = isTab(bucketParam) ? bucketParam : "to_file";

  const [backlog, setBacklog] = useState<IncidentalFindingBacklog | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionErr, setActionErr] = useState("");
  // Per-coordinate created-with-warning note from the just-clicked File (the forge issue WAS
  // created but its local disposition could not settle): kept so the filed row can surface the
  // warning inline, mirroring what the CLI prints. Keyed by finding_id, cleared on reload.
  const [filedWarnings, setFiledWarnings] = useState<Record<string, string>>({});
  // Coordinates that came back 409 (already filed/dismissed from elsewhere): best-effort, the
  // backlog is the source of truth, so the row shows a friendly note and a reload reconciles.
  const [resolvedIds, setResolvedIds] = useState<Set<string>>(new Set());

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const data = await api.listFindings(
        bucket as IncidentalFindingBucket,
        repoFilter || undefined,
        runAnchor || undefined,
      );
      setBacklog(data);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load the findings backlog");
    } finally {
      setLoading(false);
    }
  }, [bucket, repoFilter, runAnchor]);

  useEffect(() => {
    setFiledWarnings({});
    setResolvedIds(new Set());
    load();
  }, [load]);

  // The repo scope selector's options. Best-effort (a failure leaves All-repos as the only
  // option, and the backlog still renders); the selector defaults to All-repos-grouped.
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

  // patchRow flips a coordinate's status in place so the acted row re-renders at its new rollup
  // rather than vanishing (the judge's chosen behaviour), keyed by finding_id.
  const patchRow = useCallback((findingID: string, patch: Partial<IncidentalFinding>) => {
    setBacklog((prev) =>
      prev
        ? { ...prev, findings: prev.findings.map((f) => (f.finding_id === findingID ? { ...f, ...patch } : f)) }
        : prev,
    );
  }, []);

  const fileFinding = useCallback(
    async (findingID: string) => {
      setActionErr("");
      try {
        const res = await api.fileFinding(findingID);
        // Patch the row to its filed rollup with the DTO's link fields (filed_issue_url is now the
        // single source the status badge links through — no session-local link map). A non-empty
        // warning is created-with-warning: the issue exists, so keep the note beside the filed row.
        patchRow(findingID, {
          status: "filed",
          filed_issue_iid: res.issue.iid,
          filed_issue_url: res.issue.web_url,
        });
        setFiledWarnings((m) => ({ ...m, [findingID]: res.warning ?? "" }));
      } catch (e) {
        // A stale File on an already-resolved coordinate is the M5 409 (the guarded claim). It
        // is best-effort, not an error: show the friendly "already filed" note and reload so the
        // list reconciles to the truth.
        if (e instanceof ApiError && e.status === 409) {
          setResolvedIds((s) => new Set(s).add(findingID));
          load();
        } else {
          setActionErr(e instanceof ApiError ? e.message : "Could not file the finding");
        }
      }
    },
    [patchRow, load],
  );

  const dismissFinding = useCallback(
    async (findingID: string, reason: "wont_do" | "not_an_issue") => {
      setActionErr("");
      try {
        await api.dismissFinding(findingID, reason);
        patchRow(findingID, { status: "dismissed" });
      } catch (e) {
        if (e instanceof ApiError && e.status === 409) {
          setResolvedIds((s) => new Set(s).add(findingID));
          load();
        } else {
          setActionErr(e instanceof ApiError ? e.message : "Could not dismiss the finding");
        }
      }
    },
    [patchRow, load],
  );

  // In single-repo view the repo chip on each row is redundant (D3), so it is dropped; in the
  // default All-repos view rows are grouped under a repo_path header.
  const singleRepo = repoFilter !== "";
  const groups = useMemo(() => groupByRepo(backlog?.findings ?? []), [backlog]);

  return (
    <div className="space-y-6 pb-16">
      <PageHeader
        titleNode={
          <h1 className="flex items-center gap-2 text-xl font-semibold tracking-tight">
            <span className="text-info" aria-hidden="true">
              <AlertIcon />
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

      {/* Bucket segmented control (mirrors Judge's bucket tabs). */}
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
                  ? "border-info text-fg"
                  : "border-transparent text-faint hover:border-edge-strong hover:text-muted",
              )}
            >
              {TAB_LABEL[b]}
            </button>
          );
        })}
      </div>

      {loading && <ListSkeleton rows={4} />}

      {!loading && backlog && (
        backlog.findings.length === 0 ? (
          <EmptyState
            icon={<AlertIcon />}
            title={runAnchor ? "No findings for this run" : `Nothing under ${TAB_LABEL[bucket]}`}
            description="Switch buckets or clear a filter to see findings in another state."
          />
        ) : singleRepo ? (
          <ul className="space-y-2">
            {backlog.findings.map((f) => (
              <FindingRow
                key={rowKey(f)}
                finding={f}
                warning={f.finding_id ? filedWarnings[f.finding_id] : undefined}
                resolved={f.finding_id ? resolvedIds.has(f.finding_id) : false}
                onFile={fileFinding}
                onDismiss={dismissFinding}
              />
            ))}
          </ul>
        ) : (
          <div className="space-y-5">
            {groups.map((g) => (
              <section key={g.repo_id} className="space-y-2">
                <h2 className="font-mono text-xs font-semibold text-muted">{stripUnsafeChars(maskRepoPath(g.repo_path, demo))}</h2>
                <ul className="space-y-2">
                  {g.findings.map((f) => (
                    <FindingRow
                      key={rowKey(f)}
                      finding={f}
                      warning={f.finding_id ? filedWarnings[f.finding_id] : undefined}
                      resolved={f.finding_id ? resolvedIds.has(f.finding_id) : false}
                      onFile={fileFinding}
                      onDismiss={dismissFinding}
                    />
                  ))}
                </ul>
              </section>
            ))}
          </div>
        )
      )}
    </div>
  );
}

// rowKey keys a row by coordinate — finding_id when present, else repo+location (a display-only
// row whose evidence cascaded away still needs a stable key).
function rowKey(f: IncidentalFinding): string {
  return f.finding_id ?? `${f.repo_id}:${f.location}`;
}

interface RepoGroup {
  repo_id: string;
  repo_path: string;
  findings: IncidentalFinding[];
}

// groupByRepo groups findings under their repo, preserving the server's row order within each
// group and the first-seen repo order across groups.
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

// FindingRow is one coordinate: the inert last_title, "seen in N runs", the status, and the
// File/Dismiss actions when the coordinate is open and actionable. A null finding_id row is
// display-only (no actions); a filed row links its issue through the DTO's filed_issue_url. A
// non-empty `warning` is the created-with-warning note (the issue was created but the local
// disposition could not settle), surfaced inline beneath the row, mirroring the CLI.
function FindingRow({
  finding,
  warning,
  resolved,
  onFile,
  onDismiss,
}: {
  finding: IncidentalFinding;
  warning?: string;
  resolved: boolean;
  onFile: (id: string) => void;
  onDismiss: (id: string, reason: "wont_do" | "not_an_issue") => void;
}) {
  const [dismissing, setDismissing] = useState(false);
  const actionable = !!finding.finding_id && finding.status === "open" && !resolved;
  const seen = seenInRunsLabel(finding.seen_in_runs);

  return (
    <li className="rounded-lg border border-edge bg-raised/40">
      <div className="flex flex-wrap items-start gap-2 px-3 py-2.5">
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-fg">
            {stripUnsafeChars(finding.last_title) || "Untitled finding"}
          </p>
          <div className="mt-1 flex flex-wrap items-center gap-2 text-xs">
            <code className="max-w-full break-all rounded bg-raised px-1.5 py-0.5 font-mono text-faint">{stripUnsafeChars(finding.location)}</code>
            {seen && <span className="text-faint">{seen}</span>}
            <FindingStatusBadge finding={finding} resolved={resolved} />
          </div>
          {finding.status === "filed" && warning && (
            // warning is a server-authored constant (created-with-warning), not model text.
            <p className="mt-1 text-xs text-muted">· {warning}</p>
          )}
        </div>

        {actionable && !dismissing && (
          <div className="flex shrink-0 items-center gap-1.5">
            <Button size="sm" variant="secondary" onClick={() => onFile(finding.finding_id!)}>
              File
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setDismissing(true)}>
              Dismiss
            </Button>
          </div>
        )}
        {actionable && dismissing && (
          <div className="flex shrink-0 flex-wrap items-center gap-1.5">
            <Button size="sm" variant="secondary" onClick={() => onDismiss(finding.finding_id!, "wont_do")}>
              Won&apos;t do
            </Button>
            <Button size="sm" variant="secondary" onClick={() => onDismiss(finding.finding_id!, "not_an_issue")}>
              Not an issue
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setDismissing(false)}>
              Cancel
            </Button>
          </div>
        )}
      </div>
    </li>
  );
}

function FindingStatusBadge({
  finding,
  resolved,
}: {
  finding: IncidentalFinding;
  resolved: boolean;
}) {
  if (resolved && finding.status === "open") {
    return <Badge tone="neutral">already resolved</Badge>;
  }
  switch (finding.status) {
    case "filed": {
      const iid = finding.filed_issue_iid;
      // Link "Filed #<iid>" through the DTO's filed_issue_url whenever it is a real https URL —
      // for a backlog-loaded filed row as well as one just filed this session (patchRow stamps
      // filed_issue_url from the file result). Inert text when the url is absent/empty.
      const url = finding.filed_issue_url;
      if (url && isHttps(url)) {
        return (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 font-medium text-info underline underline-offset-2 hover:text-info"
          >
            {iid != null ? `Filed #${iid}` : "Filed"}
          </a>
        );
      }
      return <Badge tone="info">{iid != null ? `Filed #${iid}` : "Filed"}</Badge>;
    }
    case "filing":
      return <Badge tone="warning">filing…</Badge>;
    case "dismissed":
      return <Badge tone="neutral">Dismissed</Badge>;
    default:
      return <Badge tone="info">To file</Badge>;
  }
}

function isHttps(u: string): boolean {
  try {
    return new URL(u).protocol === "https:";
  } catch {
    return false;
  }
}
