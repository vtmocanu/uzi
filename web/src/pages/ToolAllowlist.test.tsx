// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ToolAllowlist } from "./ToolAllowlist";
import { api, type ToolAllowlistEntry } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listToolAllowlist: vi.fn(),
      createToolAllowlistEntry: vi.fn(),
      deleteToolAllowlistEntry: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function entry(over: Partial<ToolAllowlistEntry> & Pick<ToolAllowlistEntry, "id" | "name">): ToolAllowlistEntry {
  return {
    pinned_version: null,
    note: null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const ENTRIES: ToolAllowlistEntry[] = [
  entry({ id: "t-kubectl", name: "kubectl", note: "k8s repos" }),
  entry({ id: "t-terraform", name: "terraform", pinned_version: "1.7" }),
];

beforeEach(() => {
  mockApi.listToolAllowlist.mockResolvedValue({ allowlist: ENTRIES });
  mockApi.createToolAllowlistEntry.mockResolvedValue({ entry: entry({ id: "t-new", name: "jq" }) });
  mockApi.deleteToolAllowlistEntry.mockResolvedValue(null);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ToolAllowlist admin page", () => {
  it("lists the allowed packages with their version policy", async () => {
    render(
    <MemoryRouter>
      <ToolAllowlist />
    </MemoryRouter>,
  );
    expect(await screen.findByText("kubectl")).toBeTruthy();
    expect(screen.getByText("terraform")).toBeTruthy();
    // The pinned version renders; the any-version entry shows "any".
    expect(screen.getByText("= 1.7")).toBeTruthy();
    expect(screen.getAllByText("any").length).toBeGreaterThan(0);
  });

  it("adds a package via the form", async () => {
    render(
    <MemoryRouter>
      <ToolAllowlist />
    </MemoryRouter>,
  );
    await screen.findByText("kubectl");
    fireEvent.change(screen.getByPlaceholderText("e.g. kubectl"), { target: { value: "jq" } });
    fireEvent.click(screen.getByRole("button", { name: /add package/i }));
    await waitFor(() =>
      expect(mockApi.createToolAllowlistEntry).toHaveBeenCalledWith({ name: "jq", pinned_version: undefined, note: undefined }),
    );
  });

  it("removes a package", async () => {
    render(
    <MemoryRouter>
      <ToolAllowlist />
    </MemoryRouter>,
  );
    await screen.findByText("kubectl");
    fireEvent.click(screen.getAllByRole("button", { name: /remove/i })[0]!);
    await waitFor(() => expect(mockApi.deleteToolAllowlistEntry).toHaveBeenCalledWith("t-kubectl"));
  });
});
