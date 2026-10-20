// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { RunView } from "./RunView";
import { api, type RunMessage } from "../lib/api";
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
  static last: Socket;
  onopen: (() => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  constructor() { Socket.last = this; }
  close() { this.onclose?.(); }
  deliver(frame: unknown) { this.onmessage?.({ data: JSON.stringify(frame) } as MessageEvent); }
}
const since = "2026-10-16T12:00:00.123456789Z";
const paused = { ...mockRuns[0], id: "pause", status: "paused" as const, status_since: since, hold_reason: null };
const marker = (origin: string): RunMessage => ({
  seq: 1, kind: "paused", payload: { origin, text: "untrusted prose" }, created_at: since,
  agent: null, agent_instance: null, agent_label: null,
});
beforeEach(() => {
  vi.stubGlobal("WebSocket", Socket);
  vi.spyOn(api, "getRun").mockResolvedValue({ run: paused });
  vi.spyOn(api, "getRunMessages").mockResolvedValue({ messages: [] });
  vi.spyOn(api, "getRunInputs").mockResolvedValue({ inputs: [] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.unstubAllGlobals(); });

function mount() {
  return render(<MemoryRouter initialEntries={["/runs/pause"]}><Routes><Route path="/runs/:id" element={<RunView />} /></Routes></MemoryRouter>);
}
it.each([
  ["lead_park", "Paused by the run's lead"],
  ["owner", "Paused by you"],
  ["hostile person", "Paused"],
])("renders fixed replay label for %s through the real hook", async (origin, label) => {
  vi.mocked(api.getRunMessages).mockResolvedValue({ messages: [marker(origin)] });
  mount();
  const heading = await screen.findByText(new RegExp(label + " at"));
  expect(heading.textContent).not.toContain("untrusted prose");
  expect(screen.getAllByRole("button", { name: /Resume/ }).length).toBeGreaterThan(0);
  expect(screen.getAllByRole("button", { name: "Stop run" }).length).toBeGreaterThan(0);
});
it.each([undefined, "", "bad", "2026-10-16T12:00:00.123456788Z"])(
  "raw live timestamp %s stays neutral through applyFrame, hook and page", async created_at => {
    mount();
    await screen.findByText(/Paused at/);
    await act(async () => Socket.last.deliver({ type: "message", seq: 1, kind: "paused", payload: { origin: "lead_park" }, created_at }));
    expect(screen.getByText(/Paused at/)).toBeTruthy();
    expect(screen.queryByText(/Paused by/)).toBeNull();
  },
);
it("recomputes live markers, newest invalid evidence, and resume/re-pause epochs", async () => {
  mount();
  await screen.findByText(/Paused at/);
  await act(async () => Socket.last.deliver({ type: "message", ...marker("lead_park") }));
  expect(screen.getByText(/Paused by the run's lead at/)).toBeTruthy();
  await act(async () => Socket.last.deliver({ type: "message", ...marker("owner"), seq: 2 }));
  expect(screen.getByText(/Paused by you at/)).toBeTruthy();
  await act(async () => Socket.last.deliver({ type: "message", ...marker("unknown"), seq: 3 }));
  expect(screen.getByText(/Paused at/)).toBeTruthy();
  vi.mocked(api.getRun).mockResolvedValue({ run: { ...paused, status: "running" } });
  await act(async () => Socket.last.deliver({ type: "state" }));
  await waitFor(() => expect(screen.queryByText(/Paused(?: by| at)/)).toBeNull());
  vi.mocked(api.getRun).mockResolvedValue({ run: { ...paused, status_since: "2026-10-16T12:01:00Z" } });
  await act(async () => Socket.last.deliver({ type: "state" }));
  await screen.findByText(/Paused at/);
  await act(async () => Socket.last.deliver({ type: "message", ...marker("lead_park"), seq: 4, created_at: "2026-10-16T12:01:00Z" }));
  await screen.findByText(/Paused by the run's lead at/);
});
it("cached marker becomes neutral after failed reconnect catchup", async () => {
  vi.mocked(api.getRunMessages).mockResolvedValue({ messages: [marker("lead_park")] });
  mount();
  await screen.findByText(/Paused by the run's lead at/);
  vi.mocked(api.getRunMessages).mockRejectedValue(new Error("offline"));
  const retired = Socket.last;
  act(() => retired.close());
  await waitFor(() => expect(Socket.last).not.toBe(retired), { timeout: 2500 });
  await act(async () => Socket.last.onopen?.());
  expect(screen.getByText(/Paused at/)).toBeTruthy();
});
it.each(["completion_blocked", "credential_disabled", "provider_unavailable"])("keeps %s hold out of the ordinary paused panel", async hold_reason => {
  vi.mocked(api.getRun).mockResolvedValue({ run: { ...paused, hold_reason } });
  vi.mocked(api.getRunMessages).mockResolvedValue({ messages: [marker("owner")] });
  mount();
  await act(async () => {});
  expect(screen.queryByText(/Paused(?: by| at)/)).toBeNull();
  expect(screen.queryByRole("button", { name: "▶ Resume" })).toBeNull();
});
function Navigation() {
  const navigate = useNavigate();
  return <>{["b", "pause"].map(id => <button key={id} onClick={() => navigate("/runs/" + id)}>Navigate to {id}</button>)}</>;
}

it.each([["b"], ["b", "pause"]])("keeps the pause heading isolated after navigation through %j", async (...ids) => {
  vi.mocked(api.getRun).mockImplementation(async id => ({ run: { ...paused, id } }));
  render(<MemoryRouter initialEntries={["/runs/pause"]}>
    <Navigation /><Routes><Route path="/runs/:id" element={<RunView />} /></Routes>
  </MemoryRouter>);
  await screen.findByText(/Paused at/);
  const oldMessage = Socket.last.onmessage!;
  await act(async () => Socket.last.deliver({ type: "message", ...marker("owner") }));
  expect(screen.getByText(/Paused by you at/).tagName).toBe("P");
  for (const id of ids) {
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Navigate to " + id })));
    await screen.findByText(/Paused at/);
  }
  await act(async () => oldMessage({ data: JSON.stringify({ type: "message", ...marker("lead_park") }) } as MessageEvent));
  const heading = screen.getByText(/Paused.* at/);
  expect(heading.tagName).toBe("P");
  expect(heading.textContent).toMatch(/^‖ Paused at /);
  await act(async () => Socket.last.deliver({ type: "message", ...marker("lead_park") }));
  expect(screen.getByText(/Paused.* at/).textContent).toMatch(/^‖ Paused by the run's lead at /);
});

it("retains the resume action with validated lead evidence", async () => {
  vi.mocked(api.getRunMessages).mockResolvedValue({ messages: [marker("lead_park")] });
  vi.spyOn(api, "resumeRun").mockResolvedValue({ run: paused });
  mount();
  await screen.findByText(/Paused by the run's lead at/);
  fireEvent.click(screen.getAllByRole("button", { name: /Resume/ })[0]);
  await waitFor(() => expect(api.resumeRun).toHaveBeenCalledWith("pause"));
});
