// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppearanceSettings } from "./AppearanceSettings";
import { api, ApiError, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      // AppearanceSettings persists appearance through PUT /me/settings; that is the
      // only api method it calls, so the rest of the surface is left off the mock.
      putMySettings: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const refresh = vi.fn();

const baseUser: User = {
  id: "u1",
  email: "vlad@uzi.local",
  display_name: "Robin Diaz",
  is_admin: true,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default",
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

// A resolved AppearanceState fixture. Default: dark mode, hall/ember pair, system
// typeface, no per-field overrides (so it tracks the instance defaults). Tests pass
// `over` to exercise a different painted polarity or a synthesised-legacy shape.
const baseAppearance = (
  over: Partial<import("../lib/api").AppearanceState> = {},
): import("../lib/api").AppearanceState => ({
  mode: "dark",
  light_theme: "hall",
  dark_theme: "ember",
  typeface: "system",
  overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
  defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
  ...over,
});

// installMatchMedia stubs window.matchMedia (jsdom ships none) with a controllable
// (prefers-color-scheme: dark) query: `flip` changes `matches` and notifies the
// change listeners, modelling an OS light/dark switch. Returns { flip, restore }.
function installMatchMedia(initialDark: boolean) {
  const original = window.matchMedia;
  let dark = initialDark;
  const listeners = new Set<() => void>();
  const mql = {
    get matches() {
      return dark;
    },
    media: "(prefers-color-scheme: dark)",
    addEventListener: (_: string, cb: () => void) => listeners.add(cb),
    removeEventListener: (_: string, cb: () => void) => listeners.delete(cb),
  };
  window.matchMedia = (() => mql) as unknown as typeof window.matchMedia;
  return {
    flip(next: boolean) {
      dark = next;
      listeners.forEach((cb) => cb());
    },
    restore() {
      window.matchMedia = original;
    },
  };
}

function mockAuth(user: User, appearance = baseAppearance()) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh,
  });
}

beforeEach(() => {
  window.localStorage.clear();
  mockApi.putMySettings.mockResolvedValue({ settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, appearance_mode: null, light_theme: null, dark_theme: null, typeface: null, theme: "mission" } });
  mockAuth(baseUser);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.removeAttribute("data-font");
});

describe("AppearanceSettings — Appearance card (PRD #1167 'Lights on')", () => {
  const modeGroup = () => screen.getByRole("radiogroup", { name: "Appearance mode" });
  const lightGroup = () => screen.getByRole("radiogroup", { name: "Lights on theme" });
  const darkGroup = () => screen.getByRole("radiogroup", { name: "Lights off theme" });
  const typefaceGroup = () => screen.getByRole("radiogroup", { name: "Typeface" });

  it("renders the mode switch and both polarity theme pickers", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(modeGroup()).toBeTruthy());
    // Mode: System / Lights on / Lights off, with the dark (default) painted.
    expect(within(modeGroup()).getByRole("radio", { name: "Lights off" })).toBeTruthy();
    expect(
      (within(modeGroup()).getByRole("radio", { name: "Lights off" }) as HTMLInputElement).checked,
    ).toBe(true);
    // Lights-on picker offers the three light themes; lights-off the two dark ones.
    expect(within(lightGroup()).getByRole("radio", { name: "Dawn" })).toBeTruthy();
    expect(within(lightGroup()).getByRole("radio", { name: "Hall" })).toBeTruthy();
    expect(within(lightGroup()).getByRole("radio", { name: "Shadow" })).toBeTruthy();
    expect(within(darkGroup()).getByRole("radio", { name: "Ember" })).toBeTruthy();
    expect(within(darkGroup()).getByRole("radio", { name: "Mission control" })).toBeTruthy();
    // The resolved dark slot (ember) is the selected dark card.
    expect(
      (within(darkGroup()).getByRole("radio", { name: "Ember" }) as HTMLInputElement).checked,
    ).toBe(true);
  });

  it("dims the picker for the polarity that cannot currently apply", async () => {
    // Dark mode paints the dark picker, so the LIGHT picker is dimmed (not disabled).
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(lightGroup()).toBeTruthy());
    expect(lightGroup().className).toContain("opacity-60");
    expect(darkGroup().className).not.toContain("opacity-60");
    // Dimmed, but still operable: its radios are not disabled.
    expect(
      (within(lightGroup()).getByRole("radio", { name: "Dawn" }) as HTMLInputElement).disabled,
    ).toBe(false);
  });

  it("applies a dark-theme pick live (optimistic) and persists it", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(darkGroup()).toBeTruthy());
    fireEvent.click(within(darkGroup()).getByRole("radio", { name: "Mission control" }));
    // Optimistic: <html data-theme> flips immediately (dark mode -> dark slot), before
    // the request resolves.
    expect(document.documentElement.dataset.theme).toBe("mission");
    await waitFor(() =>
      expect(mockApi.putMySettings).toHaveBeenCalledWith({ dark_theme: "mission" }),
    );
    // Reconciled by a session refresh (syncs overrides + re-applies authoritative).
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("persists a mode change with the appearance_mode field", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(modeGroup()).toBeTruthy());
    fireEvent.click(within(modeGroup()).getByRole("radio", { name: "Lights on" }));
    await waitFor(() =>
      expect(mockApi.putMySettings).toHaveBeenCalledWith({ appearance_mode: "light" }),
    );
  });

  it("surfaces an error and refreshes (reverts) on a failed save", async () => {
    mockApi.putMySettings.mockRejectedValue(new ApiError(400, "unknown theme"));
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(darkGroup()).toBeTruthy());
    fireEvent.click(within(darkGroup()).getByRole("radio", { name: "Mission control" }));
    expect(await screen.findByText("unknown theme")).toBeTruthy();
    // refresh is the revert mechanism: re-fetch me() and re-apply the server truth.
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("enables the typeface control and persists a pick (PRD #1167 m6)", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(typefaceGroup()).toBeTruthy());
    // The control is now live (IBM Plex bundled in m6): its radios are enabled.
    expect(
      (within(typefaceGroup()).getByRole("radio", { name: "IBM Plex" }) as HTMLInputElement)
        .disabled,
    ).toBe(false);
    expect(
      (within(typefaceGroup()).getByRole("radio", { name: "System" }) as HTMLInputElement).disabled,
    ).toBe(false);
    // The "ships in a later step" hint is gone now that the family ships.
    expect(screen.queryByText(/IBM Plex ships in a later step/i)).toBeNull();
    // Picking Plex stamps <html data-font> optimistically and persists the typeface field.
    fireEvent.click(within(typefaceGroup()).getByRole("radio", { name: "IBM Plex" }));
    expect(document.documentElement.dataset.font).toBe("plex");
    await waitFor(() => expect(mockApi.putMySettings).toHaveBeenCalledWith({ typeface: "plex" }));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("'Use instance defaults' clears all four overrides", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /use instance defaults/i })).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: /use instance defaults/i }));
    await waitFor(() =>
      expect(mockApi.putMySettings).toHaveBeenCalledWith({
        appearance_mode: null,
        light_theme: null,
        dark_theme: null,
        typeface: null,
      }),
    );
  });

  it("gives each appearance option card a keyboard focus ring (WCAG 2.4.7)", async () => {
    // The radio input is sr-only, so the global input:focus-visible ring is clipped
    // and invisible; the visible ring must sit on the wrapping label card via
    // :has(:focus-visible). Assert the card class is present in every group so a
    // keyboard user gets a focus indicator (removing it reddens this).
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(modeGroup()).toBeTruthy());
    const cardOf = (radio: HTMLElement) => radio.closest("label");
    expect(cardOf(within(modeGroup()).getByRole("radio", { name: "System" }))?.className).toContain(
      "has-[:focus-visible]:outline",
    );
    expect(cardOf(within(darkGroup()).getByRole("radio", { name: "Ember" }))?.className).toContain(
      "has-[:focus-visible]:outline",
    );
    expect(
      cardOf(within(typefaceGroup()).getByRole("radio", { name: "IBM Plex" }))?.className,
    ).toContain("has-[:focus-visible]:outline");
  });

  it("updates the polarity hint live when the OS scheme flips under system mode", async () => {
    // System mode + OS light paints the light slot (hall); flipping the OS to dark
    // must re-render the "Showing…" hint to the dark slot (ember) without a manual
    // re-render — the reactive matchMedia hook, not a once-at-render sample.
    const mm = installMatchMedia(false);
    try {
      mockAuth(baseUser, baseAppearance({ mode: "system" }));
      render(
        <MemoryRouter>
          <AppearanceSettings />
        </MemoryRouter>,
      );
      const modeSection = () => modeGroup().parentElement as HTMLElement;
      await waitFor(() => expect(modeSection().textContent).toMatch(/Showing Hall \(light\)/));
      act(() => mm.flip(true));
      await waitFor(() => expect(modeSection().textContent).toMatch(/Showing Ember \(dark\)/));
    } finally {
      mm.restore();
    }
  });

  it("renders a legacy fixture (appearance synthesised from the deprecated trio)", async () => {
    // A pre-m5 server sends only the single-theme trio; AuthContext synthesises an
    // appearance (mode dark, hall light slot, the legacy dark theme in the dark slot,
    // no overrides). The card renders it just like a first-class appearance.
    mockAuth(
      baseUser,
      baseAppearance({
        dark_theme: "mission",
        defaults: { mode: "dark", light_theme: "hall", dark_theme: "mission", typeface: "system" },
      }),
    );
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    await waitFor(() => expect(darkGroup()).toBeTruthy());
    // The synthesised dark slot (mission) is the selected dark card and paints on load.
    expect(
      (within(darkGroup()).getByRole("radio", { name: "Mission control" }) as HTMLInputElement)
        .checked,
    ).toBe(true);
  });
});

describe("AppearanceSettings — Demo mode card (PRD #886)", () => {
  const toggle = () => screen.getByRole("switch", { name: "Demo mode" });

  it("renders the toggle Off and flips per-device demo mode on when clicked", async () => {
    render(
      <MemoryRouter>
        <AppearanceSettings />
      </MemoryRouter>,
    );
    // Starts Off (localStorage cleared): the switch is unchecked and the label reads Off.
    expect(toggle().getAttribute("aria-checked")).toBe("false");
    const row = toggle().parentElement as HTMLElement;
    expect(row.textContent).toContain("Demo mode: Off");

    fireEvent.click(toggle());

    // The switch is bound to useDemoMode/setDemoMode, so the click persists the flag to
    // localStorage and the live hook flips the switch + label without a manual re-render.
    await waitFor(() => expect(toggle().getAttribute("aria-checked")).toBe("true"));
    expect(row.textContent).toContain("Demo mode: On");
    expect(window.localStorage.getItem("uzi_demo_mode")).toBe("1");
  });
});
