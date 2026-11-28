// Shared mocks for browser tests that mount the REAL AppShell (the sticky/pinned-row
// tests). vi.mock calls are hoisted per test file, so a file wires this in with a dynamic
// import inside its factory:
//
//   vi.mock("../lib/api", async (importOriginal) => {
//     const actual = await importOriginal<typeof import("../lib/api")>();
//     const { appShellApiMocks } = await import("../test-appshell-mocks");
//     return { ...actual, api: { ...appShellApiMocks(), listFindings: vi.fn() } };
//   });
//   vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
//
// and calls setupAppShellMocks(api, useAuth) from beforeEach. The api list is every call
// the shell chrome (sidebar badges, health pip, command menu) makes; page-specific calls
// are added by the test file.

import { vi } from "vitest";
import type { api as realApi } from "./lib/api";
import type { useAuth as realUseAuth } from "./auth/AuthContext";
import { healthySilentDoc, mockAgentSource } from "./mocks/data";
import { aUser, authValue, emptySelf } from "./pages/overviewFixtures";

const NAMES = [
  "listRuns",
  "listWorkers",
  "listRepos",
  "listAgentTemplates",
  "listSecrets",
  "listConnections",
  "listConnectionsBackground",
  "getUsage",
  "getAdminUsage",
  "adminListRuns",
  "getRecoveryHolds",
  "getAdminHealth",
  "listUsers",
  "workerUpgradeSummary",
  "getJudgeStats",
  "runsInProgressCount",
  "listSchedules",
  "getFindingsStats",
  "getMyRateLimits",
  "getMyCodexRateLimits",
  "getMySettings",
  "version",
  "adminRepoActivity",
  "getTemplateAllocations",
  "getAgentSource",
] as const;

export function appShellApiMocks(): Record<(typeof NAMES)[number], ReturnType<typeof vi.fn>> {
  return Object.fromEntries(NAMES.map((n) => [n, vi.fn()])) as Record<(typeof NAMES)[number], ReturnType<typeof vi.fn>>;
}

/** Quiet defaults: an admin signed in, every list empty, a healthy instance. */
export function setupAppShellMocks(api: typeof realApi, useAuth: typeof realUseAuth): void {
  const m = vi.mocked(api);
  vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, is_admin: true }));
  m.listRuns.mockResolvedValue({ runs: [] });
  m.adminListRuns.mockResolvedValue({ runs: [] });
  m.listWorkers.mockResolvedValue({ workers: [] });
  m.listRepos.mockResolvedValue({ repos: [{ enabled: true } as never] });
  m.listAgentTemplates.mockResolvedValue({ templates: [] });
  m.getTemplateAllocations.mockResolvedValue({ templates: [] });
  m.getAgentSource.mockResolvedValue({ agent_source: structuredClone(mockAgentSource) });
  m.listSecrets.mockResolvedValue({ secrets: [{ kind: "anthropic_token" } as never] });
  m.listConnections.mockResolvedValue({ connections: [{} as never] });
  m.listConnectionsBackground.mockResolvedValue({ connections: [] });
  m.getUsage.mockResolvedValue(emptySelf());
  m.getAdminUsage.mockResolvedValue({ factory: emptySelf(), users: [], earliest_run: null });
  m.getAdminHealth.mockResolvedValue(healthySilentDoc());
  m.workerUpgradeSummary.mockResolvedValue({ attention: 0, target_release: "0.6.0" });
  m.getJudgeStats.mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 });
  m.runsInProgressCount.mockResolvedValue({ count: 0 });
  m.listSchedules.mockResolvedValue([]);
  m.getFindingsStats.mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 });
  m.getMyRateLimits.mockResolvedValue({ status: "no_token" } as never);
  m.getMyCodexRateLimits.mockResolvedValue({ accounts: [] } as never);
  m.getMySettings.mockResolvedValue({ settings: { default_harness: null } } as never);
  m.version.mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" } as never);
  m.listUsers.mockResolvedValue({ users: [] });
  m.adminRepoActivity.mockResolvedValue({ repos: [] });
  m.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
}
