// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IssueTrackerLinkDialog } from "./IssueTrackerLinkDialog";
import {
  api,
  ApiError,
  type IssueTrackerConnection,
  type IssueTrackerLinkResult,
  type IssueTrackerTag,
} from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listIssueTrackerConnections: vi.fn(),
      listIssueTrackerProjects: vi.fn(),
      listIssueTrackerTags: vi.fn(),
      linkRepoToIssueTracker: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const CONN: IssueTrackerConnection = {
  id: "itc-1",
  kind: "youtrack",
  base_url: "https://youtrack.example.com",
  account_id: "1-17",
  account_login: "uzi-bot",
  created_at: "2026-11-01T09:00:00Z",
  last_verified_at: "2026-11-18T12:00:00Z",
};

const ALL_USERS = { id: "3-0", name: "All Users", all_users: true, users_count: 0 };

function tag(over: Partial<IssueTrackerTag> = {}): IssueTrackerTag {
  return {
    id: "6-2",
    name: "uzi",
    owner: { id: "1-17", login: "uzi-bot" },
    read: { groups: [ALL_USERS], users: [] },
    use: { groups: [ALL_USERS], users: [] },
    account_can_apply: true,
    others_can_apply: true,
    ...over,
  };
}

const D13 = "This choice cannot be changed afterwards: there is no unlink or relink.";
const D14 =
  "uzi checks who can apply the tag, but not whether those people can edit issues in this project. Confirm they can.";

let onLinked: Mock<(link: IssueTrackerLinkResult, connection: IssueTrackerConnection | null) => void>;
let onClose: Mock<() => void>;

function renderDialog() {
  return render(
    <MemoryRouter>
      <IssueTrackerLinkDialog
        repoId="repo-1"
        repoLabel="acme/atlas-web"
        demo={false}
        onLinked={onLinked}
        onClose={onClose}
      />
    </MemoryRouter>,
  );
}

const linkButton = () => screen.getByRole("button", { name: "Link" }) as HTMLButtonElement;
const confirmBox = () => screen.getByRole("checkbox", { name: "They can edit issues in this project" }) as HTMLInputElement;

async function choose(tagId = "6-2") {
  const project = (await screen.findByLabelText("Project")) as HTMLSelectElement;
  await waitFor(() => expect(project.disabled).toBe(false));
  fireEvent.change(project, { target: { value: "0-4" } });
  const tagSelect = screen.getByLabelText("Eligibility tag") as HTMLSelectElement;
  await waitFor(() => expect(tagSelect.disabled).toBe(false));
  fireEvent.change(tagSelect, { target: { value: tagId } });
}

beforeEach(() => {
  onLinked = vi.fn<(link: IssueTrackerLinkResult, connection: IssueTrackerConnection | null) => void>();
  onClose = vi.fn<() => void>();
  mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [CONN] });
  mockApi.listIssueTrackerProjects.mockResolvedValue({
    projects: [
      { id: "0-4", short_name: "ATL", name: "Atlas" },
      { id: "0-7", short_name: "OPS", name: "Operations" },
    ],
  });
  mockApi.listIssueTrackerTags.mockResolvedValue({
    tags: [
      tag(),
      tag({
        id: "6-9",
        name: "triage",
        owner: { id: "1-3", login: "dana" },
        read: { groups: [], users: [{ id: "1-17", login: "uzi-bot" }] },
        use: { groups: [], users: [] },
        account_can_apply: false,
        others_can_apply: false,
      }),
      tag({
        id: "6-12",
        name: "bot-only",
        read: { groups: [{ id: "3-4", name: "Release crew", all_users: false, users_count: 1 }], users: [] },
        use: { groups: [{ id: "3-4", name: "Release crew", all_users: false, users_count: 1 }], users: [] },
        others_can_apply: false,
      }),
      tag({
        id: "6-20",
        name: "shared",
        use: {
          groups: [
            { id: "3-5", name: "Developers", all_users: false, users_count: 6 },
            { id: "3-6", name: "Ghosts", all_users: false, users_count: 0 },
          ],
          users: [{ id: "1-5", login: "erin" }],
        },
      }),
    ],
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("IssueTrackerLinkDialog", () => {
  it("lists the connection, projects as short name and name, and tags", async () => {
    renderDialog();
    const conn = (await screen.findByLabelText("Connection")) as HTMLSelectElement;
    expect(conn.value).toBe("itc-1");
    expect(conn.textContent).toBe("YouTrack https://youtrack.example.com (acts as uzi-bot)");
    await choose();
    expect(mockApi.listIssueTrackerProjects).toHaveBeenCalledWith("itc-1");
    expect(within(screen.getByLabelText("Project")).getByText("ATL — Atlas")).toBeTruthy();
    expect(within(screen.getByLabelText("Eligibility tag")).getByText("triage (owner dana)")).toBeTruthy();
  });

  it("links after the confirmation and hands the link back", async () => {
    const link = {
      repo_id: "repo-1",
      connection_id: "itc-1",
      project_id: "0-4",
      project_short_name: "ATL",
      tag_id: "6-2",
      tag_name: "uzi",
      linked_at: "2026-11-19T10:00:00Z",
    };
    mockApi.linkRepoToIssueTracker.mockResolvedValue({ link });
    renderDialog();
    await choose();

    expect(screen.getByText(D13)).toBeTruthy();
    expect(screen.getByText(D14)).toBeTruthy();
    // The confirmation is required: Link stays disabled until it is checked.
    expect(confirmBox().checked).toBe(false);
    expect(linkButton().disabled).toBe(true);
    fireEvent.click(linkButton());
    expect(mockApi.linkRepoToIssueTracker).not.toHaveBeenCalled();

    fireEvent.click(confirmBox());
    expect(linkButton().disabled).toBe(false);
    fireEvent.click(linkButton());
    await waitFor(() => expect(onLinked).toHaveBeenCalledWith(link, CONN));
    expect(mockApi.linkRepoToIssueTracker).toHaveBeenCalledWith("repo-1", {
      connection_id: "itc-1",
      project_id: "0-4",
      tag_id: "6-2",
    });
  });

  it.each([
    ["project", "Project", "0-7"],
    ["tag", "Eligibility tag", "6-20"],
  ])("clears the confirmation when the %s changes, so Link needs a fresh one", async (_what, label, value) => {
    renderDialog();
    await choose();
    fireEvent.click(confirmBox());
    expect(linkButton().disabled).toBe(false);

    fireEvent.change(screen.getByLabelText(label), { target: { value } });
    expect(confirmBox().checked).toBe(false);
    expect(linkButton().disabled).toBe(true);
    fireEvent.click(linkButton());
    expect(mockApi.linkRepoToIssueTracker).not.toHaveBeenCalled();
  });

  it("clears the confirmation when the connection changes", async () => {
    const OTHER: IssueTrackerConnection = { ...CONN, id: "itc-2", base_url: "https://youtrack-2.example.com" };
    mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [CONN, OTHER] });
    renderDialog();
    await choose();
    fireEvent.click(confirmBox());
    expect(linkButton().disabled).toBe(false);

    fireEvent.change(screen.getByLabelText("Connection"), { target: { value: "itc-2" } });
    await waitFor(() => expect(mockApi.listIssueTrackerProjects).toHaveBeenCalledWith("itc-2"));
    expect(confirmBox().checked).toBe(false);
    // Re-picking the same project and tag under the new connection still needs a fresh confirmation.
    await choose();
    expect(confirmBox().checked).toBe(false);
    expect(linkButton().disabled).toBe(true);
  });

  it("describes the chosen tag's owner, who can see it and who can apply it", async () => {
    renderDialog();
    await choose("6-20");
    const sharing = screen.getByRole("region", { name: "Who can apply shared" });
    const text = sharing.textContent ?? "";
    expect(text).toContain("Owneruzi-bot");
    expect(text).toContain("Can see itthe owner and All Users");
    expect(text).toContain(
      "Can apply itthe owner and the group Developers (6 members), the group Ghosts (member count not reported), erin",
    );
    expect(text).toContain("The connection's account, uzi-bot, can apply this tag.");
    expect(text).toContain("People other than uzi-bot can apply it, so they can start work by tagging an issue.");
  });

  it("says what to change in YouTrack and disables Link when the account cannot apply the tag", async () => {
    renderDialog();
    await choose("6-9");
    const text = screen.getByRole("region", { name: "Who can apply triage" }).textContent ?? "";
    expect(text).toContain("Can apply itonly the owner");
    expect(text).toContain(
      "The connection's account, uzi-bot, cannot apply this tag. In YouTrack, share the tag's use settings with uzi-bot or with All Users, or pick a tag uzi-bot owns.",
    );
    fireEvent.click(confirmBox());
    expect(linkButton().disabled).toBe(true);
  });

  it("says what to change in YouTrack and disables Link when nobody else can apply the tag", async () => {
    renderDialog();
    await choose("6-12");
    const text = screen.getByRole("region", { name: "Who can apply bot-only" }).textContent ?? "";
    expect(text).toContain("the group Release crew (1 member)");
    expect(text).toContain(
      "Nobody other than uzi-bot can apply it, so nobody could start work by tagging an issue. In YouTrack, share the tag's use settings with named users or with All Users. A named group counts only when YouTrack reports at least two members for it.",
    );
    fireEvent.click(confirmBox());
    expect(linkButton().disabled).toBe(true);
  });

  it.each([
    [409, "only a repo that has never been enabled can be linked to an issue tracker; this one was enabled before"],
    [400, "project not found among the projects this connection can see"],
    [
      409,
      "the connection's account cannot apply this tag; in the issue tracker, share the tag's use settings with this account, or with All Users, or pick a tag the account owns",
    ],
    [
      409,
      "nobody other than the connection's account can apply this tag; in the issue tracker, share the tag's use settings with named users or with All Users. This only checks who can apply the tag; it does not show who can edit issues in the project",
    ],
    [409, "this repo already stores issue numbers and cannot be linked to an issue tracker"],
    [
      409,
      "project ATL uses a state-machine workflow on its State field; uzi needs a plain State field. Link a project with a plain State field, or remove the state-machine rule from the State field in YouTrack.",
    ],
    [409, "this repo is already linked to an issue tracker project; a link cannot be changed"],
  ])("shows a %i refusal verbatim and keeps every selection: %s", async (status, message) => {
    mockApi.linkRepoToIssueTracker.mockRejectedValue(new ApiError(status, message));
    renderDialog();
    await choose();
    fireEvent.click(confirmBox());
    fireEvent.click(linkButton());

    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(onLinked).not.toHaveBeenCalled();
    expect((screen.getByLabelText("Connection") as HTMLSelectElement).value).toBe("itc-1");
    expect((screen.getByLabelText("Project") as HTMLSelectElement).value).toBe("0-4");
    expect((screen.getByLabelText("Eligibility tag") as HTMLSelectElement).value).toBe("6-2");
    expect(confirmBox().checked).toBe(true);
    expect(linkButton().disabled).toBe(false);
  });

  it("points to Settings → Issue trackers when there is no connection", async () => {
    mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [] });
    renderDialog();
    const msg = await screen.findByTestId("issue-tracker-no-connection");
    expect(msg.textContent).toBe(
      "No issue tracker connection yet. Connect one in Settings → Issue trackers, then link this repo.",
    );
    expect(within(msg).getByRole("link").getAttribute("href")).toBe("/settings/issue-trackers");
    expect(screen.queryByRole("button", { name: "Link" })).toBeNull();
    expect(screen.queryByLabelText("Project")).toBeNull();
  });

  it("closes on Cancel", async () => {
    renderDialog();
    await screen.findByLabelText("Connection");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalled();
  });

  it("strips invisible characters from API text", async () => {
    const RLO = String.fromCharCode(0x202e);
    const ZWSP = String.fromCharCode(0x200b);
    const UNSAFE = new RegExp(`[${RLO}${ZWSP}]`);
    mockApi.listIssueTrackerConnections.mockResolvedValue({
      connections: [{ ...CONN, base_url: `${CONN.base_url}${RLO}`, account_login: `uzi${ZWSP}-bot` }],
    });
    mockApi.listIssueTrackerProjects.mockResolvedValue({
      projects: [{ id: "0-4", short_name: `AT${RLO}L`, name: `At${ZWSP}las` }],
    });
    mockApi.listIssueTrackerTags.mockResolvedValue({
      tags: [
        tag({
          name: `u${RLO}zi`,
          owner: { id: "1-3", login: `da${ZWSP}na` },
          use: { groups: [{ id: "3-5", name: `Dev${RLO}s`, all_users: false, users_count: 3 }], users: [{ id: "1-5", login: `er${RLO}in` }] },
        }),
      ],
    });
    mockApi.linkRepoToIssueTracker.mockRejectedValue(new ApiError(409, `refused${RLO} here${ZWSP}`));
    const { baseElement } = renderDialog();
    await choose();
    fireEvent.click(confirmBox());
    fireEvent.click(linkButton());
    expect((await screen.findByRole("alert")).textContent).toBe("refused here");
    expect(screen.getByText("ATL — Atlas")).toBeTruthy();
    expect(baseElement.textContent).not.toMatch(UNSAFE);
    for (const el of baseElement.querySelectorAll("[aria-label]")) {
      expect(el.getAttribute("aria-label")).not.toMatch(UNSAFE);
    }
  });
});
