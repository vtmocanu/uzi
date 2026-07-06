// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSettings } from "./AdminSettings";
import { api, ApiError } from "../lib/api";

// Only the api object is swapped; ApiError and types stay real so the page's
// `instanceof ApiError` checks match what the mocked methods throw.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { getSettings: vi.fn(), updateSettings: vi.fn() } };
});
// The page re-resolves the admin's own theme after a save via useAuth().refresh.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

beforeEach(() => {
  mockApi.getSettings.mockResolvedValue({
    settings: { prd_label: "PRD", autopilot_label: "autopilot", default_theme: "ember" },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderPage() {
  return render(
    <MemoryRouter>
      <AdminSettings />
    </MemoryRouter>,
  );
}

const input = (name: string) => screen.getByLabelText(name) as HTMLInputElement;
const saveButton = () => screen.getByRole("button", { name: /save settings/i }) as HTMLButtonElement;

describe("AdminSettings", () => {
  it("loads the current labels into the fields", async () => {
    renderPage();
    const prd = (await screen.findByLabelText("PRD label")) as HTMLInputElement;
    expect(prd.value).toBe("PRD");
    expect(input("Autopilot label").value).toBe("autopilot");
    // Save is disabled until a field changes.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves edited labels and shows a success notice", async () => {
    mockApi.updateSettings.mockResolvedValue({
      settings: { prd_label: "Feature", autopilot_label: "autopilot", default_theme: "ember" },
    });
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(input("PRD label"), { target: { value: "Feature" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        prd_label: "Feature",
        autopilot_label: "autopilot",
        default_theme: "ember",
      }),
    );
    expect(await screen.findByText(/Settings saved/i)).toBeTruthy();
    // Back to a clean (non-dirty) state after a successful save.
    expect(saveButton().disabled).toBe(true);
  });

  it("saves the default theme selection (PRD #21)", async () => {
    mockApi.updateSettings.mockResolvedValue({
      settings: { prd_label: "PRD", autopilot_label: "autopilot", default_theme: "mission" },
    });
    renderPage();
    await screen.findByLabelText("PRD label");
    const theme = screen.getByLabelText("Default theme") as HTMLSelectElement;
    // Loads at the current instance default.
    expect(theme.value).toBe("ember");
    fireEvent.change(theme, { target: { value: "mission" } });

    expect(saveButton().disabled).toBe(false);
    fireEvent.click(saveButton());

    await waitFor(() =>
      expect(mockApi.updateSettings).toHaveBeenCalledWith({
        prd_label: "PRD",
        autopilot_label: "autopilot",
        default_theme: "mission",
      }),
    );
    expect(await screen.findByText(/Settings saved/i)).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
  });

  it("surfaces a server validation error", async () => {
    mockApi.updateSettings.mockRejectedValue(new ApiError(400, "prd_label already in use"));
    renderPage();
    await screen.findByLabelText("PRD label");
    fireEvent.change(input("PRD label"), { target: { value: "Backlog" } });
    fireEvent.click(saveButton());

    expect(await screen.findByText("prd_label already in use")).toBeTruthy();
  });

  it("blocks equal labels client-side without calling the API", async () => {
    renderPage();
    await screen.findByLabelText("Autopilot label");
    fireEvent.change(input("Autopilot label"), { target: { value: "PRD" } });
    fireEvent.click(saveButton());

    // The danger Alert carries role="alert"; assert on it so the match is not
    // ambiguous with the section's help copy (which also mentions differing).
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/must differ/i);
    expect(mockApi.updateSettings).not.toHaveBeenCalled();
  });
});
