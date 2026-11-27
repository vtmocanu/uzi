// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { IssueTrackerSettings } from "./IssueTrackerSettings";
import { api, ApiError, type IssueTrackerConnection } from "../lib/api";

// Only the api object is swapped; ApiError and the types stay real, as in
// ForgeSettings.test.tsx.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      issueTrackerConfig: vi.fn(),
      listIssueTrackerConnections: vi.fn(),
      createIssueTrackerConnection: vi.fn(),
      deleteIssueTrackerConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

// Assembled at runtime so no token-shaped literal sits in the source.
const TOKEN = ["perm", "dGVzdA", "not-a-real-token"].join(":");
const BASE = "https://youtrack.example.com";

function conn(over: Partial<IssueTrackerConnection> = {}): IssueTrackerConnection {
  return {
    id: "itc-1",
    kind: "youtrack",
    base_url: BASE,
    account_id: "1-17",
    account_login: "uzi-bot",
    created_at: "2026-11-01T09:00:00Z",
    last_verified_at: "2026-11-18T12:00:00Z",
    ...over,
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <IssueTrackerSettings />
    </MemoryRouter>,
  );
}

function tokenInput(): HTMLInputElement {
  return screen.getByLabelText("Permanent token (scope: YouTrack)") as HTMLInputElement;
}

// The connection's row. Its account login is unique on the page (the base URL also
// shows as a form option).
async function findRow(): Promise<HTMLElement> {
  return (await screen.findByText("uzi-bot")).closest("li")!;
}

async function submitToken() {
  fireEvent.change(tokenInput(), { target: { value: TOKEN } });
  fireEvent.click(screen.getByRole("button", { name: "Connect" }));
}

beforeEach(() => {
  mockApi.issueTrackerConfig.mockResolvedValue({ allowed_base_urls: [BASE], kinds: ["youtrack"] });
  mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [conn()] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("IssueTrackerSettings", () => {
  it("lists a connection with its kind, base URL, account and dates", async () => {
    renderPage();
    const row = await findRow();
    expect(within(row).getByText("YouTrack")).toBeTruthy();
    expect(within(row).getByText(BASE)).toBeTruthy();
    expect(within(row).getByText("Connected")).toBeTruthy();
    expect(within(row).getByText("Last verified")).toBeTruthy();
    expect(screen.getByText(/To replace a token, connect again with the same base URL\./)).toBeTruthy();
  });

  it("strips invisible characters from API-provided text", async () => {
    mockApi.listIssueTrackerConnections.mockResolvedValue({
      connections: [conn({ account_login: "uzi‮-bot​" })],
    });
    const { container } = renderPage();
    await screen.findByText("uzi-bot");
    expect(container.textContent).not.toMatch(/[‮​]/);
  });

  it("hides the kind picker with one kind and defaults to it", async () => {
    renderPage();
    await findRow();
    expect(screen.queryByLabelText("Kind")).toBeNull();
    expect((screen.getByLabelText("Base URL") as HTMLSelectElement).value).toBe(BASE);
  });

  it("shows the kind picker with more than one kind, defaulting to the first", async () => {
    mockApi.issueTrackerConfig.mockResolvedValue({ allowed_base_urls: [BASE], kinds: ["youtrack", "other"] });
    renderPage();
    const picker = (await screen.findByLabelText("Kind")) as HTMLSelectElement;
    expect(picker.value).toBe("youtrack");
    expect(within(picker).getByText("YouTrack")).toBeTruthy();
  });

  it("connects, clears the token field and reloads the list", async () => {
    mockApi.createIssueTrackerConnection.mockResolvedValue({ connection: conn() });
    const { container } = renderPage();
    await findRow();
    const input = tokenInput();
    expect(input.getAttribute("type")).toBe("password");
    expect(input.getAttribute("autocomplete")).toBe("off");
    await submitToken();

    await screen.findByText(`Connected to ${BASE} as uzi-bot.`);
    expect(mockApi.createIssueTrackerConnection).toHaveBeenCalledWith("youtrack", BASE, TOKEN);
    expect(tokenInput().value).toBe("");
    expect(container.innerHTML).not.toContain(TOKEN);
    await waitFor(() => expect(mockApi.listIssueTrackerConnections).toHaveBeenCalledTimes(2));
  });

  it("passes the token to the create call and nowhere else", async () => {
    mockApi.createIssueTrackerConnection.mockResolvedValue({ connection: conn() });
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    renderPage();
    await findRow();
    await submitToken();
    await screen.findByText(/Connected to/);
    await waitFor(() => expect(mockApi.listIssueTrackerConnections).toHaveBeenCalledTimes(2));

    for (const [name, fn] of Object.entries(mockApi)) {
      const calls = JSON.stringify(vi.mocked(fn as (...a: unknown[]) => unknown).mock.calls);
      if (name === "createIssueTrackerConnection") {
        expect(calls).toContain(TOKEN);
      } else {
        expect(calls, name).not.toContain(TOKEN);
      }
    }
    expect(JSON.stringify(setItem.mock.calls)).not.toContain(TOKEN);
    setItem.mockRestore();
  });

  it.each([
    [400, "base URL is not in the allowed issue tracker list"],
    [400, "invalid base URL: use https scheme and host only, with no credentials, path, query or fragment"],
    [502, "token verification failed: the issue tracker answered 401"],
    [
      409,
      "this connection is linked to a repo and the new token belongs to a different account; connect with a token of the linked account",
    ],
    [400, "a token is required"],
  ])("shows a %i refusal verbatim and clears the token: %s", async (status, message) => {
    mockApi.createIssueTrackerConnection.mockRejectedValue(new ApiError(status, message));
    const { container } = renderPage();
    await findRow();
    await submitToken();

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(tokenInput().value).toBe("");
    expect(container.innerHTML).not.toContain(TOKEN);
    // A refusal does not reload the list.
    expect(mockApi.listIssueTrackerConnections).toHaveBeenCalledTimes(1);
  });

  it("deletes a connection after an inline confirm", async () => {
    mockApi.deleteIssueTrackerConnection.mockResolvedValue(null);
    renderPage();
    await findRow();
    fireEvent.click(screen.getByRole("button", { name: `Delete the YouTrack connection to ${BASE}` }));
    const confirm = screen.getByRole("group", { name: `Confirm deleting the YouTrack connection to ${BASE}` });
    expect(document.activeElement).toBe(confirm);
    expect(mockApi.deleteIssueTrackerConnection).not.toHaveBeenCalled();

    mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [] });
    fireEvent.click(within(confirm).getByRole("button", { name: "Delete connection" }));
    await screen.findByText("No issue tracker connection yet");
    expect(mockApi.deleteIssueTrackerConnection).toHaveBeenCalledWith("itc-1");
    expect(screen.getByText("Connection deleted.")).toBeTruthy();
    // Focus does not fall back to <body> when the focused confirm disappears.
    const empty = screen.getByText("No issue tracker connection yet").closest("[tabindex='-1']");
    await waitFor(() => expect(document.activeElement).toBe(empty));
  });

  it("cancels a delete with Escape", async () => {
    renderPage();
    await findRow();
    fireEvent.click(screen.getByRole("button", { name: `Delete the YouTrack connection to ${BASE}` }));
    fireEvent.keyDown(screen.getByRole("group"), { key: "Escape" });
    expect(screen.queryByRole("group")).toBeNull();
    expect(mockApi.deleteIssueTrackerConnection).not.toHaveBeenCalled();
  });

  it("shows the refusal to delete a linked connection on its row", async () => {
    const message =
      "this connection is linked to a repo and cannot be deleted while it is linked; a link cannot be removed";
    mockApi.deleteIssueTrackerConnection.mockRejectedValue(new ApiError(409, message));
    renderPage();
    const row = await findRow();
    fireEvent.click(within(row).getByRole("button", { name: /^Delete the YouTrack connection/ }));
    fireEvent.click(within(row).getByRole("button", { name: "Delete connection" }));
    const alert = await within(row).findByRole("alert");
    expect(alert.textContent).toBe(message);
    expect(within(row).getByText(BASE)).toBeTruthy();
  });

  it("explains the empty allowlist, links the setup page and offers no form", async () => {
    mockApi.issueTrackerConfig.mockResolvedValue({ allowed_base_urls: [], kinds: ["youtrack"] });
    mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [] });
    renderPage();
    const msg = await screen.findByTestId("issue-tracker-no-allowlist");
    expect(msg.textContent?.replace(/\s+/g, " ").trim()).toBe(
      "An operator must set ISSUE_TRACKER_ALLOWED_BASE_URLS (chart value issueTracker.allowedBaseURLs) before an issue tracker can be connected.",
    );
    const link = screen.getByRole("link", { name: "issue trackers setup guide" });
    expect(link.getAttribute("href")).toBe("/docs/issue-trackers");
    expect(screen.queryByLabelText("Permanent token (scope: YouTrack)")).toBeNull();
    expect(screen.queryByRole("button", { name: "Connect" })).toBeNull();
  });

  describe("API text with invisible characters", () => {
    // Built at runtime so the source holds no invisible character a reviewer cannot see.
    const RLO = String.fromCharCode(0x202e);
    const ZWSP = String.fromCharCode(0x200b);
    const UNSAFE = new RegExp(`[${RLO}${ZWSP}]`);
    const dirtyConn = () =>
      conn({ base_url: `${BASE}${RLO}${ZWSP}`, account_login: `uzi${RLO}-bot${ZWSP}` });

    function expectClean(container: HTMLElement) {
      expect(container.textContent).not.toMatch(UNSAFE);
      for (const el of container.querySelectorAll("[aria-label]")) {
        expect(el.getAttribute("aria-label")).not.toMatch(UNSAFE);
      }
    }

    it("strips them from a connection's base URL and account login, in text and labels", async () => {
      mockApi.listIssueTrackerConnections.mockResolvedValue({ connections: [dirtyConn()] });
      const { container } = renderPage();
      const row = await findRow();
      expect(within(row).getByText(BASE)).toBeTruthy();
      expect(within(row).getByRole("button", { name: `Delete the YouTrack connection to ${BASE}` })).toBeTruthy();
      fireEvent.click(within(row).getByRole("button", { name: /^Delete the YouTrack connection/ }));
      expect(within(row).getByRole("group").getAttribute("aria-label")).toBe(
        `Confirm deleting the YouTrack connection to ${BASE}`,
      );
      expectClean(container);
    });

    it("strips them from the connect notice and a connect refusal", async () => {
      mockApi.createIssueTrackerConnection.mockResolvedValueOnce({ connection: dirtyConn() });
      const { container } = renderPage();
      await findRow();
      await submitToken();
      await screen.findByText(`Connected to ${BASE} as uzi-bot.`);
      expectClean(container);

      mockApi.createIssueTrackerConnection.mockRejectedValueOnce(
        new ApiError(400, `base URL${RLO} is not${ZWSP} allowed`),
      );
      await submitToken();
      expect((await screen.findByRole("alert")).textContent).toBe("base URL is not allowed");
      expectClean(container);
    });

    it("strips them from a delete refusal", async () => {
      mockApi.deleteIssueTrackerConnection.mockRejectedValue(new ApiError(409, `linked${RLO} repo${ZWSP}`));
      const { container } = renderPage();
      const row = await findRow();
      fireEvent.click(within(row).getByRole("button", { name: /^Delete the YouTrack connection/ }));
      fireEvent.click(within(row).getByRole("button", { name: "Delete connection" }));
      expect((await within(row).findByRole("alert")).textContent).toBe("linked repo");
      expectClean(container);
    });
  });

  describe("focus after the delete confirm closes", () => {
    const deleteButton = () => screen.getByRole("button", { name: `Delete the YouTrack connection to ${BASE}` });

    it("returns to the row's Delete button after Cancel", async () => {
      renderPage();
      await findRow();
      fireEvent.click(deleteButton());
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      await waitFor(() => expect(document.activeElement).toBe(deleteButton()));
    });

    it("returns to the row's Delete button after Escape", async () => {
      renderPage();
      await findRow();
      fireEvent.click(deleteButton());
      fireEvent.keyDown(screen.getByRole("group"), { key: "Escape" });
      await waitFor(() => expect(document.activeElement).toBe(deleteButton()));
    });

    it("returns to the row's Delete button after a refused delete", async () => {
      mockApi.deleteIssueTrackerConnection.mockRejectedValue(new ApiError(409, "refused"));
      renderPage();
      const row = await findRow();
      fireEvent.click(deleteButton());
      fireEvent.click(within(row).getByRole("button", { name: "Delete connection" }));
      await within(row).findByRole("alert");
      await waitFor(() => expect(document.activeElement).toBe(deleteButton()));
    });
  });

  it("says the token is stored sealed", async () => {
    renderPage();
    await findRow();
    expect(screen.getByText(/it is stored sealed and never shown again/)).toBeTruthy();
  });

  it("shows a load failure", async () => {
    mockApi.issueTrackerConfig.mockRejectedValue(new ApiError(500, "boom"));
    renderPage();
    expect((await screen.findByRole("alert")).textContent).toBe("boom");
  });
});
