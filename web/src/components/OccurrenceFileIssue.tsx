import { useState } from "react";
import { api, ApiError, type IssueDraft, type JudgeFiledIssueRef, type Repo } from "../lib/api";
import { Alert, Badge, Button, Input, Select, Textarea, cx } from "./ui";
import { ExternalLinkIcon, FileTextIcon } from "./icons";

// isHttpsUrl mirrors RunView: a filed-issue URL is judge-adjacent data, so a link is
// only rendered when the URL is genuinely https (never javascript:/data:).
function isHttpsUrl(u: string): boolean {
  try {
    return new URL(u).protocol === "https:";
  } catch {
    return false;
  }
}

type JustFiled = { iid: number; web_url: string; warning?: string };

// OccurrenceFileIssue is the per-recommendation File-issue affordance INSIDE the Judge
// menu's occurrence expander (PRD #98 M3, Decision 3): a group is disposed in bulk, but
// FILING stays the existing #68 per-recommendation browser draft, reachable from one
// run's occurrence. It drives the same /runs/{id}/review/recommendations/{recID}
// issue-draft + issue endpoints RunView's RecommendationFiler uses, on the same
// cookie+CSRF forge-limited path — this is deliberately a separate, self-contained
// surface (a group occurrence, not a run-page rec) so RunView stays untouched.
//
// Every draft field is INERT text like ProposalCard: title/description are edited raw and
// the load-bearing sanitizer re-runs server-side at the POST. The draft shows RAW markdown
// (no rendered preview) by design. Bulk "file N as one issue" is NOT here — it is a
// follow-up PRD (Decision 3).
export function OccurrenceFileIssue({
  runId,
  recId,
  filed,
  repos,
  onFiled,
}: {
  runId: string;
  recId: string;
  // A settled forge link for this occurrence's coordinate, if any — renders the filed row
  // instead of the File-issue button.
  filed?: JudgeFiledIssueRef;
  repos: Repo[];
  // Called after a successful create so the page can re-read the backlog (the coordinate
  // moves to the `filed` rung, so its group rollup may change).
  onFiled?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<IssueDraft | null>(null);
  const [loadingDraft, setLoadingDraft] = useState(false);
  const [draftErr, setDraftErr] = useState("");
  const [repoId, setRepoId] = useState("");
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [busy, setBusy] = useState(false);
  const [fileErr, setFileErr] = useState("");
  const [local, setLocal] = useState<JustFiled | null>(null);

  // Already filed (a server link) or just filed (local) → the filed row. The occurrence's
  // filed_issue is only ever a SETTLED link (#68) — no in-flight/claimed state reaches here.
  //
  // NO STALE-LINK WARNING, AND THAT IS A DATA LIMIT — NOT A PROOF THE HAZARD IS ABSENT.
  // RunView flags a link filed against an earlier revision of the recommendation
  // (`filed_at < review.updated_at` → "filed for an earlier version"). The same hazard
  // exists on this surface: a re-judge UPSERTS the review IN PLACE and replaces its
  // recommendations (`ON CONFLICT (target_run_id) DO UPDATE … updated_at = now()`, then
  // `DELETE FROM review_recommendations` — api/internal/store/queries/judge.sql), while
  // recommendation_filed_issues is keyed (review_id, category, target) and cascades only
  // from run_reviews — so the link survives the re-judge and can end up pointing at an
  // issue written from text that no longer exists.
  //
  // What is missing is the ability to SEE it: JudgeOccurrence (and its enclosing group)
  // ships no review timestamp — see lib/api.ts — so the comparison RunView makes is not
  // computable from this DTO. The server does not merely have the column, it ALREADY READS
  // it: `rv.updated_at` is the leading sort key of the backlog query
  // (api/internal/store/queries/judge_recommendations.sql:100, `ORDER BY rv.updated_at
  // DESC …`). So it is loaded and simply not selected, and projecting it onto the occurrence
  // is what would let this filed row carry the same warning.
  if (filed || local) {
    const iid = local ? local.iid : filed!.issue_iid;
    const url = local ? local.web_url : filed!.issue_url;
    return (
      <div className="mt-2 rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-xs text-ok">
        <span className="font-medium">Filed.</span>{" "}
        {isHttpsUrl(url) ? (
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="inline-flex items-center gap-1 font-medium underline underline-offset-2 hover:text-ok"
          >
            #{iid} <ExternalLinkIcon />
          </a>
        ) : (
          <span className="font-medium">#{iid}</span>
        )}
        {local?.warning && <p className="mt-1 text-warn">{local.warning}</p>}
      </div>
    );
  }

  const openDraft = async () => {
    setOpen(true);
    setLoadingDraft(true);
    setDraftErr("");
    try {
      const { draft } = await api.getIssueDraft(runId, recId);
      setDraft(draft);
      setRepoId(draft.default_repo_id);
      setTitle(draft.title);
      setDescription(draft.description);
    } catch (e) {
      setDraftErr(e instanceof ApiError ? e.message : "Could not load the draft");
    } finally {
      setLoadingDraft(false);
    }
  };

  const create = async () => {
    setFileErr("");
    setBusy(true);
    try {
      const { issue, warning } = await api.fileIssue(runId, recId, { repo_id: repoId, title, description });
      setLocal({ iid: issue.iid, web_url: issue.web_url, warning });
      onFiled?.();
    } catch (e) {
      // Forge rejected the write: the draft stays open with its edits intact.
      setFileErr(e instanceof ApiError ? e.message : "Could not file the issue");
    } finally {
      setBusy(false);
    }
  };

  if (!open) {
    return (
      <div className="mt-2">
        <Button size="sm" variant="secondary" onClick={openDraft}>
          File issue
        </Button>
      </div>
    );
  }

  return (
    <div className="mt-2 overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center justify-between gap-2 border-b border-brand/20 bg-brand/10 px-3 py-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-brand">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          Draft issue
        </span>
        <Badge tone="brand">needs your review</Badge>
      </div>

      <div className="space-y-3 px-3 py-3">
        {loadingDraft && (
          <p role="status" className="text-sm text-faint">
            Loading draft…
          </p>
        )}
        {draftErr && <Alert message={draftErr} />}
        {draftErr && !draft && (
          <div className="flex flex-wrap gap-2">
            <Button size="sm" onClick={openDraft}>
              Retry
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setOpen(false)}>
              Cancel
            </Button>
          </div>
        )}
        {draft && (
          <>
            {/* Provenance (#68 Decision 8): whose worker produced this (attacker-
                influencable) text — boxed + labeled so an admin filing another user's
                review notices whose text they are about to publish. */}
            {draft.provenance && (
              <div className="rounded-md border border-edge bg-raised/50 px-2.5 py-1.5 text-xs text-muted">
                <span className="font-semibold text-fg">Source:</span> {draft.provenance}
              </div>
            )}
            {fileErr && <Alert message={fileErr} />}

            <div className="space-y-1">
              <label className="block text-xs text-muted">Repo</label>
              <Select value={repoId} onChange={(e) => setRepoId(e.target.value)}>
                <option value="">Select a repo…</option>
                {repos.map((r) => (
                  <option key={r.id} value={r.id}>
                    {r.path_with_namespace}
                  </option>
                ))}
              </Select>
              {draft.default_note && (
                <p
                  role="status"
                  className={cx(
                    "text-xs",
                    repoId
                      ? "text-faint"
                      : "rounded-md border border-info/40 bg-info/10 px-2.5 py-1.5 text-info",
                  )}
                >
                  {draft.default_note}
                </p>
              )}
            </div>

            {/* Inert text (never Markdown): the server re-sanitizes at the POST boundary. */}
            <div className="space-y-1">
              <label className="block text-xs text-muted">Title</label>
              <Input value={title} onChange={(e) => setTitle(e.target.value)} />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Description</label>
              <Textarea
                rows={10}
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                className="max-h-72 font-mono text-xs"
              />
            </div>

            <div className="space-y-1">
              <label className="block text-xs text-muted">Labels</label>
              <div className="flex flex-wrap gap-1">
                {draft.labels.map((l) => (
                  <Badge key={l} tone="neutral">
                    {l}
                  </Badge>
                ))}
              </div>
              <p className="text-xs text-faint">
                Lands on the board and is startable without a PRD file. No autopilot label — nothing runs until you click
                Start.
              </p>
            </div>

            <div className="flex flex-wrap gap-2 pt-0.5">
              <Button size="sm" disabled={busy || !repoId || title.trim() === ""} onClick={create}>
                Create issue
              </Button>
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => setOpen(false)}>
                Cancel
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
