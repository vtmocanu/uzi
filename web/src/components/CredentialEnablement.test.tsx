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
    expect(notice.textContent).toMatch(/Runs that spend your default token fail/);
    expect(notice.textContent).not.toMatch(/default token waits/);
    expect(screen.getByRole("button", { name: "Disabled (2)" }).getAttribute("aria-expanded")).toBe("false");
    // The notice opens the shelf, where every row reads "Enable and make default".
    fireEvent.click(within(notice).getByRole("button", { name: "Show disabled tokens" }));
    expect(screen.getAllByRole("button", { name: /^Enable and make default/ })).toHaveLength(2);
    // Focus follows into the shelf: its first row's action, not the vanished notice button.
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Enable and make default: a" }));
  });

  it("is absent while the slot has an enabled default, where rows read plain 'Enable'", () => {
    window.localStorage.setItem(SHELF_KEY, "true");
    renderTokens([tok(), off()]);
    expect(screen.queryByTestId("no-default-anthropic")).toBeNull();
    expect(screen.getByRole("button", { name: "Enable: old-laptop" })).toBeTruthy();
  });
});

// PRD #1732: the server refuses to delete the default only while ANOTHER ENABLED token
// remains (DeleteAnthropicTokenByID / the Codex twin count enabled rows). Tokens on the
// Disabled shelf do not block it, so neither may the card.
describe("Delete of the default counts enabled rows only", () => {
  it("lets the Anthropic default be deleted when only disabled tokens remain", () => {
    renderTokens([tok(), off()]);
    const del = within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Delete" });
    expect(del.getAttribute("aria-disabled")).toBeNull();
    expect(del.getAttribute("aria-describedby")).toBeNull();
  });

  it("still blocks it while another enabled token exists", () => {
    renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false }), off()]);
    const del = within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Delete" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
  });

  it("names the no-default consequence, not a disconnect, when disabled tokens stay behind", () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderTokens([tok(), off()]);
    fireEvent.click(within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Delete" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringMatching(/last enabled token, so you will have no default/));
  });

  it("lets the Codex default be deleted when only disabled credentials remain", () => {
    render(
      <MemoryRouter>
        <CodexCredentials
          secrets={[
            tok({ id: "c1", kind: "codex_auth", codex_status: "linked", label: "personal" }),
            tok({ id: "c2", kind: "openai_api_key", codex_status: "static", label: "key", is_default: false, enabled: false }),
          ]}
          loading={false}
          busy={false}
          reload={async () => {}}
          onError={() => {}}
          onNotice={() => {}}
        />
      </MemoryRouter>,
    );
    const del = within(screen.getByTestId("codex-c1")).getByRole("button", { name: "Delete" });
    expect(del.getAttribute("aria-disabled")).toBeNull();
  });
});

describe("Disable dialog", () => {
  it("cannot confirm disabling the default until a replacement is chosen, then sends it", async () => {
    const { reload } = renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false })]);
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ enabled: false, is_default: false }) });
    const row = screen.getByTestId("token-sec-1");
    fireEvent.click(within(row).getByRole("button", { name: "Disable main" }));

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

  it("hands focus to the Disabled shelf toggle once the row has moved there", async () => {
    render(<ReloadingCard initial={[tok(), tok({ id: "sec-2", label: "spare", is_default: false })]} after={(rows) =>
      rows.map((r) => (r.id === "sec-2" ? { ...r, enabled: false, disabled_at: "2026-09-27T00:00:00Z" } : r))
    } />);
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ id: "sec-2", enabled: false, is_default: false }) });
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable spare" }));
    const dialog = await screen.findByRole("dialog");
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: "Disable token" }));
    });
    const toggle = await screen.findByRole("button", { name: "Disabled (1)" });
    await waitFor(() => expect(document.activeElement).toBe(toggle));
  });

  it("names each live row's Disable button after its credential", () => {
    renderTokens([tok(), tok({ id: "sec-2", label: "spare", is_default: false })]);
    expect(within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Disable main" })).toBeTruthy();
    expect(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable spare" })).toBeTruthy();
  });

  it("states the no-default consequence for the last enabled token and confirms without a choice", async () => {
    renderTokens([tok(), off()]);
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ enabled: false, is_default: false }) });
    fireEvent.click(within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Disable main" }));
    const dialog = await screen.findByRole("dialog");
    const consequence = within(dialog).getByTestId("no-default-consequence").textContent;
    expect(consequence).toMatch(/no default Anthropic token/);
    // Default-based ordinary runs fail (D15); only chat, the judge and self-improvement wait.
    expect(consequence).toMatch(/Runs that spend your default fail/);
    expect(consequence).not.toMatch(/Work that spends your default waits/);
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
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable spare" }));
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
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Disable spare" }));
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

  it("puts focus on the restored row's Disable button after Enable", async () => {
    window.localStorage.setItem(SHELF_KEY, "true");
    mockApi.setSecretEnabled.mockResolvedValue({ secret: tok({ id: "sec-off", label: "old-laptop", is_default: false }) });
    render(<ReloadingCard />);
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Enable: old-laptop" }));
    });
    const row = await screen.findByTestId("token-sec-off");
    await waitFor(() => expect(document.activeElement).toBe(within(row).getByRole("button", { name: "Disable old-laptop" })));
  });

  // Each credential gets its own give-up clock: enabling a second one must not restart the
  // first one's probes, or "checking usage…" could outlive CHECK_MAX_PROBES indefinitely.
  it("gives up on each credential after its own probe budget, whatever else was enabled later", async () => {
    window.localStorage.setItem(SHELF_KEY, "true");
    const enableRow = (rows: SecretMeta[], id: string) =>
      rows.map((r) => (r.id === id ? { ...r, enabled: true, disabled_at: null } : r));
    let enabling = "a";
    // Step the clock so React commits (and the hook re-arms its timer) between probes.
    const advance = async (ms: number) => {
      for (let t = 0; t < ms; t += 500) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(500);
        });
      }
    };
    mockApi.setSecretEnabled.mockImplementation(async (_k, id) => ({ secret: tok({ id, is_default: false }) }));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(
        <ReloadingCard
          initial={[tok(), off({ id: "a", label: "a" }), off({ id: "b", label: "b" })]}
          after={(rows) => enableRow(rows, enabling)}
        />,
      );
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Enable: a" }));
      });
      // ~20 of a's 24 probes (1.5 s, then every 5 s), never a fresh reading.
      await advance(100_000);
      expect(within(screen.getByTestId("token-a")).getByText("checking usage…")).toBeTruthy();
      enabling = "b";
      await act(async () => {
        fireEvent.click(screen.getByRole("button", { name: "Enable: b" }));
      });
      await advance(30_000);
      // a has spent its budget and fell back; b is still inside its own.
      expect(within(screen.getByTestId("token-a")).queryByText("checking usage…")).toBeNull();
      expect(within(screen.getByTestId("token-b")).getByText("checking usage…")).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });
});

// ReloadingCard owns the list the way Settings does: its reload swaps in the post-Enable
// state (the token live again).
function ReloadingCard({
  initial = [tok(), off()],
  after = () => [tok(), tok({ id: "sec-off", label: "old-laptop", is_default: false })],
}: {
  initial?: SecretMeta[];
  after?: (rows: SecretMeta[]) => SecretMeta[];
}) {
  const [secrets, setSecrets] = useState<SecretMeta[]>(initial);
  return (
    <MemoryRouter>
      <AnthropicTokens
        secrets={secrets}
        loading={false}
        busy={false}
        reload={async () => setSecrets((rows) => after(rows))}
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
    fireEvent.click(within(screen.getByTestId("codex-c2")).getByRole("button", { name: "Disable team-codex" }));
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

  it("drops a stale sibling note when a re-read finds no enabled sibling", async () => {
    window.localStorage.setItem("uzi.settings.disabledCodexOpen", "true");
    mockApi.getSecretDependents.mockResolvedValueOnce(
      deps({ enabled_siblings: { items: [{ id: "c1", label: "team-codex" }], total: 1 } }),
    );
    renderCodex([
      codex({ id: "c1", label: "team-codex" }),
      codex({ id: "c2", label: "laptop", is_default: false, enabled: false, disabled_at: "2026-09-20T00:00:00Z" }),
    ]);
    await waitFor(() => expect(screen.getByTestId("codex-c2").textContent).toMatch(/stays live through/));
    // The sibling was disabled meanwhile: the next read (shelf re-opened) reports none.
    mockApi.getSecretDependents.mockResolvedValue(deps());
    const toggle = screen.getByRole("button", { name: "Disabled (1)" });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    await waitFor(() => expect(screen.getByTestId("codex-c2").textContent).toMatch(/Disabled since/));
    expect(screen.getByTestId("codex-c2").textContent).not.toMatch(/stays live through/);
  });

  it("offers replacement defaults with their kind and status, failed logins last, and no auto-select talk", async () => {
    renderCodex([
      codex({ id: "c1", label: "personal" }),
      codex({ id: "c2", label: "broken", is_default: false, codex_status: "failed" }),
      tok({ id: "c3", kind: "openai_api_key", codex_status: "static", label: "api", is_default: false }),
      codex({ id: "c4", label: "team", is_default: false }),
    ]);
    fireEvent.click(within(screen.getByTestId("codex-c1")).getByRole("button", { name: "Disable personal" }));
    const dialog = await screen.findByRole("dialog");
    const radios = within(dialog).getAllByRole("radio");
    const labels = radios.map((r) => r.closest("label")?.textContent ?? "");
    expect(labels).toHaveLength(3);
    expect(labels[labels.length - 1]).toMatch(/broken.*Codex login.*failed/);
    expect(labels.find((l) => l.includes("api"))).toMatch(/OpenAI API key.*static/);
    expect(labels.find((l) => l.includes("team"))).toMatch(/Codex login.*linked/);
    expect(dialog.textContent).not.toMatch(/auto-select/i);
  });

  it("renders the no-default notice for an all-disabled Codex slot with the shelf collapsed", () => {
    renderCodex([codex({ id: "c1", is_default: false, enabled: false, disabled_at: "2026-09-20T00:00:00Z" })]);
    expect(screen.getByTestId("no-default-codex").textContent).toMatch(/No default Codex credential/);
    expect(screen.getByRole("button", { name: "Disabled (1)" }).getAttribute("aria-expanded")).toBe("false");
  });
});
