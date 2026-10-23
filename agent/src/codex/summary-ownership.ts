// Private composition binding for executor-owned intent/plan summaries. Labels and refresh
// policy never select ownership; ordinary advice keeps its existing grace behavior.
import type { CodexRootHandle, StartupCleanupAuthorization } from "./launcher.js";

export interface SummaryOwnership {
  guard(): void;
  launch(start: (startupCleanup: StartupCleanupAuthorization) => Promise<CodexRootHandle>): Promise<CodexRootHandle>;
}

export const ownedSummaryFactories = new WeakSet<object>();
export const ownedSummaryHarnesses = new WeakMap<object, () => void>();
export const summaryCallDeadlines = new WeakMap<object, number>();
export const summaryOwnership = new WeakMap<object, SummaryOwnership>();
