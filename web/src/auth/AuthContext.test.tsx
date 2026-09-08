// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { AuthProvider, useAuth } from "./AuthContext";
import { api } from "../lib/api";
import type { SessionResponse } from "../lib/api";

// Only api.me is swapped; the un/vault-locked handler setters and everything else
// stay real so the provider's effects compose as they do in the app.
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, me: vi.fn() } };
});

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "vlad@uzi.local",
  display_name: "Robin Diaz",
  is_admin: true,
} as unknown as SessionResponse["user"];

const baseSession = (over: Partial<SessionResponse> = {}): SessionResponse => ({
  user,
  uzi_label: "uzi",
  autopilot_label: "autopilot",
  // PRD #1167 "Lights on" m2: the resolved appearance object (required on SessionResponse).
  // No overrides here, so it resolves to the instance defaults; the deprecated trio below
  // stays for the pre-m2 picker.
  appearance: {
    mode: "dark",
    light_theme: "hall",
    dark_theme: "ember",
    typeface: "system",
    overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
    defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
  },
  theme: "ember",
  theme_override: null,
  default_theme: "ember",
  ...over,
});

// A tiny consumer that renders the uzi label and the resolved appearance so the
// test can assert on what the provider exposes (PRD #764, PRD #1167).
function Probe() {
  const { uziLabel, appearance } = useAuth();
  return (
    <div>
      <span data-testid="uzi">{uziLabel}</span>
      <span data-testid="mode">{appearance.mode}</span>
      <span data-testid="dark">{appearance.dark_theme}</span>
      <span data-testid="override-dark">{String(appearance.overrides.dark_theme)}</span>
    </div>
  );
}

function renderProbe() {
  return render(
    <AuthProvider>
      <Probe />
    </AuthProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AuthContext — uzi label (PRD #764)", () => {
  it("exposes the session-delivered uzi label", async () => {
    mockApi.me.mockResolvedValue(baseSession({ uzi_label: "runnable" }));
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("uzi").textContent).toBe("runnable"));
  });

  it("falls back to the compiled-in default when an older server omits the field", async () => {
    // A server that predates uzi_label sends an empty value; the provider uses the
    // compiled-in DEFAULT_UZI_LABEL ("uzi").
    mockApi.me.mockResolvedValue(baseSession({ uzi_label: "" }));
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("uzi").textContent).toBe("uzi"));
  });
});

describe("AuthContext — appearance fallback for an older API (PRD #1167)", () => {
  // Build a session as an older (pre-#1167) server sends it: the resolved theme
  // trio, but NO `appearance` object.
  const olderSession = (over: Partial<SessionResponse> = {}): SessionResponse => {
    const s = baseSession(over);
    delete (s as Partial<SessionResponse>).appearance;
    return s;
  };

  it("honors the older server's resolved theme, not just the instance default", async () => {
    // The user saved `mission` on the old server (theme resolves to mission,
    // override = mission) while the instance default is ember. The new SPA must
    // paint mission, not repaint the user as the instance-default ember.
    mockApi.me.mockResolvedValue(
      olderSession({ theme: "mission", theme_override: "mission", default_theme: "ember" }),
    );
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("dark").textContent).toBe("mission"));
    expect(screen.getByTestId("mode").textContent).toBe("dark");
    expect(screen.getByTestId("override-dark").textContent).toBe("mission");
    // applyAppearance stamps <html data-theme> from the resolved appearance.
    expect(document.documentElement.dataset.theme).toBe("mission");
  });

  it("falls back to the compiled dark theme when the older server's theme is unusable", async () => {
    // A garbage resolved theme (or a light id the pre-#1167 world never produced)
    // must not be painted; it falls to the compiled ember dark slot.
    mockApi.me.mockResolvedValue(
      olderSession({ theme: "neon", theme_override: null, default_theme: "neon" }),
    );
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("dark").textContent).toBe("ember"));
    expect(screen.getByTestId("override-dark").textContent).toBe("null");
  });
});
