// @vitest-environment jsdom
import { StrictMode, useLayoutEffect, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { api } from "./api";
import type { EffectiveSettings, StartPreview, StartPreviewRequest } from "./apiTypes";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { deferred } from "../test-helpers";
import { DEFAULT_START_PREVIEW_REQUEST } from "./startPreview";
import { StartPreviewProvider, useStartPreview } from "./startPreviewStore";

const SCENARIO = RECORDED_EFFECTIVE_SCENARIOS[0];
const GET: EffectiveSettings = SCENARIO.get;
const byName = (n: string): StartPreview => structuredClone(SCENARIO.start_previews.find((p) => p.name === n)!.response);
const CLAUDE = byName("claude");
const CODEX = byName("codex");

const REQ_CLAUDE: StartPreviewRequest = { harness: "claude", credential_override: null };
const REQ_CODEX: StartPreviewRequest = { harness: "codex", credential_override: null };

// Mutable api surface: the tests replace the two methods per case.
const apiMock = api as unknown as {
  getEffectiveSettings: ReturnType<typeof vi.fn> | undefined;
  previewStartSettings: ReturnType<typeof vi.fn> | undefined;
};
let original: { g: unknown; p: unknown };

beforeEach(() => {
  original = { g: apiMock.getEffectiveSettings, p: apiMock.previewStartSettings };
  apiMock.getEffectiveSettings = vi.fn(() => Promise.resolve(GET));
  apiMock.previewStartSettings = vi.fn((req: StartPreviewRequest) => Promise.resolve(req.harness === "codex" ? CODEX : CLAUDE));
});

afterEach(() => {
  cleanup();
  apiMock.getEffectiveSettings = original.g as never;
  apiMock.previewStartSettings = original.p as never;
});

interface Props {
  userId: string | null;
  generation: number | null;
  request: StartPreviewRequest | null;
}

// The store reads only user.id from useOptionalAuth; the mock serves the id the test sets.
const auth = vi.hoisted(() => ({ userId: null as string | null }));
vi.mock("../auth/AuthContext", () => ({
  useOptionalAuth: () => ({ user: auth.userId ? { id: auth.userId } : null }),
}));

// A harness whose props drive both the provider and the hook.
function mount(initial: Props) {
  let current = initial;
  auth.userId = current.userId;
  const result = renderHook(
    () => useStartPreview(current.request),
    {
      wrapper: ({ children }: { children?: ReactNode }) => (
        <StartPreviewProvider generation={current.generation}>{children}</StartPreviewProvider>
      ),
    },
  );
  return {
    result,
    update(next: Partial<Props>) {
      current = { ...current, ...next };
      auth.userId = current.userId;
      result.rerender();
    },
  };
}

const getCalls = () => apiMock.getEffectiveSettings!.mock.calls.length;
const postCalls = () => apiMock.previewStartSettings!.mock.calls.length;

describe("StartPreviewProvider / useStartPreview", () => {
  it("returns null outside a provider", () => {
    const { result } = renderHook(() => useStartPreview(DEFAULT_START_PREVIEW_REQUEST));
    expect(result.current).toBeNull();
  });

  it("fetches nothing while the generation is null, then one GET + one POST on the first apply", async () => {
    const m = mount({ userId: "u1", generation: null, request: DEFAULT_START_PREVIEW_REQUEST });
    await act(async () => {
      await Promise.resolve();
    });
    expect(getCalls()).toBe(0);
    expect(postCalls()).toBe(0);
    expect(m.result.result.current?.status).toBe("loading");

    m.update({ generation: 1 });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    expect(getCalls()).toBe(1);
    expect(postCalls()).toBe(1);
    expect(m.result.result.current?.view?.harness).toBe("claude");
  });

  it("fetches nothing without a user, and logout clears the published value", async () => {
    const none = mount({ userId: null, generation: 1, request: REQ_CLAUDE });
    await act(async () => {
      await Promise.resolve();
    });
    expect(getCalls() + postCalls()).toBe(0);
    expect(none.result.result.current?.status).toBe("loading");
    cleanup();

    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    const before = getCalls() + postCalls();
    m.update({ userId: null });
    expect(m.result.result.current?.status).toBe("loading");
    await act(async () => {
      await Promise.resolve();
    });
    expect(m.result.result.current?.view).toBeNull();
    expect(getCalls() + postCalls()).toBe(before);
  });

  it("shares one GET and one POST between consumers of the same choice, and one POST per distinct choice", async () => {
    auth.userId = "u1";
    const results = renderHook(
      () => ({
        a: useStartPreview(DEFAULT_START_PREVIEW_REQUEST),
        b: useStartPreview(DEFAULT_START_PREVIEW_REQUEST),
        c: useStartPreview(REQ_CODEX),
        pinnedA: useStartPreview({ harness: null, credential_override: { mode: "pinned", secret_id: "a" } }),
        pinnedB: useStartPreview({ harness: null, credential_override: { mode: "pinned", secret_id: "b" } }),
      }),
      {
        wrapper: ({ children }: { children?: ReactNode }) => (
          <StartPreviewProvider generation={1}>{children}</StartPreviewProvider>
        ),
      },
    );
    await waitFor(() => expect(results.result.current.pinnedB?.status).toBe("ready"));
    expect(getCalls()).toBe(1);
    expect(postCalls()).toBe(4);
  });

  it("deduplicates an in-flight request across rerenders", async () => {
    const d = deferred<StartPreview>();
    apiMock.previewStartSettings = vi.fn(() => d.promise);
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    m.update({});
    m.update({});
    await act(async () => {
      await Promise.resolve();
    });
    expect(postCalls()).toBe(1);
    await act(async () => d.resolve(CLAUDE));
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
  });

  it("refetches on a generation bump and keeps the published value until the new scope answers", async () => {
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    const d = deferred<StartPreview>();
    apiMock.previewStartSettings = vi.fn(() => d.promise);
    m.update({ generation: 2 });
    expect(m.result.result.current?.status).toBe("ready");
    expect(m.result.result.current?.view?.harness).toBe("claude");
    await waitFor(() => expect(postCalls()).toBe(1));
    expect(getCalls()).toBe(2);
    await act(async () => d.resolve(byName("claude with a model")));
    await waitFor(() => expect(m.result.result.current?.view?.model?.source).toBe("this run"));
  });

  it("discards the superseded generation's response when it arrives AFTER the current one", async () => {
    const d1 = deferred<StartPreview>();
    const d2 = deferred<StartPreview>();
    const queue = [d1, d2];
    apiMock.previewStartSettings = vi.fn(() => queue.shift()!.promise);
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(postCalls()).toBe(1));
    m.update({ generation: 2 });
    await waitFor(() => expect(postCalls()).toBe(2));
    await act(async () => d2.resolve(byName("claude with a model")));
    await waitFor(() => expect(m.result.result.current?.view?.model?.source).toBe("this run"));
    await act(async () => d1.resolve(CLAUDE));
    expect(m.result.result.current?.view?.model?.source).toBe("this run");
  });

  it("discards the superseded generation's response when it arrives BEFORE the current one", async () => {
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    const d1 = deferred<StartPreview>();
    const d2 = deferred<StartPreview>();
    const queue = [d1, d2];
    apiMock.previewStartSettings = vi.fn(() => queue.shift()!.promise);
    m.update({ generation: 2 });
    await waitFor(() => expect(postCalls()).toBe(1));
    // Make gen 2 pending, then supersede it with gen 3 so gen 2's answer is the stale one.
    m.update({ generation: 3 });
    await waitFor(() => expect(postCalls()).toBe(2));
    await act(async () => d1.resolve(byName("claude with a model")));
    // The superseded response was dropped: the prior value stays.
    expect(m.result.result.current?.view?.model?.source).toBe("lead template");
    await act(async () => d2.resolve(byName("effort only on claude")));
    await waitFor(() => expect(m.result.result.current?.view?.effort?.source).toBe("this run"));
  });

  it("invalidates immediately: a pending old generation never publishes, even before the new one answers", async () => {
    const g1 = deferred<EffectiveSettings>();
    const p1 = deferred<StartPreview>();
    const p2 = deferred<StartPreview>();
    apiMock.getEffectiveSettings = vi.fn().mockReturnValueOnce(g1.promise).mockReturnValue(Promise.resolve(GET));
    apiMock.previewStartSettings = vi.fn().mockReturnValueOnce(p1.promise).mockReturnValueOnce(p2.promise);
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(postCalls()).toBe(1));
    m.update({ generation: 2 });
    await waitFor(() => expect(postCalls()).toBe(2));
    expect(getCalls()).toBe(2);
    await act(async () => {
      g1.resolve(GET);
      p1.resolve(CLAUDE);
    });
    expect(m.result.result.current?.status).toBe("loading");
    await act(async () => p2.resolve(byName("claude with a model")));
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    expect(m.result.result.current?.view?.model?.source).toBe("this run");
  });

  it("shows loading with no preview while a new key's POST is pending, then that key's preview", async () => {
    const dCodex = deferred<StartPreview>();
    apiMock.previewStartSettings = vi.fn((req: StartPreviewRequest) => (req.harness === "codex" ? dCodex.promise : Promise.resolve(CLAUDE)));
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    m.update({ request: REQ_CODEX });
    await waitFor(() => expect(postCalls()).toBe(2));
    expect(m.result.result.current?.status).toBe("loading");
    expect(m.result.result.current?.preview).toBeNull();
    await act(async () => dCodex.resolve(CODEX));
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    expect(m.result.result.current?.preview).toEqual(CODEX);
  });

  it("never publishes a superseded scope's response that settles between the commit and its effect cleanup", async () => {
    // Outside act, React commits the new scope and runs the old effect's passive cleanup as a
    // later task; the old POST settles inside that gap (from a layout effect of the new commit).
    const env = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
    const prevEnv = env.IS_REACT_ACT_ENVIRONMENT;
    env.IS_REACT_ACT_ENVIRONMENT = false;
    const p1 = deferred<StartPreview>();
    const queue: Array<Promise<StartPreview>> = [p1.promise, new Promise<StartPreview>(() => {})];
    apiMock.previewStartSettings = vi.fn(() => queue.shift()!);
    auth.userId = "u1";
    const log: string[] = [];
    function Probe({ gen }: { gen: number }) {
      const r = useStartPreview(REQ_CLAUDE);
      log.push(`${gen}:${r?.status}`);
      useLayoutEffect(() => {
        if (gen === 2) p1.resolve(CLAUDE);
      }, [gen]);
      return null;
    }
    const tree = (gen: number) => (
      <StartPreviewProvider generation={gen}>
        <Probe gen={gen} />
      </StartPreviewProvider>
    );
    const container = document.createElement("div");
    const root = createRoot(container);
    const settle = () => new Promise<void>((r) => setTimeout(r, 50));
    try {
      root.render(tree(1));
      await settle();
      expect(postCalls()).toBe(1);
      root.render(tree(2));
      await settle();
      expect(postCalls()).toBe(2);
      expect(log).not.toContain("2:ready");
      expect(log[log.length - 1]).toBe("2:loading");
    } finally {
      root.unmount();
      env.IS_REACT_ACT_ENVIRONMENT = prevEnv;
    }
  });

  it("discards a response for a key the consumer no longer requests", async () => {
    const dClaude = deferred<StartPreview>();
    apiMock.previewStartSettings = vi.fn((req: StartPreviewRequest) => (req.harness === "claude" ? dClaude.promise : Promise.resolve(CODEX)));
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(postCalls()).toBe(1));
    m.update({ request: REQ_CODEX });
    await waitFor(() => expect(m.result.result.current?.view?.harness).toBe("codex"));
    await act(async () => dClaude.resolve(CLAUDE));
    expect(m.result.result.current?.view?.harness).toBe("codex");
  });

  it("never shows user A's value to B, nor a late A response after A -> B -> A", async () => {
    const dA = deferred<StartPreview>();
    const queue: Array<Promise<StartPreview>> = [dA.promise, Promise.resolve(CODEX), new Promise<StartPreview>(() => {})];
    apiMock.previewStartSettings = vi.fn(() => queue.shift()!);
    const m = mount({ userId: "A", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(postCalls()).toBe(1));
    m.update({ userId: "B" });
    await waitFor(() => expect(m.result.result.current?.view?.harness).toBe("codex"));
    m.update({ userId: "A" });
    expect(m.result.result.current?.status).toBe("loading");
    await waitFor(() => expect(postCalls()).toBe(3));
    await act(async () => dA.resolve(CLAUDE));
    expect(m.result.result.current?.status).toBe("loading");
    expect(m.result.result.current?.view).toBeNull();
  });

  it("keeps nothing from a previous user once the identity changes", async () => {
    const m = mount({ userId: "A", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    apiMock.previewStartSettings = vi.fn(() => new Promise<StartPreview>(() => {}));
    m.update({ userId: "B" });
    expect(m.result.result.current?.status).toBe("loading");
    m.update({ userId: "A" });
    await act(async () => {
      await Promise.resolve();
    });
    expect(m.result.result.current?.status).toBe("loading");
  });

  it("deduplicates by the full key including the pinned secret id", async () => {
    const m = mount({ userId: "u1", generation: 1, request: { harness: null, credential_override: { mode: "pinned", secret_id: "a" } } });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    m.update({ request: { harness: null, credential_override: { mode: "pinned", secret_id: "b" } } });
    await waitFor(() => expect(postCalls()).toBe(2));
    m.update({ request: { harness: null, credential_override: { mode: "pinned", secret_id: "a" } } });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    expect(postCalls()).toBe(2);
  });

  it("reports an error when an api method is missing, and keeps the POST rows when only the GET fails", async () => {
    apiMock.previewStartSettings = undefined;
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("error"));
    cleanup();

    apiMock.previewStartSettings = vi.fn(() => Promise.resolve(CLAUDE));
    apiMock.getEffectiveSettings = vi.fn(() => Promise.reject(new Error("down")));
    const n = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(n.result.result.current?.status).toBe("ready"));
    expect(n.result.result.current?.effective).toBeNull();
    expect(n.result.result.current?.view?.model?.text).toBe("unknown");
    expect(n.result.result.current?.view?.credential).toBe("chosen when the run starts");
  });

  it("does not fetch for a null request", async () => {
    const m = mount({ userId: "u1", generation: 1, request: null });
    await act(async () => {
      await Promise.resolve();
    });
    expect(m.result.result.current?.status).toBe("idle");
    expect(getCalls() + postCalls()).toBe(0);
  });

  it("publishes under StrictMode with exactly one GET and one POST per scope", async () => {
    auth.userId = "u1";
    let generation = 1;
    const result = renderHook(() => useStartPreview(REQ_CLAUDE), {
      wrapper: ({ children }: { children?: ReactNode }) => (
        <StrictMode>
          <StartPreviewProvider generation={generation}>{children}</StartPreviewProvider>
        </StrictMode>
      ),
    });
    await waitFor(() => expect(result.result.current?.status).toBe("ready"));
    expect(getCalls()).toBe(1);
    expect(postCalls()).toBe(1);

    generation = 2;
    result.rerender();
    await waitFor(() => expect(postCalls()).toBe(2));
    await waitFor(() => expect(result.result.current?.status).toBe("ready"));
    expect(getCalls()).toBe(2);
    expect(postCalls()).toBe(2);
  });

  it("fetches fresh after an unmount and remount", async () => {
    const m = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(m.result.result.current?.status).toBe("ready"));
    m.result.unmount();
    const n = mount({ userId: "u1", generation: 1, request: REQ_CLAUDE });
    await waitFor(() => expect(n.result.result.current?.status).toBe("ready"));
    expect(getCalls()).toBe(2);
    expect(postCalls()).toBe(2);
  });
});
