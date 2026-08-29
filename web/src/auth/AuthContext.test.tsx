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
  theme: "ember",
  theme_override: null,
  default_theme: "ember",
  ...over,
});

// A tiny consumer that renders the uzi label so the test can assert on what the
// provider exposes (PRD #764).
function Probe() {
  const { uziLabel } = useAuth();
  return (
    <div>
      <span data-testid="uzi">{uziLabel}</span>
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
