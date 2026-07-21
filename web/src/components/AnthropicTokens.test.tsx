// @vitest-environment jsdom
// PRD #104 M6: the Settings token list. The value must appear nowhere, the D6
// delete rule must be visible (not just enforced by a server 409), and the
// affected-workers warning must be stated BEFORE a destructive click (D5).

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { AnthropicTokens } from "./AnthropicTokens";
import { api, type SecretMeta } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      createAnthropicToken: vi.fn(),
      patchAnthropicToken: vi.fn(),
      deleteAnthropicTokenById: vi.fn(),
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
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    ...over,
  };
}

const noop = async () => {};

function renderList(secrets: SecretMeta[], reload = noop) {
  return render(
    <AnthropicTokens
      secrets={secrets}
      loading={false}
      busy={false}
      reload={reload}
      onError={() => {}}
      onNotice={() => {}}
      error=""
    />,
  );
}

beforeEach(() => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
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
  it("disables deleting the default while another token exists, and explains why", () => {
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    const defaultRow = screen.getByTestId("token-sec-1");
    const del = within(defaultRow).getByRole("button", { name: "Delete" });
    expect((del as HTMLButtonElement).disabled).toBe(true);
    expect(del.getAttribute("title")).toMatch(/another token the default first/i);
  });

  it("allows deleting the default when it is the LAST token", () => {
    renderList([secret()]);
    const del = screen.getByRole("button", { name: "Delete" });
    expect((del as HTMLButtonElement).disabled).toBe(false);
  });

  // D5: deleting a bound token silently returns its workers to the default. The
  // confirmation must SAY so — a quiet fallback is acceptable behavior but not
  // acceptable UX.
  it("warns that bound workers fall back to the default before deleting a non-default token", async () => {
    mockApi.deleteAnthropicTokenById.mockResolvedValue(null);
    renderList([secret(), secret({ id: "sec-2", label: "console-key", is_default: false })]);
    const row = screen.getByTestId("token-sec-2");
    fireEvent.click(within(row).getByRole("button", { name: "Delete" }));
    expect(window.confirm).toHaveBeenCalledWith(
      expect.stringMatching(/falls back to your default token/i),
    );
    await waitFor(() => expect(mockApi.deleteAnthropicTokenById).toHaveBeenCalledWith("sec-2"));
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
});
