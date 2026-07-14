// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ChatConversation, ChatList } from "./Chat";
import { api, type Chat, type Worker } from "../lib/api";

// Mock only the two network calls the list makes; keep the real helpers.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { listChats: vi.fn(), listWorkers: vi.fn(), createChat: vi.fn(), continueChat: vi.fn() },
  };
});

// The conversation view reuses the run-view stream; stub it so the seed behaviour
// is testable without a WebSocket or the run REST fetches.
vi.mock("../lib/useRunStream", () => ({
  useRunStream: vi.fn(() => ({
    run: null,
    messages: [],
    connected: false,
    error: "",
    refreshRun: vi.fn(),
  })),
}));

const mockApi = vi.mocked(api);

function aChat(over: Partial<Chat> = {}): Chat {
  return {
    id: "c1",
    title: "How does the gate work?",
    status: "running",
    turn_count: 1,
    resume_of_run_id: null,
    last_message_at: "2026-07-10T00:00:00Z",
    created_at: "2026-07-10T00:00:00Z",
    updated_at: "2026-07-10T00:00:00Z",
    ...over,
  };
}

// listChats returns the Chat items plus the max_turns envelope constant.
function chatList(chats: Chat[]) {
  return { chats, max_turns: 50 };
}

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: null,
    last_heartbeat_at: null,
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    ...over,
  };
}

beforeEach(() => {
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ChatList — conversation list from fixtures", () => {
  it("renders each conversation's title and status", async () => {
    mockApi.listChats.mockResolvedValue(
      chatList([
        aChat({ id: "a", title: "Active one", status: "running" }),
        aChat({ id: "b", title: "Ended one", status: "completed" }),
      ]),
    );

    render(
      <MemoryRouter>
        <ChatList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Active one")).toBeTruthy());
    expect(screen.getByText("Ended one")).toBeTruthy();
  });

  it("shows an empty state when there are no conversations", async () => {
    mockApi.listChats.mockResolvedValue(chatList([]));
    render(
      <MemoryRouter>
        <ChatList />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("No conversations yet")).toBeTruthy());
  });
});

describe("ChatList — ended-conversation Continue affordance (Decision 11)", () => {
  it("shows Continue only for ended conversations", async () => {
    mockApi.listChats.mockResolvedValue(
      chatList([
        aChat({ id: "a", title: "Active one", status: "running" }),
        aChat({ id: "b", title: "Ended one", status: "completed" }),
      ]),
    );

    render(
      <MemoryRouter>
        <ChatList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Ended one")).toBeTruthy());
    // Exactly one Continue button — the ended conversation's.
    expect(screen.getAllByRole("button", { name: "Continue" })).toHaveLength(1);
  });
});

describe("ChatList — worker-offline banner (Decision 15)", () => {
  it("shows the banner when no worker is online", async () => {
    mockApi.listChats.mockResolvedValue(chatList([]));
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "offline" })] });

    render(
      <MemoryRouter>
        <ChatList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText(/No worker connected/)).toBeTruthy());
  });

  it("hides the banner when a worker is online", async () => {
    mockApi.listChats.mockResolvedValue(chatList([]));
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "online" })] });

    render(
      <MemoryRouter>
        <ChatList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("No conversations yet")).toBeTruthy());
    expect(screen.queryByText(/No worker connected/)).toBeNull();
  });
});

describe("ChatConversation — optimistic seed after create/continue", () => {
  it("renders the header title from the route-state seed, before the list has it", async () => {
    // The list does NOT yet contain the just-created chat (eventual consistency).
    mockApi.listChats.mockResolvedValue(chatList([]));
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "online" })] });

    const seed = aChat({ id: "new-1", title: "My brand new chat", turn_count: 0, last_message_at: null });

    render(
      <MemoryRouter initialEntries={[{ pathname: "/chat/new-1", state: { seed } }]}>
        <Routes>
          <Route path="/chat/:id" element={<ChatConversation />} />
        </Routes>
      </MemoryRouter>,
    );

    // Present on the FIRST paint — sourced from the seed, not the (empty) list.
    expect(screen.getByText("My brand new chat")).toBeTruthy();
    // And it survives the list refetch that does not yet include this chat.
    await waitFor(() => expect(mockApi.listChats).toHaveBeenCalled());
    expect(screen.getByText("My brand new chat")).toBeTruthy();
  });
});
