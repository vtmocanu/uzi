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
      // ONLY the Codex wrappers — this card uses no workers/rate-limit APIs. The last two
      // are the issue #3719 pool toggle and account-mode write.
      createCodexAuth: vi.fn(),
      testSecret: vi.fn(),
      patchCodexAuth: vi.fn(),
      deleteCodexAuthById: vi.fn(),
      createOpenAIApiKey: vi.fn(),
      patchOpenAIApiKey: vi.fn(),
      deleteOpenAIApiKeyById: vi.fn(),
      setCodexAliasAutoEligible: vi.fn(),
      setCodexAccountMode: vi.fn(),
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
    enabled: true,
    disabled_at: null,
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
  handlers: {
    onNotice?: (m: string) => void;
    onError?: (m: string) => void;
    accountMode?: string;
    onAccountModeChange?: (m: "default" | "auto") => void;
  } = {},
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
        accountMode={handlers.accountMode}
        onAccountModeChange={handlers.onAccountModeChange}
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

describe("Codex/OpenAI Test action", () => {
  it("routes both kinds, announces sanitized outcomes, and refreshes Codex status", async () => {
    mockApi.testSecret.mockResolvedValueOnce({ status: "ok", display: "models endpoint accessible" })
      .mockResolvedValueOnce({ status: "permission_denied" });
    const reload = vi.fn(async () => {});
    renderCard([secret(), secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false })], reload);
    expect(mockApi.testSecret).not.toHaveBeenCalled();
    const login = screen.getByTestId("codex-sec-1");
    const key = screen.getByTestId("codex-sec-2");
    fireEvent.click(within(key).getByRole("button", { name: "Test console-key" }));
    await waitFor(() => expect(within(key).getByRole("status").textContent).toContain("models endpoint accessible"));
    fireEvent.click(within(login).getByRole("button", { name: "Test default" }));
    await waitFor(() => expect(within(login).getByRole("status").textContent).toMatch(/lacks permission/i));
    expect(mockApi.testSecret).toHaveBeenNthCalledWith(1, "openai_api_key", "sec-2");
    expect(mockApi.testSecret).toHaveBeenNthCalledWith(2, "codex_auth", "sec-1");
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  });

  it("clears the verdict when a credential is replaced", async () => {
    mockApi.testSecret.mockResolvedValue({ status: "rejected" });
    mockApi.patchOpenAIApiKey.mockResolvedValue({ secret: secret() });
    renderCard([secret(), secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false })]);
    const row = screen.getByTestId("codex-sec-2");
    fireEvent.click(within(row).getByRole("button", { name: "Test console-key" }));
    await waitFor(() => expect(within(row).getByRole("status").textContent).toMatch(/rejected/i));
    fireEvent.change(screen.getByLabelText("Credential to replace"), { target: { value: "sec-2" } });
    fireEvent.change(screen.getByPlaceholderText("Paste the replacement credential"), { target: { value: "replacement" } });
    fireEvent.click(screen.getByRole("button", { name: "Replace value" }));
    await waitFor(() => expect(within(row).getByRole("status").textContent).toBe(""));
    expect(mockApi.testSecret).toHaveBeenCalledTimes(1);
  });
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
    // ...but only while Codex usage polling is on, and a rejected login goes to failed.
    expect(legend.textContent).toContain("while Codex usage polling is on (the default)");
    expect(legend.textContent).toContain("or failed if the provider rejects it");
    // The failed entry points at a NEW dedicated login, never a re-paste.
    expect(legend.textContent).toContain("Replace it with a new login dedicated to uzi");
    expect(legend.textContent).not.toContain("re-add the login");
    // The per-badge sr-only description + aria-describedby scaffolding is retained.
    const badge = within(screen.getByTestId("codex-sec-1")).getByText("staging");
    expect(badge.getAttribute("aria-describedby")).toBe("codex-status-sec-1");
    const srOnly = document.getElementById("codex-status-sec-1");
    expect(srOnly?.className).toContain("sr-only");
    expect(srOnly?.textContent).toBe(
      "Saved and encrypted; identity not yet verified. uzi verifies a new Codex login automatically while Codex usage polling is on (the default); it moves to linked once verified, or failed if the provider rejects it.",
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
    // A failed login's hint sends the user to a NEW dedicated login, not a re-paste.
    const failedBadge = within(screen.getByTestId("codex-sec-3")).getByText("failed");
    expect(failedBadge.getAttribute("title")).toBe(
      "Verification failed. Replace it with a new login dedicated to uzi (see “How to get this”), never a re-paste of the old one.",
    );
    expect(screen.getByTestId("codex-sec-3").textContent).not.toContain("re-add the login");
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

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ vaultMode: "protected", hasPassword: true }) }));

// ── Issue #3719: the Codex auto-selection pool and account mode ─────────────────────

describe("Codex pool checkbox", () => {
  const linked = (over: Partial<SecretMeta> = {}) =>
    secret({ codex_status: "linked", ...over });

  it("appears on codex_auth rows only, never on an OpenAI API key", () => {
    renderCard([
      linked(),
      secret({ id: "sec-2", kind: "openai_api_key", label: "console-key", is_default: false, codex_status: "static" }),
    ]);
    expect(within(screen.getByTestId("codex-sec-1")).getByRole("checkbox", { name: "Auto-select from default" })).toBeTruthy();
    // Paired with the positive above: the same query on the key row finds nothing.
    expect(within(screen.getByTestId("codex-sec-2")).queryByRole("checkbox")).toBeNull();
  });

  it("pools a login, announces it, and refetches the list rather than trusting the answer", async () => {
    // The answer omits codex_status (as the server's does); the card must not patch its
    // cached row from it, so the only way the row can change is the reload.
    mockApi.setCodexAliasAutoEligible.mockResolvedValue({
      secret: { ...linked(), auto_eligible: true, codex_status: undefined },
    });
    const reload = vi.fn(async () => {});
    const onNotice = vi.fn();
    renderCard([linked()], reload, { onNotice });
    fireEvent.click(screen.getByRole("checkbox", { name: "Auto-select from default" }));
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(mockApi.setCodexAliasAutoEligible).toHaveBeenCalledWith("sec-1", true);
    expect(onNotice).toHaveBeenLastCalledWith("“default” is now in the Codex auto-selection pool.");
  });

  it("un-pools a pooled login", async () => {
    mockApi.setCodexAliasAutoEligible.mockResolvedValue({ secret: linked({ auto_eligible: false }) });
    const reload = vi.fn(async () => {});
    const onNotice = vi.fn();
    renderCard([linked({ auto_eligible: true })], reload, { onNotice });
    const box = screen.getByRole("checkbox", { name: "Auto-select from default" }) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(mockApi.setCodexAliasAutoEligible).toHaveBeenCalledWith("sec-1", false);
    expect(onNotice).toHaveBeenLastCalledWith("“default” is no longer in the Codex auto-selection pool.");
  });

  it("explains a 409 for a login disabled since the list loaded, and still refetches", async () => {
    const { ApiError } = await import("../lib/apiError");
    mockApi.setCodexAliasAutoEligible.mockRejectedValue(
      new ApiError(409, "credential is disabled; enable it in Settings"),
    );
    const reload = vi.fn(async () => {});
    const onError = vi.fn();
    renderCard([linked({ id: "sec-9", label: "laptop", is_default: false }), linked()], reload, { onError });
    fireEvent.click(screen.getByRole("checkbox", { name: "Auto-select from laptop" }));
    await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
    expect(onError).toHaveBeenLastCalledWith("“laptop” is disabled, so it cannot join the pool. Enable it first.");
  });

  it("says in visible text when a pooled login is not linked, and describes the box with it", () => {
    renderCard([secret({ auto_eligible: true, codex_status: "staging" })]);
    const hint = screen.getByText("In the pool, but uzi picks only linked logins.");
    const box = screen.getByRole("checkbox", { name: "Auto-select from default" });
    expect(box.getAttribute("aria-describedby")).toBe(hint.id);
  });

  // The box is disabled while its write runs, which drops a keyboard user's focus to
  // <body>; the row hands focus back to the box once it is enabled again.
  it("returns keyboard focus to the box after the write", async () => {
    let resolve!: (v: { secret: SecretMeta }) => void;
    mockApi.setCodexAliasAutoEligible.mockReturnValue(new Promise((r) => (resolve = r)));
    renderCard([linked()]);
    const box = screen.getByRole("checkbox", { name: "Auto-select from default" }) as HTMLInputElement;
    fireEvent.click(box);
    await waitFor(() => expect(box.disabled).toBe(true));
    // A browser drops focus from a control that becomes disabled; jsdom does not, so the
    // test starts from that dropped state: focus is on <body> while the write runs.
    expect(document.activeElement).toBe(document.body);
    resolve({ secret: linked({ auto_eligible: true }) });
    await waitFor(() => expect(document.activeElement).toBe(box));
  });

  it("mentions the pool in the delete confirmation of a pooled login", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderCard([linked(), linked({ id: "sec-9", label: "laptop", is_default: false, auto_eligible: true })]);
    fireEvent.click(within(screen.getByTestId("codex-sec-9")).getByRole("button", { name: "Delete" }));
    expect(confirm.mock.calls[0][0]).toMatch(/leaves your Codex auto-selection pool/);
  });
});

describe("Codex account mode picker", () => {
  it("is hidden until the mode is known", () => {
    renderCard([secret()]);
    expect(screen.queryByRole("group", { name: "Which login a new Codex run uses" })).toBeNull();
  });

  it("shows the stored mode as the checked tile", () => {
    renderCard([secret()], noop, { accountMode: "default" });
    const group = screen.getByRole("group", { name: "Which login a new Codex run uses" });
    expect((within(group).getByRole("radio", { name: /Use the default/ }) as HTMLInputElement).checked).toBe(true);
    expect((within(group).getByRole("radio", { name: /Pick by headroom/ }) as HTMLInputElement).checked).toBe(false);
  });

  it("switches to auto through the API and reports the saved mode", async () => {
    mockApi.setCodexAccountMode.mockResolvedValue({ codex_account_mode: "auto" });
    const onAccountModeChange = vi.fn();
    const onNotice = vi.fn();
    renderCard([secret()], noop, { accountMode: "default", onAccountModeChange, onNotice });
    fireEvent.click(screen.getByRole("radio", { name: /Pick by headroom/ }));
    await waitFor(() => expect(onAccountModeChange).toHaveBeenCalledWith("auto"));
    expect(mockApi.setCodexAccountMode).toHaveBeenCalledWith("auto");
    expect(onNotice).toHaveBeenLastCalledWith(
      "New Codex runs now pick the pooled login with the most limit headroom.",
    );
  });

  // A disabled radio loses keyboard focus, so the group must stay enabled while the write
  // runs; a second pick in that window is ignored instead.
  it("keeps the radios enabled while the write is in flight, and ignores a second pick", async () => {
    let resolve!: (v: { codex_account_mode: "default" | "auto" }) => void;
    mockApi.setCodexAccountMode.mockReturnValue(new Promise((r) => (resolve = r)));
    renderCard([secret()], noop, { accountMode: "default" });
    const auto = screen.getByRole("radio", { name: /Pick by headroom/ }) as HTMLInputElement;
    fireEvent.click(auto);
    await waitFor(() => expect(mockApi.setCodexAccountMode).toHaveBeenCalledTimes(1));
    expect(auto.matches(":disabled")).toBe(false);
    fireEvent.click(screen.getByRole("radio", { name: /Use the default/ }));
    expect(mockApi.setCodexAccountMode).toHaveBeenCalledTimes(1);
    resolve({ codex_account_mode: "auto" });
  });

  it("keeps the mode and surfaces the error when the write fails", async () => {
    const { ApiError } = await import("../lib/apiError");
    mockApi.setCodexAccountMode.mockRejectedValue(new ApiError(500, "internal error"));
    const onAccountModeChange = vi.fn();
    const onError = vi.fn();
    renderCard([secret()], noop, { accountMode: "auto", onAccountModeChange, onError });
    fireEvent.click(screen.getByRole("radio", { name: /Use the default/ }));
    await waitFor(() => expect(onError).toHaveBeenLastCalledWith("internal error"));
    expect(onAccountModeChange).not.toHaveBeenCalled();
  });

  it("hints that auto uses the default until a login is pooled", () => {
    renderCard([secret({ codex_status: "linked" })], noop, { accountMode: "auto" });
    expect(screen.getByText(/runs use your default until you pool one/)).toBeTruthy();
  });

  it("warns that Codex is unavailable when no pooled login is enabled and linked", () => {
    renderCard(
      [
        secret({ codex_status: "linked" }),
        secret({ id: "sec-2", label: "laptop", is_default: false, auto_eligible: true, codex_status: "failed" }),
        secret({
          id: "sec-3", label: "old", is_default: false, auto_eligible: true, codex_status: "linked",
          enabled: false, disabled_at: "2026-01-03T00:00:00Z",
        }),
      ],
      noop,
      { accountMode: "auto" },
    );
    expect(screen.getByText(/^Codex unavailable: no pooled login is enabled and linked, so runs fall back to Claude/)).toBeTruthy();
  });

  it("counts the pickable logins when the pool is usable", () => {
    renderCard(
      [
        secret({ codex_status: "linked", auto_eligible: true }),
        secret({ id: "sec-2", label: "laptop", is_default: false, auto_eligible: true, codex_status: "staging" }),
      ],
      noop,
      { accountMode: "auto" },
    );
    expect(screen.getByText("uzi picks from 1 of 2 logins in the pool; the others are not linked or are disabled.")).toBeTruthy();
    // Paired with the positive above: a usable pool never shows the unavailable warning.
    expect(screen.queryByText(/Codex unavailable/)).toBeNull();
  });

  it("says the pool is idle in default mode", () => {
    renderCard([secret({ codex_status: "linked", auto_eligible: true })], noop, { accountMode: "default" });
    expect(screen.getByText("1 login is in the pool. The pool is used only when uzi picks by headroom.")).toBeTruthy();
  });
});
