// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { CliAuth } from "./CliAuth";
import { api, ApiError, type CliAuthRequestMeta, type User } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getCliAuthRequest: vi.fn(),
      approveCliAuth: vi.fn(),
      denyCliAuth: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function asUser(isAdmin: boolean, loading = false): void {
  vi.mocked(useAuth).mockReturnValue({
    user: loading ? null : ({ id: "u1", email: "vlad@uzi.local", is_admin: isAdmin } as User),
    loading,
  } as unknown as ReturnType<typeof useAuth>);
}

function signedOut(): void {
  vi.mocked(useAuth).mockReturnValue({
    user: null,
    loading: false,
  } as unknown as ReturnType<typeof useAuth>);
}

const pending = (over: Partial<CliAuthRequestMeta> = {}): CliAuthRequestMeta => ({
  client_desc: "uzi CLI on demo-laptop (darwin/arm64)",
  status: "pending",
  expires_at: new Date(Date.now() + 5 * 60_000).toISOString(),
  ...over,
});

// Render at /cli-auth?request=<id>. A Routes with /login lets the signed-out
// redirect be observed.
function renderPage(request: string | null = "req-1") {
  const entry = request === null ? "/cli-auth" : `/cli-auth?request=${request}`;
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <Routes>
        <Route path="/cli-auth" element={<CliAuth />} />
        <Route path="/login" element={<div>LOGIN PAGE</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  asUser(false);
  mockApi.getCliAuthRequest.mockResolvedValue(pending());
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("CliAuth requires a typed code (anti-phishing)", () => {
  it("renders an EMPTY code input (never pre-filled) and disables Approve until it is typed", async () => {
    renderPage();
    const input = (await screen.findByLabelText(/Enter the code shown in your terminal/)) as HTMLInputElement;
    // The server withholds the code; the page must not fill it in.
    expect(input.value).toBe("");
    expect(screen.getByRole("button", { name: "Approve" })).toHaveProperty("disabled", true);

    fireEvent.change(input, { target: { value: "ABCD-2345" } });
    expect(screen.getByRole("button", { name: "Approve" })).toHaveProperty("disabled", false);
  });

  it("names the requesting device from client_desc", async () => {
    renderPage();
    expect(await screen.findByText(/uzi CLI on demo-laptop/)).toBeTruthy();
  });
});

describe("CliAuth approve", () => {
  it("approves with the typed code and the chosen scope", async () => {
    mockApi.approveCliAuth.mockResolvedValue({ status: "approved" });
    renderPage("req-42");
    fireEvent.change(await screen.findByLabelText(/Enter the code/), { target: { value: "ABCD-2345" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));

    // Non-admin ⇒ scope pinned to user.
    await waitFor(() => expect(mockApi.approveCliAuth).toHaveBeenCalledWith("req-42", "ABCD-2345", "user"));
    expect(await screen.findByText(/Return to your terminal/)).toBeTruthy();
  });

  it("offers the admin scope picker only to admins", async () => {
    asUser(true);
    renderPage();
    await screen.findByLabelText(/Enter the code/);
    expect(screen.getByLabelText("Scope")).toBeTruthy();
    expect(screen.getByRole("option", { name: /Admin \(read-only\)/ })).toBeTruthy();

    cleanup();
    asUser(false);
    mockApi.getCliAuthRequest.mockResolvedValue(pending());
    renderPage();
    await screen.findByLabelText(/Enter the code/);
    expect(screen.queryByLabelText("Scope")).toBeNull();
  });
});

describe("CliAuth error codes", () => {
  it("keeps the form up on a 400 code mismatch with a retry message", async () => {
    mockApi.approveCliAuth.mockRejectedValue(new ApiError(400, "the code you entered does not match"));
    renderPage();
    fireEvent.change(await screen.findByLabelText(/Enter the code/), { target: { value: "0000-0000" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));

    expect(await screen.findByText(/does not match the one in your terminal/)).toBeTruthy();
    // Still on the form (recoverable) — the Approve button is present.
    expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy();
  });

  it("maps 403 (non-admin picked admin_ro) to an actionable message", async () => {
    asUser(true);
    mockApi.approveCliAuth.mockRejectedValue(new ApiError(403, "admin access required"));
    renderPage();
    fireEvent.change(await screen.findByLabelText(/Enter the code/), { target: { value: "ABCD-2345" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/not an administrator/)).toBeTruthy();
  });

  it("maps 409 not-pending and 410 expired to clear messages", async () => {
    mockApi.approveCliAuth.mockRejectedValueOnce(new ApiError(409, "no longer pending"));
    renderPage();
    fireEvent.change(await screen.findByLabelText(/Enter the code/), { target: { value: "ABCD-2345" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/no longer pending/)).toBeTruthy();

    cleanup();
    mockApi.getCliAuthRequest.mockResolvedValue(pending());
    mockApi.approveCliAuth.mockRejectedValue(new ApiError(410, "expired"));
    renderPage();
    fireEvent.change(await screen.findByLabelText(/Enter the code/), { target: { value: "ABCD-2345" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(await screen.findByText(/has expired/)).toBeTruthy();
  });

  it("shows a not-found message when the request is unknown (404 on load)", async () => {
    mockApi.getCliAuthRequest.mockRejectedValue(new ApiError(404, "request not found"));
    renderPage();
    expect(await screen.findByText(/was not found/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("shows a missing-request message and never calls the API when ?request= is absent", async () => {
    renderPage(null);
    expect(await screen.findByText(/missing its login request/)).toBeTruthy();
    expect(mockApi.getCliAuthRequest).not.toHaveBeenCalled();
  });
});

describe("CliAuth terminal states and deny", () => {
  it("renders a done message instead of the form when the request is already approved", async () => {
    mockApi.getCliAuthRequest.mockResolvedValue(pending({ status: "approved" }));
    renderPage();
    expect(await screen.findByText(/already approved/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("denies the request", async () => {
    mockApi.denyCliAuth.mockResolvedValue({ status: "denied" });
    renderPage("req-7");
    await screen.findByLabelText(/Enter the code/);
    fireEvent.click(screen.getByRole("button", { name: "Deny" }));

    await waitFor(() => expect(mockApi.denyCliAuth).toHaveBeenCalledWith("req-7"));
    expect(await screen.findByText(/No token was issued/)).toBeTruthy();
  });
});

describe("CliAuth auth gate", () => {
  it("redirects a signed-out user to login, preserving the request in ?next=", async () => {
    signedOut();
    renderPage("req-99");
    // Lands on the login route (the redirect target carries ?next=/cli-auth?request=req-99).
    expect(await screen.findByText("LOGIN PAGE")).toBeTruthy();
    expect(mockApi.getCliAuthRequest).not.toHaveBeenCalled();
  });
});
