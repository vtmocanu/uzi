// @vitest-environment jsdom
// PRD #104 M6: the Settings token list. The value must appear nowhere, the D6
// delete rule must be visible (not just enforced by a server 409), and the
// affected-workers warning must be stated BEFORE a destructive click (D5).

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { AnthropicTokens } from "./AnthropicTokens";
import { api, type SecretMeta, type Worker } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      createAnthropicToken: vi.fn(),
      patchAnthropicToken: vi.fn(),
      deleteAnthropicTokenById: vi.fn(),
      // The card fetches workers itself so a delete can NAME the affected ones
      // (D5). Defaulted per-test in beforeEach.
      listWorkers: vi.fn(),
      // PRD #111 M2: the pool toggle, and the per-token live eligibility the card
      // fetches so the chip sits beside the toggle rather than a card away.
      setTokenAutoEligible: vi.fn(),
      getMyRateLimits: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function secret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
    // PRD #111 M2: the auto-selection pool opt-in, false unless a test says otherwise.
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    ...over,
  };
}

const noop = async () => {};

function worker(over: Partial<Worker> = {}): Worker {
  return {
    ...(baseWorker as Worker),
    ...over,
  };
}

// Only the three fields this component reads; the rest of Worker is irrelevant
// here and casting once keeps the fixtures legible.
const baseWorker = {
  id: "wrk-1",
  name: "alpha",
  anthropic_secret_id: null,
  anthropic_secret_label: null,
} as unknown as Worker;

function renderList(secrets: SecretMeta[], reload = noop, judgeSecretId: string | null = null) {
  return render(
    <AnthropicTokens
      secrets={secrets}
      loading={false}
      busy={false}
      reload={reload}
      onError={() => {}}
      onNotice={() => {}}
      judgeSecretId={judgeSecretId}
    />,
  );
}

beforeEach(() => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.getMyRateLimits.mockResolvedValue({ tokens: [] });
  mockApi.setTokenAutoEligible.mockResolvedValue({ secret: secret() });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("AnthropicTokens", () => {
  it("lists each token with its label and marks exactly one default", () => {
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    const defaultRow = screen.getByTestId("token-sec-1");
    const otherRow = screen.getByTestId("token-sec-2");
    // Two matches on the default row: its label, and the default badge beside it.
    expect(within(defaultRow).getAllByText("default").length).toBe(2);
    expect(within(otherRow).getByText("console-key")).toBeTruthy();
    // The badge is on the default row ONLY — exactly one token is the default.
    expect(within(otherRow).queryByText("default")).toBeNull();
  });

  it("never renders a token VALUE — the API returns none and the DTO has no field for one", () => {
    const { container } = renderList([secret({ label: "console-key", is_default: false })]);
    // The only password inputs are the paste fields, which are empty.
    container.querySelectorAll("input[type=password]").forEach((el) => {
      expect((el as HTMLInputElement).value).toBe("");
    });
    expect(container.textContent).not.toMatch(/sk-ant|ciphertext|sealed/i);
  });

  // D6: the default cannot be deleted while other tokens exist. The UI disables it
  // rather than letting the server 409 — and says why, so the user knows the fix is
  // "promote another", not "try again".
  it("refuses deleting the default while another token exists, and explains why", () => {
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    const defaultRow = screen.getByTestId("token-sec-1");
    const del = within(defaultRow).getByRole("button", { name: "Delete" });
    // aria-disabled, NOT `disabled`: a `disabled` button leaves the tab order, so
    // the reason was reachable only by hovering a control you could not focus.
    expect(del.getAttribute("aria-disabled")).toBe("true");
    expect((del as HTMLButtonElement).disabled).toBe(false);
    expect(del.getAttribute("title")).toMatch(/another token the default first/i);
  });

  // The half that was missing (web-ux D3): keyboard and screen-reader users got a
  // button they could not reach and a reason that existed only in a tooltip.
  it("gives the refusal a screen-reader description, not just a tooltip", () => {
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    const del = within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Delete" });
    const describedBy = del.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy!)?.textContent).toMatch(
      /another token the default first/i,
    );
  });

  it("does not delete when the D6 rule blocks it, even if the click lands", () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    fireEvent.click(within(screen.getByTestId("token-sec-1")).getByRole("button", { name: "Delete" }));
    expect(window.confirm).not.toHaveBeenCalled();
    expect(mockApi.deleteAnthropicTokenById).not.toHaveBeenCalled();
  });

  it("allows deleting the default when it is the LAST token", () => {
    renderList([secret()]);
    const del = screen.getByRole("button", { name: "Delete" });
    expect((del as HTMLButtonElement).disabled).toBe(false);
    expect(del.getAttribute("aria-disabled")).toBeNull();
  });

  // D5: deleting a bound token silently returns its workers to the default, and
  // the confirmation must NAME them. The generic "any worker bound to it" wording
  // is precisely what D5 rejects — it tells a user something MIGHT move, not what
  // did — so these assert the names, not the sentence shape.
  it("NAMES the workers bound to a token before deleting it", async () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        worker({ id: "w1", name: "alpha", anthropic_secret_id: "sec-2" }),
        worker({ id: "w2", name: "beta", anthropic_secret_id: "sec-2" }),
        worker({ id: "w3", name: "gamma", anthropic_secret_id: null }),
      ],
    });
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    await waitFor(() => expect(mockApi.listWorkers).toHaveBeenCalled());
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Delete" }));
    const msg = vi.mocked(window.confirm).mock.calls[0][0] as string;
    expect(msg).toMatch(/alpha/);
    expect(msg).toMatch(/beta/);
    // An UNBOUND worker must not be named — a warning that over-claims is as
    // useless as one that under-claims.
    expect(msg).not.toMatch(/gamma/);
    expect(msg).toMatch(/fall back to your default token/i);
    await waitFor(() => expect(mockApi.deleteAnthropicTokenById).toHaveBeenCalledWith("sec-2"));
  });

  it("names the run judge too when the judge lane is bound to the token", async () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    renderList(
      [secret(), secret({ id: "sec-2", label: "console-key", is_default: false })],
      noop,
      "sec-2",
    );
    await waitFor(() => expect(mockApi.listWorkers).toHaveBeenCalled());
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Delete" }));
    expect(vi.mocked(window.confirm).mock.calls[0][0]).toMatch(/your run judge falls back/i);
  });

  it("says plainly when nothing is bound, rather than implying something might move", async () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    await waitFor(() => expect(mockApi.listWorkers).toHaveBeenCalled());
    fireEvent.click(within(screen.getByTestId("token-sec-2")).getByRole("button", { name: "Delete" }));
    expect(vi.mocked(window.confirm).mock.calls[0][0]).toMatch(/nothing is bound to it/i);
  });

  it("warns that deleting the LAST token disconnects the account", () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    renderList([secret()]);
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringMatching(/no longer be connected to your Anthropic account/i),
    );
  });

  it("promotes another token to default", async () => {
    mockApi.patchAnthropicToken.mockResolvedValue({ secret: secret() });
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    fireEvent.click(screen.getByRole("button", { name: "Make default" }));
    await waitFor(() =>
      expect(mockApi.patchAnthropicToken).toHaveBeenCalledWith("sec-2", { default: true }),
    );
  });

  // The default row offers no "Make default" — it already is one.
  it("offers no Make default on the token that is already the default", () => {
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    expect(screen.getAllByRole("button", { name: "Make default" }).length).toBe(1);
  });

  it("renames a token", async () => {
    mockApi.patchAnthropicToken.mockResolvedValue({ secret: secret() });
    renderList([secret({ label: "console-key", is_default: false })]);
    fireEvent.click(screen.getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText("Rename console-key");
    fireEvent.change(input, { target: { value: "cheap-console" } });
    fireEvent.submit(input);
    await waitFor(() =>
      expect(mockApi.patchAnthropicToken).toHaveBeenCalledWith("sec-1", { label: "cheap-console" }),
    );
  });

  // A user's FIRST token needs no name field: the server forces label "default"
  // and forces it default, so asking would offer a choice that does not exist.
  it("asks for no name on the very first token, and requires one afterwards", () => {
    const { unmount } = renderList([]);
    expect(screen.queryByLabelText("Token name")).toBeNull();
    unmount();
    renderList([secret()]);
    expect(screen.getByLabelText("Token name")).toBeTruthy();
  });

  it("creates a named token", async () => {
    mockApi.createAnthropicToken.mockResolvedValue({ secret: secret() });
    renderList([secret()]);
    fireEvent.change(screen.getByPlaceholderText("Paste your Anthropic token"), {
      target: { value: "sk-ant-new-value" },
    });
    fireEvent.change(screen.getByLabelText("Token name"), { target: { value: "console-key" } });
    fireEvent.click(screen.getByRole("button", { name: "Add token" }));
    await waitFor(() =>
      expect(mockApi.createAnthropicToken).toHaveBeenCalledWith("sk-ant-new-value", "console-key", false),
    );
  });

  // web-ux D1, and the reason this is a bug and not a nit: the Name input UNMOUNTS
  // when the card collapses to first-token mode, so a label typed and never
  // submitted would name the next token through a field that is not on screen.
  // Deterministic repro was: type a name, delete every token, paste, Save.
  it("never names the first token from a field the user cannot see", async () => {
    mockApi.createAnthropicToken.mockResolvedValue({ secret: secret() });
    const { rerender } = render(
      <AnthropicTokens
        secrets={[secret()]}
        loading={false}
        busy={false}
        reload={noop}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
      />,
    );
    fireEvent.change(screen.getByLabelText("Token name"), { target: { value: "staging-key" } });
    // Every token goes away — the card collapses and the Name field unmounts.
    rerender(
      <AnthropicTokens
        secrets={[]}
        loading={false}
        busy={false}
        reload={noop}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
      />,
    );
    expect(screen.queryByLabelText("Token name")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("Paste your Anthropic token"), {
      target: { value: "sk-ant-first" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save token" }));
    await waitFor(() =>
      expect(mockApi.createAnthropicToken).toHaveBeenCalledWith("sk-ant-first", "default", false),
    );
  });

  // web-ux D1's other half: the pasted VALUE must not survive the collapse either.
  it("clears a half-typed token value when the card changes mode", () => {
    const { rerender } = render(
      <AnthropicTokens
        secrets={[secret()]}
        loading={false}
        busy={false}
        reload={noop}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
      />,
    );
    fireEvent.change(screen.getByPlaceholderText("Paste your Anthropic token"), {
      target: { value: "sk-ant-half-typed" },
    });
    rerender(
      <AnthropicTokens
        secrets={[]}
        loading={false}
        busy={false}
        reload={noop}
        onError={() => {}}
        onNotice={() => {}}
        judgeSecretId={null}
      />,
    );
    expect(
      (screen.getByPlaceholderText("Paste your Anthropic token") as HTMLInputElement).value,
    ).toBe("");
  });

  // web-ux D4: the card used to keep its own sr-only copy of the parent's error, so
  // a duplicate-label failure sat in the DOM twice and was announced twice. The
  // `error` prop is gone entirely now (TypeScript enforces that half); this asserts
  // the runtime half. Single-token on purpose — that is the one shape where D6's
  // own sr-only hint cannot render, so any sr-only node here would be a new echo.
  it("keeps no second copy of an error for screen readers", () => {
    const { container } = renderList([secret()]);
    expect(container.querySelectorAll(".sr-only").length).toBe(0);
  });

  // ── PRD #111 M2: the auto-selection pool toggle ───────────────────────────

  it("reflects the pool opt-in and toggles it both ways", async () => {
    renderList([secret({ auto_eligible: false })]);
    const box = screen.getByLabelText("Auto-select from this token") as HTMLInputElement;
    expect(box.checked).toBe(false);

    fireEvent.click(box);
    await waitFor(() => expect(mockApi.setTokenAutoEligible).toHaveBeenCalledWith("sec-1", true));

    cleanup();
    renderList([secret({ auto_eligible: true })]);
    const on = screen.getByLabelText("Auto-select from this token") as HTMLInputElement;
    expect(on.checked).toBe(true);
    fireEvent.click(on);
    await waitFor(() => expect(mockApi.setTokenAutoEligible).toHaveBeenCalledWith("sec-1", false));
  });

  // The consequence has to be visible at the moment of the choice. Opting in a
  // token whose gauge has never polled is a silent no-op — it looks active and can
  // never be picked — so the chip lives beside the toggle rather than a card away.
  it("shows the SERVER's eligibility beside the toggle for a pooled token", async () => {
    mockApi.getMyRateLimits.mockResolvedValue({
      tokens: [
        {
          secret_id: "sec-1",
          label: "default",
          is_default: true,
          auto_eligible: true,
          auto_status: "no_reading",
          limits: { status: "unavailable" },
        },
      ],
    });
    renderList([secret({ auto_eligible: true })]);
    // "never polled" is autoStatusChip's rendering of no_reading — rendered, not
    // re-derived: nothing in this component looks at `limits`.
    expect(await screen.findByText("never polled")).toBeTruthy();
  });

  // An UN-pooled token gets no chip: the unchecked box beside it already says so,
  // and a "not in pool" badge on every row a user has not opted into is noise.
  it("shows no eligibility chip for an un-pooled token", async () => {
    mockApi.getMyRateLimits.mockResolvedValue({
      tokens: [
        {
          secret_id: "sec-1",
          label: "default",
          is_default: true,
          auto_eligible: false,
          auto_status: "not_pooled",
          limits: { status: "unavailable" },
        },
      ],
    });
    renderList([secret({ auto_eligible: false })]);
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    expect(screen.queryByText("not in pool")).toBeNull();
  });

  // A failed meters fetch must not break the toggle or blank the card: the chip is
  // secondary information, and an error banner over it would be worse than its
  // absence. Same stance as the workers fetch above it.
  it("still renders the toggle when the eligibility fetch fails", async () => {
    mockApi.getMyRateLimits.mockRejectedValue(new Error("network"));
    renderList([secret({ auto_eligible: true })]);
    expect(screen.getByLabelText("Auto-select from this token")).toBeTruthy();
    await waitFor(() => expect(mockApi.getMyRateLimits).toHaveBeenCalled());
    expect(screen.queryByText("never polled")).toBeNull();
  });
});
