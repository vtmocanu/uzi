// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Memory as MemoryComponent } from "./Memory";
import { api, type Memory } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listMemory: vi.fn(),
      deleteMemory: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();
const minsAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();

function aMemory(over: Partial<Memory> = {}): Memory {
  return {
    id: "m1",
    repo_id: "repo-uzi",
    repo_name: "vtmocanu/uzi",
    title: "gcc is baked in 0.8.3",
    body: "No need to install build-essential.",
    run_id: "e2d7427b",
    created_at: minsAgo(30),
    basis: "observed",
    ...over,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// Issue #124, item 9: cross-run memory is agent-written free text, same untrusted class as
// judge output. Title and body both render raw.
describe("Memory — agent-written entries carry no format characters (#124)", () => {
  it("strips bidi/zero-width characters from title and body", async () => {
    mockApi.listMemory.mockResolvedValue({
      memories: [aMemory({ title: "gcc is \u202Ebaked in 0.8.3", body: "No need to \u202Einstall\u200B build-essential." })],
    });
    const { container } = render(<MemoryComponent />);
    // Anchored on the repo name, which the mutation cannot move.
    await waitFor(() => expect(screen.getByText(/vtmocanu\/uzi/)).toBeTruthy());
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("gcc is baked in 0.8.3")).toBeTruthy();
    expect(container.textContent).toContain("No need to install build-essential.");
  });
});

describe("Memory list + grouping", () => {
  it("renders the empty state when the user has no memory", async () => {
    mockApi.listMemory.mockResolvedValue({ memories: [] });
    render(<MemoryComponent />);
    expect(await screen.findByText("No agent memory yet")).toBeTruthy();
  });

  it("frames memory as inert/advisory (the read-back trust model)", async () => {
    mockApi.listMemory.mockResolvedValue({ memories: [aMemory()] });
    render(<MemoryComponent />);
    await screen.findByText("gcc is baked in 0.8.3");
    expect(screen.getByText(/inert and advisory/i)).toBeTruthy();
  });

  it("groups entries by repo, preserving newest-first order and per-repo counts", async () => {
    // Returned newest-first across repos: repo A is seen first (floats to top), and
    // its two entries stay together; repo B forms its own bucket below.
    mockApi.listMemory.mockResolvedValue({
      memories: [
        aMemory({ id: "a1", repo_name: "vtmocanu/uzi", title: "flag A1" }),
        aMemory({ id: "b1", repo_id: "repo-atlas", repo_name: "vtmocanu/atlas", title: "flag B1", created_at: daysAgo(2) }),
        aMemory({ id: "a2", repo_name: "vtmocanu/uzi", title: "flag A2", created_at: daysAgo(3) }),
      ],
    });
    render(<MemoryComponent />);

    // Both repo headings render, uzi (first-seen) ahead of atlas.
    const uzi = await screen.findByText("vtmocanu/uzi");
    const atlas = screen.getByText("vtmocanu/atlas");
    expect(uzi.compareDocumentPosition(atlas) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // uzi bucket has both A entries, atlas has one.
    expect(screen.getByText("2 entries")).toBeTruthy();
    expect(screen.getByText("1 entry")).toBeTruthy();
    expect(screen.getByText("flag A1")).toBeTruthy();
    expect(screen.getByText("flag A2")).toBeTruthy();
    expect(screen.getByText("flag B1")).toBeTruthy();
  });

  it("renders provenance run_id when present", async () => {
    mockApi.listMemory.mockResolvedValue({ memories: [aMemory({ run_id: "e2d7427b" })] });
    render(<MemoryComponent />);
    expect(await screen.findByText("e2d7427b")).toBeTruthy();
  });

  // PRD #266 M3: provenance is a human audit signal — an unverified (inferred) fact must
  // read differently from an observed one, and its evidence must be visible.
  it("marks an inferred entry as unverified", async () => {
    mockApi.listMemory.mockResolvedValue({ memories: [aMemory({ basis: "inferred" })] });
    render(<MemoryComponent />);
    await screen.findByText("gcc is baked in 0.8.3");
    expect(screen.getByText("inferred")).toBeTruthy();
    expect(screen.queryByText("observed")).toBeNull();
  });

  it("treats a missing/unknown basis defensively as inferred", async () => {
    // A legacy row (or any unexpected value) must never render as verified.
    mockApi.listMemory.mockResolvedValue({
      memories: [aMemory({ basis: undefined as unknown as Memory["basis"] })],
    });
    render(<MemoryComponent />);
    await screen.findByText("gcc is baked in 0.8.3");
    expect(screen.getByText("inferred")).toBeTruthy();
  });

  it("marks an observed entry and renders its evidence (stripped)", async () => {
    mockApi.listMemory.mockResolvedValue({
      memories: [
        aMemory({ basis: "observed", evidence: "Dockerfile:‮12 — gcc 0.8.3​" }),
      ],
    });
    const { container } = render(<MemoryComponent />);
    await screen.findByText("gcc is baked in 0.8.3");
    expect(screen.getByText("observed")).toBeTruthy();
    // Evidence renders, run through stripUnsafeChars (no bidi/zero-width survives).
    expect(container.textContent).toContain("Dockerfile:12 — gcc 0.8.3");
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("omits the evidence line when no evidence is present", async () => {
    mockApi.listMemory.mockResolvedValue({ memories: [aMemory({ evidence: undefined })] });
    render(<MemoryComponent />);
    await screen.findByText("gcc is baked in 0.8.3");
    expect(screen.queryByText(/evidence:/)).toBeNull();
  });

  it("surfaces a load error", async () => {
    mockApi.listMemory.mockRejectedValue(new Error("boom"));
    render(<MemoryComponent />);
    expect(await screen.findByText(/Failed to load memory/)).toBeTruthy();
  });
});

describe("Memory delete", () => {
  beforeEach(() => {
    mockApi.listMemory.mockResolvedValue({ memories: [aMemory({ id: "m9" })] });
  });

  it("arms a confirm before deleting, then purges and reloads on confirm", async () => {
    mockApi.deleteMemory.mockResolvedValue(null);
    render(<MemoryComponent />);

    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    // The first click only arms — it must NOT delete on a single stray click.
    expect(mockApi.deleteMemory).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(mockApi.deleteMemory).toHaveBeenCalledWith("m9"));
    // A reload follows the purge (initial load + post-delete reload).
    await waitFor(() => expect(mockApi.listMemory).toHaveBeenCalledTimes(2));
  });

  it("cancel disarms without deleting", async () => {
    render(<MemoryComponent />);
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(mockApi.deleteMemory).not.toHaveBeenCalled();
  });
});
