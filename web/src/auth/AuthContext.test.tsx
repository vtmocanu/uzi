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
  display_name: "Vlad",
  is_admin: true,
} as unknown as SessionResponse["user"];

const baseSession = (over: Partial<SessionResponse> = {}): SessionResponse => ({
  user,
  prd_label: "PRD",
  autopilot_label: "autopilot",
  theme: "ember",
  theme_override: null,
  default_theme: "ember",
  ...over,
});

// A tiny consumer that renders the two PRD #196 fields so the test can assert on
// what the provider exposes.
function Probe() {
  const { runEligibleLabels, eligibleLabelWaivesPrdLink } = useAuth();
  return (
    <div>
      <span data-testid="eligible">{runEligibleLabels.join(",")}</span>
      <span data-testid="waiver">{String(eligibleLabelWaivesPrdLink)}</span>
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

describe("AuthContext — run-eligibility fields (PRD #196 M2)", () => {
  it("exposes the session-delivered eligible set and waiver", async () => {
    mockApi.me.mockResolvedValue(
      baseSession({
        run_eligible_labels: ["PRD", "bug", "security"],
        eligible_label_waives_prd_link: false,
      }),
    );
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("eligible").textContent).toBe("PRD,bug,security"));
    // A bool: `?? true` must preserve an explicit false, not coerce it back on.
    expect(screen.getByTestId("waiver").textContent).toBe("false");
  });

  it("falls back to [prdLabel] and waiver=true when an older server omits the fields", async () => {
    mockApi.me.mockResolvedValue(baseSession({ prd_label: "Feature" }));
    renderProbe();
    await waitFor(() => expect(screen.getByTestId("eligible").textContent).toBe("Feature"));
    expect(screen.getByTestId("waiver").textContent).toBe("true");
  });
});
