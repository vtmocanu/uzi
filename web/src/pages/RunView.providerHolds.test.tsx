// @vitest-environment jsdom
// PRD #4320 module 5: the two provider-binding holds announce their own guidance (never the generic
// "Resume it from this page" paused copy, which is false: the server refuses Resume with 409) and
// offer no Resume button.
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
const base = { ...mockRuns.find(r => r.id === "run-cred-reset")! };

beforeEach(() => {
  vi.stubGlobal("WebSocket", Socket);
  vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
  vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

it.each([
  ["provider_destination_not_allowed", /waits for the operator to allow the URL again/, /Cancel the run/],
  ["provider_binding_stale", /provider or its key changed/, /start a new one/],
] as const)("%s announces its own guidance and offers no Resume", async (hold_reason, own, action) => {
  vi.spyOn(api, "getRun").mockResolvedValue({ run: { ...base, hold_reason } });
  render(<MemoryRouter initialEntries={["/runs/run-cred-reset"]}><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
  const announce = await screen.findByText(own);
  expect(announce.textContent).toMatch(action);
  expect(announce.textContent).not.toMatch(/Resume it from this page/);
  expect(screen.queryByRole("button", { name: /Resume/ })).toBeNull();
});
