// The run-view surfaces for a run's per-run credential OVERRIDE, the pending
// held-state SWITCH, the applied-epoch HISTORY, and the owner's "Switch token"
// action (PRD #1247 M7). These are NEW and sit BESIDE RunCredential (which shows the
// credential a claim already SPENT, PRD #111) — they never replace it.
//
// Every user-authored token label is routed through sanitizeLabel before render
// (React escaping does not neutralise a bidi override), the same rule RunCredential
// follows. The switch endpoint's refused lanes (task_review / chat / judge /
// self_improve) are HIDDEN, never rendered as a control that would 409.

import { useState } from "react";

import { api, type CredentialEpoch, type CredentialOverride, type Run, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import {
  isCredentialSwitchRefusedLane,
  setTokenBody,
} from "../lib/credentialOverride";
import { isTerminalRun } from "../lib/runStatus";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { useSeededCredential } from "../lib/useSeededCredential";
import { TokenPicker } from "./TokenPicker";
import { Alert, Badge, Button } from "./ui";

// overrideText renders the chosen override's mode plus, for a pin, its snapshotted
// label. auto/default carry no label. A pinned override with a null label (a deleted
// token) still names the mode.
function overrideText(o: CredentialOverride): string {
  if (o.mode === "pinned") {
    const safe = sanitizeLabel(o.label ?? "");
    return safe ? `pinned “${safe}”` : "pinned";
  }
  return o.mode;
}

// RunCredentialOverride renders the run's chosen override (mode + label) and, when the
// DTO still reports one, the pending held-state switch. Step A already suppresses a
// stale/applied switch server-side, so this renders exactly what the DTO says: a null
// credential_switch shows no badge (the whole point of the lingering-stamp fixture).
export function RunCredentialOverride({
  run,
}: {
  run: Pick<Run, "credential_override" | "credential_switch">;
}) {
  const override = run.credential_override ?? null;
  const sw = run.credential_switch ?? null;
  if (!override && !sw) return null;
  const safeOverride = override ? overrideText(override) : "";
  return (
    <>
      {override && (
        <Badge
          tone="info"
          title={
            override.mode === "pinned"
              ? `This run's Anthropic credential is overridden to ${safeOverride}.`
              : `This run's Anthropic credential is overridden to ${override.mode}.`
          }
        >
          token override: {safeOverride}
        </Badge>
      )}
      {sw === "requested" && (
        <Badge tone="warning" dot title="A token switch was requested and has not been applied to a claim yet.">
          switch pending
        </Badge>
      )}
      {sw === "released" && (
        <Badge tone="warning" dot title="The holding worker released its claim; the run awaits reclaim on the new token.">
          switch released
        </Badge>
      )}
    </>
  );
}

// selectReasonText maps the server's select_reason enum to short human copy; an unknown
// or null reason falls back to the raw value (or nothing), never invents meaning.
function selectReasonText(reason: string | null): string {
  switch (reason) {
    case "override_pinned":
      return "pinned by override";
    case "override_auto":
      return "auto-selected by override";
    case "override_default":
      return "default by override";
    case "auto":
      return "auto-selected";
    case "default":
      return "default token";
    case "pinned":
      return "pinned token";
    default:
      return reason ?? "";
  }
}

// CredentialEpochList renders the applied-switch history — one row per claim, oldest
// first, each naming the token label it spent, why, and when. Renders nothing for a run
// with no epochs (a run with no switch, or a pre-#1247 run).
export function CredentialEpochList({ epochs }: { epochs?: CredentialEpoch[] }) {
  if (!epochs || epochs.length === 0) return null;
  return (
    <div className="rounded-lg border border-edge bg-surface/60 p-3">
      <p className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-faint">Token history</p>
      <ol className="space-y-1.5 text-xs text-fg">
        {epochs.map((e) => {
          const safe = sanitizeLabel(e.label ?? "");
          const reason = selectReasonText(e.select_reason);
          const when = new Date(e.applied_at).toLocaleString();
          return (
            <li key={e.claim_generation} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
              <span className="font-medium" title={safe || "unknown token"} aria-label={safe || "unknown token"}>
                {safe || "unknown token"}
              </span>
              {reason && <span className="text-muted">· {reason}</span>}
              <span className="text-faint tabular-nums">· {when}</span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}

// SwitchTokenAction is the run-header "Switch token" control (PRD #1247 M7), modelled
// on the Resume action: a primary Button for the owner, inert text for a non-owner
// (never a button that 409s), and HIDDEN entirely for a refused lane (task_review /
// chat / judge / self_improve) or a terminal run. Opening it reveals the shared
// TokenPicker, the interrupt caveat for a held/running run, and a confirm that calls
// api.setRunCredential, surfacing any D6 warning the endpoint returns.
export function SwitchTokenAction({
  run,
  canSteer,
  tokens,
  onSwitched,
}: {
  run: Run;
  canSteer: boolean;
  // Injected token list for the picker; omitted → the picker self-fetches.
  tokens?: SecretMeta[];
  // Called with the updated run after a successful switch, so the page can refresh.
  onSwitched?: (run: Run) => void;
}) {
  const [open, setOpen] = useState(false);
  // Seed the picker from the run's stored override, and re-resolve a pinned label→id once
  // the token list loads (self-fetched when none is injected), so a run pinned to an
  // EXISTING token opens with that token SELECTED and the confirm ENABLED — never as a
  // spurious "(unavailable)". A touched ref freezes the async re-seed behind a live edit,
  // mirroring the ScheduleModal pattern. The picker (hence the fetch) only shows for a
  // steering owner on a non-refused, non-terminal run, so gate `enabled` on that.
  const pickerVisible = canSteer && !isCredentialSwitchRefusedLane(run) && !isTerminalRun(run.status);
  const {
    tokens: pickerTokens,
    selection,
    onSelectionChange,
  } = useSeededCredential(run.credential_override, { tokens, enabled: pickerVisible });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [warning, setWarning] = useState("");

  // The refused lanes 409 server-side; a terminal run cannot switch. Hide the control
  // rather than render a button that fails. (Checked after the hooks so hook order is
  // stable across renders.)
  if (isCredentialSwitchRefusedLane(run) || isTerminalRun(run.status)) return null;

  if (!canSteer) {
    return <span className="text-xs text-muted">Only the run&rsquo;s owner can switch its token.</span>;
  }

  // A held or running run has a live claim, so switching interrupts it (losing at most
  // the in-flight step); a queued run has no claim yet, so the choice just applies to
  // its first claim.
  const willInterrupt = run.status !== "queued";
  // A pinned choice with no resolved id cannot be sent (the server 400s it), so block
  // the confirm until the user picks a real token.
  const pinnedUnresolved = selection.mode === "pinned" && !selection.secret_id;

  const submit = async () => {
    setBusy(true);
    setError("");
    setWarning("");
    try {
      const res = await api.setRunCredential(run.id, setTokenBody(selection));
      setWarning(res.warning ?? "");
      setOpen(false);
      onSwitched?.(res.run);
    } catch (e) {
      setError(errorMessage(e, "Could not switch the token"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <span className="inline-flex flex-col gap-1.5">
      <Button variant="secondary" size="sm" disabled={busy} onClick={() => setOpen((v) => !v)}>
        Switch token
      </Button>
      {open && (
        <div className="w-72 space-y-2 rounded-lg border border-edge bg-surface p-3">
          <TokenPicker
            label="Switch this run's Anthropic token"
            className="h-8 w-full text-xs"
            value={selection}
            onChange={onSelectionChange}
            tokens={pickerTokens}
            disabled={busy}
          />
          {willInterrupt && (
            <p className="text-[11px] text-warn">
              Switching now interrupts the run — it loses at most the in-flight step, then continues on the new token.
            </p>
          )}
          <div className="flex gap-2">
            <Button size="sm" disabled={busy || pinnedUnresolved} onClick={() => void submit()}>
              Switch to this token
            </Button>
            <Button variant="ghost" size="sm" disabled={busy} onClick={() => setOpen(false)}>
              Cancel
            </Button>
          </div>
        </div>
      )}
      {error && <Alert message={error} />}
      {/* D6: a benign caveat the endpoint returns on the 200 (e.g. an auto pool with no
          currently-eligible token). Surfaced as a warning, not an error. */}
      {warning && <Alert message={warning} tone="warning" />}
    </span>
  );
}
