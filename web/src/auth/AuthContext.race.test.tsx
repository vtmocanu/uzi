// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { StrictMode } from "react";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "./AuthContext";
import { ProtectedRoute } from "../components/RouteGuards";
import { api } from "../lib/api";

// These drive the REAL api.request() through a stubbed fetch, so the global 401
// handler that request() calls before rejecting runs exactly as in the app (#1991).

const session = {
  user: { id: "u1", email: "a@b.c" },
  uzi_label: "uzi",
  autopilot_label: "autopilot",
  theme: "ember",
  theme_override: null,
  default_theme: "ember",
};

function deferredFetch() {
  const resolves: Array<(r: Response) => void> = [];
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>((resolve) => resolves.push(resolve))));
  return resolves;
}

function Probe() {
  const { user, loading, retry } = useAuth();
  return (
    <>
      <span data-testid="user">{user?.id ?? "none"}</span>
      <span data-testid="loading">{String(loading)}</span>
      <button type="button" onClick={() => void retry()}>
        retry
      </button>
    </>
  );
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it("a successful retry after a real 401 probe signs the user in", async () => {
  const resolves = deferredFetch();
  render(
    <AuthProvider>
      <Probe />
    </AuthProvider>,
  );
  await act(async () => {
    resolves[0](new Response('{"error":"unauthorized"}', { status: 401 }));
  });
  expect(screen.getByTestId("user").textContent).toBe("none");
  await act(async () => {
    screen.getByText("retry").click();
  });
  expect(resolves).toHaveLength(2);
  await act(async () => {
    resolves[1](new Response(JSON.stringify(session), { status: 200 }));
  });
  expect(screen.getByTestId("loading").textContent).toBe("false");
  expect(screen.getByTestId("user").textContent).toBe("u1");
});

it("under StrictMode an initial 503 shows the retry panel, not /login, from one probe", async () => {
  const resolves = deferredFetch();
  render(
    <StrictMode>
      <MemoryRouter initialEntries={["/private"]}>
        <AuthProvider>
          <Routes>
            <Route
              path="/private"
              element={
                <ProtectedRoute>
                  <div>private</div>
                </ProtectedRoute>
              }
            />
            <Route path="/login" element={<div>login-page</div>} />
          </Routes>
        </AuthProvider>
      </MemoryRouter>
    </StrictMode>,
  );
  expect(resolves).toHaveLength(1);
  await act(async () => {
    resolves[0](new Response('{"error":"unavailable"}', { status: 503 }));
  });
  expect(screen.queryByText("login-page")).toBeNull();
  expect(screen.getByRole("alert")).toBeTruthy();
});

function LoginProbe() {
  const { user, login } = useAuth();
  return (
    <>
      <span data-testid="user">{user?.id ?? "none"}</span>
      <button type="button" onClick={() => void login("a@b.c", "pw")}>
        login
      </button>
    </>
  );
}

it("an older probe's real 401 cannot sign out a login that completed after it started", async () => {
  const resolves = deferredFetch();
  render(
    <AuthProvider>
      <LoginProbe />
    </AuthProvider>,
  );
  await act(async () => {
    screen.getByText("login").click();
  });
  expect(resolves).toHaveLength(2);
  await act(async () => {
    resolves[1](new Response(JSON.stringify(session), { status: 200 }));
  });
  expect(screen.getByTestId("user").textContent).toBe("u1");
  await act(async () => {
    resolves[0](new Response('{"error":"unauthorized"}', { status: 401 }));
  });
  expect(screen.getByTestId("user").textContent).toBe("u1");
});

it("another request's 401 stops a pending probe from restoring the session", async () => {
  const resolves = deferredFetch();
  render(
    <AuthProvider>
      <Probe />
    </AuthProvider>,
  );
  // A different authenticated request fails with 401 while the probe is pending.
  await act(async () => {
    const other = api.listRepos().catch(() => undefined);
    resolves[1](new Response('{"error":"unauthorized"}', { status: 401 }));
    await other;
  });
  await act(async () => {
    resolves[0](new Response(JSON.stringify(session), { status: 200 }));
  });
  expect(screen.getByTestId("user").textContent).toBe("none");
  expect(screen.getByTestId("loading").textContent).toBe("false");
});
