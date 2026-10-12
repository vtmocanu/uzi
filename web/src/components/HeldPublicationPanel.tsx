import { useEffect, useState } from "react";

import { api, type HeldPublication, type Run } from "../lib/api";
import { formatCountdown } from "../lib/limitWait";
import { shortSha } from "../lib/recovery";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { Badge, Button, Card, SectionTitle, type BadgeTone } from "./ui";
import { BranchIcon } from "./icons";

// HeldPublicationPanel is the failed-run page's view of the held-work publication
// (issue #2545): the failed run's commits and uncommitted work, secret-scanned and published
// to a run-scoped forge ref before the worker's copy was archived. It sits beside SalvagePanel
// and shares its visual language and copy discipline.
//
// Data comes from the owner route GET /api/runs/{id}/held-publications, fetched only for a
// failed run. An empty list, a failed fetch or an older server (404) renders nothing.
//
// Every server field is untrusted text: it renders escaped through stripUnsafeChars, never as
// Markdown. The ref and the fetch command are shown only when the ref is exactly
// refs/uzi-held/<this run's id>/<generation>, so the panel never hands an operator a foreign
// ref to paste into a shell.

// Same charset discipline as SalvagePanel's SALVAGE_REF_SAFE_RE: even an unexpected run id
// cannot put a shell metacharacter or a line break into the command.
const HELD_REF_SAFE_RE = /^refs\/uzi-held\/[A-Za-z0-9-]+\/[0-9]+$/;

type StateView = { badge: string; tone: BadgeTone };

// The badge carries the meaning in words, so colour is never the only signal (WCAG 1.4.1).
const STATE_VIEWS: Record<string, StateView> = {
  acknowledged: { badge: "Published", tone: "ok" },
  created: { badge: "Publishing", tone: "info" },
  invoked: { badge: "Confirming", tone: "info" },
  create_unknown: { badge: "Confirming", tone: "info" },
  delete_unknown: { badge: "Removing", tone: "neutral" },
  deleted: { badge: "Removed", tone: "neutral" },
  refused: { badge: "Not published", tone: "warning" },
  abandoned: { badge: "Gone", tone: "neutral" },
};

// An unknown (or not yet vetted, such as prepared) state gets a neutral generic badge.
const GENERIC_VIEW: StateView = { badge: "Held", tone: "neutral" };

// Only these states can have a ref on the forge worth fetching.
const FETCHABLE_STATES = new Set(["acknowledged", "created"]);

function safeHeldRef(runId: string, pub: HeldPublication): string | null {
  const ref = pub.ref;
  if (typeof ref !== "string" || !ref.startsWith(`refs/uzi-held/${runId}/`)) return null;
  return HELD_REF_SAFE_RE.test(ref) ? ref : null;
}

export function HeldPublicationPanel({ run }: { run: Run }) {
  const failed = run.status === "failed";
  const [pubs, setPubs] = useState<HeldPublication[]>([]);

  useEffect(() => {
    if (!failed) {
      setPubs([]);
      return;
    }
    let live = true;
    api
      .getRunHeldPublications(run.id)
      .then((r) => {
        if (live) setPubs(Array.isArray(r?.publications) ? r.publications : []);
      })
      .catch(() => {
        if (live) setPubs([]);
      });
    return () => {
      live = false;
    };
  }, [failed, run.id]);

  if (!failed || pubs.length === 0) return null;

  return (
    <Card id="held-publication" className="scroll-mt-20 space-y-3 p-4">
      <div className="flex items-center gap-2">
        <BranchIcon className="h-4 w-4 text-muted" aria-hidden="true" />
        <SectionTitle>Held work</SectionTitle>
      </div>
      <p className="text-sm text-muted">
        This failed run's commits and uncommitted work were secret-scanned and published to a
        run-scoped ref. The ref is deleted when retention expires; run{" "}
        <code className="font-mono text-xs">uzi run held expire {stripUnsafeChars(run.id)}</code> to
        delete it sooner.
      </p>
      <ul className="space-y-3">
        {pubs.map((p) => (
          <li key={p.id}>
            <PublicationRow runId={run.id} pub={p} />
          </li>
        ))}
      </ul>
    </Card>
  );
}

function PublicationRow({ runId, pub }: { runId: string; pub: HeldPublication }) {
  const now = useNow(60_000);
  const view = STATE_VIEWS[pub.state] ?? GENERIC_VIEW;
  const ref = safeHeldRef(runId, pub);
  const tip = pub.tip ? stripUnsafeChars(shortSha(pub.tip)) : null;
  const expiresAt = pub.expires_at ?? null;
  const expiresMs = expiresAt ? Date.parse(expiresAt) : NaN;
  const expiresAbs = Number.isFinite(expiresMs)
    ? new Date(expiresMs).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })
    : null;
  const expiresIn = formatCountdown(expiresAt, now);
  const fetchCommand = ref && FETCHABLE_STATES.has(pub.state) ? `git fetch origin ${ref}` : null;
  const lastError = pub.last_error?.trim() ? stripUnsafeChars(pub.last_error) : null;
  const refusal = pub.refusal_reason?.trim() ? stripUnsafeChars(pub.refusal_reason) : null;

  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <Badge tone={view.tone}>{view.badge}</Badge>
        {ref && (
          <span className="break-all font-mono text-xs text-muted">
            {ref}
            {tip ? ` @ ${tip}` : ""}
          </span>
        )}
      </div>
      {expiresAbs && expiresAt && (
        <p className="text-xs text-faint">
          Expires:{" "}
          <span className="text-muted">
            {expiresIn ? `in ${expiresIn}, ` : "due now, "}
            <time dateTime={expiresAt}>{expiresAbs}</time>
          </span>
        </p>
      )}
      {fetchCommand && <FetchCommand command={fetchCommand} />}
      {refusal && (
        <p className="whitespace-pre-wrap break-words rounded-md bg-surface/60 px-2 py-1 text-xs text-muted">
          <span className="text-faint">Reason: </span>
          <span>{refusal}</span>
        </p>
      )}
      {lastError && (
        <p className="whitespace-pre-wrap break-words rounded-md bg-surface/60 px-2 py-1 text-xs text-muted">
          <span className="text-faint">Last error: </span>
          <span>{lastError}</span>
        </p>
      )}
    </div>
  );
}

function FetchCommand({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
    } catch {
      // Clipboard may be unavailable (insecure context); the command stays visible to copy
      // by hand, as in SalvagePanel.
    }
  };
  return (
    <div className="space-y-2 rounded-md border border-edge bg-surface/60 p-2">
      <p className="text-xs text-muted">Fetch the held work into a clone of this repository:</p>
      <div className="flex items-center gap-2">
        <code className="console flex-1 overflow-x-auto rounded-md border border-edge bg-ink px-2 py-1 font-mono text-xs text-fg">
          {command}
        </code>
        <Button variant="secondary" size="sm" onClick={copy}>
          {copied ? "Copied" : "Copy fetch command"}
        </Button>
      </div>
      <span role="status" aria-live="polite" className="sr-only">
        {copied ? "Fetch command copied" : ""}
      </span>
    </div>
  );
}
