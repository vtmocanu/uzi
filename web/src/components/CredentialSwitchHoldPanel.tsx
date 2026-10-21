// CredentialSwitchHoldPanel — the run view's panel for the two issue #3181 token-switch
// holds. Both sit in raw status `paused` with a hold_reason, so this panel replaces the
// owner-pause PausedPanel for them (a plain "Paused" card would misname the wait).
//
// settling: the owner switched a retained-recovery run's token, the run's processes could not
//   be proven stopped, and the worker is finishing its own operations for the run. The server
//   is the authority on Resume: it refuses (409) while that worker is live and finishing, and
//   lifts the fence on its own when the worker is lost, goes stale or re-registers, with
//   hold_reason unchanged. No field reveals the lift, so Resume stays enabled here; a refusal
//   is shown inline as an alert (`refusal`, the server's 409 text) beside the button it
//   answers. Cancel always works.
// unsettled: the worker acknowledged it finished its own operations. That is not proof the
//   run's processes stopped, so the run is held for its owner with its work and custody kept
//   on that worker; Resume requeues it on the newly chosen token, or the owner cancels.
//
// The copy promises no completion (the worker may be lost rather than finish). Not a live
// region itself: RunView announces each hold from its always-mounted park region, and the
// refusal is a role="alert" that mounts fresh for each refused attempt.

import { useId } from "react";
import type { Run } from "../lib/api";
import { credentialSwitchHold } from "../lib/credentialSwitchHold";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Alert, Button } from "./ui";

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

export function CredentialSwitchHoldPanel({ run, ownerProof, busy, refusal, onResume, onCancel }: {
  run: Run;
  ownerProof: boolean;
  busy: boolean;
  /** The server's refusal of the last Resume on a settling hold, or "" when none. */
  refusal?: string;
  onResume: () => void;
  onCancel: () => void;
}) {
  const reasonId = useId();
  const hold = credentialSwitchHold(run);
  if (!hold) return null;
  const settling = hold === "settling";
  const token = nextTokenText(run);
  const shownRefusal = settling ? refusal ?? "" : "";
  const describedBy = settling
    ? [`${reasonId}-why`, shownRefusal ? `${reasonId}-refused` : ""].filter(Boolean).join(" ")
    : undefined;
  return (
    <section
      aria-labelledby={`${reasonId}-h`}
      className="rounded-xl border border-warn/40 bg-warn/10 p-4"
      data-testid="credential-switch-hold-panel"
    >
      <h2 id={`${reasonId}-h`} className="text-sm font-semibold text-warn">
        {settling ? "Settling after a token switch" : "Held: token switch"}
      </h2>
      {settling ? (
        <>
          <p className="mt-1 text-sm text-fg">
            The worker is finishing its own operations for this run. The run&rsquo;s processes could not be proven stopped after you switched its token.
          </p>
          <p className="mt-1 text-xs text-muted">
            Its work and custody stay on that worker. Resume continues it on {token}.
          </p>
        </>
      ) : (
        <>
          <p className="mt-1 text-sm text-fg">
            Processes could not be proven stopped; work is kept on its worker. Resume to continue on the new token, or Cancel.
          </p>
          <p className="mt-1 text-xs text-muted">Resume requeues this run on {token}. No new work starts while it is held.</p>
        </>
      )}
      {ownerProof ? (
        <>
          <div className="mt-3 flex flex-wrap items-center gap-2">
            <Button size="sm" disabled={busy} aria-describedby={describedBy} onClick={onResume}>
              ▶ Resume
            </Button>
            <Button size="sm" variant="danger" disabled={busy} onClick={onCancel}>Cancel run</Button>
            {settling && (
              <p id={`${reasonId}-why`} className="text-xs text-muted">
                Resume may be refused until the worker finishes its own operations; then try again, or Cancel the run.
              </p>
            )}
          </div>
          {shownRefusal && (
            <div id={`${reasonId}-refused`} className="mt-2" data-testid="credential-switch-hold-refusal">
              <Alert message={`Resume refused: ${shownRefusal}`} />
            </div>
          )}
        </>
      ) : (
        <p className="mt-2 text-xs text-muted">Only the run owner can resume or cancel this hold.</p>
      )}
    </section>
  );
}
