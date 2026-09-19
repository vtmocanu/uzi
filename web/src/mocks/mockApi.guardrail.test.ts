// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";

// issue #1432: the mock is the browsable spec, so requestGuardrailOverride must
// re-implement the server's 422 for a NON-waivable refusal (a protection_unreadable
// finding an admin override cannot clear) rather than queueing a request the server
// would refuse. repo-ledger's seed carries a "could not read" finding (non-waivable);
// repo-billing's is a push-only block (waivable). Each test reloads the module so the
// in-memory repos/queue start fresh.

const KEY = "uzi.mock.v4";

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

beforeEach(() => installStorage({ [KEY]: "" }));
afterEach(() => vi.resetModules());

describe("mockApi — requestGuardrailOverride waivability (issue #1432)", () => {
  it("rejects a NON-waivable repo (repo-ledger) with a 422 waivable:false, without queueing it", async () => {
    const api = await reload();
    const err = await api.requestGuardrailOverride("repo-ledger", "please allow it").catch((e) => e);
    // A 422 whose body says the refusal cannot be waived (module reload gives ApiError a
    // fresh class identity, so assert on the shape, not instanceof).
    expect(err).toMatchObject({ status: 422 });
    expect((err as { body: { waivable: boolean } }).body.waivable).toBe(false);

    // The refused request never lands in the admin queue.
    const { requests } = await api.adminListBlockedRepos();
    expect(requests.some((q) => q.repo_id === "repo-ledger")).toBe(false);
  });

  it("accepts a WAIVABLE repo (repo-billing), queueing a pending request with real coded findings", async () => {
    const api = await reload();
    const { override_request } = await api.requestGuardrailOverride(
      "repo-billing",
      "we tightened the push rule",
    );
    expect(override_request.status).toBe("pending");

    // The queued row carries the classified push finding (not a doomed unreadable one).
    const { requests } = await api.adminListBlockedRepos();
    const queued = requests.find((q) => q.repo_id === "repo-billing");
    expect(queued).toBeTruthy();
    expect(queued!.findings.map((f) => f.code)).toEqual(["write_role_can_push"]);
  });
});
