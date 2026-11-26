// The per-page error card for a module that failed to load or render (PRD #4686,
// "Failure isolation"). Admins see the reason, the module id and its version; other users
// see a generic message. The action depends on the failure class:
//   transient (before the module ever loaded)  Retry: re-register and load again; after a
//                                              Retry that failed again, Reload page too
//   render (error inside the loaded tree)       Retry: remount, no re-registration
//   stale (chunk failed after a load)           Reload page
//   incompatible                                none; Retry cannot fix it

import { useAuth } from "../auth/AuthContext";
import { Button, Card, CardHeader } from "../components/ui";
import type { ModuleEntry } from "./manifest";

export type ModuleErrorKind = "transient" | "incompatible" | "stale" | "render";

const GENERIC: Record<ModuleErrorKind, string> = {
  transient: "This module could not be loaded. Try again in a moment.",
  incompatible: "This module is not compatible with this version of uzi.",
  stale: "This module was updated while you had the page open. Reload the page to continue.",
  render: "This module hit an error and stopped.",
};

export function ModuleErrorCard({
  kind,
  reason,
  entry,
  onRetry,
  offerReload,
}: {
  kind: ModuleErrorKind;
  reason: string;
  entry: Pick<ModuleEntry, "id" | "version">;
  // Present for the two Retry classes.
  onRetry?: () => void;
  // Transient only: a Retry already failed in this document, so Reload page is offered next
  // to Retry (the module may still be down, or an engine may keep a failed module file).
  offerReload?: boolean;
}) {
  const isAdmin = useAuth()?.user?.is_admin === true;
  return (
    <Card>
      <div role="alert" className="space-y-3">
        <CardHeader title="Module unavailable" />
        <p className="text-sm text-muted">{GENERIC[kind]}</p>
        {isAdmin && (
          <div className="space-y-1 text-xs text-faint">
            <p>
              Module <span className="font-mono text-muted">{entry.id}</span>, version{" "}
              <span className="font-mono text-muted">{entry.version}</span>
            </p>
            <p className="break-words font-mono" data-testid="module-error-reason">
              {reason}
            </p>
          </div>
        )}
        {kind === "transient" && offerReload && <p className="text-sm text-muted">If it keeps failing, reload the page.</p>}
        {(kind === "stale" || (kind === "transient" && offerReload)) && (
          <Button variant="secondary" onClick={() => window.location.reload()}>
            Reload page
          </Button>
        )}
        {(kind === "transient" || kind === "render") && onRetry && (
          <Button variant="secondary" onClick={onRetry}>
            Retry
          </Button>
        )}
      </div>
    </Card>
  );
}
