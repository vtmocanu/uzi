// @vitest-environment jsdom
//
// TokenPicker (PRD #1247 M7): the shared four-state credential picker. It must render
// ALL FOUR states — inherit, default, auto, pin — even with ZERO or ONE token (unlike
// the Workers rebind select, which hides itself below two tokens), reflect its
// controlled value, emit the right selection on change, and sanitize a user-authored
// token label before rendering it.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { TokenPicker } from "./TokenPicker";
import type { SecretMeta } from "../lib/api";
import type { CredentialSelection } from "../lib/credentialOverride";

function token(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "console-key",
    is_default: false,
    enabled: true,
    disabled_at: null,
    auto_eligible: true,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function renderPicker(value: CredentialSelection, tokens: SecretMeta[]) {
  const onChange = vi.fn();
  render(<TokenPicker value={value} onChange={onChange} tokens={tokens} label="Anthropic token" />);
  const select = screen.getByLabelText("Anthropic token") as HTMLSelectElement;
  return { onChange, select };
}

afterEach(cleanup);

describe("TokenPicker — always renders the four states", () => {
  it.each([
    ["zero tokens", [] as SecretMeta[], 0],
    ["one token", [token()], 1],
    [
      "many tokens",
      [token({ id: "a", label: "one" }), token({ id: "b", label: "two" }), token({ id: "c", label: "three" })],
      3,
    ],
  ])("renders inherit/default/auto plus one option per token with %s", (_name, tokens, pinnedCount) => {
    renderPicker({ mode: "inherit" }, tokens);
    // The three account-level states are present regardless of how many tokens exist.
    expect(screen.getByRole("option", { name: /Inherit the worker/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Use my default token/ })).toBeTruthy();
    expect(screen.getByRole("option", { name: /Auto-select from my pool/ })).toBeTruthy();
    // Plus exactly one pin option per token — 3 base options + the pinned ones.
    expect(screen.getAllByRole("option")).toHaveLength(3 + pinnedCount);
  });
});

describe("TokenPicker — controlled value + change events", () => {
  it("reflects the controlled selection", () => {
    const { select } = renderPicker({ mode: "pinned", secret_id: "sec-1" }, [token()]);
    const pinned = screen.getByRole("option", { name: /console-key/ }) as HTMLOptionElement;
    expect(select.value).toBe(pinned.value);
    expect(pinned.value).toBe("sec-1");
  });

  it("emits {mode:'auto'} when switched to auto", () => {
    const { onChange, select } = renderPicker({ mode: "inherit" }, [token()]);
    const auto = screen.getByRole("option", { name: /Auto-select from my pool/ }) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: auto.value } });
    expect(onChange).toHaveBeenCalledWith({ mode: "auto" });
  });

  it("emits {mode:'pinned', secret_id} when a token is pinned", () => {
    const { onChange, select } = renderPicker({ mode: "inherit" }, [token({ id: "sec-9", label: "prod" })]);
    const opt = screen.getByRole("option", { name: /prod/ }) as HTMLOptionElement;
    fireEvent.change(select, { target: { value: opt.value } });
    expect(onChange).toHaveBeenCalledWith({ mode: "pinned", secret_id: "sec-9" });
  });

  it("marks the account default token", () => {
    renderPicker({ mode: "inherit" }, [token({ id: "sec-d", label: "primary", is_default: true })]);
    expect(screen.getByRole("option", { name: /primary \(your default\)/ })).toBeTruthy();
  });
});

describe("TokenPicker — label sanitization", () => {
  it("strips a bidi-override (Cf) character from the rendered token label + its title", () => {
    // U+202E RIGHT-TO-LEFT OVERRIDE is category Cf; sanitizeLabel drops it. Built from a
    // code point at runtime so no invisible byte lands in this source file.
    const bidi = String.fromCodePoint(0x202e);
    renderPicker({ mode: "inherit" }, [token({ id: "sec-x", label: `console${bidi}key` })]);
    const opt = screen.getByRole("option", { name: /consolekey/ }) as HTMLOptionElement;
    // The user-authored label reaches the DOM sanitized, on both the text and the title.
    expect(opt.getAttribute("title")).toBe("consolekey");
    expect(opt.getAttribute("title")).not.toContain(bidi);
    expect(opt.textContent).not.toContain(bidi);
  });
});

// PRD #1732: pickers never offer a disabled token (the server refuses a new pin to one,
// D5). They say how many they left out and link to Settings, and a selection ALREADY
// pinned to a disabled token names it "(disabled)" instead of snapping elsewhere.
describe("TokenPicker — disabled tokens (PRD #1732)", () => {
  const off = () =>
    token({ id: "sec-off", label: "old-laptop", enabled: false, disabled_at: "2026-01-05T00:00:00Z" });

  function renderRouted(value: CredentialSelection, tokens: SecretMeta[]) {
    render(
      <MemoryRouter>
        <TokenPicker value={value} onChange={vi.fn()} tokens={tokens} label="Anthropic token" />
      </MemoryRouter>,
    );
  }

  it("omits a disabled token and shows the 'N disabled not listed' footer with a Settings link", () => {
    renderRouted({ mode: "inherit" }, [token(), off()]);
    expect(screen.queryByRole("option", { name: /old-laptop/ })).toBeNull();
    // 3 base states + the one enabled token.
    expect(screen.getAllByRole("option")).toHaveLength(4);
    const footer = screen.getByTestId("token-picker-disabled-footer");
    expect(footer.textContent).toContain("1 disabled not listed");
    expect(screen.getByRole("link", { name: "Manage tokens" }).getAttribute("href")).toBe("/settings");
  });

  it("renders no footer when nothing is disabled", () => {
    renderRouted({ mode: "inherit" }, [token()]);
    expect(screen.queryByTestId("token-picker-disabled-footer")).toBeNull();
  });

  it("names an existing pin to a disabled token as '(disabled)', never as another option", () => {
    renderRouted({ mode: "pinned", secret_id: "sec-off", label: "old-laptop" }, [token(), off()]);
    const select = screen.getByLabelText("Anthropic token") as HTMLSelectElement;
    const selected = select.options[select.selectedIndex];
    expect(selected.textContent).toBe("old-laptop (disabled)");
    expect(selected.disabled).toBe(true);
    expect(screen.getByTestId("token-picker-disabled-footer").textContent).toMatch(/“old-laptop” is disabled/);
  });
});
