// @vitest-environment jsdom
// The ONE path nothing covered: a NON-EMPTY version travelling the whole chain,
// GET /api/version -> useBuildInfoSnapshot -> useBuildInfo -> useAppVersion ->
// WorkersSettings cpVersion -> FleetUpgradePanel.
//
// Everything either side of that chain was tested and the chain itself was not.
// WorkerUpgradeBadge.test.tsx passes cpVersion as a PROP, so no hook runs, and its
// only target-release assertion is an ABSENCE (`queryByText(...)).toBeNull()`).
// WorkersSettings.test.tsx resolves `{version: ""}` — the SETTLED-UNKNOWN arm.
// (When this file was written that value folded to null and drove the PENDING arm;
// the tri-state fix changed which of the two it exercises, and neither is this
// one.) So the positive arm had never been driven from a wire response at all.
//
// Measured, not inferred: with this file removed and `cpVersion` in
// WorkersSettings.tsx hard-wired to null — the page ignoring the hook completely —
// the whole suite stayed green at 1373/1373.
//
// Own file, for the reason the other build-info test files have their own: the
// promise behind these hooks is memoised at module scope with no reset seam, and
// vitest isolates per FILE.

import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkersSettings } from "./WorkersSettings";
import { api, type Worker } from "../lib/api";

// The VERBATIM body a live Go server emitted on GET /api/version — a stamped
// docker build, captured with curl during PRD #175's validation. Kept exactly as it
// came off the wire, because a hand-written approximation of a response is the one
// thing this test is not allowed to be.
//
// The 40-char commit is a REAL commit in this repository (verified with
// `git cat-file -t`), which is what keeps it from reading as an invented
// high-entropy literal to a secret scanner.
//
// KEEP THE FIELD VALUES DISTINCT FROM ONE ANOTHER. That is what makes the assertion
// below discriminating rather than decorative: projecting the wrong field in
// useAppVersion (`?.version` -> `?.founded`) has to produce a visibly WRONG string
// here. Making `founded` or any other field share the version's value would
// silently disarm this test while leaving it green.
const LIVE_BODY = {
  version: "0.11.99",
  founded: "2026-07-03",
  built_at: "2026-07-28T18:45:57Z",
  commit: "1130b3920a2346d02708827aa0d19486c995541f",
  commits: 2117,
  uptime_seconds: 16,
};

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listWorkers: vi.fn(),
      version: vi.fn(),
      listSecrets: vi.fn(),
      setWorkerBindMode: vi.fn(),
      createWorker: vi.fn(),
      deleteWorker: vi.fn(),
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
    },
  };
});
// HostedWorkers (a child of this page) reads useAuth for the ephemeral toggle (PRD
// #649). These tests keep ephemeral_enabled off, so the toggle never renders; a null
// user is enough to keep the hook from throwing.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: null, refresh: vi.fn() }) }));

const mockApi = vi.mocked(api);

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: "base",
    version: "0.11.99",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.11.99",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    draining_since: null,
    ...over,
  };
}

beforeEach(() => {
  mockApi.version.mockResolvedValue(LIVE_BODY);
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("cpVersion still classifies off the WIDENED /api/version body", () => {
  it("carries the wire's `version` all the way to the fleet target-release line", async () => {
    render(
      <MemoryRouter>
        <WorkersSettings />
      </MemoryRouter>,
    );

    // Two steps rather than one waitFor on the whole string, for the failure
    // MESSAGE: matching the label first means a chain that delivers the WRONG value
    // fails immediately with a diff naming both strings, instead of timing out after
    // 5s with "unable to find element" — which is the same report a chain delivering
    // NOTHING gives, and those are different bugs.
    const line = await screen.findByText(/target release v/);
    expect(line.textContent).toContain("target release v0.11.99");

    // The POSITIVE arm specifically: not the pending one (cpVersion === null), and
    // not the unstamped one.
    const page = document.body.textContent ?? "";
    expect(page).not.toMatch(/no release stamp/);

    // …and none of the widened body's OTHER fields leaked into the badge. This panel
    // is a fleet-classification surface, not a build-info surface — the rest of the
    // coordinate set belongs to the footer popover.
    expect(page).not.toContain("1130b392");
    expect(page).not.toContain("2026-07-03");

    // One request feeds the page, the footer, and anything else on screen.
    expect(mockApi.version).toHaveBeenCalledTimes(1);
  });
});
