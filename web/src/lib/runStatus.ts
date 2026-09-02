// TERMINAL_RUN_STATUSES / isTerminalRun live in their own leaf module (not the
// `api.ts` barrel) so the mock-mode client `mocks/mockApi/` can import
// isTerminalRun as a runtime value without creating a runtime import cycle with
// `lib/api.ts` — the cycle (api → mockApi → api) that caused the vitest
// `No "ApiError" export is defined on the "../lib/api" mock` flake (issue #165).
// The `RunStatus` import is type-only, so it introduces no runtime edge back to
// the barrel.

import type { RunStatus } from "./api";

// TERMINAL_RUN_STATUSES mirrors the DB CHECK: a run in any of these is finished.
export const TERMINAL_RUN_STATUSES: RunStatus[] = [
  "completed",
  "failed",
  "cancelled",
];

export function isTerminalRun(status: string): boolean {
  return (TERMINAL_RUN_STATUSES as string[]).includes(status);
}
