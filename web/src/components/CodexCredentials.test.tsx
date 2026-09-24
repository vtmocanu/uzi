// @vitest-environment jsdom
// PRD #1147 M3: the Settings OpenAI / Codex credential card. Two kinds share one
// card and one default; the value must appear nowhere; the stateless codex_status
// badge is read straight off the row; and set-default works across BOTH kinds.

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { CodexCredentials, codexShapeError } from "./CodexCredentials";
import { api, type SecretMeta } from "../lib/api";

// A valid codex_auth JSON paste — a flat object with access_token/refresh_token.
// Dummy values only; never a real token. Used wherever a codex create/rotate must
// actually reach the API (the wrong-shape pre-check would otherwise block it).
const VALID_CODEX_JSON = '{"access_token":"dummy-access","refresh_token":"dummy-refresh"}';

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

function renderCard(
  secrets: SecretMeta[],
  reload = noop,
  handlers: { onNotice?: (m: string) => void; onError?: (m: string) => void } = {},
) {
  return render(
    <MemoryRouter>
      <CodexCredentials
        secrets={secrets}
        loading={false}
        busy={false}
        reload={reload}
        onError={handlers.onError ?? (() => {})}
        onNotice={handlers.onNotice ?? (() => {})}
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
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: { value: VALID_CODEX_JSON },
    });
    fireEvent.change(screen.getByLabelText("Codex login name"), {
      target: { value: "my-codex" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add Codex login" }));
    await waitFor(() =>
      expect(mockApi.createCodexAuth).toHaveBeenCalledWith(VALID_CODEX_JSON, "my-codex", false),
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
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: { value: VALID_CODEX_JSON },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    await waitFor(() =>
      expect(mockApi.createCodexAuth).toHaveBeenCalledWith(VALID_CODEX_JSON, "default", false),
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

  // ── issue #1174: inline help, the status legend, and the wrong-shape pre-check ──

  it("shows inline help beside the codex field and a 'How to get this' disclosure", () => {
    renderCard([secret()]);
    // The codex field exists with the JSON placeholder.
    expect(screen.getByPlaceholderText("Paste your Codex login JSON")).toBeTruthy();
    // The help line explains it imports a login (JSON) and is NOT an OpenAI API key
    // (the "not" is a <strong>, so read the paragraph's whole textContent).
    const help = screen.getByText((c) => c.includes("imports an existing Codex CLI"));
    expect(help.textContent).toMatch(/not/i);
    expect(help.textContent).toMatch(/OpenAI\s+API\s+key/i);
    expect(help.textContent).toMatch(/access_token/);
    // The "How to get this" disclosure is a native <summary> (keyboard/touch/SR).
    const summary = screen.getByText("How to get this", { selector: "summary" });
    expect(summary.tagName).toBe("SUMMARY");
    // The disclosure keeps the do-not-share warning and the full-guide link.
    const details = summary.closest("details")!;
    expect(within(details).getByText(/do not commit it, log it/i)).toBeTruthy();
    expect(within(details).getByText("Full guide")).toBeTruthy();
  });

  // ── issue #1594: a login dedicated to uzi, isolated from the everyday Codex CLI ──

  it("renders the isolated-login recipe verbatim, and not the old shared-file recipe", () => {
    renderCard([secret()]);
    const details = screen
      .getByText("How to get this", { selector: "summary" })
      .closest("details")!;
    const code = details.querySelector("pre > code");
    expect(code).not.toBeNull();
    // The block preserves its layout (whitespace-pre) and every character.
    expect(code!.parentElement!.className).toContain("whitespace-pre");
    expect(code!.textContent).toBe(
      [
        '( export CODEX_HOME="$(mktemp -d)"',
        "  codex login -c cli_auth_credentials_store=file",
        `  jq -e '.auth_mode == "chatgpt" and (.tokens.access_token | type == "string" and length > 0) and (.tokens.refresh_token | type == "string" and length > 0)' "$CODEX_HOME/auth.json" >/dev/null \\`,
        '    || { echo "isolated login did not produce a usable file-based auth.json" >&2; exit 1; }',
        "  jq -c '{access_token: .tokens.access_token, refresh_token: .tokens.refresh_token}' \"$CODEX_HOME/auth.json\" | pbcopy",
        '  echo "Copied. Paste it into uzi, then remove $CODEX_HOME" )',
      ].join("\n"),
    );
    expect(code!.textContent).toContain("codex login -c cli_auth_credentials_store=file");
    expect(code!.textContent).toContain('"$CODEX_HOME/auth.json"');
    // The old recipe read the user's everyday login file; paired with the presence
    // checks above on the same render, so this absence is not vacuous.
    expect(details.textContent).not.toContain("~/.codex/auth.json");
    // The Linux clipboard note survives.
    expect(within(details).getByText("xclip -selection clipboard")).toBeTruthy();
    expect(within(details).getByText("wl-copy")).toBeTruthy();
  });

  it("warns beside the Codex login field (outside the disclosure), never beside the API key field", () => {
    renderCard([secret()]);
    const warning = /Use a login dedicated to uzi\. Pasting from a Codex login your own CLI keeps using breaks one side or the other, because the refresh token rotates\. After a failure, re-paste from a new isolated login, never the old file\./;
    const matches = screen.getAllByText(warning);
    // Exactly one: the codex_auth form carries it, the openai_api_key form does not.
    expect(matches).toHaveLength(1);
    const codexForm = screen.getByPlaceholderText("Paste your Codex login JSON").closest("form")!;
    const keyForm = screen.getByPlaceholderText("Paste your OpenAI API key").closest("form")!;
    expect(codexForm.contains(matches[0])).toBe(true);
    expect(keyForm.contains(matches[0])).toBe(false);
    // Visible without opening the disclosure.
    expect(matches[0].closest("details")).toBeNull();
  });

  it("renders a keyboard-reachable status legend explaining all four statuses, and keeps per-badge sr-only scaffolding", () => {
    renderCard([secret()]);
    const summary = screen.getByText("What do these statuses mean?");
    expect(summary.tagName).toBe("SUMMARY");
    const legend = summary.closest("details")!;
    // All four status meanings are enumerated.
    for (const status of ["staging", "linked", "failed", "static"]) {
      expect(within(legend).getByText(status)).toBeTruthy();
    }
    expect(within(legend).getByText(/has not been verified/i)).toBeTruthy();
    expect(within(legend).getByText(/No\s+provider\s+or\s+secret\s+details/i)).toBeTruthy();
    // The staging entry says what happens next: uzi verifies it automatically.
    expect(within(legend).getByText(/uzi verifies a new Codex login automatically/)).toBeTruthy();
    // The per-badge sr-only description + aria-describedby scaffolding is retained.
    const badge = within(screen.getByTestId("codex-sec-1")).getByText("staging");
    expect(badge.getAttribute("aria-describedby")).toBe("codex-status-sec-1");
    const srOnly = document.getElementById("codex-status-sec-1");
    expect(srOnly?.className).toContain("sr-only");
    expect(srOnly?.textContent).toBe(
      "Saved and encrypted; identity not yet verified. uzi verifies a new Codex login automatically; it moves to linked once verified.",
    );
  });

  it("renders all four status badges straight from codex_status", () => {
    renderCard([
      secret({ id: "sec-1", codex_status: "staging" }),
      secret({ id: "sec-2", label: "l", is_default: false, codex_status: "linked" }),
      secret({ id: "sec-3", label: "f", is_default: false, codex_status: "failed" }),
      secret({
        id: "sec-4",
        kind: "openai_api_key",
        label: "s",
        is_default: false,
        codex_status: "static",
      }),
    ]);
    expect(within(screen.getByTestId("codex-sec-1")).getByText("staging")).toBeTruthy();
    expect(within(screen.getByTestId("codex-sec-2")).getByText("linked")).toBeTruthy();
    expect(within(screen.getByTestId("codex-sec-3")).getByText("failed")).toBeTruthy();
    expect(within(screen.getByTestId("codex-sec-4")).getByText("static")).toBeTruthy();
  });

  // The wrong-shape pre-check: each bad paste shows its OWN message, sends NOTHING,
  // and surfaces a help link. Driven in first mode so the Save button needs no Name.
  it("blocks a raw-token paste with a raw-token message and does not call createCodexAuth", () => {
    renderCard([]);
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: { value: "sk-not-json-raw-token" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    expect(screen.getByText(/not a raw token/i)).toBeTruthy();
    expect(mockApi.createCodexAuth).not.toHaveBeenCalled();
    // The "How to get this" affordance is the disclosure <summary> (a single
    // named control the message points at), not a link; the one docs link in the
    // block is "Full guide".
    expect(screen.getByText("How to get this", { selector: "summary" }).tagName).toBe(
      "SUMMARY",
    );
    expect(screen.getByRole("link", { name: /full guide/i })).toBeTruthy();
  });

  it("blocks a whole Codex auth.json paste with the flat-object message and sends nothing", () => {
    renderCard([]);
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: {
        value: '{"tokens":{"access_token":"a","refresh_token":"b"},"OPENAI_API_KEY":null}',
      },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    expect(screen.getByText(/looks like the whole/i)).toBeTruthy();
    expect(mockApi.createCodexAuth).not.toHaveBeenCalled();
  });

  it("blocks a JSON object with no access_token and sends nothing", () => {
    renderCard([]);
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: { value: '{"refresh_token":"b"}' },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    expect(screen.getByText(/has no access_token/i)).toBeTruthy();
    expect(mockApi.createCodexAuth).not.toHaveBeenCalled();
  });

  it("codexShapeError accepts a flat object and rejects the mistaken shapes (pure, secret-free)", () => {
    // Valid: a flat object with a non-empty access_token → null (acceptable).
    expect(codexShapeError('{"access_token":"a","refresh_token":"b"}')).toBeNull();
    // The three rejected shapes each get their own message…
    expect(codexShapeError("raw-token")).toMatch(/not a raw token/);
    expect(codexShapeError('{"tokens":{"access_token":"a"}}')).toMatch(
      /looks like the whole/,
    );
    expect(codexShapeError('{"foo":"bar"}')).toMatch(/has no access_token/);
    // …and no message ever echoes the pasted value.
    const secretish = "super-secret-value-1234";
    expect(codexShapeError(secretish)).not.toContain(secretish);
  });

  it("blocks a wrong-shape codex_auth ROTATION inline and does not call patchCodexAuth", () => {
    renderCard([
      secret(),
      secret({ id: "sec-2", label: "codex-2", is_default: false }),
    ]);
    // Target the codex_auth default for a value replacement.
    fireEvent.change(screen.getByLabelText("Credential to replace"), {
      target: { value: "sec-1" },
    });
    fireEvent.change(screen.getByPlaceholderText("Paste the replacement credential"), {
      target: { value: "raw-token-not-json" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Replace value" }));
    expect(screen.getByText(/not a raw token/i)).toBeTruthy();
    expect(mockApi.patchCodexAuth).not.toHaveBeenCalled();
  });

  it("names the resulting status in the post-save notice — staging for a login", async () => {
    mockApi.createCodexAuth.mockResolvedValue({ secret: secret() });
    const onNotice = vi.fn();
    renderCard([], noop, { onNotice });
    fireEvent.change(screen.getByPlaceholderText("Paste your Codex login JSON"), {
      target: { value: VALID_CODEX_JSON },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save Codex login" }));
    await waitFor(() =>
      expect(onNotice).toHaveBeenCalledWith(
        "Saved and encrypted. Status: staging (not verified).",
      ),
    );
  });

  it("names the resulting status in the post-save notice — static for an API key", async () => {
    mockApi.createOpenAIApiKey.mockResolvedValue({ secret: secret() });
    const onNotice = vi.fn();
    renderCard([], noop, { onNotice });
    fireEvent.change(screen.getByPlaceholderText("Paste your OpenAI API key"), {
      target: { value: "sk-dummy-openai" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save OpenAI API key" }));
    await waitFor(() =>
      expect(onNotice).toHaveBeenCalledWith(
        "Saved and encrypted. Status: static.",
      ),
    );
  });

  it("no longer says Codex does not run agents or that logins are not auto-verified (#1594)", () => {
    const { container } = renderCard([secret()]);
    // Presence on the same render: the header and the accurate staging copy are there.
    expect(screen.getByText(/Store your OpenAI Codex logins and API keys\./)).toBeTruthy();
    expect(container.textContent).toContain("uzi verifies a new Codex login automatically");
    // The retired sentences are gone from visible and sr-only text alike.
    expect(container.textContent).not.toContain("Codex is not yet used to run agents");
    expect(container.textContent).not.toContain(
      "This build does not verify Codex logins automatically",
    );
  });
});
