// CredentialsResetPanel — the run view's panel for a run HELD on `credentials_reset`
// (PRD #3989 M3): status `paused`, parked by the server because an admin reset the owner's
// account, which deleted every model credential the run could spend. Only Claude-harness and
// chat runs park this way (a Codex run created before the reset fails instead). The run
// resumes by itself once the owner adds a credential in Settings; a manual Resume is refused
// (409), so this panel replaces the owner-pause PausedPanel and offers no Resume.

import { Link } from "react-router-dom";
import type { Run } from "../lib/api";

export function isCredentialsResetHold(run: Pick<Run, "status" | "hold_reason">): boolean {
  return run.status === "paused" && run.hold_reason === "credentials_reset";
}

export function CredentialsResetPanel({ run, canSteer }: { run: Run; canSteer: boolean }) {
  if (!isCredentialsResetHold(run)) return null;
  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4" data-testid="credentials-reset-panel">
      <p className="text-sm font-semibold text-warn">Waiting: credentials were reset</p>
      <p className="mt-1 text-sm text-fg">
        This account was reset by an admin, which deleted its credentials, so this run has no
        credential to use. It resumes by itself once the owner adds a credential in Settings.
        Resuming it by hand would not help.
      </p>
      {canSteer ? (
        <div className="mt-3 flex flex-wrap items-start gap-2">
          <Link
            to="/settings"
            className="inline-flex h-7 items-center rounded-lg px-2.5 text-xs font-medium text-muted hover:bg-raised hover:text-fg"
          >
            Add a credential in Settings
          </Link>
        </div>
      ) : (
        <p className="mt-2 text-xs text-muted">Only the run&rsquo;s owner can add the credential it needs.</p>
      )}
    </div>
  );
}
