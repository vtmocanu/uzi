// @vitest-environment jsdom
//
// useSeededCredential re-seed reactivity (PRD #1247, CodeRabbit finding [O3]). The seed
// effect must react to a CHANGED override, a flipped `enabled`, or a changed injected token
// list — not run once at mount — while NEVER clobbering a user edit (touchedRef). These
// pin exactly those three behaviours with renderHook.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { useSeededCredential } from "./useSeededCredential";
import { api, type CredentialOverride, type SecretMeta } from "./api";

vi.mock("./api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api")>();
  return { ...actual, api: { ...actual.api, listSecrets: vi.fn() } };
});
const mockApi = vi.mocked(api);

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

beforeEach(() => {
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("useSeededCredential — re-seeds when its inputs change (PRD #1247 O3)", () => {
  it("(a) re-seeds an INJECTED picker when the override prop changes while untouched", async () => {
    const props0: { override: CredentialOverride | null; opts: { tokens: SecretMeta[] } } = {
      override: null,
      opts: { tokens: [token()] },
    };
    const { result, rerender } = renderHook(
      ({ override, opts }) => useSeededCredential(override, opts),
      { initialProps: props0 },
    );
    // A null override seeds to inherit.
    expect(result.current.selection).toEqual({ mode: "inherit" });

    // The override changes to a pinned token in the injected list — the picker must re-seed
    // (resolving the label→id) rather than stay on the mount-time inherit.
    rerender({ override: { mode: "pinned", label: "console-key" }, opts: { tokens: [token()] } });
    await waitFor(() =>
      expect(result.current.selection).toEqual({ mode: "pinned", secret_id: "sec-1", label: "console-key" }),
    );
  });

  it("(b) self-fetches when `enabled` flips false → true", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [token(), token({ id: "sec-2", kind: "codex_token", label: "codex" }) as SecretMeta],
    });
    const props0: { override: CredentialOverride | null; opts: { enabled: boolean } } = {
      override: null,
      opts: { enabled: false },
    };
    const { result, rerender } = renderHook(
      ({ override, opts }) => useSeededCredential(override, opts),
      { initialProps: props0 },
    );
    // Disabled: no fetch fires and the pin list is empty.
    expect(mockApi.listSecrets).not.toHaveBeenCalled();
    expect(result.current.tokens).toHaveLength(0);

    // Enabling it late must fire the fetch and populate the (Anthropic-only) token list.
    rerender({ override: null, opts: { enabled: true } });
    await waitFor(() => expect(mockApi.listSecrets).toHaveBeenCalled());
    await waitFor(() => expect(result.current.tokens).toHaveLength(1));
    expect(result.current.tokens[0].id).toBe("sec-1");
  });

  it("(c) does NOT clobber a touched selection when the override later changes", async () => {
    const props0: { override: CredentialOverride | null; opts: { tokens: SecretMeta[] } } = {
      override: null,
      opts: { tokens: [token()] },
    };
    const { result, rerender } = renderHook(
      ({ override, opts }) => useSeededCredential(override, opts),
      { initialProps: props0 },
    );

    // The user edits the picker (marks it touched).
    act(() => result.current.onSelectionChange({ mode: "default" }));
    expect(result.current.selection).toEqual({ mode: "default" });
    expect(result.current.touched).toBe(true);

    // A later override change must be IGNORED — the live edit stands.
    rerender({ override: { mode: "pinned", label: "console-key" }, opts: { tokens: [token()] } });
    await waitFor(() => expect(result.current.selection).toEqual({ mode: "default" }));
  });
});
