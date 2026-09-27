// @vitest-environment jsdom
//
// PRD #1732 D14/D16: the run view's panel for a run held on credential_disabled. It names
// the disabled credential the run waits on and offers Enable; "Run with another token"
// appears ONLY for a lane that accepts a per-run override (never chat, Judge or
// self-improve, and never a Codex run).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CredentialDisabledPanel, heldCredential, type HeldBindings } from "./CredentialDisabledPanel";
import { api, type Run, type SecretMeta } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(),
      listWorkers: vi.fn(),
      me: vi.fn(),
      setSecretEnabled: vi.fn(),
      setRunCredential: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function secret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-main",
    kind: "anthropic_token",
    label: "main",
    is_default: true,
    enabled: true,
    disabled_at: null,
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

const laptop = secret({
  id: "sec-laptop",
  label: "old-laptop",
  is_default: false,
  enabled: false,
  disabled_at: "2026-09-18T00:00:00Z",
});

function heldRun(over: Partial<Run> = {}): Run {
  return {
    id: "run-1",
    kind: "issue",
    harness: "claude",
    status: "paused",
    hold_reason: "credential_disabled",
    worker_id: null,
    trigger_source: null,
    credential_override: { mode: "pinned", label: "old-laptop" },
    credential_switch: null,
    anthropic_secret_id: "sec-laptop",
    anthropic_select_reason: "run_pinned",
    codex_secret_id: null,
    ...over,
  } as unknown as Run;
}

function renderPanel(run: Run, canSteer = true, onChanged = vi.fn(), onEnabled = vi.fn()) {
  render(
    <MemoryRouter>
      <CredentialDisabledPanel run={run} canSteer={canSteer} onChanged={onChanged} onEnabled={onEnabled} />
    </MemoryRouter>,
  );
  return { onChanged, onEnabled };
}

const NO_BINDINGS: HeldBindings = { workerSecretId: null, judgeSecretId: null };

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [secret(), laptop] });
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.me.mockResolvedValue({
    user: { judge_anthropic_secret_id: null, judge_anthropic_bind_mode: "default" },
  } as unknown as Awaited<ReturnType<typeof api.me>>);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("CredentialDisabledPanel", () => {
  it("renders nothing unless the run is held on credential_disabled", () => {
    renderPanel(heldRun({ hold_reason: null }));
    expect(screen.queryByTestId("credential-disabled-panel")).toBeNull();
  });

  it("offers Enable <label> and Run with another token on an issue run", async () => {
    renderPanel(heldRun());
    expect(await screen.findByRole("button", { name: "Enable old-laptop" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Run with another token" })).toBeTruthy();
    expect(screen.queryByTestId("no-reassign-note")).toBeNull();
  });

  it.each(["chat", "judge", "self_improve"] as const)(
    "offers no reassignment action on a %s run, only Enable and Settings",
    async (kind) => {
      // These lanes spend the default (the judge token is unbound), so they wait only when
      // the slot has no default: every token disabled.
      mockApi.listSecrets.mockResolvedValue({ secrets: [laptop] });
      renderPanel(heldRun({ kind, credential_override: null }));
      expect(await screen.findByRole("button", { name: "Enable old-laptop" })).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Run with another token" })).toBeNull();
      expect(screen.queryByRole("button", { name: /Switch token/ })).toBeNull();
      expect(screen.getByTestId("no-reassign-note").textContent).toMatch(/can’t switch to another token/);
      expect(screen.getByRole("link", { name: "Manage tokens in Settings" }).getAttribute("href")).toBe("/settings");
    },
  );

  it("offers Enable only on a Codex run held on its frozen alias", async () => {
    const alias = secret({
      id: "cdx-laptop",
      kind: "codex_auth",
      label: "team-codex-laptop",
      is_default: false,
      enabled: false,
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: [secret(), alias] });
    renderPanel(heldRun({ harness: "codex", credential_override: null, anthropic_secret_id: null, codex_secret_id: "cdx-laptop" }));
    expect(await screen.findByRole("button", { name: "Enable team-codex-laptop" })).toBeTruthy();
    // No reassignment of any kind: the Codex run's lane (an issue run) WOULD accept an
    // Anthropic override, so only the harness check keeps these away.
    expect(screen.queryByRole("button", { name: "Run with another token" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Switch token/ })).toBeNull();
    const note = screen.getByTestId("no-reassign-note").textContent ?? "";
    expect(note).toMatch(/Enable it, or cancel the run/);
    // A Codex run is tied to its login: no default or binding change moves it.
    expect(note).not.toMatch(/default|binding/);
    expect(screen.getByTestId("credential-disabled-panel").textContent).toMatch(/tied to its Codex login “team-codex-laptop”/);
  });

  it.each(["judge", "self_improve"] as const)("names the judge token on a held %s run", async (kind) => {
    const judgeTok = secret({ id: "sec-judge", label: "judge-key", is_default: false, enabled: false });
    mockApi.listSecrets.mockResolvedValue({ secrets: [secret(), laptop, judgeTok] });
    mockApi.me.mockResolvedValue({
      user: { judge_anthropic_secret_id: "sec-judge", judge_anthropic_bind_mode: "pinned" },
    } as unknown as Awaited<ReturnType<typeof api.me>>);
    renderPanel(heldRun({ kind, credential_override: null }));
    expect(await screen.findByRole("button", { name: "Enable judge-key" })).toBeTruthy();
    expect(screen.getByTestId("no-reassign-note").textContent).toMatch(/judge token or default/);
  });

  it("enables the credential, reports it to the run view and refreshes the run", async () => {
    mockApi.setSecretEnabled.mockResolvedValue({ secret: { ...laptop, enabled: true, disabled_at: null } });
    const { onChanged, onEnabled } = renderPanel(heldRun());
    const btn = await screen.findByRole("button", { name: "Enable old-laptop" });
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(mockApi.setSecretEnabled).toHaveBeenCalledWith("anthropic_token", "sec-laptop", true);
    expect(onEnabled).toHaveBeenCalledWith("old-laptop");
    expect(onChanged).toHaveBeenCalled();
    // The announcement is the run view's (its region outlives this panel), not a
    // live region of the panel's own.
    const panel = screen.getByTestId("credential-disabled-panel");
    expect(panel.querySelector("[role=status]")).toBeNull();
    expect(screen.getByTestId("credential-enabled-note").textContent).toMatch(/Enabled “old-laptop”/);
    expect(screen.queryByRole("button", { name: "Enable old-laptop" })).toBeNull();
  });

  it("shows inert text, not buttons, to a non-owner", () => {
    renderPanel(heldRun(), false);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText(/Only the run’s owner can enable/)).toBeTruthy();
    expect(mockApi.listSecrets).not.toHaveBeenCalled();
  });
});

describe("heldCredential", () => {
  const worker = secret({ id: "sec-w", label: "worker-bound", is_default: false, enabled: false });
  const judgeTok = secret({ id: "sec-judge", label: "judge-key", is_default: false, enabled: false });
  const all = [secret(), laptop, worker, judgeTok];

  it("prefers the override pin, then the worker binding; an enabled credential is never the answer", () => {
    expect(heldCredential(heldRun(), all, NO_BINDINGS)?.secret.id).toBe("sec-laptop");
    expect(
      heldCredential(heldRun({ credential_override: null, anthropic_secret_id: null }), all, {
        ...NO_BINDINGS,
        workerSecretId: "sec-w",
      })?.secret.id,
    ).toBe("sec-w");
    // The default lane with an enabled default: nothing disabled to wait on.
    expect(
      heldCredential(heldRun({ credential_override: null, anthropic_secret_id: "sec-main" }), all, NO_BINDINGS),
    ).toBeNull();
  });

  it("returns a Codex run's frozen alias", () => {
    const alias = secret({ id: "cdx", kind: "codex_auth", label: "cdx", is_default: false, enabled: false });
    expect(
      heldCredential(heldRun({ harness: "codex", codex_secret_id: "cdx" }), [...all, alias], NO_BINDINGS)?.secret.id,
    ).toBe("cdx");
  });

  // Server order (claimSecretID): the Judge and self-improve resolve from the owner's judge
  // binding FIRST, so neither a worker binding nor the token the run last spent applies.
  it.each(["judge", "self_improve"] as const)("resolves a %s run from the judge binding", (kind) => {
    const run = heldRun({ kind, credential_override: null, anthropic_secret_id: "sec-laptop" });
    expect(heldCredential(run, all, { workerSecretId: "sec-w", judgeSecretId: "sec-judge" })?.secret.id).toBe(
      "sec-judge",
    );
    // Unbound judge = the default, which is enabled here: nothing to wait on.
    expect(heldCredential(run, all, { workerSecretId: "sec-w", judgeSecretId: null })).toBeNull();
  });

  // runOverrideChoice applies the override BEFORE the worker binding: "default" and "auto"
  // decide the credential themselves and never fall through to the worker's pin.
  it.each(["default", "auto"])("never falls through to the worker binding under a %s override", (mode) => {
    const run = heldRun({ credential_override: { mode, label: null }, anthropic_secret_id: "sec-main" });
    expect(heldCredential(run, all, { ...NO_BINDINGS, workerSecretId: "sec-w" })).toBeNull();
  });

  it("offers the last-spent token when a default-lane run's slot has no default at all", () => {
    const allOff = [laptop, worker];
    const run = heldRun({ kind: "chat", credential_override: null, anthropic_secret_id: "sec-laptop" });
    expect(heldCredential(run, allOff, NO_BINDINGS)).toEqual({ secret: laptop, viaDefault: true });
  });

  // The override's label is a SNAPSHOT: a rename, or a label reused by another token, must
  // not redirect Enable. The run's own pinned claim carries the id.
  it("matches a pinned override by id when the run's last claim spent the pin", () => {
    const renamed = { ...laptop, label: "renamed-laptop" };
    const reused = secret({ id: "sec-other", label: "old-laptop", is_default: false, enabled: false });
    const run = heldRun({
      credential_override: { mode: "pinned", label: "old-laptop" },
      anthropic_secret_id: "sec-laptop",
      anthropic_select_reason: "run_pinned",
    });
    expect(heldCredential(run, [secret(), renamed, reused], NO_BINDINGS)?.secret.id).toBe("sec-laptop");
    // Without an id to go on, the snapshot label is the fallback.
    expect(
      heldCredential({ ...run, anthropic_select_reason: "pinned" }, [secret(), renamed, reused], NO_BINDINGS)?.secret.id,
    ).toBe("sec-other");
  });
});
