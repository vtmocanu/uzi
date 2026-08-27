// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Login, safeNextPath } from "./Login";
import { api, type AuthConfig } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Login fetches /auth/config on mount and drives login() through useAuth. Mock
// both (keep the real ApiError/MOCK_MODE exports); each test sets its own policy.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { authConfig: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function renderLogin(entry = "/login") {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Login />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    login: vi.fn().mockResolvedValue(undefined),
  } as unknown as ReturnType<typeof useAuth>);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const cfg = (over: Partial<AuthConfig> = {}): AuthConfig => ({
  registration_enabled: true,
  allowed_email_domains: [],
  password_login_enabled: true,
  oidc_enabled: false,
  oidc_provider_name: "SSO",
  ...over,
});

describe("Login SSO + gating", () => {
  it("shows the password form and no SSO button by default", async () => {
    mockApi.authConfig.mockResolvedValue(cfg());
    const { container } = renderLogin();
    await screen.findByRole("button", { name: /log in/i });
    expect(container.querySelector('input[type="password"]')).toBeTruthy();
    expect(screen.queryByText(/Sign in with/i)).toBeNull();
  });

  it("shows the SSO button linking to the login endpoint when OIDC is enabled", async () => {
    mockApi.authConfig.mockResolvedValue(cfg({ oidc_enabled: true, oidc_provider_name: "Keycloak" }));
    renderLogin();
    const link = await screen.findByRole("link", { name: /Sign in with Keycloak/i });
    expect(link.getAttribute("href")).toBe("/api/auth/oidc/login");
    // Password login is still available alongside SSO.
    expect(screen.getByRole("button", { name: /log in/i })).toBeTruthy();
  });

  it("hides the password form and register link in SSO-only mode", async () => {
    mockApi.authConfig.mockResolvedValue(
      cfg({ password_login_enabled: false, oidc_enabled: true, oidc_provider_name: "Keycloak" }),
    );
    const { container } = renderLogin();
    await screen.findByRole("link", { name: /Sign in with Keycloak/i });
    expect(container.querySelector('input[type="password"]')).toBeNull();
    expect(screen.queryByRole("button", { name: /log in/i })).toBeNull();
    expect(screen.queryByRole("link", { name: /^register$/i })).toBeNull();
  });

  it("maps a known ?error= code to its message", async () => {
    mockApi.authConfig.mockResolvedValue(cfg());
    renderLogin("/login?error=oidc_forbidden");
    await screen.findByText(/isn't permitted to sign in here/i);
  });

  it("shows a generic message for an unknown ?error= code and never the raw value", async () => {
    mockApi.authConfig.mockResolvedValue(cfg());
    renderLogin("/login?error=totally-made-up-code");
    await screen.findByText(/Sign-in failed\. Please try again\./i);
    expect(screen.queryByText(/totally-made-up-code/)).toBeNull();
  });
});

describe("safeNextPath open-redirect guard", () => {
  it("keeps a same-origin internal path (query preserved)", () => {
    expect(safeNextPath("/cli-auth?request=abc")).toBe("/cli-auth?request=abc");
  });

  it("rejects a protocol-relative //host", () => {
    expect(safeNextPath("//evil.com")).toBe("/dashboard");
  });

  it("rejects an absolute URL", () => {
    expect(safeNextPath("https://evil.com")).toBe("/dashboard");
  });

  it("rejects the backslash vector /\\evil.com (window.location normalizes \\ to /)", () => {
    expect(safeNextPath("/\\evil.com")).toBe("/dashboard");
  });

  it("defaults to /dashboard for a null/absent next", () => {
    expect(safeNextPath(null)).toBe("/dashboard");
  });
});
