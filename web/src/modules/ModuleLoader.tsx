// Loads one module's page after mount and renders it inside its error boundary (PRD
// #4686). This is the ONLY caller of the loader, and it runs in a post-mount effect, so a
// route guard that redirects (signed out, or a non-admin on an adminOnly module) never
// reaches loadRemote.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useAuth } from "../auth/AuthContext";
import { Spinner } from "../components/ui";
import { ModuleErrorBoundary } from "./ModuleErrorBoundary";
import { ModuleErrorCard, type ModuleErrorKind } from "./ModuleErrorCard";
import { hasModuleLoaded, hasRetryFailed, loadModulePage, retryModuleLoad, ModuleLoadError, type ModulePageComponent } from "./loader";
import type { ModuleEntry } from "./manifest";
import { createHostSdk } from "./sdk";
import { describeThrown } from "./thrown";

type LoadState =
  | { phase: "loading" }
  | { phase: "ready"; Page: ModulePageComponent }
  | { phase: "error"; kind: ModuleErrorKind; reason: string };

export function ModuleLoader({ entry }: { entry: ModuleEntry }) {
  const { user } = useAuth();
  const navigate = useNavigate();
  const [state, setState] = useState<LoadState>({ phase: "loading" });
  // Only the newest attempt may set state: a superseded or unmounted one is discarded.
  const attempt = useMemo(() => ({ n: 0 }), []);

  const sdk = useMemo(
    () => (user ? createHostSdk({ user, navigate: (to) => void navigate(to) }) : null),
    [user, navigate],
  );

  const run = useCallback(
    (retry: boolean) => {
      const mine = ++attempt.n;
      setState({ phase: "loading" });
      (retry ? retryModuleLoad(entry) : loadModulePage(entry)).then(
        (Page) => {
          if (attempt.n === mine) setState({ phase: "ready", Page });
        },
        (e: unknown) => {
          if (attempt.n !== mine) return;
          const err = e instanceof ModuleLoadError ? e : new ModuleLoadError("transient", describeThrown(e));
          setState({ phase: "error", kind: err.kind, reason: err.message });
        },
      );
    },
    [entry, attempt],
  );

  useEffect(() => {
    run(false);
    return () => {
      attempt.n++;
    };
  }, [run, attempt]);

  if (state.phase === "error") {
    return (
      <ModuleErrorCard
        kind={state.kind}
        reason={state.reason}
        entry={entry}
        // A transient failure before the module ever loaded re-registers with a fresh URL.
        // After it loaded once there is nothing to re-register ("stale" has no Retry), so
        // any other late failure just loads again.
        onRetry={state.kind === "transient" ? () => run(!hasModuleLoaded(entry.id)) : undefined}
        // A Retry that already failed in this document also offers Reload page.
        offerReload={state.kind === "transient" && hasRetryFailed(entry.id)}
      />
    );
  }
  if (state.phase === "ready" && sdk) {
    const { Page } = state;
    return (
      <ModuleErrorBoundary entry={entry}>
        <Page sdk={sdk} />
      </ModuleErrorBoundary>
    );
  }
  return (
    <div role="status" className="p-6 text-sm text-faint">
      <Spinner /> Loading module…
    </div>
  );
}
