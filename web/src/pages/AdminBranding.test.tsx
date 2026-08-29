// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminBranding } from "./AdminBranding";
import { api, ApiError } from "../lib/api";
import type { AppSettings, Branding, SettingsResponse } from "../lib/api";

// Swap only the api object; ApiError/types stay real so the page's `instanceof
// ApiError` checks match what the mocked methods throw.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getSettings: vi.fn(),
      branding: vi.fn(),
      updateSettings: vi.fn(),
      uploadBrandingLogo: vi.fn(),
      deleteBrandingLogo: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

// A full AppSettings fixture; tests override the branding fields they exercise.
const settings = (over: Partial<AppSettings> = {}): AppSettings => ({
  prd_label: "PRD",
  autopilot_label: "autopilot",
  default_theme: "ember",
  prdless_enabled: "true",
  prdless_label: "PRDLESS",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  ephemeral_workers_enabled: "false",
  summary_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_slow_seconds: "2700",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
  capability_aware_scheduling: "true",
  github_project_sync_enabled: "false",
  run_eligible_labels: "PRD,bug",
  board_extra_labels: "bug",
  eligible_label_waives_prd_link: "true",
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
  ...over,
});

const response = (over: Partial<AppSettings> = {}): SettingsResponse => ({
  settings: settings(over),
  secrets: {},
  sources: {},
  slack_status: "disabled",
});

const branding = (over: Partial<Branding> = {}): Branding => ({
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_present: false,
  app_logo_keep_name: true,
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: false,
  brand_logo_present: false,
  ...over,
});

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/admin/branding"]}>
      <AdminBranding />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.getSettings.mockResolvedValue(response());
  mockApi.branding.mockResolvedValue(branding());
  mockApi.updateSettings.mockImplementation(async (payload) =>
    response(payload as Partial<AppSettings>),
  );
  mockApi.uploadBrandingLogo.mockResolvedValue(undefined);
  mockApi.deleteBrandingLogo.mockResolvedValue({ status: "ok" });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AdminBranding", () => {
  it("renders the default (unbranded) state with no upload controls", async () => {
    renderPage();
    // Mode selects default to their saved values.
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    expect((appMode as HTMLSelectElement).value).toBe("default");
    const brandMode = screen.getByLabelText("POWERED BY mode", { selector: "#brand-mode" });
    expect((brandMode as HTMLSelectElement).value).toBe("none");
    // Default app mode hides the upload input; none brand mode hides its controls.
    expect(screen.queryByLabelText("Upload app logo")).toBeNull();
    expect(screen.queryByLabelText("Upload brand logo")).toBeNull();
  });

  it("reveals the custom app-logo controls when custom mode is selected", async () => {
    renderPage();
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });
    expect(screen.getByLabelText("Upload app logo")).toBeTruthy();
    expect(
      screen.getByLabelText("Keep the app name next to the logo", { selector: "[role=switch]" }),
    ).toBeTruthy();
  });

  it("reveals the company input in text brand mode and the logo controls in logo mode", async () => {
    renderPage();
    const brandMode = await screen.findByLabelText("POWERED BY mode", { selector: "#brand-mode" });

    fireEvent.change(brandMode, { target: { value: "text" } });
    expect(screen.getByLabelText("Company name")).toBeTruthy();

    fireEvent.change(brandMode, { target: { value: "logo" } });
    expect(screen.getByLabelText("Upload brand logo")).toBeTruthy();
    expect(screen.getByLabelText("Placement")).toBeTruthy();
  });

  it("saves only the six branding keys with the edited values", async () => {
    renderPage();
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });

    const brandMode = screen.getByLabelText("POWERED BY mode", { selector: "#brand-mode" });
    fireEvent.change(brandMode, { target: { value: "text" } });
    fireEvent.change(screen.getByLabelText("Company name"), {
      target: { value: "Acme, Inc." },
    });

    fireEvent.click(screen.getByRole("button", { name: "Save branding" }));

    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalledTimes(1));
    const payload = mockApi.updateSettings.mock.calls[0][0];
    expect(payload).toEqual({
      app_logo_mode: "custom",
      app_logo_keep_name: "true",
      brand_mode: "text",
      brand_company: "Acme, Inc.",
      brand_placement: "below",
      brand_plaque: "false",
    });
    // No non-branding setting key leaks into the payload.
    expect(Object.keys(payload as object)).toHaveLength(6);
  });

  it("uploads a valid file to the app slot", async () => {
    renderPage();
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });

    const file = new File([new Uint8Array(1024)], "logo.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Upload app logo"), {
      target: { files: [file] },
    });

    await waitFor(() => expect(mockApi.uploadBrandingLogo).toHaveBeenCalledTimes(1));
    expect(mockApi.uploadBrandingLogo).toHaveBeenCalledWith("app", file);
    // On success the preview shows the uploaded asset via <img src=/api/branding/logo/app>.
    await waitFor(() => {
      const img = screen.getByAltText("app logo") as HTMLImageElement;
      expect(img.getAttribute("src")).toContain("/api/branding/logo/app");
    });
  });

  it("blocks an over-cap file client-side and never calls upload", async () => {
    renderPage();
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });

    // 262145 bytes = cap + 1.
    const big = new File([new Uint8Array(262145)], "big.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Upload app logo"), {
      target: { files: [big] },
    });

    await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/256 KiB/));
    expect(mockApi.uploadBrandingLogo).not.toHaveBeenCalled();
  });

  it("rejects a non-image type client-side and never calls upload", async () => {
    renderPage();
    const brandMode = await screen.findByLabelText("POWERED BY mode", { selector: "#brand-mode" });
    fireEvent.change(brandMode, { target: { value: "logo" } });

    const gif = new File([new Uint8Array(16)], "x.gif", { type: "image/gif" });
    fireEvent.change(screen.getByLabelText("Upload brand logo"), {
      target: { files: [gif] },
    });

    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/PNG, WebP or SVG/),
    );
    expect(mockApi.uploadBrandingLogo).not.toHaveBeenCalled();
  });

  it("removes an uploaded logo via the delete endpoint", async () => {
    mockApi.branding.mockResolvedValue(branding({ app_logo_present: true, app_logo_mode: "custom" }));
    renderPage();
    // Custom mode is loaded from saved settings; make it custom to show the remove button.
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });

    fireEvent.click(screen.getByRole("button", { name: "Remove logo" }));
    await waitFor(() => expect(mockApi.deleteBrandingLogo).toHaveBeenCalledWith("app"));
  });

  it("surfaces a server upload error", async () => {
    mockApi.uploadBrandingLogo.mockRejectedValue(new ApiError(400, "logo must be at most 262144 bytes"));
    renderPage();
    const appMode = await screen.findByLabelText("App logo mode", { selector: "#app-logo-mode" });
    fireEvent.change(appMode, { target: { value: "custom" } });

    const file = new File([new Uint8Array(1024)], "logo.png", { type: "image/png" });
    fireEvent.change(screen.getByLabelText("Upload app logo"), {
      target: { files: [file] },
    });
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toMatch(/262144 bytes/),
    );
  });
});
