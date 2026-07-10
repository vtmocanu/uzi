import { useState } from "react";
import { api, ApiError, isHttpsUrl, type IssueProposal, type ProposalStatus } from "../lib/api";
import { Badge, Button, cx } from "./ui";
import { ExternalLinkIcon, FileTextIcon } from "./icons";

// ProposalCard renders a chat agent's issue draft (PRD #39 Decision 8) as a
// human-gated card. The load-bearing rule: title/description/labels are
// MODEL-authored and untrusted, so they render as plain INERT JSX text — never
// through Markdown — and no model-supplied link is ever made clickable (the same
// rule PRD #37 applies to repo-agent descriptions). The card holds no forge tool;
// the write happens only when the human clicks Create, and only then does a real,
// app-rendered issue link appear.
export function ProposalCard({
  chatId,
  proposal,
  onResolved,
}: {
  chatId: string;
  proposal: IssueProposal;
  // Lets the conversation persist the new status if it wants to; the card also
  // keeps its own resolved copy so a stream append never reverts the button state.
  onResolved?: (p: IssueProposal) => void;
}) {
  const [current, setCurrent] = useState<IssueProposal>(proposal);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");

  const act = async (fn: () => Promise<{ proposal: IssueProposal }>) => {
    setErr("");
    setBusy(true);
    try {
      const { proposal: p } = await fn();
      setCurrent(p);
      onResolved?.(p);
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "Action failed");
    } finally {
      setBusy(false);
    }
  };

  const status: ProposalStatus = current.status;
  const issueUrl = current.created_issue_url;
  const issueIid = current.created_issue_iid;

  return (
    <div className="overflow-hidden rounded-xl border border-brand/40 bg-brand/[0.06]">
      <div className="flex items-center justify-between gap-2 border-b border-brand/20 bg-brand/10 px-3 py-2">
        <span className="inline-flex items-center gap-1.5 text-xs font-semibold text-brand">
          <span aria-hidden="true">
            <FileTextIcon />
          </span>
          Proposed issue
        </span>
        <ProposalStatusBadge status={status} />
      </div>

      <div className="space-y-3 px-3 py-3">
        {/* Every field below is inert model text: rendered as escaped JSX, never
            Markdown, so a link in the title/description is not clickable. */}
        <div className="space-y-1">
          {current.repo_path && (
            <p className="font-mono text-[11px] text-faint">{current.repo_path}</p>
          )}
          <p className="text-sm font-semibold text-fg">{current.title}</p>
        </div>
        {current.description && (
          <p className="whitespace-pre-wrap break-words text-sm text-muted">
            {current.description}
          </p>
        )}
        {current.labels.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {current.labels.map((l) => (
              <Badge key={l} tone="neutral">
                {l}
              </Badge>
            ))}
          </div>
        )}

        {err && <p className="text-xs text-danger">{err}</p>}

        {status === "pending" && (
          <div className="flex flex-wrap gap-2 pt-0.5">
            <Button size="sm" disabled={busy} onClick={() => act(() => api.confirmProposal(chatId, current.id))}>
              Create issue
            </Button>
            <Button
              size="sm"
              variant="secondary"
              disabled={busy}
              onClick={() => act(() => api.dismissProposal(chatId, current.id))}
            >
              Dismiss
            </Button>
          </div>
        )}

        {status === "confirmed" && (
          <div className="rounded-lg border border-ok/40 bg-ok/10 px-3 py-2 text-sm text-ok">
            <span className="font-medium">Issue created.</span>{" "}
            {/* The link is app-rendered from the confirm response, not model text,
                and only turned into an anchor when it is a real https URL. */}
            {isHttpsUrl(issueUrl) ? (
              <a
                href={issueUrl!}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 font-medium underline underline-offset-2 hover:text-ok"
              >
                {issueIid != null ? `#${issueIid}` : "Open issue"} <ExternalLinkIcon />
              </a>
            ) : (
              issueIid != null && <span className="font-medium">#{issueIid}</span>
            )}
          </div>
        )}

        {status === "dismissed" && (
          <p className={cx("text-sm text-faint")}>Dismissed. Nothing was written to the forge.</p>
        )}
      </div>
    </div>
  );
}

function ProposalStatusBadge({ status }: { status: ProposalStatus }) {
  if (status === "confirmed") return <Badge tone="ok">created</Badge>;
  if (status === "dismissed") return <Badge tone="neutral">dismissed</Badge>;
  return <Badge tone="brand">needs your review</Badge>;
}
