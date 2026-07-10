// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ProposalCard } from "./ProposalCard";
import { api, type IssueProposal } from "../lib/api";

// Keep the real module (isHttpsUrl, ApiError, types) and mock only the two
// network verbs the card calls.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { confirmProposal: vi.fn(), dismissProposal: vi.fn() },
  };
});

const mockApi = vi.mocked(api);

function aProposal(over: Partial<IssueProposal> = {}): IssueProposal {
  return {
    id: "prop-1",
    run_id: "chat-1",
    repo_id: "repo-1",
    repo_path: "grp/proj",
    title: "Add a metrics dashboard",
    description: "Plain draft text.",
    labels: ["PRD"],
    status: "pending",
    created_issue_iid: null,
    created_issue_url: null,
    created_at: "2026-07-10T00:00:00Z",
    resolved_at: null,
    ...over,
  };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("ProposalCard — model text is inert (Decision 8)", () => {
  it("renders a link-bearing description as plain text, never as an anchor", () => {
    const { container } = render(
      <ProposalCard
        chatId="chat-1"
        proposal={aProposal({
          title: "Look at [this](http://evil.example)",
          description: "Idea sketched at https://evil.example/x — see [click me](http://evil.example).",
        })}
      />,
    );
    // The load-bearing assertion: a pending proposal renders ZERO anchors, so a
    // model-supplied link is never clickable.
    expect(container.querySelector("a")).toBeNull();
    // The raw markdown/url text shows literally (escaped, not parsed).
    expect(screen.getByText(/click me/)).toBeTruthy();
    expect(screen.getByText(/https:\/\/evil\.example\/x/)).toBeTruthy();
  });

  it("shows Create issue / Dismiss on a pending proposal", () => {
    render(<ProposalCard chatId="chat-1" proposal={aProposal()} />);
    expect(screen.getByRole("button", { name: "Create issue" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
  });
});

describe("ProposalCard — confirm is the only write path", () => {
  it("on Create, calls confirmProposal and shows the returned issue link (app-rendered)", async () => {
    mockApi.confirmProposal.mockResolvedValue({
      proposal: aProposal({
        status: "confirmed",
        created_issue_iid: 321,
        created_issue_url: "https://gitlab.example.com/grp/proj/-/issues/321",
      }),
    });

    const { container } = render(<ProposalCard chatId="chat-1" proposal={aProposal()} />);
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() => expect(screen.getByText(/Issue created/)).toBeTruthy());
    expect(mockApi.confirmProposal).toHaveBeenCalledWith("chat-1", "prop-1");
    // Now — and only now — exactly one anchor exists: the real created-issue link.
    const anchors = container.querySelectorAll("a");
    expect(anchors).toHaveLength(1);
    expect(anchors[0].getAttribute("href")).toBe("https://gitlab.example.com/grp/proj/-/issues/321");
  });

  it("on Dismiss, writes nothing to the forge and shows the dismissed state", async () => {
    mockApi.dismissProposal.mockResolvedValue({
      proposal: aProposal({ status: "dismissed", resolved_at: "2026-07-10T01:00:00Z" }),
    });

    render(<ProposalCard chatId="chat-1" proposal={aProposal()} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));

    await waitFor(() => expect(screen.getByText(/Nothing was written to the forge/)).toBeTruthy());
    expect(mockApi.dismissProposal).toHaveBeenCalledWith("chat-1", "prop-1");
    expect(mockApi.confirmProposal).not.toHaveBeenCalled();
  });
});
