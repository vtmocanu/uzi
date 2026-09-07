// @vitest-environment jsdom
// PRD #1147 M3: the Settings OpenAI / Codex credential card. Two kinds share one
// card and one default; the value must appear nowhere; the stateless codex_status
// badge is read straight off the row; and set-default works across BOTH kinds.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { CodexCredentials } from "./CodexCredentials";
import { api, type SecretMeta } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      // ONLY the six Codex wrappers — this card uses no workers/rate-limit/pool APIs.
      createCodexAuth: vi.fn(),
      patchCodexAuth: vi.fn(),
      deleteCodexAuthById: vi.fn(),
      createOpenAIApiKey: vi.fn(),
      patchOpenAIApiKey: vi.fn(),
      deleteOpenAIApiKeyById: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function secret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "codex_auth",
    label: "default",
    is_default: true,
    auto_eligible: false,
    codex_status: "staging",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-02T00:00:00Z",
    ...over,
  };
}

const noop = async () => {};

function renderCard(secrets: SecretMeta[], reload = noop) {
  return render(
    <MemoryRouter>
      <CodexCredentials
        secrets={secrets}
        loading={false}
        busy={false}
        reload={reload}
        onError={() => {}}
        onNotice={() => {}}
      />
    </MemoryRouter>,
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

describe("CodexCredentials", () => {
  it("renders each codex row with its kind and stateless status badge", () => {
    renderCard([
      secret(),
      secret({
        id: "sec-2",
        kind: "openai_api_key",
        label: "console-key",
        is_default: false,
        codex_status: "static",
      }),
    ]);
    const codexRow = screen.getByTestId("codex-sec-1");
    const openaiRow = screen.getByTestId("codex-sec-2");
    // The kind is named on each row.
    expect(within(codexRow).getByText("Codex login")).toBeTruthy();
    expect(within(openaiRow).getByText("OpenAI API key")).toBeTruthy();
    // The status badge is read straight off codex_status (no second fetch).
    expect(within(codexRow).getByText("staging")).toBeTruthy();
    expect(within(openaiRow).getByText("static")).toBeTruthy();
    // Exactly one default across both kinds. Two matches on the default row: its
    // label ("default") and the default badge beside it.
    expect(within(codexRow).getAllByText("default").length).toBe(2);
    expect(within(openaiRow).queryByText("default")).toBeNull();
  });

  it("renders a linked/failed status badge from the row without re-fetching", () => {
    renderCard([secret({ codex_status: "linked" }), secret({ id: "sec-2", label: "broken", is_default: false, codex_status: "failed" })]);
    expect(within(screen.getByTestId("codex-sec-1")).getByText("linked")).toBeTruthy();
    expect(within(screen.getByTestId("codex-sec-2")).getByText("failed")).toBeTruthy();
  });

  it("creates a Codex login → calls createCodexAuth", async () => {
    mockApi.createCodexAuth.mockResolvedValue({ secret: secret() });
    renderCard([secret()]);
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login token"), {
      target: { value: "codex-login-value" },
    });
    fireEvent.change(screen.getByLabelText("Codex login name"), {
      target: { value: "my-codex" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add Codex login" }));
    await waitFor(() =>
      expect(mockApi.createCodexAuth).toHaveBeenCalledWith("codex-login-value", "my-codex", false),
    );
  });

  it("creates an OpenAI API key → calls createOpenAIApiKey", async () => {
    mockApi.createOpenAIApiKey.mockResolvedValue({ secret: secret() });
    renderCard([secret()]);
    fireEvent.change(screen.getByPlaceholderText("Paste your OpenAI API key"), {
      target: { value: "sk-openai-value" },
    });
    fireEvent.change(screen.getByLabelText("OpenAI API key name"), {
      target: { value: "console-key" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add OpenAI API key" }));
    await waitFor(() =>
      expect(mockApi.createOpenAIApiKey).toHaveBeenCalledWith("sk-openai-value", "console-key", false),
    );
  });

  it("asks for no name on the very first credential, and forces label default", async () => {
    mockApi.createCodexAuth.mockResolvedValue({ secret: secret() });
    renderCard([]);
    // First mode: the Name fields collapse.
    expect(screen.queryByLabelText("Codex login name")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login token"), {
      target: { value: "codex-first" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    await waitFor(() =>
      expect(mockApi.createCodexAuth).toHaveBeenCalledWith("codex-first", "default", false),
    );
  });

  it("renames a credential via its kind's patch route", async () => {
    mockApi.patchCodexAuth.mockResolvedValue({ secret: secret() });
    renderCard([
      secret(),
      secret({ id: "sec-2", label: "codex-2", is_default: false }),
    ]);
    const row = screen.getByTestId("codex-sec-2");
    fireEvent.click(within(row).getByRole("button", { name: "Rename" }));
    const input = screen.getByLabelText("Rename codex-2");
    fireEvent.change(input, { target: { value: "renamed-codex" } });
    fireEvent.submit(input);
    await waitFor(() =>
      expect(mockApi.patchCodexAuth).toHaveBeenCalledWith("sec-2", { label: "renamed-codex" }),
    );
  });

  it("replaces a credential value → patches with the token via its kind's route", async () => {
    mockApi.patchOpenAIApiKey.mockResolvedValue({ secret: secret() });
    renderCard([
      secret(),
      secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false, codex_status: "static" }),
    ]);
    fireEvent.change(screen.getByLabelText("Credential to replace"), {
      target: { value: "sec-2" },
    });
    fireEvent.change(screen.getByPlaceholderText("Paste the replacement credential"), {
      target: { value: "sk-rotated" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Replace value" }));
    await waitFor(() =>
      expect(mockApi.patchOpenAIApiKey).toHaveBeenCalledWith("sec-2", { token: "sk-rotated" }),
    );
  });

  it("deletes a non-default credential after confirming", async () => {
    mockApi.deleteCodexAuthById.mockResolvedValue(null);
    renderCard([
      secret(),
      secret({ id: "sec-2", label: "codex-2", is_default: false }),
    ]);
    fireEvent.click(
      within(screen.getByTestId("codex-sec-2")).getByRole("button", { name: "Delete" }),
    );
    expect(window.confirm).toHaveBeenCalled();
    await waitFor(() => expect(mockApi.deleteCodexAuthById).toHaveBeenCalledWith("sec-2"));
  });

  it("refuses deleting the default while another credential exists, and explains why", () => {
    renderCard([
      secret(),
      secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false, codex_status: "static" }),
    ]);
    const del = within(screen.getByTestId("codex-sec-1")).getByRole("button", { name: "Delete" });
    expect(del.getAttribute("aria-disabled")).toBe("true");
    expect((del as HTMLButtonElement).disabled).toBe(false);
    expect(del.getAttribute("title")).toMatch(/another credential the default first/i);
  });

  it("sets the default across kinds — an OpenAI key can become the single default", async () => {
    mockApi.patchOpenAIApiKey.mockResolvedValue({ secret: secret() });
    renderCard([
      secret(),
      secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false, codex_status: "static" }),
    ]);
    // The only "Make default" is on the non-default OpenAI row.
    fireEvent.click(screen.getByRole("button", { name: "Make default" }));
    await waitFor(() =>
      expect(mockApi.patchOpenAIApiKey).toHaveBeenCalledWith("sec-2", { default: true }),
    );
  });

  it("never renders a credential VALUE — the API returns none and the DTO has no field for one", () => {
    const { container } = renderCard([
      secret({ label: "console-key", is_default: false }),
    ]);
    container.querySelectorAll("input[type=password]").forEach((el) => {
      expect((el as HTMLInputElement).value).toBe("");
    });
    expect(container.textContent).not.toMatch(/sk-|ciphertext|sealed/i);
  });
});
