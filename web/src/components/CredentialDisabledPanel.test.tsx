// @vitest-environment jsdom
//
// PRD #1732 D14/D16: the run view's panel for a run held on credential_disabled. It names
// the disabled credential the run waits on and offers Enable; "Run with another token"
// appears ONLY for a lane that accepts a per-run override (never chat, Judge or
// self-improve, and never a Codex run).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CredentialDisabledPanel, heldCredential } from "./CredentialDisabledPanel";
import { api, type Run, type SecretMeta } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(),
      listWorkers: vi.fn(),
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
    codex_secret_id: null,
    ...over,
  } as unknown as Run;
}

function renderPanel(run: Run, canSteer = true, onChanged = vi.fn()) {
  render(
    <MemoryRouter>
      <CredentialDisabledPanel run={run} canSteer={canSteer} onChanged={onChanged} />
    </MemoryRouter>,
  );
  return { onChanged };
}

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [secret(), laptop] });
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
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
    expect(screen.queryByRole("button", { name: "Run with another token" })).toBeNull();
  });

  it("enables the credential and refreshes the run", async () => {
    mockApi.setSecretEnabled.mockResolvedValue({ secret: { ...laptop, enabled: true, disabled_at: null } });
    const { onChanged } = renderPanel(heldRun());
    const btn = await screen.findByRole("button", { name: "Enable old-laptop" });
    await act(async () => {
      fireEvent.click(btn);
    });
    expect(mockApi.setSecretEnabled).toHaveBeenCalledWith("anthropic_token", "sec-laptop", true);
    expect(onChanged).toHaveBeenCalled();
    expect(screen.getByRole("status").textContent).toMatch(/Enabled “old-laptop”/);
  });

  it("shows inert text, not buttons, to a non-owner", () => {
    renderPanel(heldRun(), false);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText(/Only the run’s owner can enable/)).toBeTruthy();
    expect(mockApi.listSecrets).not.toHaveBeenCalled();
  });
});

describe("heldCredential", () => {
  it("prefers a Codex run's frozen alias, then the override pin, then the worker binding, then the spent token", () => {
    const worker = secret({ id: "sec-w", label: "worker-bound", is_default: false, enabled: false });
    const all = [secret(), laptop, worker];
    expect(heldCredential(heldRun(), all, null)?.id).toBe("sec-laptop");
    expect(heldCredential(heldRun({ credential_override: null, anthropic_secret_id: null }), all, "sec-w")?.id).toBe(
      "sec-w",
    );
    // An enabled credential is never the answer: the default-slot case resolves to none.
    expect(heldCredential(heldRun({ credential_override: null, anthropic_secret_id: "sec-main" }), all, null)).toBeNull();
  });
});
