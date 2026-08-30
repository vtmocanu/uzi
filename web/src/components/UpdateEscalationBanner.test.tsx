// @vitest-environment jsdom
// Tests for the admin escalation banner (PRD #836 M6, surface 4). The component is
// admin-only and self-gating, fetches getReleaseCheck once on mount, and dismisses via
// a server-side snooze. Only `api` and `useAuth` are swapped; the Link needs a router,
// so renders are wrapped in a MemoryRouter.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { UpdateEscalationBanner } from "./UpdateEscalationBanner";
import { api } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getReleaseCheck: vi.fn(),
      snoozeReleaseBanner: vi.fn(),
    },
  };
});

// A mutable current user the mocked useAuth reads, so each test picks admin / member /
// signed-out without re-mocking the module.
let mockUser: { is_admin: boolean } | null = { is_admin: true };
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: mockUser }) }));

const mockApi = vi.mocked(api);

const releaseCheck = (
  over: Partial<import("../lib/api").ReleaseCheckStatus> = {},
): import("../lib/api").ReleaseCheckStatus => ({
  release_check_enabled: true,
  release_check_banner_enabled: true,
  interval: "6h",
  running_version: "0.4.2",
  latest_tag: "v0.5.0",
  latest_name: "Hosted worker drain controls",
  body: "### Added\n- stuff\n",
  notes_url: "https://github.com/vtmocanu/uzi/releases/tag/v0.5.0",
  published_at: "2026-08-27T00:00:00Z",
  checked_at: "2026-08-30T00:00:00Z",
  update_available: true,
  far_behind: true,
  security: false,
  banner_snoozed: false,
  status: "ok",
  ...over,
});

beforeEach(() => {
  mockUser = { is_admin: true };
  mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck() });
  mockApi.snoozeReleaseBanner.mockResolvedValue({ release_check: releaseCheck({ banner_snoozed: true }) });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderBanner() {
  return render(
    <MemoryRouter>
      <UpdateEscalationBanner />
    </MemoryRouter>,
  );
}

describe("UpdateEscalationBanner (PRD #836 M6)", () => {
  it("shows for an admin when banner enabled + far_behind + not snoozed + latest_tag present", async () => {
    renderBanner();
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain("A newer uzi (v0.5.0) is available");
    // Update guide is a client-side Link (internal route), not a raw anchor to /admin/settings.
    const guide = screen.getByRole("link", { name: "Update guide" }) as HTMLAnchorElement;
    expect(guide.getAttribute("href")).toBe("/admin/settings");
    // Release notes link out opens in a new tab with the safe rel.
    const notes = screen.getByRole("link", { name: /Release notes/ }) as HTMLAnchorElement;
    expect(notes.getAttribute("href")).toBe("https://github.com/vtmocanu/uzi/releases/tag/v0.5.0");
    expect(notes.getAttribute("target")).toBe("_blank");
    expect(notes.getAttribute("rel")).toBe("noopener noreferrer");
    // The decorative ↗ glyph is hidden from screen readers, and a visually-hidden cue
    // announces the new-tab behavior (so the accessible name carries it, not the glyph).
    const arrow = notes.querySelector('[aria-hidden="true"]');
    expect(arrow?.textContent).toBe("↗");
    expect(notes.querySelector(".sr-only")?.textContent).toBe("(opens in new tab)");
    // The Dismiss button's accessible name ties the action to the specific release.
    const dismiss = screen.getByRole("button", { name: /Dismiss/ });
    expect(dismiss.getAttribute("aria-label")).toBe("Dismiss update banner for v0.5.0");
  });

  it("renders nothing for a non-admin and makes NO api call", async () => {
    mockUser = { is_admin: false };
    renderBanner();
    // Give any stray effect a chance to fire, then assert no fetch and no banner.
    await new Promise((r) => setTimeout(r, 0));
    expect(mockApi.getReleaseCheck).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders nothing when the banner toggle is off", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck({ release_check_banner_enabled: false }) });
    renderBanner();
    await waitFor(() => expect(mockApi.getReleaseCheck).toHaveBeenCalled());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders nothing when the instance is not far behind (near-current)", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck({ far_behind: false }) });
    renderBanner();
    await waitFor(() => expect(mockApi.getReleaseCheck).toHaveBeenCalled());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders nothing when already snoozed for the current release", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck({ banner_snoozed: true }) });
    renderBanner();
    await waitFor(() => expect(mockApi.getReleaseCheck).toHaveBeenCalled());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("Dismiss snoozes and hides the banner, and it stays hidden (snooze persists per tag)", async () => {
    renderBanner();
    const dismiss = await screen.findByRole("button", { name: /Dismiss/ });
    fireEvent.click(dismiss);
    await waitFor(() => expect(mockApi.snoozeReleaseBanner).toHaveBeenCalledTimes(1));
    // Optimistic hide already took effect.
    expect(screen.queryByRole("alert")).toBeNull();
    // Flush the snooze-response refresh (banner_snoozed:true) and re-assert it stays hidden.
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("emphasizes a security release", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({ release_check: releaseCheck({ security: true }) });
    renderBanner();
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain("Security release v0.5.0 is available");
    expect(banner.textContent).toMatch(/security release/i);
  });

  it("shows for a security release even when NOT far behind (security arm)", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ security: true, far_behind: false, update_available: true }),
    });
    renderBanner();
    const banner = await screen.findByRole("alert");
    expect(banner.textContent).toContain("Security release v0.5.0 is available");
  });

  it("renders nothing for a security flag when no update is available (security ⇏ newer release)", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ security: true, far_behind: false, update_available: false }),
    });
    renderBanner();
    await waitFor(() => expect(mockApi.getReleaseCheck).toHaveBeenCalled());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("renders no Release notes link when notes_url is not HTTPS", async () => {
    mockApi.getReleaseCheck.mockResolvedValue({
      release_check: releaseCheck({ notes_url: "javascript:alert(1)" }),
    });
    renderBanner();
    // The banner still renders; only the non-HTTPS notes link is suppressed.
    await screen.findByRole("alert");
    expect(screen.queryByRole("link", { name: /Release notes/ })).toBeNull();
  });
});
