// CredentialSwitchHoldPanel — the run view's panel for the two issue #3181 token-switch
// holds. Both sit in raw status `paused` with a hold_reason, so this panel replaces the
// owner-pause PausedPanel for them (a plain "Paused" card would misname the wait).
//
// settling: the owner switched a retained-recovery run's token and the run's processes could
//   not be proven stopped; the worker is still stopping them. The server refuses Resume and a
//   further token switch with a 409 until it finishes, so Resume shows disabled with the
//   reason beside it. Cancel always works.
// unsettled: the worker finished without proving the processes stopped. The run is held for
//   its owner with its work and custody kept on that worker; Resume requeues it on the newly
//   chosen token, or the owner cancels.
//
// The two states are one sequence (settling becomes unsettled), so the settling copy says
// what comes next. Not a live region: RunView announces each hold from its always-mounted
// park region, which also re-announces the settling → unsettled change.

import { useId } from "react";
import type { Run } from "../lib/api";
import { credentialSwitchHold } from "../lib/credentialSwitchHold";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Button } from "./ui";

// nextTokenText names the token Resume continues on, from the run's stored override (the
// switch the owner just made). Never invents a label: an unresolved pin reads generically.
function nextTokenText(run: Pick<Run, "credential_override">): string {
  const o = run.credential_override ?? null;
  if (!o) return "the new token";
  if (o.mode === "pinned") {
    const safe = o.label ? sanitizeLabel(o.label) : "";
    return safe ? `“${safe}”` : "the new token";
  }
  if (o.mode === "auto") return "an auto-selected token";
  return "your default token";
}

export function CredentialSwitchHoldPanel({ run, ownerProof, busy, onResume, onCancel }: {
  run: Run;
  ownerProof: boolean;
  busy: boolean;
  onResume: () => void;
  onCancel: () => void;
}) {
  const reasonId = useId();
  const hold = credentialSwitchHold(run);
  if (!hold) return null;
  const settling = hold === "settling";
  const token = nextTokenText(run);
  return (
    <section
      aria-labelledby={`${reasonId}-h`}
      className="rounded-xl border border-warn/40 bg-warn/10 p-4"
      data-testid="credential-switch-hold-panel"
    >
      <h2 id={`${reasonId}-h`} className="flex items-center gap-2 text-sm font-semibold text-warn">
        {/* The one moving mark: the worker is actively stopping processes. Decorative, and
            still under prefers-reduced-motion. */}
        {settling && <span aria-hidden="true" className="inline-block h-2 w-2 rounded-full bg-warn motion-safe:animate-pulse" />}
        {settling ? "Stopping processes after a token switch" : "Held: token switch"}
      </h2>
      {settling ? (
        <>
          <p className="mt-1 text-sm text-fg">
            The run&rsquo;s processes could not be proven stopped after you switched its token, so the worker is stopping them now.
          </p>
          <p className="mt-1 text-xs text-muted">
            Its work and custody stay on that worker. When the worker finishes, this run is held for you and Resume continues it on {token}.
          </p>
        </>
      ) : (
        <>
          <p className="mt-1 text-sm text-fg">
            Processes could not be proven stopped; work is kept on its worker. Resume to continue on the new token, or Cancel.
          </p>
          <p className="mt-1 text-xs text-muted">Resume requeues this run on {token}. Nothing runs or is spent while it is held.</p>
        </>
      )}
      {ownerProof ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <Button
            size="sm"
            disabled={busy || settling}
            aria-describedby={settling ? `${reasonId}-why` : undefined}
            onClick={onResume}
          >
            ▶ Resume
          </Button>
          <Button size="sm" variant="danger" disabled={busy} onClick={onCancel}>Cancel run</Button>
          {settling && (
            <p id={`${reasonId}-why`} className="text-xs text-muted">
              Resume and Switch token become available when the worker finishes. Cancel works now.
            </p>
          )}
        </div>
      ) : (
        <p className="mt-2 text-xs text-muted">
          {settling ? "Only the run owner can cancel this hold." : "Only the run owner can resume or cancel this hold."}
        </p>
      )}
    </section>
  );
}
