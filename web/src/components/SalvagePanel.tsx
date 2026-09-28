import { useState } from "react";

import type { Run } from "../lib/api";
import { formatCountdown } from "../lib/limitWait";
import { shortSha } from "../lib/recovery";
import { stripUnsafeChars } from "../lib/safeText";
import { useNow } from "../lib/useNow";
import { Badge, Button, Card, SectionTitle, type BadgeTone } from "./ui";
import { BranchIcon } from "./icons";

// SalvagePanel is the failed-run page's view of the checkpoint salvage (PRD #1867, R6/R7):
// a bounded, run-scoped archive copy of the run's last PUBLISHED checkpoint, created at
// refs/uzi-salvage/<run-id>. It sits beside RecoveryArchivesPanel and shares its visual
// language (a top-level Card, an icon + SectionTitle heading, muted explanatory copy).
//
// Copy discipline: the saved commits are a checkpoint, which may be behind the run's final
// local work, so this panel never says "recovered" and never names the branch checkpoint
// ref (that ref is #1810's retention, not salvage's).
//
// Every salvage_* field is server-supplied; salvage_last_error is a bounded, scrubbed forge
// error. All of them render as escaped plain text through stripUnsafeChars, never Markdown.

// The ref row and the fetch command are shown only when salvage_ref is exactly
// refs/uzi-salvage/<this run's id>, as the CLI does (salvageRefFor). Anything else (an empty,
// truncated, foreign-namespace or other run's ref) shows neither, so the panel never names a
// ref salvage does not own or hands an operator a foreign ref to paste into a shell. The
// charset check keeps even an unexpected run id from putting a shell metacharacter or a line
// break into the command.
const SALVAGE_REF_SAFE_RE = /^refs\/uzi-salvage\/[A-Za-z0-9-]+$/;

type StateView = { badge: string; tone: BadgeTone; lead: string };

// One entry per server state. The badge carries the meaning in words, so the tone colour is
// never the only signal (WCAG 1.4.1).
const STATE_VIEWS: Record<string, StateView> = {
  promoted: { badge: "Saved", tone: "ok", lead: "Checkpointed commits saved" },
  pending: {
    badge: "Saving",
    tone: "info",
    lead: "Saving a copy of the last published checkpoint…",
  },
  unavailable: {
    badge: "Not saved",
    tone: "warning",
    lead: "No copy saved: the published checkpoint was no longer at its recorded tip on the forge.",
  },
  refused: {
    badge: "Not saved",
    tone: "warning",
    lead: "No copy saved: a different commit already holds this run's salvage ref.",
  },
  failed: {
    badge: "Not saved",
    tone: "danger",
    lead: "Salvage stopped after repeated attempts.",
  },
  skipped_secret: {
    badge: "Not saved",
    tone: "neutral",
    lead: "Not saved: this run failed on a secret-scan block.",
  },
  expired: { badge: "Expired", tone: "neutral", lead: "The saved copy expired and was removed." },
  disabled: { badge: "Off", tone: "neutral", lead: "Not saved: salvage was turned off for this forge before a copy was made." },
};

// ownSalvageRef returns the run's salvage ref when it is exactly refs/uzi-salvage/<run.id>,
// null otherwise. It gates both the Ref row and the fetch command.
function ownSalvageRef(run: Run): string | null {
  const ref = run.salvage_ref;
  if (typeof ref !== "string" || ref !== `refs/uzi-salvage/${run.id}`) return null;
  return SALVAGE_REF_SAFE_RE.test(ref) ? ref : null;
}

export function SalvagePanel({ run }: { run: Run }) {
  const state = run.salvage_state;
  if (run.status !== "failed" || state == null) return null;
  // An unknown future state has no vetted copy; render nothing rather than guess at wording.
  const view = STATE_VIEWS[state];
  if (!view) return null;
  return <SalvageCard run={run} state={state} view={view} />;
}

function SalvageCard({ run, state, view }: { run: Run; state: string; view: StateView }) {
  const lastError = run.salvage_last_error?.trim() ? stripUnsafeChars(run.salvage_last_error) : null;
  // Shown in every state that carries one, as the CLI does (a promoted copy can carry an
  // expiry-delete error, for instance).
  const showError = lastError !== null;

  return (
    <Card id="salvage" className="scroll-mt-20 space-y-3 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <BranchIcon className="h-4 w-4 text-muted" aria-hidden="true" />
          <SectionTitle>Salvage</SectionTitle>
        </div>
        <Badge tone={view.tone} dot={state === "pending"} pulse={state === "pending"}>
          {view.badge}
        </Badge>
      </div>

      <p className="text-sm font-medium text-fg">{view.lead}</p>

      {state === "promoted" && <PromotedDetails run={run} />}

      {showError && (
        <p className="whitespace-pre-wrap break-words rounded-md bg-surface/60 px-2 py-1 text-xs text-muted">
          <span className="text-faint">{state === "pending" ? "Last attempt: " : "Last error: "}</span>
          <span>{lastError}</span>
        </p>
      )}
    </Card>
  );
}

function PromotedDetails({ run }: { run: Run }) {
  const now = useNow(60_000);
  const ref = ownSalvageRef(run);
  const tip = run.salvage_tip ? stripUnsafeChars(shortSha(run.salvage_tip)) : null;
  const expiresAt = run.salvage_expires_at ?? null;
  const expiresMs = expiresAt ? Date.parse(expiresAt) : NaN;
  const expiresAbs = Number.isFinite(expiresMs)
    ? new Date(expiresMs).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })
    : null;
  const expiresIn = formatCountdown(expiresAt, now);
  const fetchCommand = ref ? `git fetch origin ${ref}` : null;

  return (
    <>
      <p className="text-sm text-muted">
        This is a copy of the run's last published checkpoint. It may be behind the run's final
        local work.
      </p>

      <dl className="grid grid-cols-1 gap-x-4 gap-y-1 text-xs text-faint sm:grid-cols-2">
        {ref && (
          <div className="sm:col-span-2">
            <dt className="inline text-faint">Ref: </dt>
            <dd className="inline break-all font-mono text-muted">{ref}</dd>
          </div>
        )}
        {tip && (
          <div>
            <dt className="inline text-faint">Tip: </dt>
            <dd className="inline font-mono text-muted">{tip}</dd>
          </div>
        )}
        {expiresAbs && expiresAt && (
          <div>
            <dt className="inline text-faint">Expires: </dt>
            <dd className="inline text-muted">
              {expiresIn ? `in ${expiresIn}, ` : "due now, "}
              <time dateTime={expiresAt}>{expiresAbs}</time>
            </dd>
          </div>
        )}
      </dl>

      {fetchCommand && <FetchCommand command={fetchCommand} />}
    </>
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
      // by hand — mirrors RecoveryArchives' export-hint copy handling.
    }
  };
  return (
    <div className="space-y-2 rounded-md border border-edge bg-surface/60 p-2">
      <p className="text-xs text-muted">Fetch the saved commits into a clone of this repository:</p>
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
