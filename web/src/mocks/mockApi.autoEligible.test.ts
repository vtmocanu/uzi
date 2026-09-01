// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// issue #804: the api now births a user's FIRST/SOLE anthropic_token with
// auto_eligible=true, so a single-token user has a non-empty auto-select pool;
// token #2+ stays opt-in (auto_eligible=false). Both mock creation paths —
// createAnthropicToken (PRD #104 M2) and the putAnthropicToken D14 alias — must
// mirror that first-token-only rule, or the mock teaches the wrong lesson. The
// multi-token assertion is REQUIRED so the mock cannot silently diverge from the
// server's first-token-ONLY behaviour by making every token eligible.
//
// Each test reloads the module against a fresh (empty) localStorage so the
// in-memory store re-seeds, then clears the seeded anthropic tokens to drive the
// first-token rule from an empty pool. The demo vault seeds UNLOCKED, so no
// explicit unlock is needed before a create.

const KEY = "uzi.mock.v3";

function installStorage(initial: Record<string, string> = {}): void {
  const m = new Map<string, string>(Object.entries(initial));
  const storage = {
    getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
    setItem: (k: string, v: string) => void m.set(k, String(v)),
    removeItem: (k: string) => void m.delete(k),
    clear: () => m.clear(),
    key: (i: number) => [...m.keys()][i] ?? null,
    get length() {
      return m.size;
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", { configurable: true, value: storage });
}

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

// Remove every seeded anthropic token so the next create is genuinely the first.
// The default cannot be deleted while siblings exist (D6), so drop the
// non-defaults first, then the (now sole) default.
async function clearAnthropicTokens(api: Awaited<ReturnType<typeof reload>>): Promise<void> {
  const toks = (await api.listSecrets()).secrets.filter((s) => s.kind === "anthropic_token");
  for (const s of toks.filter((t) => !t.is_default)) await api.deleteAnthropicTokenById(s.id);
  for (const s of toks.filter((t) => t.is_default)) await api.deleteAnthropicTokenById(s.id);
  const after = (await api.listSecrets()).secrets.filter((s) => s.kind === "anthropic_token");
  expect(after).toHaveLength(0);
}

beforeEach(() => installStorage({ [KEY]: "" }));
afterEach(() => vi.resetModules());

describe("mockApi — anthropic_token auto_eligible birth rule (issue #804)", () => {
  it("createAnthropicToken: the FIRST token is born eligible, the SECOND is opt-in", async () => {
    const api = await reload();
    await clearAnthropicTokens(api);

    const { secret: first } = await api.createAnthropicToken("sk-ant-mock-first", "first-key", false);
    expect(first.auto_eligible).toBe(true);

    const { secret: second } = await api.createAnthropicToken("sk-ant-mock-second", "second-key", false);
    expect(second.auto_eligible).toBe(false);
  });

  it("putAnthropicToken (D14 alias): the sole first token is born eligible", async () => {
    const api = await reload();
    await clearAnthropicTokens(api);

    const { secret } = await api.putAnthropicToken("sk-ant-mock-default");
    expect(secret.label).toBe("default");
    expect(secret.auto_eligible).toBe(true);
  });

  it("putAnthropicToken then createAnthropicToken: the second token via the CRUD path is opt-in", async () => {
    const api = await reload();
    await clearAnthropicTokens(api);

    const { secret: first } = await api.putAnthropicToken("sk-ant-mock-default");
    expect(first.auto_eligible).toBe(true);

    const { secret: second } = await api.createAnthropicToken("sk-ant-mock-next", "next-key", false);
    expect(second.auto_eligible).toBe(false);
  });
});
