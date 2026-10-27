// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { StrictMode } from "react";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AuthProvider, useAuth } from "./AuthContext";
import { ProtectedRoute } from "../components/RouteGuards";
import { api } from "../lib/api";
import { CredentialStorageHint, VaultSettings } from "../components/VaultSettings";
import { VaultLockedBanner } from "../components/VaultControls";

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
  const { user, loading, retry, refresh, vaultMode, vaultPolicy, vaultUnlocked, login, logout } = useAuth();
  return (
    <>
      <span data-testid="user">{user?.id ?? "none"}</span>
      <span data-testid="vault">{vaultMode}/{vaultPolicy}/{String(vaultUnlocked)}</span>
      <button onClick={() => void refresh(true)}>fresh session</button>
      <button onClick={() => void login("a@b.c", "pw")}>login</button>
      <button onClick={() => void logout()}>logout</button>
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


it("fresh vault refresh invalidates a pending session and fetches post-toggle mode", async () => {
  const resolves = deferredFetch();
  render(<AuthProvider><Probe /></AuthProvider>);
  await act(async () => { screen.getByText("fresh session").click(); });
  await act(async () => {
    resolves[0](new Response(JSON.stringify({ ...session, vault: { mode: "protected", policy: "required", unlocked: false } }), { status: 200 }));
  });
  expect(screen.getByTestId("user").textContent).toBe("none");
  expect(resolves).toHaveLength(2);
  await act(async () => {
    resolves[1](new Response(JSON.stringify({ ...session, vault: { mode: "unprotected", policy: "optional", unlocked: false } }), { status: 200 }));
  });
  expect(screen.getByTestId("vault").textContent).toBe("unprotected/optional/false");
  expect(screen.getByTestId("user").textContent).toBe("u1");
});

it("older servers retain protected/required defaults", async () => {
  const resolves = deferredFetch();
  render(<AuthProvider><Probe /></AuthProvider>);
  await act(async () => { resolves[0](new Response(JSON.stringify(session), { status: 200 })); });
  expect(screen.getByTestId("vault").textContent).toBe("protected/required/true");
});

function vaultSession(mode: "protected" | "unprotected", id = "u1") {
  return { ...session, user: { ...session.user, id }, has_password: true,
    vault: { mode, policy: "optional", unlocked: mode === "protected", exists: mode === "protected" } };
}
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

it("keeps the confirmed SSO wrapper after enable succeeds but metadata refresh fails", async () => {
  const resolves = deferredFetch();
  render(<AuthProvider><VaultSettings onChanged={async () => undefined} /><VaultLockedBanner /></AuthProvider>);
  await act(async () => {
    resolves[0](response({ ...vaultSession("unprotected"), has_password: false }));
  });
  fireEvent.click(screen.getByRole("button", { name: "Enable protection" }));
  fireEvent.change(screen.getByLabelText("New vault passphrase"), { target: { value: "chosen-passphrase" } });
  fireEvent.change(screen.getByLabelText("Confirm new vault passphrase"), { target: { value: "chosen-passphrase" } });
  await act(async () => { screen.getByRole("button", { name: "Confirm enable protection" }).click(); });
  await act(async () => { resolves[1](response({ mode: "protected" })); });
  await act(async () => { resolves[2](response({ error: "unavailable" }, 503)); });
  expect(screen.getByText(/vault\/session metadata could not be refreshed/)).toBeTruthy();
  expect(screen.getByLabelText("Vault passphrase")).toBeTruthy();
  expect(screen.queryByLabelText("Confirm vault passphrase")).toBeNull();
  expect(screen.queryByLabelText("New vault passphrase")).toBeNull();
});

function renderVault() {
  const reload = vi.fn().mockResolvedValue(undefined);
  render(<AuthProvider><Probe /><VaultSettings onChanged={reload} /><CredentialStorageHint /></AuthProvider>);
  return reload;
}

async function startToggle(mode: "protected" | "unprotected") {
  await act(async () => {
    if (mode === "protected") screen.getByRole("button", { name: "Disable protection" }).click();
    else {
      screen.getByRole("button", { name: "Enable protection" }).click();
    }
  });
  if (mode === "unprotected") {
    fireEvent.change(screen.getByLabelText("Verify login password"), { target: { value: "private-password" } });
    await act(async () => { screen.getByRole("button", { name: "Confirm enable protection" }).click(); });
  }
}

it.each(["protected", "unprotected"] as const)("reconciles a successful toggle from %s despite me503 and an older pending me", async mode => {
  const resolves = deferredFetch();
  const reload = renderVault();
  await act(async () => { resolves[0](response(vaultSession(mode))); });
  await act(async () => { screen.getByText("retry").click(); });
  await startToggle(mode);
  const nextMode = mode === "protected" ? "unprotected" : "protected";
  await act(async () => { resolves[2](response({ mode: nextMode })); });
  expect(resolves).toHaveLength(4);
  await act(async () => { resolves[3](response({ error: "unavailable" }, 503)); });
  expect(screen.getByText(/vault\/session metadata could not be refreshed/)).toBeTruthy();
  expect(screen.getByText(/metadata are unknown/)).toBeTruthy();
  expect(screen.getByTestId("vault").textContent).toBe(`${nextMode}/optional/false`);
  expect(reload).toHaveBeenCalledTimes(1);
  if (nextMode === "unprotected") {
    expect(screen.getByText("Vault off")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Enable protection" }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText(/Encrypted with the server key/)).toBeTruthy();
  } else {
    expect(screen.getByText("Vault locked")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Disable protection" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByText(/Encrypted with the server key/)).toBeNull();
  }
  expect(screen.queryByDisplayValue("private-password")).toBeNull();
  await act(async () => { resolves[1](response(vaultSession(mode))); });
  expect(screen.getByTestId("vault").textContent).toBe(`${nextMode}/optional/false`);
  // An ordinary caller still resolves on failure, and a later success clears uncertainty.
  await act(async () => { screen.getByText("retry").click(); });
  await act(async () => { resolves[4](response({ error: "unavailable" }, 503)); });
  await act(async () => { screen.getByText("retry").click(); });
  await act(async () => { resolves[5](response(vaultSession(nextMode))); });
  expect(screen.queryByText(/metadata are unknown/)).toBeNull();
});

it.each(["u1", "u2"])("rejects an old toggle after logout/login as %s", async id => {
  const resolves = deferredFetch();
  const reload = renderVault();
  await act(async () => { resolves[0](response(vaultSession("protected"))); });
  await startToggle("protected");
  await act(async () => { screen.getByText("logout").click(); });
  await act(async () => { resolves[2](response({ status: "ok" })); });
  await act(async () => { screen.getByText("login").click(); });
  await act(async () => { resolves[3](response(vaultSession("protected", id))); });
  await act(async () => { resolves[1](response({ mode: "unprotected" })); });
  expect(screen.getByTestId("user").textContent).toBe(id);
  expect(screen.getByTestId("vault").textContent).toBe("protected/optional/true");
  expect(screen.queryByText(/Protection is off/)).toBeNull();
  expect(reload).not.toHaveBeenCalled();
  expect(resolves).toHaveLength(4);
});
