// @vitest-environment jsdom
// Transport-only mocks: the page, panel, stream hook and request serializer are real.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { RunView } from "./RunView";
import { mockRuns } from "../mocks/data/runs";
import { MOCK_MODE, type Run } from "../lib/api";

class Socket {
  static last: Socket;
  onopen: (() => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  constructor() { Socket.last = this; }
  close() {}
  state() { this.onmessage?.({ data: JSON.stringify({ type: "state" }) } as MessageEvent); }
}
let current: Run;
let refusal: string | undefined;
let writes: { url: string; body: Record<string, unknown> }[];
let reads: string[];
function response(body: unknown, status = 200) {
  return { ok: status < 400, status, text: async () => JSON.stringify(body) } as Response;
}
beforeEach(() => {
  expect(MOCK_MODE).toBe(false);
  current = { ...mockRuns[0], id: "reset-run", harness: "claude", status: "awaiting_approval",
    gate_revision: 4, completion_revision: 2, reset_state_digest: "displayed-digest", plan_md: "New plan",
    milestones: [{ id: "m1", title: "Original title" }], milestones_candidate: [{ id: "m1", title: "Replacement title" }],
    milestones_completed: ["m1"], milestones_in_progress: [], milestones_agents: [],
    own_agents: [], repo_agents: [], required_capabilities: ["docker"], required_tools: [],
    completion_accepted: [], completion_deferred: [], plan_cross_check_summary: undefined,
    mr_iid: null, outcome_pending: undefined };
  writes = []; reads = []; refusal = undefined;
  vi.stubGlobal("WebSocket", Socket);
  vi.stubGlobal("fetch", vi.fn(async (url: string, init: RequestInit) => {
    if (init?.method !== "GET") {
      writes.push({ url, body: JSON.parse(init.body as string) });
      if (refusal) return response({ error: "Reset refused", reason: refusal }, 409);
      return response({ server_side: true });
    }
    reads.push(url);
    if (url === "/api/runs/reset-run") return response({ run: current });
    if (url.endsWith("/messages")) return response({ messages: [] });
    if (url.endsWith("/inputs")) return response({ inputs: [] });
    if (url === "/api/repos") return response({ repos: [] });
    if (url === "/api/workers") return response({ workers: [] });
    if (url.includes("/secrets")) return response({ secrets: [] });
    if (url.endsWith("/review")) return response({ review: null, pending_judge: null });
    return response({}, 404);
  }));
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

async function mount() {
  const router = createMemoryRouter([{ path: "/runs/:id", element: <RunView /> }], {
    initialEntries: ["/runs/reset-run"],
  });
  const rendered = render(<RouterProvider router={router} />);
  await screen.findByRole("button", { name: /^Approve plan/ });
  return { ...rendered, router };
}
async function open(override: boolean) {
  fireEvent.click(screen.getByRole("button", { name: override ? /Run without docker/ : /^Approve plan/ }));
  const region = within(await screen.findByRole("region", { name: "Confirm milestone reset" }));
  fireEvent.change(region.getByLabelText("Reason for reset"), { target: { value: "Replace scope" } });
  fireEvent.click(region.getByRole("checkbox"));
  return region;
}
it.each([false, true])("real approval pipeline serializes explicit reset, capability override=%s, without credential writes", async override => {
  current.completion_revision = null;
  await mount();
  const region = await open(override);
  fireEvent.click(region.getByRole("button", { name: "Reset progress and approve plan" }));
  await waitFor(() => expect(writes).toHaveLength(1));
  expect(writes[0]).toEqual({ url: "/api/runs/reset-run/inputs", body: {
    kind: "approve_plan", body: "", selection: { source: "own", exclusions: [] },
    ...(override ? { override_capabilities: true } : {}),
    expected_gate_revision: 4, expected_reset_state_digest: "displayed-digest",
    reconciliation: { mode: "reset", reason: "Replace scope", expected_completion_revision: null },
  } });
  await waitFor(() => expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull());
  expect(writes.some(w => w.url.includes("credential"))).toBe(false);
});

it.each(["milestone_reconciliation_required", "milestone_reconciliation_conflict", "milestone_reconciliation_not_ready"])(
  "refreshes after %s and requires a new review without retry", async reason => {
    refusal = reason;
    await mount();
    const region = await open(false);
    const before = reads.filter(r => r === "/api/runs/reset-run").length;
    fireEvent.click(region.getByRole("button", { name: "Reset progress and approve plan" }));
    await waitFor(() => expect(reads.filter(r => r === "/api/runs/reset-run").length).toBeGreaterThan(before));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull());
    expect(writes).toHaveLength(1);
    expect(screen.getByText(reason.endsWith("not_ready") ? /Request a plan revision so/ : /milestone reset requires a fresh review/)).toBeTruthy();
    await open(false);
    expect(writes).toHaveLength(1);
  },
);

it("live stream refresh invalidates the displayed reset snapshot", async () => {
  await mount();
  await open(false);
  current = { ...current, reset_state_digest: "new-digest", gate_revision: 5 };
  act(() => Socket.last.state());
  await waitFor(() => expect(screen.queryByRole("region", { name: "Confirm milestone reset" })).toBeNull());
  expect(writes).toHaveLength(0);
  expect(await screen.findByText(/displayed plan or reset effects changed/)).toBeTruthy();
});
