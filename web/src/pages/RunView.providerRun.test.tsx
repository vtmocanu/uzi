// @vitest-environment jsdom
//
// PRD #4320 M7: a run bound to a custom model provider names the provider in the header
// instead of an Anthropic token, and offers no credential-switch affordance (the server
// refuses credential switches on provider runs).
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import type { Run } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: true, legacy: false, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
      archives: [],
    }),
  } };
});

function mount(run: Run) {
  vi.mocked(useRunStream).mockImplementation(() => ({
    run, messages: [], connected: true, error: "", submit: vi.fn(), refreshRun: vi.fn(),
    inputs: [], refreshInputs: vi.fn(), canSteer: true, confirmedOwner: true, pauseOriginReady: true,
  } as unknown as ReturnType<typeof useRunStream>));
  return render(<MemoryRouter initialEntries={["/runs/A"]}>
    <Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
}

const base = (): Run => ({
  ...mockRuns.find((r) => r.id === "run-cred-switch-settling")!,
  id: "A",
  credential_switch: "requested",
});

beforeEach(() => vi.clearAllMocks());
afterEach(cleanup);

it("a vendor run with a pending token switch still shows the switch badge (control)", () => {
  mount({ ...base(), provider_id: null, provider_name: null });
  expect(screen.getAllByText("switch pending").length).toBeGreaterThan(0);
});

it("a provider run names the provider and hides the credential override", () => {
  mount({ ...base(), provider_id: "prov-1", provider_name: "team-gateway" });
  expect(screen.getAllByText(/team-gateway/).length).toBeGreaterThan(0);
  expect(screen.queryByText("switch pending")).toBeNull();
  expect(screen.queryByText(/token override/)).toBeNull();
});
