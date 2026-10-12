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
// to a run-scoped forge ref in place of a recovery archive. It sits beside SalvagePanel
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
  abandoned: { badge: "Not ours", tone: "neutral" },
};

// An unknown (or not yet vetted, such as prepared) state gets a neutral generic badge.
const GENERIC_VIEW: StateView = { badge: "Held", tone: "neutral" };

// Only these states can have a ref on the forge worth fetching.
const FETCHABLE_STATES = new Set(["acknowledged", "created"]);

// States that settle on their own: the panel re-fetches while any row is in one. A prepared
// row (not yet vetted or published) and an empty list (the server marks the run failed before
// the worker scans and uploads, so the first answer is normally empty) are also still
// settling; see stillSettling.
const IN_PROGRESS_STATES = new Set(["created", "invoked", "create_unknown", "delete_unknown"]);
const REFRESH_MS = 20_000;
// About ten minutes of polling per mount.
const MAX_POLLS = 30;

// stillSettling reports whether a failed run's publication list can still change on its own:
// nothing has appeared yet, a row is prepared, or a row is in an in-progress state. Only
// all-terminal rows (acknowledged, deleted, refused, abandoned, or an unknown state) end polling.
function stillSettling(rows: HeldPublication[]): boolean {
  return rows.length === 0 || rows.some((p) => p.state === "prepared" || IN_PROGRESS_STATES.has(p.state));
}

// leadLine is the per-row, state-keyed sentence beside the badge. It states only what that
// row's state establishes; the panel makes no aggregate claim across rows. Unknown and
// prepared states get none.
function leadLine(pub: HeldPublication): string | null {
  switch (pub.state) {
    case "acknowledged":
      return "On the forge; deleted when retention expires.";
    case "created":
      return "On the forge; uzi either keeps it as the run's held copy or deletes it.";
    case "delete_unknown":
      return "Being removed from the forge.";
    case "deleted":
      return pub.ref_created_at || pub.acknowledged_at ? "Was on the forge; since removed." : "Not on the forge.";
    case "abandoned":
      return "The ref now points at a commit uzi did not publish; uzi leaves it untouched.";
    case "refused":
      return "The forge refused the publication; the recovery archive path applies.";
    case "invoked":
    case "create_unknown":
      return "Waiting for the forge to confirm; the recovery archive path applies.";
    default:
      return null;
  }
}

function safeHeldRef(runId: string, pub: HeldPublication): string | null {
  const ref = pub.ref;
  if (typeof ref !== "string" || !ref.startsWith(`refs/uzi-held/${runId}/`)) return null;
  return HELD_REF_SAFE_RE.test(ref) ? ref : null;
}

export function HeldPublicationPanel({ run }: { run: Run }) {
  const failed = run.status === "failed";
  const [pubs, setPubs] = useState<HeldPublication[]>([]);
  const [pollsExhausted, setPollsExhausted] = useState(false);

  // One chain per (failed, run.id): fetch, then, while the list is empty or a row is still
  // settling (stillSettling), schedule the next fetch only after this one has settled, so
  // requests never overlap and an older response cannot overwrite a newer one. The chain stops
  // after MAX_POLLS re-fetches (create_unknown is never terminal by design), at the first
  // answer whose rows are all terminal, on unmount and on a run change. A failed first fetch
  // (an older server answering 404) stops at once.
  useEffect(() => {
    setPubs([]);
    setPollsExhausted(false);
    if (!failed) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let polls = 0;
    const load = () => {
      api
        .getRunHeldPublications(run.id)
        .then((r) => {
          if (!live) return;
          const rows = Array.isArray(r?.publications) ? r.publications : [];
          setPubs(rows);
          schedule(stillSettling(rows));
        })
        .catch(() => {
          if (!live) return;
          // Keep the last known rows on a poll failure; the first load failing leaves none.
          if (polls === 0) setPubs([]);
          schedule(polls > 0);
        });
    };
    const schedule = (again: boolean) => {
      if (!again) return;
      if (polls >= MAX_POLLS) {
        setPollsExhausted(true);
        return;
      }
      polls += 1;
      timer = setTimeout(load, REFRESH_MS);
    };
    load();
    return () => {
      live = false;
      if (timer !== undefined) clearTimeout(timer);
    };
  }, [failed, run.id]);

  if (!failed || pubs.length === 0) return null;
  const acknowledged = pubs.some((p) => p.state === "acknowledged");
  const capped = pollsExhausted && stillSettling(pubs);

  return (
    <Card id="held-publication" className="scroll-mt-20 space-y-3 p-4">
      <div className="flex items-center gap-2">
        <BranchIcon className="h-4 w-4 text-muted" aria-hidden="true" />
        <SectionTitle>Held work</SectionTitle>
      </div>
      <p className="text-sm text-muted">
        A failed run's commits and uncommitted changes, after a fresh secret scan, can be published to a
        run-scoped ref on the forge instead of kept only as a recovery archive.
        {acknowledged && (
          <>
            {" "}
            Delete this run's published refs now with{" "}
            <code className="font-mono text-xs">uzi run held expire {stripUnsafeChars(run.id)}</code>.
          </>
        )}
      </p>
      {capped && <p className="text-xs text-faint">Reload the page to refresh.</p>}
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
  const lead = leadLine(pub);
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
        {lead && <span className="text-xs text-muted">{lead}</span>}
        {ref && (
          <span className="break-all font-mono text-xs text-muted">
            {ref}
            {tip ? ` @ ${tip}` : ""}
          </span>
        )}
      </div>
      {pub.state === "acknowledged" && expiresAbs && expiresAt && (
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
