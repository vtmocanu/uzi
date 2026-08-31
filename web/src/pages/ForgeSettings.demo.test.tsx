// @vitest-environment jsdom
//
// PRD #886 M5 — demo-mode masking of forge display fields (connection table).
//
// The connection row displays c.bot_username (maskUsername) and c.base_url (maskHost).
// Two-way: real values with demo OFF, reserved/fake values with demo ON, and the real
// host + bot username ABSENT with demo on (not merely that the mask is present).
//
// The `allowed_base_urls` here is a NEUTRAL host (gitlab.example.com), distinct from the
// connection's real base_url — that dropdown is input-bound and deliberately NOT masked
// (decision 1), so keeping it neutral means the only place the real host renders is the
// masked connection display, letting the "real string absent" assertion hold.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ForgeSettings } from "./ForgeSettings";
import { api, type ForgeConnection } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      forgeConfig: vi.fn(),
      listConnections: vi.fn(),
      createConnection: vi.fn(),
      verifyConnection: vi.fn(),
      updateConnection: vi.fn(),
      privilegeCheck: vi.fn(),
      deleteConnection: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

const REAL_HOST = "https://gitlab.metaminds.com";
const REAL_BOT = "realbot";

const connection: ForgeConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: REAL_HOST,
  bot_username: REAL_BOT,
  bot_forge_user_id: 1,
  human_username: null,
  created_at: "2026-01-01T00:00:00Z",
  last_verified_at: "2026-07-05T12:00:00Z",
  privilege_status: null,
  privilege_checked_at: null,
  privilege_report: null,
};

function renderPage() {
  return render(
    <MemoryRouter>
      <ForgeSettings />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  mockApi.forgeConfig.mockResolvedValue({
    // Neutral, input-bound (unmasked) host — must NOT contain the real host string.
    allowed_base_urls: ["https://gitlab.example.com"],
    forge_types: ["gitlab"],
  });
  mockApi.listConnections.mockResolvedValue({ connections: [connection] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.localStorage.clear();
});

describe("ForgeSettings — forge field masking (PRD #886 M5)", () => {
  it("demo OFF: renders the real forge host and bot username", async () => {
    renderPage();
    // Table cell text is exactly the host / bot username (field-label sentences that embed
    // the host are longer strings, so an exact-text match hits only the display cells).
    expect(await screen.findByText(REAL_HOST)).toBeTruthy();
    expect(screen.getByText(REAL_BOT)).toBeTruthy();
  });

  it("demo ON: renders the masked host + bot username and the real values are ABSENT", async () => {
    window.localStorage.setItem("uzi_demo_mode", "1");
    renderPage();

    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());

    // Reserved/fake masks render.
    expect(await screen.findByText("https://forge.example.com")).toBeTruthy();
    expect(screen.getByText("demo-bot")).toBeTruthy();

    // The real host and bot username must not survive anywhere in the rendered page.
    expect(screen.queryByText(REAL_HOST)).toBeNull();
    expect(screen.queryByText(REAL_BOT)).toBeNull();
  });
});
