// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider } from "../auth/AuthContext";
import { AdminRoute, GuestRoute, ProtectedRoute } from "./RouteGuards";
import { api, ApiError } from "../lib/api";
import type { SessionResponse } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, me: vi.fn() } };
});

const mockApi = vi.mocked(api);

const session = {
  user: { id: "u1", email: "a@uzi.local", display_name: "A", is_admin: false },
  uzi_label: "uzi",
  autopilot_label: "autopilot",
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
} as unknown as SessionResponse;

function renderApp() {
  return render(
    <MemoryRouter initialEntries={["/dashboard"]}>
      <AuthProvider>
        <Routes>
          <Route
            path="/dashboard"
            element={
              <ProtectedRoute>
                <div>protected app</div>
              </ProtectedRoute>
            }
          />
          <Route path="/login" element={<div>login page</div>} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

function renderRoute(path: string, element: ReactNode) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AuthProvider>
        <Routes>
          <Route path={path} element={element} />
          <Route path="/login" element={<div>login page</div>} />
          <Route path="/dashboard" element={<div>dashboard page</div>} />
        </Routes>
      </AuthProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

describe("RouteGuards — server unreachable (#1991)", () => {
  it("shows a retry panel instead of redirecting to /login on a 503", async () => {
    mockApi.me.mockRejectedValue(new ApiError(503, "service unavailable"));
    renderApp();
    expect(await screen.findByRole("alert")).toBeTruthy();
    expect(screen.getByText("Can't reach the server")).toBeTruthy();
    expect(screen.queryByText("login page")).toBeNull();
    expect(screen.queryByText("protected app")).toBeNull();
  });

  it("Retry now recovers to the protected app once the server answers", async () => {
    mockApi.me.mockRejectedValueOnce(new ApiError(503, "service unavailable"));
    renderApp();
    const btn = await screen.findByRole("button", { name: "Retry now" });
    mockApi.me.mockResolvedValue(session);
    fireEvent.click(btn);
    expect(await screen.findByText("protected app")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("still redirects to /login on a 401", async () => {
    mockApi.me.mockRejectedValue(new ApiError(401, "unauthorized"));
    renderApp();
    expect(await screen.findByText("login page")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("auto-retries every 5s, chaining attempts, and recovers without a click", async () => {
    vi.useFakeTimers();
    mockApi.me
      .mockRejectedValueOnce(new ApiError(503, "service unavailable"))
      .mockRejectedValueOnce(new ApiError(503, "service unavailable"))
      .mockResolvedValue(session);
    renderApp();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("Can't reach the server")).toBeTruthy();
    expect(mockApi.me).toHaveBeenCalledTimes(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(4999);
    });
    expect(mockApi.me).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Can't reach the server")).toBeTruthy();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(mockApi.me).toHaveBeenCalledTimes(2);
    expect(screen.getByText("Can't reach the server")).toBeTruthy();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(mockApi.me).toHaveBeenCalledTimes(3);
    expect(screen.getByText("protected app")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a signed-in user is unaffected by a later 503 and nothing polls", async () => {
    vi.useFakeTimers();
    mockApi.me.mockResolvedValueOnce(session);
    renderApp();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("protected app")).toBeTruthy();
    expect(mockApi.me).toHaveBeenCalledTimes(1);

    mockApi.me.mockRejectedValue(new ApiError(503, "service unavailable"));
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(mockApi.me).toHaveBeenCalledTimes(2);
    expect(screen.getByText("protected app")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(mockApi.me).toHaveBeenCalledTimes(2);
    expect(screen.getByText("protected app")).toBeTruthy();
  });

  it("AdminRoute shows the panel on an initial 503", async () => {
    mockApi.me.mockRejectedValue(new ApiError(503, "service unavailable"));
    renderRoute(
      "/admin",
      <AdminRoute>
        <div>admin page</div>
      </AdminRoute>,
    );
    expect(await screen.findByText("Can't reach the server")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByText("login page")).toBeNull();
    expect(screen.queryByText("admin page")).toBeNull();
  });

  it("GuestRoute shows the panel on an initial 503", async () => {
    mockApi.me.mockRejectedValue(new ApiError(503, "service unavailable"));
    renderRoute(
      "/guest",
      <GuestRoute>
        <div>guest page</div>
      </GuestRoute>,
    );
    expect(await screen.findByText("Can't reach the server")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByText("guest page")).toBeNull();
    expect(screen.queryByText("dashboard page")).toBeNull();
  });
});
