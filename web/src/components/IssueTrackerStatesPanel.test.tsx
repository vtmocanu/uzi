// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { IssueTrackerStatesPanel } from "./IssueTrackerStatesPanel";
import { api, ApiError, type Board, type IssueTrackerState } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getIssueTrackerStates: vi.fn(),
      putIssueTrackerStates: vi.fn(),
      getBoard: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const STATES: IssueTrackerState[] = [
  { name: "Submitted", resolved: false, archived: false, closable: false, openable: true },
  { name: "Open", resolved: false, archived: false, closable: false, openable: true },
  { name: "In Progress", resolved: false, archived: false, closable: false, openable: true },
  { name: "Review", resolved: false, archived: false, closable: false, openable: true },
  { name: "Fixed", resolved: true, archived: false, closable: true, openable: false },
  { name: "Won't fix", resolved: true, archived: false, closable: false, openable: false },
];

const UNMAPPED = "Unmapped — a move into this column is refused until it is mapped.";

function board(columns: string[]): Board {
  return {
    repo_id: "repo-1",
    path_with_namespace: "acme/atlas-web",
    web_url: "https://gitlab.example.com/acme/atlas-web",
    forge_type: "gitlab",
    columns: columns.map((label_name, position) => ({ label_name, position })),
    cards: [],
    pipeline: null,
    eligibility_label: "uzi",
  };
}

let onClose: Mock<() => void>;

function renderPanel(columns?: string[]) {
  return render(<IssueTrackerStatesPanel repoId="repo-1" repoLabel="acme/atlas-web" columns={columns} onClose={onClose} />);
}

const select = (name: string) => screen.getByLabelText(name) as HTMLSelectElement;
const optionFor = (sel: HTMLSelectElement, value: string) =>
  Array.from(sel.options).find((o) => o.value === value) as HTMLOptionElement;
const rowOf = (name: string) => select(name).closest("li") as HTMLElement;

beforeEach(() => {
  onClose = vi.fn<() => void>();
  mockApi.getIssueTrackerStates.mockResolvedValue({ mapping: null, states: STATES });
  mockApi.getBoard.mockResolvedValue({ board: board(["Ready", "In progress", "Human Review"]) });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("IssueTrackerStatesPanel", () => {
  it("reads the board's columns and saves the mapping", async () => {
    mockApi.putIssueTrackerStates.mockImplementation(async (_id, mapping) => ({ mapping }));
    renderPanel();
    await screen.findByLabelText("Ready");
    expect(mockApi.getBoard).toHaveBeenCalledWith("repo-1");

    fireEvent.change(select("Open"), { target: { value: "Open" } });
    fireEvent.change(select("Closed"), { target: { value: "Fixed" } });
    fireEvent.change(select("Ready"), { target: { value: "Submitted" } });
    fireEvent.change(select("In progress"), { target: { value: "In Progress" } });
    fireEvent.click(screen.getByRole("button", { name: "Save States" }));

    await screen.findByText("State mapping saved.");
    expect(mockApi.putIssueTrackerStates).toHaveBeenCalledWith("repo-1", {
      open_state: "Open",
      closed_state: "Fixed",
      columns: [
        { label_name: "Ready", state: "Submitted" },
        { label_name: "In progress", state: "In Progress" },
      ],
    });
  });

  // PR #4757 review: column state lookups must not reach Object.prototype members.
  it("renders and saves with columns named like Object.prototype members left unmapped", async () => {
    mockApi.getBoard.mockResolvedValue({ board: board(["toString", "constructor", "__proto__", "Ready"]) });
    mockApi.putIssueTrackerStates.mockImplementation(async (_id, mapping) => ({ mapping }));
    renderPanel();
    await screen.findByLabelText("toString");
    for (const name of ["toString", "constructor", "__proto__"]) {
      expect(select(name).value).toBe("");
      expect(within(rowOf(name)).getByText(UNMAPPED)).toBeTruthy();
    }

    fireEvent.change(select("Open"), { target: { value: "Open" } });
    fireEvent.change(select("Closed"), { target: { value: "Fixed" } });
    fireEvent.change(select("Ready"), { target: { value: "Submitted" } });
    fireEvent.click(screen.getByRole("button", { name: "Save States" }));

    await screen.findByText("State mapping saved.");
    expect(mockApi.putIssueTrackerStates).toHaveBeenCalledWith("repo-1", {
      open_state: "Open",
      closed_state: "Fixed",
      columns: [{ label_name: "Ready", state: "Submitted" }],
    });
  });

  it("shows the saved mapping and uses given columns without reading the board", async () => {
    mockApi.getIssueTrackerStates.mockResolvedValue({
      mapping: { open_state: "Open", closed_state: "Fixed", columns: [{ label_name: "Doing", state: "Review" }] },
      states: STATES,
    });
    renderPanel(["Doing", "Done-ish"]);
    await screen.findByLabelText("Doing");
    expect(mockApi.getBoard).not.toHaveBeenCalled();
    expect(select("Open").value).toBe("Open");
    expect(select("Closed").value).toBe("Fixed");
    expect(select("Doing").value).toBe("Review");
    expect(select("Done-ish").value).toBe("");
  });

  it("offers only openable States for the open State and the columns", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    for (const name of ["Open", "Ready"]) {
      const values = Array.from(select(name).options).map((o) => o.value);
      expect(values).not.toContain("Fixed");
      expect(values).not.toContain("Won't fix");
      expect(values).toContain("Review");
    }
  });

  it("does not let a non-closable State be the closed State", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    const closed = select("Closed");
    expect(optionFor(closed, "Fixed").disabled).toBe(false);
    for (const name of ["Won't fix", "Open", "Review"]) {
      const opt = optionFor(closed, name);
      expect(opt.disabled).toBe(true);
      expect(opt.textContent).toBe(`${name} (cannot be the closed State: it does not mean done)`);
    }
  });

  it("does not offer a State that another row already uses", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    fireEvent.change(select("Open"), { target: { value: "Open" } });
    const opt = optionFor(select("Ready"), "Open");
    expect(opt.disabled).toBe(true);
    expect(opt.textContent).toBe("Open (used for the open State)");
    expect(optionFor(select("Open"), "Open").disabled).toBe(false);
  });

  it("says an unmapped column refuses moves until it is mapped", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    expect(screen.getAllByText(UNMAPPED)).toHaveLength(3);
    fireEvent.change(select("Ready"), { target: { value: "Review" } });
    expect(within(rowOf("Ready")).queryByText(UNMAPPED)).toBeNull();
    expect(within(rowOf("Human Review")).getByText(UNMAPPED)).toBeTruthy();
    expect(screen.getAllByText(UNMAPPED)).toHaveLength(2);
  });

  it("keeps the choices and shows the message when the save is refused", async () => {
    const message =
      'the state "Fixed" cannot be the closed state: it must be a resolved state that means done (for example Fixed or Done), never one such as Won\'t fix';
    mockApi.putIssueTrackerStates.mockRejectedValue(new ApiError(400, message));
    renderPanel();
    await screen.findByLabelText("Ready");
    fireEvent.change(select("Open"), { target: { value: "Submitted" } });
    fireEvent.change(select("Closed"), { target: { value: "Fixed" } });
    fireEvent.change(select("Human Review"), { target: { value: "Review" } });
    fireEvent.click(screen.getByRole("button", { name: "Save States" }));

    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(select("Open").value).toBe("Submitted");
    expect(select("Closed").value).toBe("Fixed");
    expect(select("Human Review").value).toBe("Review");
    expect(screen.queryByText("State mapping saved.")).toBeNull();
  });

  it("keeps the open and closed rows usable when the board cannot be read", async () => {
    mockApi.getBoard.mockRejectedValue(new ApiError(409, "the repo has never been enabled; enable it first"));
    mockApi.getIssueTrackerStates.mockResolvedValue({
      mapping: { open_state: "Open", closed_state: "Fixed", columns: [{ label_name: "Doing", state: "Review" }] },
      states: STATES,
    });
    mockApi.putIssueTrackerStates.mockImplementation(async (_id, mapping) => ({ mapping }));
    renderPanel();
    const note = await screen.findByTestId("states-board-note");
    expect(note.textContent).toContain("the repo has never been enabled; enable it first");
    // The saved column stays, so a save does not drop it.
    expect(select("Doing").value).toBe("Review");
    fireEvent.click(screen.getByRole("button", { name: "Save States" }));
    await waitFor(() =>
      expect(mockApi.putIssueTrackerStates).toHaveBeenCalledWith("repo-1", {
        open_state: "Open",
        closed_state: "Fixed",
        columns: [{ label_name: "Doing", state: "Review" }],
      }),
    );
  });

  it("marks a saved State the project no longer offers and does not resend it", async () => {
    const STALE_NOTE = (why: string, alt: string) =>
      `The saved State ${why}. Choose ${alt}; Save stays off until then.`;
    mockApi.getIssueTrackerStates.mockResolvedValue({
      // "Triage" was renamed away; "Won't fix" is still a State but cannot be the closed one.
      mapping: {
        open_state: "Open",
        closed_state: "Won't fix",
        columns: [
          { label_name: "Ready", state: "Triage" },
          { label_name: "In progress", state: "In Progress" },
        ],
      },
      states: STATES,
    });
    mockApi.putIssueTrackerStates.mockImplementation(async (_id, mapping) => ({ mapping }));
    renderPanel();
    await screen.findByLabelText("Ready");

    // The column keeps its saved value, marked, with the note instead of the unmapped one.
    const ready = select("Ready");
    expect(ready.value).toBe("Triage");
    expect(optionFor(ready, "Triage").textContent).toBe("Triage (no longer a State of the project)");
    expect(optionFor(ready, "Triage").disabled).toBe(true);
    expect(within(rowOf("Ready")).getByText(STALE_NOTE("is no longer a State of the project", "another State or Unmapped"))).toBeTruthy();
    expect(within(rowOf("Ready")).queryByText(UNMAPPED)).toBeNull();
    // The closed row shows its saved non-closable State, saying it is not allowed there.
    expect(select("Closed").value).toBe("Won't fix");
    expect(within(rowOf("Closed")).getByText(STALE_NOTE("is not allowed for this row", "another State"))).toBeTruthy();
    // Rows still offered stay unmarked.
    expect(within(rowOf("Open")).queryByText(/The saved State/)).toBeNull();

    const save = screen.getByRole("button", { name: "Save States" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.click(save);
    expect(mockApi.putIssueTrackerStates).not.toHaveBeenCalled();

    fireEvent.change(ready, { target: { value: "" } });
    expect(within(rowOf("Ready")).getByText(UNMAPPED)).toBeTruthy();
    expect(save.disabled).toBe(true);
    fireEvent.change(select("Closed"), { target: { value: "Fixed" } });
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() =>
      expect(mockApi.putIssueTrackerStates).toHaveBeenCalledWith("repo-1", {
        open_state: "Open",
        closed_state: "Fixed",
        columns: [{ label_name: "In progress", state: "In Progress" }],
      }),
    );
  });

  it("marks a saved open State that is no longer openable", async () => {
    mockApi.getIssueTrackerStates.mockResolvedValue({
      mapping: { open_state: "Fixed", closed_state: "Fixed", columns: [] },
      states: STATES,
    });
    renderPanel([]);
    await screen.findByLabelText("Open");
    expect(select("Open").value).toBe("Fixed");
    expect(optionFor(select("Open"), "Fixed").textContent).toBe("Fixed (no longer offered for this row)");
    expect(
      within(rowOf("Open")).getByText(
        "The saved State is not allowed for this row. Choose another State; Save stays off until then.",
      ),
    ).toBeTruthy();
    expect((screen.getByRole("button", { name: "Save States" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows a load refusal", async () => {
    mockApi.getIssueTrackerStates.mockRejectedValue(new ApiError(409, "this repo is not linked to an issue tracker project"));
    renderPanel();
    expect((await screen.findByRole("alert")).textContent).toBe("this repo is not linked to an issue tracker project");
  });

  it("shows the state-machine refusal as its load error", async () => {
    const reason =
      "project ATL uses a state-machine workflow on its State field; uzi needs a plain State field. Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack.";
    mockApi.getIssueTrackerStates.mockRejectedValue(new ApiError(409, reason));
    renderPanel();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(reason);
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("disables Save until the open and the closed State are chosen", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    const save = screen.getByRole("button", { name: "Save States" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
    fireEvent.change(select("Open"), { target: { value: "Open" } });
    expect(save.disabled).toBe(true);
    fireEvent.change(select("Closed"), { target: { value: "Fixed" } });
    expect(save.disabled).toBe(false);
  });

  it("strips invisible characters from State names, column labels and errors", async () => {
    const RLO = String.fromCharCode(0x202e);
    const ZWSP = String.fromCharCode(0x200b);
    mockApi.getIssueTrackerStates.mockResolvedValue({
      mapping: null,
      states: [
        { name: `Op${RLO}en`, resolved: false, archived: false, closable: false, openable: true },
        { name: `Fi${ZWSP}xed`, resolved: true, archived: false, closable: true, openable: false },
      ],
    });
    mockApi.putIssueTrackerStates.mockRejectedValue(new ApiError(400, `no${RLO} way${ZWSP}`));
    const { container } = renderPanel([`Rea${ZWSP}dy`]);
    await screen.findByLabelText("Ready");
    fireEvent.change(select("Open"), { target: { value: `Op${RLO}en` } });
    fireEvent.change(select("Closed"), { target: { value: `Fi${ZWSP}xed` } });
    fireEvent.click(screen.getByRole("button", { name: "Save States" }));
    expect((await screen.findByRole("alert")).textContent).toBe("no way");
    expect(container.textContent).not.toMatch(new RegExp(`[${RLO}${ZWSP}]`));
    // The exact name still goes back to the server.
    expect(mockApi.putIssueTrackerStates.mock.calls[0][1].open_state).toBe(`Op${RLO}en`);
  });

  it("focuses its heading on open and closes on Close", async () => {
    renderPanel();
    await screen.findByLabelText("Ready");
    expect(document.activeElement).toBe(screen.getByRole("heading", { name: "Issue tracker States" }));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(onClose).toHaveBeenCalled();
  });
});

describe("IssueTrackerStatesPanel repo id", () => {
  it("shows the repo id as selectable text with the CLI it is for", async () => {
    renderPanel();
    await screen.findByLabelText("Open");
    const id = screen.getByText("repo-1");
    expect(id.tagName).toBe("CODE");
    expect(id.parentElement?.textContent).toContain("uzi repo issue-tracker-states");
  });
});
