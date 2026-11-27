// @vitest-environment jsdom
// The board's column settings on a repo linked to an issue tracker (PRD #4173 M6): the
// entry to the State mapping shows only on a linked board.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ColumnSettings } from "./ColumnSettings";
import { api, type Board } from "../../lib/api";
import { useAuth } from "../../auth/AuthContext";

vi.mock("../../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../../lib/api")>();
  return {
    ...actual,
    api: {
      configureColumns: vi.fn(),
      getIssueTrackerStates: vi.fn(),
      putIssueTrackerStates: vi.fn(),
      getBoard: vi.fn(),
    },
  };
});

vi.mock("../../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function board(over: Partial<Board> = {}): Board {
  return {
    repo_id: "repo-1",
    path_with_namespace: "acme/atlas-web",
    web_url: "https://gitlab.example.com/acme/atlas-web",
    forge_type: "gitlab",
    columns: [
      { label_name: "Ready", position: 0 },
      { label_name: "Doing", position: 1 },
    ],
    cards: [],
    pipeline: null,
    ...over,
  };
}

function renderSettings(b: Board) {
  return render(<ColumnSettings board={b} onSaved={vi.fn()} onError={vi.fn()} />);
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ autopilotLabel: "uzi-autopilot" } as unknown as ReturnType<typeof useAuth>);
  mockApi.getIssueTrackerStates.mockResolvedValue({
    mapping: null,
    states: [
      { name: "Open", resolved: false, archived: false, closable: false, openable: true },
      { name: "Fixed", resolved: true, archived: false, closable: true, openable: false },
    ],
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ColumnSettings and issue tracker States", () => {
  it("offers no State mapping on an unlinked board", () => {
    renderSettings(board());
    expect(screen.queryByRole("button", { name: "Map States" })).toBeNull();
    expect(
      screen.getByText("Each column is a label. Order is left-to-right. New names are created as labels on the forge."),
    ).toBeTruthy();
  });

  it("opens the State mapping for the board's columns on a linked board", async () => {
    renderSettings(board({ eligibility_label: "uzi" }));
    expect(screen.queryByText(/Each column is a label/)).toBeNull();
    expect(screen.getByText(/Each column maps to a State of the linked issue tracker project/)).toBeTruthy();
    const trigger = screen.getByRole("button", { name: "Map States" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(trigger);
    const panel = (await screen.findByRole("heading", { name: "Issue tracker States" })).closest("section") as HTMLElement;
    expect(await within(panel).findByLabelText("Ready")).toBeTruthy();
    expect(within(panel).getByLabelText("Doing")).toBeTruthy();
    expect(mockApi.getIssueTrackerStates).toHaveBeenCalledWith("repo-1");
    // The board is already here: the panel does not read it again.
    expect(mockApi.getBoard).not.toHaveBeenCalled();

    fireEvent.click(within(panel).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("heading", { name: "Issue tracker States" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Map States" }));
  });
});
