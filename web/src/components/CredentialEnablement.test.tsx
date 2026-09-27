// @vitest-environment jsdom
//
// PRD #1732 M5: Disable / Enable on the Settings credential cards. Driven through the real
// cards (AnthropicTokens, CodexCredentials) so the shelf, the dialog and the notice are
// tested where the owner meets them.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { MemoryRouter } from "react-router-dom";
import { AnthropicTokens } from "./AnthropicTokens";
import { CodexCredentials } from "./CodexCredentials";
import { api, type SecretDependents, type SecretMeta } from "../lib/api";
import { ApiError } from "../lib/apiError";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listWorkers: vi.fn(),
      getMyRateLimits: vi.fn(),
      getMyCodexRateLimits: vi.fn(),
      setSecretEnabled: vi.fn(),
      getSecretDependents: vi.fn(),
      deleteAnthropicTokenById: vi.fn(),
      patchAnthropicToken: vi.fn(),
      setTokenAutoEligible: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);
const SHELF_KEY = "uzi.settings.disabledTokensOpen";

function tok(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "main",
    is_default: true,
    enabled: true,
    disabled_at: null,
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    ...over,
  };
}

const off = (over: Partial<SecretMeta> = {}) =>
  tok({ id: "sec-off", label: "old-laptop", is_default: false, enabled: false, disabled_at: "2026-09-18T10:00:00Z", ...over });

function deps(over: Partial<SecretDependents> = {}): SecretDependents {
  const empty = { items: [], total: 0 };
  return {
    default: false,
    judge: false,
    workers: empty,
    schedules: empty,
    runs: empty,
    enabled_siblings: empty,
    ...over,
  };
}

function renderTokens(secrets: SecretMeta[], reload = vi.fn(async () => {})) {
  render(
    <MemoryRouter>
      <AnthropicTokens
        secrets={secrets}
        loading={false}
        busy={false}
        reload={reload}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
        sidebarTokenIds={[]}
        onToggleSidebarToken={async () => {}}
      />
    </MemoryRouter>,
  );
  return { reload };
}

beforeEach(() => {
  window.localStorage.clear();
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.getMyRateLimits.mockResolvedValue({ tokens: [] });
  mockApi.getMyCodexRateLimits.mockResolvedValue({ accounts: [] });
  mockApi.getSecretDependents.mockResolvedValue(deps());
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("Disabled (n) shelf", () => {
  it("is collapsed by default: a real button with aria-expanded=false, disabled rows not drawn", () => {
    renderTokens([tok(), off()]);
    const toggle = screen.getByRole("button", { name: "Disabled (1)" });
    expect(toggle.tagName).toBe("BUTTON");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("old-laptop")).toBeNull();
    // The live list still shows the enabled token.
    expect(screen.getByText("main")).toBeTruthy();
  });

  it("expands on click, shows 'Disabled since', Enable and Delete, and remembers the choice", () => {
    renderTokens([tok(), off()]);
    fireEvent.click(screen.getByRole("button", { name: "Disabled (1)" }));
    const toggle = screen.getByRole("button", { name: "Disabled (1)" });
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const row = screen.getByTestId("token-sec-off");
    expect(row.textContent).toMatch(/Disabled since/);
    expect(within(row).getByRole("button", { name: "Enable: old-laptop" })).toBeTruthy();
    expect(within(row).getByRole("button", { name: "Delete old-laptop" })).toBeTruthy();
    // Auto-select and sidebar checkboxes are hidden (not cleared) on a disabled row.
    expect(within(row).queryByRole("checkbox")).toBeNull();
    expect(window.localStorage.getItem(SHELF_KEY)).toBe("true");

    cleanup();
    renderTokens([tok(), off()]);
    expect(screen.getByRole("button", { name: "Disabled (1)" }).getAttribute("aria-expanded")).toBe("true");
  });

  it("survives a localStorage that throws (collapsed, no crash)", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    renderTokens([tok(), off()]);
    expect(screen.getByRole("button", { name: "Disabled (1)" }).getAttribute("aria-expanded")).toBe("false");
  });

  it("renders no shelf when nothing is disabled", () => {
    renderTokens([tok()]);
    expect(screen.queryByRole("button", { name: /^Disabled/ })).toBeNull();
  });
});

describe("No-default notice", () => {
  it("renders at the top of the card while the shelf is still collapsed", () => {
    renderTokens([off({ id: "a", label: "a" }), off({ id: "b", label: "b" })]);
    const notice = screen.getByTestId("no-default-anthropic");
    expect(notice.textContent).toMatch(/No default Anthropic token/);
    expect(screen.getByRole("button", { name: "Disabled (2)" }).getAttribute("aria-expanded")).toBe("false");
    // The notice opens the shelf, where every row reads "Enable and make default".
    fireEvent.click(within(notice).getByRole("button", { name: "Show disabled tokens" }));
    expect(screen.getAllByRole("button", { name: /^Enable and make default/ })).toHaveLength(2);
  });

  it("is absent while the slot has an enabled default, where rows read plain 'Enable'", () => {
    window.localStorage.setItem(SHELF_KEY, "true");
    renderTokens([tok(), off()]);
    expect(screen.queryByTestId("no-default-anthropic")).toBeNull();
    expect(screen.getByRole("button", { name: "Enable: old-laptop" })).toBeTruthy();
  });
});

describe("Disable dialog", () => {
  it("cannot confirm disabling the default until a replacement is chosen, then sends it", async () => {
    const { reload } = renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false })]);
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ enabled: false, is_default: false }) });
    const row = screen.getByTestId("token-sec-1");
    fireEvent.click(within(row).getByRole("button", { name: "Disable" }));

    const dialog = await screen.findByRole("dialog", { name: "Disable main" });
    const confirm = within(dialog).getByRole("button", { name: "Disable token" });
    expect((confirm as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(confirm);
    expect(mockApi.setSecretEnabled).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole("radio", { name: "Make “spare” the default" }));
    expect((confirm as HTMLButtonElement).disabled).toBe(false);
    await act(async () => {
      fireEvent.click(confirm);
    });
    expect(mockApi.setSecretEnabled).toHaveBeenCalledWith("anthropic_token", "sec-1", false, "sec-2");
    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("states the no-default consequence for the last enabled token and confirms without a choice", async () => {
    renderTokens([tok(), off()]);
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ enabled: false, is_default: false }) });
    fireEvent.click(within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Disable" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByTestId("no-default-consequence").textContent).toMatch(/no default Anthropic token/);
    expect(within(dialog).queryByRole("radio")).toBeNull();
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));
    });
    expect(mockApi.setSecretEnabled).toHaveBeenCalledWith("anthropic_token", "sec-1", false, undefined);
  });

  it("lists what uses the token right now from the dependents read", async () => {
    mockApi.getSecretDependents.mockResolvedValue(
      deps({
        workers: { items: [{ id: "w1", name: "alpha" }], total: 1 },
        schedules: { items: [{ id: "s1", target: "issue" }], total: 1 },
        runs: { items: [{ id: "r1", status: "running" }], total: 1 },
        judge: true,
      }),
    );
    renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false })]);
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable" }));
    const uses = await screen.findByRole("region", { name: "Uses right now" });
    await waitFor(() => expect(uses.textContent).toMatch(/Worker alpha is bound to it: it waits/));
    expect(uses.textContent).toMatch(/1 schedule is pinned to it: its next fire is skipped/);
    expect(uses.textContent).toMatch(/1 unfinished run uses it/);
    expect(uses.textContent).toMatch(/run judge is pinned to it/);
    expect(mockApi.getSecretDependents).toHaveBeenCalledWith("anthropic_token", "sec-2");
  });

  it("shows the server's refusal inline and keeps the dialog open", async () => {
    mockApi.setSecretEnabled.mockRejectedValue(new ApiError(409, "choose an enabled replacement in Settings"));
    renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false })]);
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable" }));
    const dialog = await screen.findByRole("dialog");
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));
    });
    expect(within(dialog).getByRole("alert").textContent).toMatch(/choose an enabled replacement/);
  });
});

describe("Enable", () => {
  const freshReading = {
    secret_id: "sec-off",
    label: "old-laptop",
    is_default: false,
    auto_eligible: false,
    auto_status: "not_pooled" as const,
    limits: {
      status: "ok" as const,
      five_hour: { pct: 1, resets_at: null },
      seven_day: { pct: 1, resets_at: null },
      source: "usage_endpoint" as const,
      synced_at: "2026-09-27T10:00:00Z",
      stale: false,
    },
  };

  it("enables from the shelf and shows 'checking usage…' until a fresh reading lands", async () => {
    window.localStorage.setItem(SHELF_KEY, "true");
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ id: "sec-off", label: "old-laptop", is_default: false }) });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(<ReloadingCard />);
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Enable: old-laptop" }));
      });
      expect(mockApi.setSecretEnabled).toHaveBeenCalledWith("anthropic_token", "sec-off", true);
      // Back in the live list, still waiting for a fresh reading.
      const row = await screen.findByTestId("token-sec-off");
      expect(within(row).getByText("checking usage…")).toBeTruthy();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2_000);
      });
      expect(within(screen.getByTestId("token-sec-off")).getByText("checking usage…")).toBeTruthy();
      // The fresh reading lands; the next probe clears the chip.
      mockApi.getMyRateLimits.mockResolvedValue({ tokens: [freshReading] });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(6_000);
      });
      await waitFor(() => expect(screen.queryByText("checking usage…")).toBeNull());
    } finally {
      vi.useRealTimers();
    }
  });
});

// ReloadingCard owns the list the way Settings does: its reload swaps in the post-Enable
// state (the token live again).
function ReloadingCard() {
  const [secrets, setSecrets] = useState<SecretMeta[]>([tok(), off()]);
  return (
    <MemoryRouter>
      <AnthropicTokens
        secrets={secrets}
        loading={false}
        busy={false}
        reload={async () => setSecrets([tok(), tok({ id: "sec-off", label: "old-laptop", is_default: false })])}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
        sidebarTokenIds={[]}
        onToggleSidebarToken={async () => {}}
      />
    </MemoryRouter>
  );
}

describe("Codex card", () => {
  function codex(over: Partial<SecretMeta> = {}): SecretMeta {
    return tok({ kind: "codex_auth", codex_status: "linked", ...over });
  }
  function renderCodex(secrets: SecretMeta[]) {
    render(
      <MemoryRouter>
        <CodexCredentials
          secrets={secrets}
          loading={false}
          busy={false}
          reload={async () => {}}
          onError={() => {}}
          onNotice={() => {}}
        />
      </MemoryRouter>,
    );
  }

  it("says in the dialog that an enabled sibling keeps the shared account live", async () => {
    mockApi.getSecretDependents.mockResolvedValue(
      deps({ enabled_siblings: { items: [{ id: "c2", label: "team-codex" }], total: 1 } }),
    );
    renderCodex([codex({ id: "c1", label: "personal" }), codex({ id: "c2", label: "team-codex", is_default: false })]);
    fireEvent.click(within(screen.getByTestId("codex-c2")).getByRole("button", { name: "Disable" }));
    expect((await screen.findByTestId("codex-sibling-note")).textContent).toMatch(
      /stays live through “team-codex”/,
    );
    expect(mockApi.getSecretDependents).toHaveBeenCalledWith("codex_auth", "c2");
  });

  it("shows the sibling fact instead of 'Disabled since' on a shelf row", async () => {
    window.localStorage.setItem("uzi.settings.disabledCodexOpen", "true");
    mockApi.getSecretDependents.mockResolvedValue(
      deps({ enabled_siblings: { items: [{ id: "c1", label: "team-codex" }], total: 1 } }),
    );
    renderCodex([
      codex({ id: "c1", label: "team-codex" }),
      codex({ id: "c2", label: "laptop", is_default: false, enabled: false, disabled_at: "2026-09-20T00:00:00Z" }),
    ]);
    const row = screen.getByTestId("codex-c2");
    await waitFor(() => expect(row.textContent).toMatch(/account stays live through “team-codex”/));
    expect(row.textContent).not.toMatch(/Disabled since/);
  });

  it("renders the no-default notice for an all-disabled Codex slot with the shelf collapsed", () => {
    renderCodex([codex({ id: "c1", is_default: false, enabled: false, disabled_at: "2026-09-20T00:00:00Z" })]);
    expect(screen.getByTestId("no-default-codex").textContent).toMatch(/No default Codex credential/);
    expect(screen.getByRole("button", { name: "Disabled (1)" }).getAttribute("aria-expanded")).toBe("false");
  });
});
