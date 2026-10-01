// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { Connect } from "./Connect";
import { api, ApiError, type OAuthAuthorizeRequest, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { consumePendingReturn, setPendingReturn } from "../lib/pendingReturn";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getOAuthRequest: vi.fn(),
      approveOAuthRequest: vi.fn(),
      denyOAuthRequest: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const ID = "6f0e4b0a-1c2d-4e3f-8a9b-0c1d2e3f4a5b";
const SERVER_REDIRECT = "https://p.example/cb?code=server-code&iss=https%3A%2F%2Fuzi.example&state=s1";
const SERVER_DENY = "https://p.example/cb?error=access_denied&iss=https%3A%2F%2Fuzi.example&state=s1";

function signedIn(): void {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1", email: "vlad@uzi.local", is_admin: false } as User,
    loading: false,
  } as unknown as ReturnType<typeof useAuth>);
}
function signedOut(): void {
  vi.mocked(useAuth).mockReturnValue({ user: null, loading: false } as unknown as ReturnType<typeof useAuth>);
}

const pending = (over: Partial<OAuthAuthorizeRequest> = {}): OAuthAuthorizeRequest => ({
  product_name: "Acme Planner",
  product_description: "Plans work and files jobs.",
  redirect_host: "p.example",
  scopes: ["jobs:run", "jobs:read"],
  status: "pending",
  expires_at: new Date(Date.now() + 5 * 60_000).toISOString(),
  ...over,
});

function LoginProbe() {
  const loc = useLocation();
  return <div>LOGIN PAGE {loc.search}</div>;
}

function renderPage(entry = `/connect?request=${ID}`) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/connect" element={<Connect />} />
        <Route path="/login" element={<LoginProbe />} />
      </Routes>
    </MemoryRouter>,
  );
}

const assign = vi.fn();
const realLocation = window.location;

beforeEach(() => {
  signedIn();
  window.sessionStorage.clear();
  mockApi.getOAuthRequest.mockResolvedValue(pending());
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...realLocation, assign },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

describe("Connect renders the request as plain text", () => {
  it("shows the product, redirect host, plain-words scopes and the fixed automation line", async () => {
    renderPage();
    expect(await screen.findByText("Acme Planner")).toBeTruthy();
    expect(screen.getByText("Plans work and files jobs.")).toBeTruthy();
    expect(screen.getByText("p.example")).toBeTruthy();
    expect(screen.getByText("Run jobs")).toBeTruthy();
    expect(screen.getByText("Read jobs and their results")).toBeTruthy();
    expect(
      screen.getByText(
        "This product may run jobs automatically on your behalf, as you, using your model credential configured in uzi.",
      ),
    ).toBeTruthy();
    expect(mockApi.getOAuthRequest).toHaveBeenCalledWith(ID);
  });

  it("renders markup in the name and description as text, never as elements", async () => {
    mockApi.getOAuthRequest.mockResolvedValue(
      pending({
        product_name: "<b>x</b>Evil",
        product_description: '<img src=x onerror="alert(1)"> **bold** [link](https://evil.example)',
      }),
    );
    const { container } = renderPage();
    expect(await screen.findByText("<b>x</b>Evil")).toBeTruthy();
    expect(screen.getByText(/<img src=x onerror="alert\(1\)"> \*\*bold\*\* \[link\]\(https:\/\/evil\.example\)/)).toBeTruthy();
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("strong")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
  });

  it("shows an unknown scope as its raw text rather than hiding it", async () => {
    mockApi.getOAuthRequest.mockResolvedValue(pending({ scopes: ["jobs:run", "jobs:delete" as never] }));
    renderPage();
    expect(await screen.findByText("jobs:delete")).toBeTruthy();
  });
});

describe("Connect approve and deny navigate only to the server-built URL", () => {
  it("approve: assigns the server's redirect_url even when the page query carries a redirect_uri", async () => {
    mockApi.approveOAuthRequest.mockResolvedValue({ redirect_url: SERVER_REDIRECT });
    renderPage(
      `/connect?request=${ID}&redirect_uri=${encodeURIComponent("https://evil.example/steal")}&next=//evil.example&state=forged`,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await waitFor(() => expect(assign).toHaveBeenCalledTimes(1));
    expect(assign).toHaveBeenCalledWith(SERVER_REDIRECT);
    expect(mockApi.approveOAuthRequest).toHaveBeenCalledWith(ID);
  });

  it("deny: assigns the server's redirect_url", async () => {
    mockApi.denyOAuthRequest.mockResolvedValue({ redirect_url: SERVER_DENY });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(SERVER_DENY));
    expect(mockApi.approveOAuthRequest).not.toHaveBeenCalled();
  });

  it("is built from buttons: no form element and no submit button", async () => {
    const { container } = renderPage();
    await screen.findByRole("button", { name: "Approve" });
    expect(container.querySelector("form")).toBeNull();
    expect(container.querySelector('button[type="submit"]')).toBeNull();
    for (const b of container.querySelectorAll("button")) expect(b.getAttribute("type")).toBe("button");
  });

  it("does not navigate when approve fails, and says why", async () => {
    mockApi.approveOAuthRequest.mockRejectedValue(new ApiError(409, "request is no longer pending"));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/already decided/)).toBeTruthy();
    expect(assign).not.toHaveBeenCalled();
    // The buttons come back so the user can leave or retry.
    expect(screen.getByRole("button", { name: "Approve" })).toHaveProperty("disabled", false);
  });
});

describe("Connect error states", () => {
  it("a 404 (expired, used or another browser) says to start again", async () => {
    mockApi.getOAuthRequest.mockRejectedValue(new ApiError(404, "request not found"));
    renderPage();
    expect(await screen.findByText(/expired, was already used, or was started in a different browser/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("a missing ?request= is explained", async () => {
    renderPage("/connect");
    expect(await screen.findByText(/missing its request/)).toBeTruthy();
    expect(mockApi.getOAuthRequest).not.toHaveBeenCalled();
  });

  it("a non-pending request offers no buttons", async () => {
    mockApi.getOAuthRequest.mockResolvedValue(pending({ status: "approved" }));
    renderPage();
    expect(await screen.findByText(/already approved/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
  });
});

describe("Connect and the login return", () => {
  it("signed out: redirects to /login with the consent path as ?next= and stores the pending return", async () => {
    signedOut();
    renderPage();
    const probe = await screen.findByText(/LOGIN PAGE/);
    expect(probe.textContent).toContain(`next=${encodeURIComponent(`/connect?request=${ID}`)}`);
    await waitFor(() => expect(consumePendingReturn()).toBe(`/connect?request=${ID}`));
    expect(mockApi.getOAuthRequest).not.toHaveBeenCalled();
  });

  it("signed out with a request id that is not a uuid stores nothing", async () => {
    signedOut();
    renderPage("/connect?request=not-a-uuid");
    await screen.findByText(/LOGIN PAGE/);
    expect(consumePendingReturn()).toBeNull();
  });

  it("clears a stale pending-return entry on load, so AppShell cannot navigate here again", async () => {
    setPendingReturn(`/connect?request=${ID}`);
    renderPage();
    await screen.findByText("Acme Planner");
    expect(consumePendingReturn()).toBeNull();
  });
});
