import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import type { Run } from "../lib/api";
import { mockRuns } from "../mocks/data";

// Issue #3767: ownerDecisionHold covers the anthropic_spend_limit hold, which
// changes the header Stop and Switch token visibility outside the hold panel.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
      rerunJudge: vi.fn(),
      resumeRun: vi.fn().mockResolvedValue({}),
      pauseRun: vi.fn().mockResolvedValue({ run: null }),
      listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      listFindings: vi.fn().mockResolvedValue({ findings: [] }),
      listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
      getMySettings: vi.fn().mockResolvedValue({ settings: { mr_rework_enabled: null } }),
      setRunMrRework: vi.fn().mockResolvedValue({ run: null }),
      getRunArchives: vi.fn().mockResolvedValue({
        supported: false,
        legacy: true,
        has_open_hold: false,
        counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 },
        archives: [],
      }),
    },
  };
});
vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
const mockUseRunStream = vi.mocked(useRunStream);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.unstubAllGlobals();
});

const held: Run = {
  ...mockRuns.find((run) => run.id === "run-queued")!,
  kind: "issue",
  harness: "claude",
  status: "recovery_wait",
  recovery_wait_cause: "anthropic_spend_limit",
  recovery_retry_not_before: null,
  outcome_pending: undefined,
};

async function mount(proof: boolean | undefined, canSteer: boolean) {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("Unexpected network request")));
  mockUseRunStream.mockReturnValue({
    run: held, messages: [], connected: true, error: "",
    submit: vi.fn().mockResolvedValue(undefined), refreshRun: vi.fn(), inputs: [],
    refreshInputs: vi.fn(), canSteer, confirmedOwner: proof,
  } as unknown as ReturnType<typeof useRunStream>);
  render(
    <MemoryRouter initialEntries={[`/runs/${held.id}`]}>
      <div className="p-4"><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></div>
    </MemoryRouter>,
  );
  await act(async () => { await Promise.resolve(); });
  await document.fonts.ready;
  // Open the disclosure when it is rendered, so Switch token is queryable in both arms.
  const settings = screen.queryByRole("button", { name: /Run settings/ });
  if (settings) fireEvent.click(settings);
}

const stopBtn = () => screen.queryByRole("button", { name: "Stop run" });
const switchBtn = () => screen.queryByRole("button", { name: "Switch token" });

it.each([320, 1024])("confirmed owner keeps header Stop and Switch token on a spend-limit hold at %ipx", async (width) => {
  await page.viewport(width, 1000);
  await mount(true, true);
  expect(screen.getAllByText("Anthropic API spend limit reached").length).toBeGreaterThan(0);
  expect(stopBtn()).not.toBeNull();
  expect(switchBtn()).not.toBeNull();
  for (const el of [stopBtn()!, switchBtn()!]) {
    const rect = el.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.height).toBeGreaterThan(0);
  }
});

it.each([undefined, false])("owner proof %s hides header Stop and Switch token on a spend-limit hold", async (proof) => {
  await page.viewport(390, 1000);
  await mount(proof, true);
  expect(screen.getAllByText("Anthropic API spend limit reached").length).toBeGreaterThan(0);
  // The disclosure is open (same path as the positive control), so absence is not a closed-menu artifact.
  expect(screen.getByRole("button", { name: /Run settings/ }).getAttribute("aria-expanded")).toBe("true");
  expect(stopBtn()).toBeNull();
  expect(switchBtn()).toBeNull();
});

it("a stream that cannot steer hides header Stop and Switch token even for a confirmed owner", async () => {
  await page.viewport(390, 1000);
  await mount(true, false);
  expect(screen.getAllByText("Anthropic API spend limit reached").length).toBeGreaterThan(0);
  expect(stopBtn()).toBeNull();
  expect(switchBtn()).toBeNull();
});
