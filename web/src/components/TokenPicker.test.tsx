// @vitest-environment jsdom
//
// TokenPicker (PRD #1247 M7): the shared four-state credential picker. It must render
// ALL FOUR states — inherit, default, auto, pin — even with ZERO or ONE token (unlike
// the Workers rebind select, which hides itself below two tokens), reflect its
// controlled value, emit the right selection on change, and sanitize a user-authored
// token label before rendering it.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { TokenPicker } from "./TokenPicker";
import type { SecretMeta } from "../lib/api";
import type { CredentialSelection } from "../lib/credentialOverride";

function token(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-1",
    kind: "anthropic_token",
    label: "console-key",
    is_default: false,
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
