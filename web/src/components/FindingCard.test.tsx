// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { FindingCard } from "./FindingCard";
import { RunEventRow } from "./RunEvent";
import { api, ApiError, type RunMessage } from "../lib/api";

// Mock only the network; the inert-text helpers (stripUnsafeChars), the shared triage components
// and the isHttpsUrl link guard all run for real via importOriginal. The finding card drives only
// the single-finding endpoints plus the disposition-keyed undoFinding (issue #1723) — extend this
// shape only if a new api.* call appears.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      fileFinding: vi.fn(),
      dismissFinding: vi.fn(),
      findingIssueDraft: vi.fn(),
      markFindingDone: vi.fn(),
      undoFinding: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function findingMessage(payload: Record<string, unknown>): RunMessage {
  return {
    seq: 1,
    kind: "finding",
    agent: "coder",
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: "2026-07-05T12:00:00Z",
  };
}

function draftFixture(over: Record<string, unknown> = {}) {
  return {
    title: "File race in sweepLoop",
    description: "## What the worker found\n\nThe ticker leaks on early return.",
    location: "api/internal/sweeper.go#sweepLoop",
    labels: ["bug", "off-task"],
    provenance: "from the coder worker, run 8f2c1d04",
    ...over,
  };
}

// Opens the shared IssueDraftCard from the run-stream card and waits for the loaded form.
async function openDraft() {
  fireEvent.click(screen.getByRole("button", { name: "File issue" }));
  await screen.findByText("Draft issue");
  await screen.findByRole("button", { name: "Create issue" });
}

describe("FindingCard (PRD #333 M7, PRD #1183 M2 shared row)", () => {
  it("renders the three-button triage row (File issue . Mark done . Dismiss), the To triage chip and inert text", () => {
    const { container } = render(
      <FindingCard
        id="find-1"
        title="Leaked ticker in sweepLoop"
        location="api/internal/sweeper.go#sweepLoop"
        confidence="high"
        labels={["bug"]}
      />,
    );
    // Info/blue accent (D10), not the amber gate / brand action tones.
    expect(container.querySelector(".border-info\\/40")).toBeTruthy();
    // The shared row, in order: File issue · Mark done · Dismiss ▾ (issue #1723 added Mark done).
    const buttons = screen.getAllByRole("button").map((b) => b.textContent);
    expect(buttons).toEqual(["File issue", "Mark done", "Dismiss ▾"]);
    // The one shared open-state chip.
    expect(screen.getByText("To triage")).toBeTruthy();
    // The old single-finding controls are gone.
    expect(screen.queryByRole("button", { name: "File" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Edit .* file/ })).toBeNull();
    // The agent text is present as escaped children.
    expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy();
    expect(screen.getByText("api/internal/sweeper.go#sweepLoop")).toBeTruthy();
  });

  it("renders a bidi/control-laden payload INERT - no format char reaches the DOM", () => {
    const RLO = "\u202E";
    const ESC = "\u001B";
    const ZWSP = "\u200B";
    const { container } = render(
      <RunEventRow
        msg={findingMessage({
          id: "find-1",
          title: `Leak${RLO}ed ${ESC}ticker`,
          location: `api/${ZWSP}sweeper.go#loop`,
          labels: [],
        })}
        live={false}
      />,
    );
    const rendered = container.textContent ?? "";
    // The dispatch worked (the card, not the unrenderable fallback).
    expect(container.querySelector(".border-info\\/40")).toBeTruthy();
    // No control/format character survived to the DOM (issue #124: escaping does not strip Cf).
    expect(rendered).not.toMatch(/[\p{Cc}\p{Cf}]/u);
    // The markup never became an element.
    expect(rendered).toContain("Leaked");
  });

  it("File issue opens the shared draft; Create posts the edits to fileFinding and shows the Filed #N chip", async () => {
    mockApi.findingIssueDraft.mockResolvedValue(draftFixture());
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "File race in sweepLoop" },
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    await openDraft();
    expect(mockApi.findingIssueDraft).toHaveBeenCalledWith("find-1");
    // The finding draft carries no repo - the card is in no-selector mode.
    expect(screen.queryByRole("combobox")).toBeNull();

    // Edit the seeded title, then Create.
    fireEvent.change(screen.getByDisplayValue(/File race in sweepLoop/), { target: { value: "edited finding title" } });
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() =>
      expect(mockApi.fileFinding).toHaveBeenCalledWith("find-1", {
        title: "edited finding title",
        description: draftFixture().description,
        labels: ["bug", "off-task"],
      }),
    );
    // The green "Issue filed." box is gone; the filed state is the shared "Filed #N" link chip.
    const link = await screen.findByRole("link", { name: /Filed #512/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example.com/vtmocanu/uzi/-/issues/512");
    expect(screen.queryByText("Issue filed.")).toBeNull();
    // Draft card unmounted on success.
    expect(screen.queryByText("Draft issue")).toBeNull();
  });

  it("strips Cf from the draft SEED (defence in depth) - asserted on the draft input value", async () => {
    // \u202E RIGHT-TO-LEFT OVERRIDE, \u200B ZERO WIDTH SPACE - built from escapes, never pasted raw.
    mockApi.findingIssueDraft.mockResolvedValue(
      draftFixture({ title: "File \u202Erace", description: "## What\n\n\u200Bmalicious\u202E line" }),
    );
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    await openDraft();
    const title = screen.getByDisplayValue(/File .*race/) as HTMLInputElement;
    const body = screen.getByDisplayValue(/What/) as HTMLTextAreaElement;
    expect(title.value).not.toMatch(/[\p{Cf}]/u);
    expect(body.value).not.toMatch(/[\p{Cf}]/u);
    expect(title.value).toBe("File race");
    // The pre-wrap surface keeps \n (only Cf/control chars are dropped).
    expect(body.value).toContain("\n");
  });

  it("surfaces a created-with-warning line under the filed chip", async () => {
    mockApi.findingIssueDraft.mockResolvedValue(draftFixture());
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "File race" },
      warning: "The issue was created on the forge, but recording it in uzi failed.",
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    await openDraft();
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    // A warning is a success (the issue exists): the filed chip AND the note both show.
    await screen.findByRole("link", { name: /Filed #512/ });
    expect(screen.getByText(/recording it in uzi failed/)).toBeTruthy();
  });

  it("Dismiss posts the chosen reason and shows the reasoned Dismissed chip", async () => {
    mockApi.dismissFinding.mockResolvedValue({ status: "dismissed", reason: "wont_do" });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    const menu = screen.getByRole("menu");
    // The finding surface's worker-voiced subline.
    expect(within(menu).getByText("False positive, the worker got it wrong")).toBeTruthy();
    fireEvent.click(within(menu).getByText("Won't do"));

    await waitFor(() => expect(mockApi.dismissFinding).toHaveBeenCalledWith("find-1", "wont_do"));
    // The reason is carried into the shared chip (the user picked it - not invented).
    expect(await screen.findByText("Dismissed · Won't do")).toBeTruthy();
  });

  it("a Create that 409s shows FindingCard's own 'already filed / resolved' advisory, not an inline draft error", async () => {
    mockApi.findingIssueDraft.mockResolvedValue(draftFixture());
    mockApi.fileFinding.mockRejectedValue(new ApiError(409, "this finding is already filed or being filed"));
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    await openDraft();
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => expect(screen.getByText(/Already filed or resolved/)).toBeTruthy());
    // The advisory replaces the draft card; the scary raw error never surfaces.
    expect(screen.queryByText("Draft issue")).toBeNull();
    expect(container.textContent).not.toContain("already filed or being filed");
    expect(screen.getByText("resolved")).toBeTruthy();
  });

  it("a stale Dismiss that 409s shows the resolved advisory, never a crash", async () => {
    mockApi.dismissFinding.mockRejectedValue(new ApiError(409, "this finding is already resolved"));
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    fireEvent.click(within(screen.getByRole("menu")).getByText("Not an issue"));

    await waitFor(() => expect(screen.getByText(/Already filed or resolved/)).toBeTruthy());
    expect(container.textContent).not.toContain("already resolved");
    expect(screen.getByText("resolved")).toBeTruthy();
  });

  it("Mark done posts the evidence id, shows the plain ✓ Done chip, announces it and focuses Undo", async () => {
    mockApi.markFindingDone.mockResolvedValue({ status: "done", disposition_id: "disp-1" });
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));

    await waitFor(() => expect(mockApi.markFindingDone).toHaveBeenCalledWith("find-1"));
    // A human done: the plain "Done" chip, never the sync's "Done via".
    expect(await screen.findByText("Done")).toBeTruthy();
    expect(screen.queryByText(/Done via/)).toBeNull();
    // The action row swapped for the disposed row; the persistent live region announced it and
    // focus moved onto the Undo that just mounted.
    const undo = screen.getByRole("button", { name: "Undo" });
    await waitFor(() => expect(document.activeElement).toBe(undo));
    expect(screen.queryByRole("button", { name: "File issue" })).toBeNull();
    const region = container.querySelector('[role="status"]');
    expect(region?.textContent).toBe("Marked done");
  });

  it("Undo keys on the returned disposition id and restores the open row", async () => {
    mockApi.markFindingDone.mockResolvedValue({ status: "done", disposition_id: "disp-1" });
    mockApi.undoFinding.mockResolvedValue({
      disposition_id: "disp-1",
      finding_id: "find-1",
      location: "a.go#loop",
      repo_id: "repo-uzi",
      repo_path: "vtmocanu/uzi",
      status: "open",
      last_title: "Leaked ticker",
      seen_in_runs: 1,
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));

    await waitFor(() => expect(mockApi.undoFinding).toHaveBeenCalledWith("disp-1"));
    const file = await screen.findByRole("button", { name: "File issue" });
    expect(screen.getByText("To triage")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(file));
  });

  it("Undo of a done whose coordinate was filed from the backlog lands on the Filed #N chip", async () => {
    mockApi.markFindingDone.mockResolvedValue({ status: "done", disposition_id: "disp-1" });
    mockApi.undoFinding.mockResolvedValue({
      disposition_id: "disp-1",
      finding_id: "find-1",
      location: "a.go#loop",
      repo_id: "repo-uzi",
      repo_path: "vtmocanu/uzi",
      status: "filed",
      last_title: "Leaked ticker",
      seen_in_runs: 1,
      filed_issue_iid: 77,
      filed_issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/77",
    });
    render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));

    const link = await screen.findByRole("link", { name: /Filed #77/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example.com/vtmocanu/uzi/-/issues/77");
    // A filed coordinate does not re-offer File issue.
    expect(screen.queryByRole("button", { name: "File issue" })).toBeNull();
    // No button replaces the Undo that just unmounted, so focus lands on the Filed #N link rather
    // than dropping to document.body.
    await waitFor(() => expect(document.activeElement).toBe(link));
  });

  it("Undo that lands on the resolved fallback focuses the card, never document.body", async () => {
    mockApi.markFindingDone.mockResolvedValue({ status: "done", disposition_id: "disp-1" });
    // Filed but with no issue link to show: the card falls back to its resolved advisory, which
    // mounts neither a button nor a link.
    mockApi.undoFinding.mockResolvedValue({
      disposition_id: "disp-1",
      finding_id: "find-1",
      location: "a.go#loop",
      repo_id: "repo-uzi",
      repo_path: "vtmocanu/uzi",
      status: "filed",
      last_title: "Leaked ticker",
      seen_in_runs: 1,
    });
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));
    fireEvent.click(await screen.findByRole("button", { name: "Undo" }));

    await waitFor(() => expect(screen.getByText(/Already filed or resolved/)).toBeTruthy());
    const card = container.firstElementChild as HTMLElement;
    await waitFor(() => expect(document.activeElement).toBe(card));
    expect(document.activeElement).not.toBe(document.body);
  });

  it("a Mark done that 409s (being filed) shows the resolved advisory", async () => {
    mockApi.markFindingDone.mockRejectedValue(new ApiError(409, "cannot mark done (finding is being filed)"));
    const { container } = render(<FindingCard id="find-1" title="Leaked ticker" location="a.go#loop" labels={[]} />);

    fireEvent.click(screen.getByRole("button", { name: "Mark done" }));

    await waitFor(() => expect(screen.getByText(/Already filed or resolved/)).toBeTruthy());
    expect(container.textContent).not.toContain("being filed");
    // The Mark done button unmounted with the action row: focus lands on the named card, never body.
    const card = screen.getByRole("group", { name: "Incidental finding: Leaked ticker" });
    expect(card).toBe(container.firstElementChild);
    await waitFor(() => expect(document.activeElement).toBe(card));
    expect(document.activeElement).not.toBe(document.body);
  });

  it("dispatches to the unrenderable fallback when the finding payload carries no id", () => {
    const { container } = render(
      <RunEventRow msg={findingMessage({ title: "no id here", location: "a.go#x", labels: [] })} live={false} />,
    );
    // No actionable id -> the muted unrenderable line, not a dead-button card.
    expect(container.querySelector(".border-info\\/40")).toBeNull();
    expect(container.textContent).toContain("unrenderable finding event");
  });
});
