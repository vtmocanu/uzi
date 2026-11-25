// One error boundary per module page (PRD #4686, "Failure isolation"): an error thrown
// while rendering, or in a lifecycle method of, the module's React tree shows the module's
// error card instead of unmounting the host. Retry resets the boundary and remounts the
// already-loaded component; it never re-registers or calls loadRemote again.
//
// A lazily imported sub-view whose chunk 404s after a rollout throws from render, so the
// stale-chunk classification applies here as well (the module has necessarily loaded).

import { Component, Suspense, type ReactNode } from "react";
import { Spinner } from "../components/ui";
import { ModuleErrorCard } from "./ModuleErrorCard";
import { isStaleChunkError } from "./loader";
import { describeThrown } from "./thrown";
import type { ModuleEntry } from "./manifest";

type Props = { entry: Pick<ModuleEntry, "id" | "version">; children: ReactNode };
type State = { error: Error | null };

export class ModuleErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: unknown): State {
    // `instanceof` throws for a revoked Proxy, and this runs while React unwinds a render.
    try {
      if (error instanceof Error) return { error };
    } catch {
      // fall through to a safe wrapper
    }
    return { error: new Error(describeThrown(error)) };
  }

  componentDidCatch(error: unknown) {
    console.error(`uzi modules: module "${this.props.entry.id}" threw while rendering`, error);
  }

  private retry = () => this.setState({ error: null });

  render() {
    const { error } = this.state;
    if (error) {
      const stale = isStaleChunkError(error, this.props.entry.id);
      return (
        <ModuleErrorCard
          kind={stale ? "stale" : "render"}
          reason={describeThrown(error)}
          entry={this.props.entry}
          onRetry={stale ? undefined : this.retry}
        />
      );
    }
    return (
      <Suspense
        fallback={
          <div role="status" className="p-6 text-sm text-faint">
            <Spinner /> Loading…
          </div>
        }
      >
        {this.props.children}
      </Suspense>
    );
  }
}
