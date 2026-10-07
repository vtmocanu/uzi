// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { TrustedReviewBotsCard } from "./TrustedReviewBotsCard";
import { api, type AppSettings, type SettingSource, type SettingsResponse } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { updateSettings: vi.fn() } };
});

const mockApi = vi.mocked(api);

// Only mr_review_trusted_bots is read by the card; the rest of AppSettings is irrelevant.
function settings(value: string): AppSettings {
  return { mr_review_trusted_bots: value } as AppSettings;
}

function response(value: string): SettingsResponse {
  return { settings: settings(value), secrets: {}, sources: {} } as unknown as SettingsResponse;
}

function renderCard(value = "", sources: Record<string, SettingSource> = {}, onSaved = vi.fn()) {
  render(<TrustedReviewBotsCard settings={settings(value)} sources={sources} onSaved={onSaved} />);
  return onSaved;
}

const baseInput = (n: number) => screen.getByLabelText(`Forge address, bot ${n}`) as HTMLInputElement;
const idInput = (n: number) => screen.getByLabelText(`Bot user id, bot ${n}`) as HTMLInputElement;
const saveBtn = () => screen.getByRole("button", { name: "Save trusted bots" }) as HTMLButtonElement;
const addBtn = () => screen.getByRole("button", { name: "Add review bot" });

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("TrustedReviewBotsCard", () => {
  it("says third-party bots are withheld when the list is empty", () => {
    renderCard("");
    expect(screen.getByText("Comments from third-party review bots are currently withheld.")).toBeTruthy();
    expect(saveBtn().disabled).toBe(true);
  });

  it("renders each saved entry as a forge address and user id row", () => {
    renderCard("https://github.com#136622811,https://gitlab.example.com:8443#42");
    expect(baseInput(1).value).toBe("https://github.com");
    expect(idInput(1).value).toBe("136622811");
    expect(baseInput(2).value).toBe("https://gitlab.example.com:8443");
    expect(idInput(2).value).toBe("42");
    expect(screen.getByText("2 review bots trusted")).toBeTruthy();
    expect(saveBtn().disabled).toBe(true);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("adds a bot and saves the stored form", async () => {
    mockApi.updateSettings.mockResolvedValue(response("https://github.com#1,https://github.com#136622811"));
    const onSaved = renderCard("https://github.com#1");
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(2), { target: { value: " https://github.com " } });
    fireEvent.change(idInput(2), { target: { value: "136622811" } });
    fireEvent.click(saveBtn());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledTimes(1));
    expect(mockApi.updateSettings.mock.calls[0][0]).toEqual({
      mr_review_trusted_bots: "https://github.com#1,https://github.com#136622811",
    });
    expect(await screen.findByText("Trusted review bots saved.")).toBeTruthy();
    expect(onSaved).toHaveBeenCalledTimes(1);
  });

  it("stays clean when an edit leaves the saved value unchanged", () => {
    renderCard("https://github.com#2,https://github.com#1");
    fireEvent.change(baseInput(1), { target: { value: "https://github.com" } });
    expect(saveBtn().disabled).toBe(true);
  });

  it("ignores an added row left blank", async () => {
    mockApi.updateSettings.mockResolvedValue(response(""));
    renderCard("https://github.com#1");
    fireEvent.click(screen.getByRole("button", { name: "Remove bot 1" }));
    fireEvent.click(addBtn());
    fireEvent.click(saveBtn());
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledWith({ mr_review_trusted_bots: "" }));
    expect(await screen.findByText(/No review bot is trusted/)).toBeTruthy();
  });

  it("blocks the save and explains each invalid field, offering the canonical address", async () => {
    renderCard("");
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(1), { target: { value: "https://GitHub.com/coderabbitai" } });
    fireEvent.change(idInput(1), { target: { value: "coderabbitai" } });
    fireEvent.click(saveBtn());
    expect(screen.getByRole("alert").textContent).toBe("Fix the highlighted entries before saving.");
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
    expect(screen.getByText(/^Address: Leave out the path\./)).toBeTruthy();
    expect(screen.getByText("User id: Use the numeric id, not the login name.")).toBeTruthy();
    expect(baseInput(1).getAttribute("aria-invalid")).toBe("true");
    expect(document.activeElement).toBe(baseInput(1));

    fireEvent.click(screen.getByRole("button", { name: "Use https://github.com" }));
    expect(baseInput(1).value).toBe("https://github.com");
    expect(baseInput(1).getAttribute("aria-invalid")).toBeNull();
  });

  it.each([
    ["http://github.com", "1", /^Address: Use https:\/\/\./],
    ["https://github.com:443", "1", /^Address: Leave out the default port :443\./],
    ["https://bot@github.com", "1", /^Address: Leave out the user name\./],
    ["https://github.com", "0", "User id: Use a positive number without leading zeros."],
    ["https://github.com", "9223372036854775808", "User id: This number is too large to be a user id."],
  ])("flags %s#%s", (base, id, message) => {
    renderCard("");
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(1), { target: { value: base } });
    fireEvent.change(idInput(1), { target: { value: id } });
    fireEvent.blur(idInput(1));
    expect(screen.getByText(message)).toBeTruthy();
  });

  it("flags a duplicate entry", () => {
    renderCard("https://github.com#7");
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(2), { target: { value: "https://github.com" } });
    fireEvent.change(idInput(2), { target: { value: "7" } });
    fireEvent.blur(idInput(2));
    expect(screen.getByText("This bot is already listed.")).toBeTruthy();
  });

  it("keeps a malformed saved entry verbatim, flagged, and never saves it silently", async () => {
    mockApi.updateSettings.mockResolvedValue(response("https://github.com#1"));
    renderCard("https://github.com#1,coderabbitai,https://GitLab.com#9");
    // Every saved token is a row; nothing is dropped.
    expect(baseInput(2).value).toBe("coderabbitai");
    expect(idInput(2).value).toBe("");
    expect(baseInput(3).value).toBe("https://GitLab.com");
    expect(idInput(3).value).toBe("9");
    const warning = screen.getByText(/2 saved entries are not in the required format/);
    expect(warning.getAttribute("role")).toBe("status");
    // Focus passing through a row is not an edit: the saved form stays on show.
    fireEvent.focus(idInput(2));
    fireEvent.blur(idInput(2));
    expect(screen.getByText("Saved as “coderabbitai”.")).toBeTruthy();
    expect(screen.getByText("Saved as “https://GitLab.com#9”.")).toBeTruthy();
    // Only the valid entry counts as trusted.
    expect(screen.getByText("1 review bot trusted")).toBeTruthy();

    // An unrelated edit cannot save past the malformed entries.
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(4), { target: { value: "https://github.com" } });
    fireEvent.change(idInput(4), { target: { value: "2" } });
    fireEvent.click(saveBtn());
    expect(mockApi.updateSettings).not.toHaveBeenCalled();

    // Removing them is an explicit choice, and then the save goes through.
    fireEvent.click(screen.getByRole("button", { name: "Remove bot 4" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove bot 3" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove bot 2" }));
    fireEvent.click(saveBtn());
    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({ mr_review_trusted_bots: "https://github.com#1" }),
    );
  });

  it("locks every control when the value comes from the environment", () => {
    renderCard("https://github.com#1", { mr_review_trusted_bots: "env" });
    expect(
      screen.getByText("This setting is fixed by an environment variable and cannot be changed here."),
    ).toBeTruthy();
    expect(baseInput(1).disabled).toBe(true);
    expect(idInput(1).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Remove bot 1" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Add review bot" })).toBeNull();
    expect(saveBtn().disabled).toBe(true);
  });

  it("surfaces a server rejection", async () => {
    mockApi.updateSettings.mockRejectedValue(new Error("mr_review_trusted_bots: duplicate entry"));
    renderCard("");
    fireEvent.click(addBtn());
    fireEvent.change(baseInput(1), { target: { value: "https://github.com" } });
    fireEvent.change(idInput(1), { target: { value: "5" } });
    fireEvent.click(saveBtn());
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/duplicate entry|Failed to save/)).toBeTruthy();
  });
});
