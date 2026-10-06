// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { AuthProvider, useAuth } from "../auth/AuthContext";
import { api, type User } from "../lib/api";
import { HostedWorkers } from "./HostedWorkers";

const user: User = {
  id: "u1", email: "alice@example.com", display_name: "Alice",
  is_admin: false, is_active: true, autopilot_enabled: false,
  judge_enabled: false, ci_autofix_enabled: null, attribution_enabled: true,
  ephemeral_workers_enabled: false, ephemeral_docker_enabled: false,
  wait_on_limit: false, notify_early_limit_reset: true,
  judge_anthropic_secret_id: null, judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default", created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};
const savedUser = { ...user, ephemeral_docker_enabled: true };
const session = {
  user, uzi_label: "ready", autopilot_label: "automatic",
  theme: "ember", theme_override: null, default_theme: "ember",
  vault: { unlocked: false, exists: false }, has_password: false,
  judge_enforced_by_admin: true, effective_judge_model: "judge-model",
};
const checkbox = () => screen.getByRole("checkbox", { name: "Docker-capable ephemeral workers" }) as HTMLInputElement;
const autoSwitch = () => screen.getByRole("switch", { name: "Auto-provision on demand" });

function AuthControls() {
  const { user, refresh, logout, login, loading, serverUnreachable, ...auth } = useAuth();
  return <>
    <output data-testid="identity">{user?.id ?? "none"}</output>
    <output data-testid="auth-metadata">{JSON.stringify({
      loading, serverUnreachable, uziLabel: auth.uziLabel,
      autopilotLabel: auth.autopilotLabel, appearance: auth.appearance,
      vaultUnlocked: auth.vaultUnlocked, vaultExists: auth.vaultExists,
      hasPassword: auth.hasPassword, judgeEnforcedByAdmin: auth.judgeEnforcedByAdmin,
      effectiveJudgeModel: auth.effectiveJudgeModel,
    })}</output>
    <button onClick={() => void refresh()}>probe</button>
    <button onClick={() => void logout()}>logout</button>
    <button onClick={() => void login("bob@example.com", "password")}>login</button>
    <button onClick={async () => {
      await login("bob@example.com", "password");
      void refresh();
      auth.updateUser(savedUser);
    }}>batched login probe old save</button>
    <button onClick={async () => {
      await logout();
      void refresh();
      auth.updateUser(savedUser);
    }}>batched logout probe old save</button>
  </>;
}

async function mount() {
  const responses: Array<(response: Response) => void> = [];
  const fetch = vi.fn(() => new Promise<Response>((resolve) => responses.push(resolve)));
  vi.stubGlobal("fetch", fetch);
  vi.spyOn(api, "hostedConfig").mockResolvedValue({
    enabled: true, quota: 0, ephemeral_enabled: true, docker_enabled: true,
  });
  let finishSave!: (result: { user: User }) => void;
  vi.spyOn(api, "setEphemeralWorkersEnabled").mockReturnValue(
    new Promise((resolve) => { finishSave = resolve; }),
  );
  render(<AuthProvider>
    <AuthControls />
    <HostedWorkers hostedCount={0} onProvisioned={() => {}} onShowWorkers={() => {}} />
  </AuthProvider>);
  await act(async () => { responses[0](new Response(JSON.stringify(session))); });
  expect(screen.getByTestId("identity").textContent).toBe("u1");
  return { responses, fetch, finishSave };
}

it.each(["login", "logout"] as const)(
  "rejected old identity save after %s must not invalidate the new session probe",
  async (action) => {
    const { responses, fetch, finishSave } = await mount();
    fireEvent.click(checkbox());
    fireEvent.click(screen.getByText(action));
    await act(async () => {
      responses[1](new Response(action === "logout" ? "{}" : JSON.stringify({
        ...session, user: { ...user, id: "u2", email: "bob@example.com" },
      })));
    });
    fireEvent.click(screen.getByText("probe"));
    expect(fetch).toHaveBeenCalledTimes(3);
    const metadata = screen.getByTestId("auth-metadata").textContent;
    await act(async () => { finishSave({ user: savedUser }); });
    expect(screen.getByTestId("identity").textContent).toBe(action === "logout" ? "none" : "u2");
    expect(screen.getByTestId("auth-metadata").textContent).toBe(metadata);
    await act(async () => {
      responses[2](new Response('{"error":"unauthorized"}', { status: 401 }));
    });
    expect(screen.getByTestId("identity").textContent).toBe("none");
    expect(JSON.parse(screen.getByTestId("auth-metadata").textContent!).serverUnreachable).toBe(false);
  },
);

it.each(["login", "logout"] as const)(
  "a rejected save cannot invalidate a probe batched with %s",
  async (action) => {
    const { responses, fetch } = await mount();
    fireEvent.click(screen.getByText(`batched ${action} probe old save`));
    await act(async () => {
      responses[1](new Response(action === "logout" ? "{}" : JSON.stringify({
        ...session, user: { ...user, id: "u2", email: "bob@example.com" },
      })));
    });
    expect(fetch).toHaveBeenCalledTimes(3);
    await act(async () => {
      responses[2](new Response(JSON.stringify({
        ...session, user: { ...user, id: "u2", email: "bob@example.com" },
        uzi_label: "current-probe",
      })));
    });
    expect(screen.getByTestId("identity").textContent).toBe("u2");
    expect(JSON.parse(screen.getByTestId("auth-metadata").textContent!).uziLabel).toBe("current-probe");
    expect(checkbox().checked).toBe(false);
  },
);

it.each([200, 503])(
  "a rejected old identity save preserves the new session probe's %s result",
  async (status) => {
    const { responses, finishSave } = await mount();
    fireEvent.click(checkbox());
    fireEvent.click(screen.getByText("login"));
    await act(async () => {
      responses[1](new Response(JSON.stringify({
        ...session, user: { ...user, id: "u2", email: "bob@example.com" },
      })));
    });
    fireEvent.click(screen.getByText("probe"));
    await act(async () => { finishSave({ user: savedUser }); });
    await act(async () => {
      responses[2](new Response(JSON.stringify(status === 200 ? {
        ...session, user: { ...user, id: "u2", email: "bob@example.com" },
        uzi_label: "current-probe",
      } : { error: "unavailable" }), { status }));
    });
    expect(screen.getByTestId("identity").textContent).toBe("u2");
    const metadata = JSON.parse(screen.getByTestId("auth-metadata").textContent!);
    expect(metadata.serverUnreachable).toBe(status === 503);
    expect(metadata.uziLabel).toBe(status === 200 ? "current-probe" : "ready");
    expect(checkbox().checked).toBe(false);
  },
);

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

it.each([200, 503, 401])("applies a successful save while an older probe is pending and ignores its late %s", async (status) => {
  const { responses, fetch, finishSave } = await mount();
  const metadata = screen.getByTestId("auth-metadata").textContent;
  fireEvent.click(screen.getByText("probe"));
  expect(fetch).toHaveBeenCalledTimes(2);
  fireEvent.click(checkbox());
  expect(api.setEphemeralWorkersEnabled).toHaveBeenCalledWith({ docker: true });
  expect(checkbox().checked).toBe(false);
  expect(checkbox().disabled).toBe(true);
  expect(autoSwitch().hasAttribute("disabled")).toBe(true);
  fireEvent.click(autoSwitch());
  expect(api.setEphemeralWorkersEnabled).toHaveBeenCalledTimes(1);
  await act(async () => { finishSave({ user: savedUser }); });
  // The saved response must update the UI and release controls before the probe settles.
  expect(checkbox().checked).toBe(true);
  expect(checkbox().disabled).toBe(false);
  expect(autoSwitch().hasAttribute("disabled")).toBe(false);
  expect(autoSwitch().getAttribute("aria-checked")).toBe("false");
  expect(screen.queryByRole("alert")).toBeNull();
  expect(fetch).toHaveBeenCalledTimes(2);
  expect(screen.getByTestId("auth-metadata").textContent).toBe(metadata);
  await act(async () => {
    responses[1](new Response(JSON.stringify(status === 200
      ? { ...session, uzi_label: "stale-label" } : { error: "probe failed" }), { status }));
  });
  expect(checkbox().checked).toBe(true);
  expect(screen.getByTestId("identity").textContent).toBe("u1");
  expect(screen.getByTestId("auth-metadata").textContent).toBe(metadata);
  expect(screen.queryByRole("alert")).toBeNull();
});

it.each(["logout", "login"] as const)("a save completing after %s cannot restore or replace the current identity", async (action) => {
  const { responses, finishSave } = await mount();
  fireEvent.click(checkbox());
  fireEvent.click(screen.getByText(action));
  await act(async () => {
    responses[1](new Response(action === "logout" ? "{}" : JSON.stringify({
      ...session, user: { ...user, id: "u2", email: "bob@example.com" },
    })));
  });
  const expectedIdentity = action === "logout" ? "none" : "u2";
  expect(screen.getByTestId("identity").textContent).toBe(expectedIdentity);
  const metadata = screen.getByTestId("auth-metadata").textContent;
  await act(async () => { finishSave({ user: savedUser }); });
  expect(screen.getByTestId("identity").textContent).toBe(expectedIdentity);
  expect(checkbox().checked).toBe(false);
  expect(checkbox().disabled).toBe(false);
  expect(screen.getByTestId("auth-metadata").textContent).toBe(metadata);
});
