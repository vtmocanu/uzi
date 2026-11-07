// @vitest-environment jsdom
// PRD #3989 M3: RunView renders CredentialsResetPanel for a credentials_reset hold and
// offers no Resume (the server refuses it with 409).
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { RunView } from "./RunView";
import { api } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";

vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api,
    getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    getRunArchives: vi.fn().mockResolvedValue({
      supported: false, legacy: true, has_open_hold: false,
      counts: { preparing: 0, uploading: 0, available: 0, needs_action: 0, expired: 0, discarded: 0 }, archives: [],
    }),
  } };
});

class Socket {
  onopen: (() => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  close() { this.onclose?.(); }
}
const reset = { ...mockRuns.find(r => r.id === "run-cred-reset")! };

beforeEach(() => {
  vi.stubGlobal("WebSocket", Socket);
  vi.spyOn(api, "getRun").mockResolvedValue({ run: reset });
  vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
  vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it("renders the credentials-reset panel and no working Resume", async () => {
  render(<MemoryRouter initialEntries={["/runs/run-cred-reset"]}><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
  expect(await screen.findByTestId("credentials-reset-panel")).toBeTruthy();
  expect(screen.getAllByText(/waiting: credentials were reset/i).length).toBeGreaterThan(0);
  expect(screen.queryByRole("button", { name: /Resume/ })).toBeNull();
  expect(screen.queryByText(/Paused(?: by| at)/)).toBeNull();
});
