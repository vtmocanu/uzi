// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CliTokens } from "./CliTokens";
import { api, type CliToken, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listCliTokens: vi.fn(),
      createCliToken: vi.fn(),
      revokeCliToken: vi.fn(),
      revokeAllCliTokens: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const NOW = Date.now();
const daysAgo = (d: number) => new Date(NOW - d * 86_400_000).toISOString();
const minsAgo = (m: number) => new Date(NOW - m * 60_000).toISOString();

function aToken(over: Partial<CliToken> = {}): CliToken {
  return {
    id: "t1",
    name: "laptop",
    token_prefix: "uzc_a1b2",
    scope: "user",
    revoked: false,
    created_at: daysAgo(10),
    last_used_at: minsAgo(9),
    last_used_ip: "192.168.1.24",
    expires_at: null,
    ...over,
  };
}

function asUser(isAdmin: boolean): void {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", is_admin: isAdmin } as User,
  } as unknown as ReturnType<typeof useAuth>);
}

function renderPage() {
  return render(
    <MemoryRouter>
      <CliTokens />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  asUser(true);
  mockApi.listCliTokens.mockResolvedValue({ tokens: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("CliTokens forensic surface (Risk 8)", () => {
  it("renders token_prefix, last_used_at and last_used_ip for each token", async () => {
    mockApi.listCliTokens.mockResolvedValue({
      tokens: [aToken({ token_prefix: "uzc_a1b2", last_used_ip: "192.168.1.24" })],
    });
    renderPage();

    expect(await screen.findByText("uzc_a1b2…")).toBeTruthy();
    // The IP is the only detection control — it must render, not be dropped as "metadata".
    expect(screen.getByText(/from 192\.168\.1\.24/)).toBeTruthy();
    expect(screen.getByText(/last used/)).toBeTruthy();
  });

  it("shows 'never used' and 'no IP recorded' for a token that has never been used", async () => {
    mockApi.listCliTokens.mockResolvedValue({
      tokens: [aToken({ last_used_at: null, last_used_ip: null })],
    });
    renderPage();
    expect(await screen.findByText(/never used/)).toBeTruthy();
    expect(screen.getByText(/no IP recorded/)).toBeTruthy();
  });

  it("flags a token unused for 90+ days as stale, and leaves a fresh one unflagged", async () => {
    mockApi.listCliTokens.mockResolvedValue({
      tokens: [
        aToken({ id: "fresh", name: "fresh", last_used_at: minsAgo(5) }),
        aToken({ id: "old", name: "old", last_used_at: daysAgo(120) }),
      ],
    });
    renderPage();
    await screen.findByText("fresh");
    // Exactly one "stale" badge — the 120-day-idle token, not the fresh one.
    expect(screen.getAllByText("stale")).toHaveLength(1);
  });

  it("renders a revoked token WITHOUT a Revoke button (the soft-deleted incident trail)", async () => {
    mockApi.listCliTokens.mockResolvedValue({
      tokens: [aToken({ id: "gone", name: "leaked", revoked: true })],
    });
    renderPage();
    expect(await screen.findByText("revoked")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
  });
});

describe("CliTokens scope picker is admin-only", () => {
  it("offers the scope picker to an admin", async () => {
    asUser(true);
    renderPage();
    await screen.findByRole("button", { name: "Create token" });
    expect(screen.getByLabelText("Scope")).toBeTruthy();
    expect(screen.getByRole("option", { name: /Admin \(read-only\)/ })).toBeTruthy();
  });

  it("hides the scope picker from a non-admin and mints a user token", async () => {
    asUser(false);
    mockApi.createCliToken.mockResolvedValue({
      token: "uzc_secretvalue",
      cli_token: aToken({ id: "new", name: "ci" }),
    });
    renderPage();

    await screen.findByRole("button", { name: "Create token" });
    expect(screen.queryByLabelText("Scope")).toBeNull();

    fireEvent.change(screen.getByPlaceholderText(/laptop, ci-runner/), { target: { value: "ci" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));
    // A non-admin can only ever mint a 'user' token.
    await waitFor(() => expect(mockApi.createCliToken).toHaveBeenCalledWith("ci", "user"));
  });
});

describe("CliTokens mint is show-once", () => {
  it("shows the plaintext token once with a copy affordance and a you-won't-see-it-again warning", async () => {
    mockApi.createCliToken.mockResolvedValue({
      token: "uzc_deadbeefdeadbeef",
      cli_token: aToken({ id: "new", name: "laptop" }),
    });
    renderPage();

    fireEvent.change(await screen.findByPlaceholderText(/laptop, ci-runner/), { target: { value: "laptop" } });
    fireEvent.click(screen.getByRole("button", { name: "Create token" }));

    expect(await screen.findByText("uzc_deadbeefdeadbeef")).toBeTruthy();
    expect(screen.getByText(/once and never again/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
  });
});

describe("CliTokens revoke", () => {
  it("revokes a single token and reloads", async () => {
    mockApi.listCliTokens.mockResolvedValue({ tokens: [aToken({ id: "t9", name: "laptop" })] });
    mockApi.revokeCliToken.mockResolvedValue(null);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(mockApi.revokeCliToken).toHaveBeenCalledWith("t9"));
  });

  it("Revoke all asks to confirm first, then revokes everything", async () => {
    mockApi.listCliTokens.mockResolvedValue({ tokens: [aToken({ id: "a" }), aToken({ id: "b", name: "ci" })] });
    mockApi.revokeAllCliTokens.mockResolvedValue(null);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Revoke all" }));
    // Does NOT revoke on the first click — it arms a confirmation.
    expect(mockApi.revokeAllCliTokens).not.toHaveBeenCalled();
    expect(screen.getByRole("group", { name: /Confirm revoking all CLI tokens/ })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Revoke all anyway" }));
    await waitFor(() => expect(mockApi.revokeAllCliTokens).toHaveBeenCalledTimes(1));
  });

  it("hides Revoke all when there is nothing active to revoke", async () => {
    mockApi.listCliTokens.mockResolvedValue({ tokens: [aToken({ id: "x", revoked: true })] });
    renderPage();
    await screen.findByText("revoked");
    expect(screen.queryByRole("button", { name: "Revoke all" })).toBeNull();
  });
});
