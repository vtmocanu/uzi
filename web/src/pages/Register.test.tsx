// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Register } from "./Register";
import { api, type AuthConfig } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Register fetches /auth/config on mount and drives register() through useAuth.
// Mock both (keep the real ApiError export); each test sets the policy it needs.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { authConfig: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const registerFn = vi.fn();

function renderRegister() {
  return render(
    <MemoryRouter>
      <Register />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  registerFn.mockReset().mockResolvedValue(undefined);
  vi.mocked(useAuth).mockReturnValue({
    user: null,
    loading: false,
    register: registerFn,
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled: false,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const cfg = (over: Partial<AuthConfig> = {}): AuthConfig => ({
  registration_enabled: true,
  allowed_email_domains: [],
  ...over,
});

async function fillAndSubmit(container: HTMLElement, email: string, password: string) {
  const emailInput = container.querySelector('input[type="email"]') as HTMLInputElement;
  const pwInput = container.querySelector('input[type="password"]') as HTMLInputElement;
  fireEvent.change(emailInput, { target: { value: email } });
  fireEvent.change(pwInput, { target: { value: password } });
  fireEvent.submit(container.querySelector("form") as HTMLFormElement);
}

describe("Register policy gating", () => {
  it("replaces the form with a disabled notice when registration is off", async () => {
    mockApi.authConfig.mockResolvedValue(cfg({ registration_enabled: false }));
    const { container } = renderRegister();
    await screen.findByText("Registration is disabled");
    expect(screen.queryByText("Create your account")).toBeNull();
    expect(container.querySelector('input[type="email"]')).toBeNull();
  });

  it("hints the allowed domains under the email field", async () => {
    mockApi.authConfig.mockResolvedValue(cfg({ allowed_email_domains: ["example.com"] }));
    renderRegister();
    await screen.findByText("Create your account");
    expect(screen.getByText(/Only example\.com addresses may register/)).toBeTruthy();
  });

  it("rejects a disallowed domain client-side without calling register", async () => {
    mockApi.authConfig.mockResolvedValue(cfg({ allowed_email_domains: ["example.com"] }));
    const { container } = renderRegister();
    await screen.findByText("Create your account");
    await fillAndSubmit(container, "someone@gmail.com", "a-long-enough-password");
    expect(screen.getByText(/Registration is restricted to: example\.com/)).toBeTruthy();
    expect(registerFn).not.toHaveBeenCalled();
  });

  it("submits when the domain is allowed", async () => {
    mockApi.authConfig.mockResolvedValue(cfg({ allowed_email_domains: ["example.com"] }));
    const { container } = renderRegister();
    await screen.findByText("Create your account");
    await fillAndSubmit(container, "alice@example.com", "a-long-enough-password");
    expect(registerFn).toHaveBeenCalledWith("alice@example.com", "a-long-enough-password", "");
  });

  it("falls open (renders the form) when the config fetch fails", async () => {
    mockApi.authConfig.mockRejectedValue(new Error("config down"));
    renderRegister();
    await screen.findByText("Create your account");
    expect(screen.queryByText("Registration is disabled")).toBeNull();
  });
});
