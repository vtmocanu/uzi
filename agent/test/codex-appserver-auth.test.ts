import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CODEX_REFRESH_BRIDGE_BUDGET_MS,
  CodexAppServerAuthError,
  createCodexAppServerAuth,
  type CodexSubscriptionRefreshBridge,
  type ServerVerifiedCodexAuthTokens,
} from "../src/codex/appserver-auth.js";
import type { CodexNotification, CodexTransport } from "../src/codex/transport.js";

const INITIAL = { accessToken: "initial-access-token", accountId: "server-account-1" };
const REFRESHED = { accessToken: "refreshed-access-token", accountId: "server-account-1" };

interface RecordedCall {
  readonly kind: "request" | "notify" | "respond";
  readonly method?: string;
  readonly params?: unknown;
  readonly requestId?: number | string;
  readonly response?: unknown;
}

class FakeTransport implements CodexTransport {
  readonly calls: RecordedCall[] = [];
  closes = 0;
  private closed = false;
  private interceptor?: (note: CodexNotification, frameBytes: number) => boolean;

  constructor(
    private readonly responder: (method: string, params: unknown) => unknown = defaultResponder,
    private readonly rejectRespond = false,
  ) {}

  request<T = unknown>(method: string, params?: unknown): Promise<T> {
    this.calls.push({ kind: "request", method, params });
    return Promise.resolve(this.responder(method, params) as T);
  }

  notify(method: string, params?: unknown): void {
    this.calls.push({ kind: "notify", method, params });
  }

  respond(
    requestId: number | string,
    response: { readonly result: unknown } | { readonly error: { readonly code: number; readonly message: string } },
  ): void {
    if (this.closed || this.rejectRespond) throw new Error("closed");
    this.calls.push({ kind: "respond", requestId, response });
  }

  installServerRequestInterceptor(interceptor: (note: CodexNotification, frameBytes: number) => boolean): () => void {
    assert.equal(this.interceptor, undefined, "only one auth request interceptor is installed");
    this.interceptor = interceptor;
    return () => {
      if (this.interceptor === interceptor) this.interceptor = undefined;
    };
  }

  emitServerRequest(note: CodexNotification, frameBytes?: number): boolean {
    const requestId = note.kind === "activity" ? note.requestId : undefined;
    const bytes = frameBytes
      ?? Buffer.byteLength(JSON.stringify({ id: requestId, method: note.method, params: note.params }), "utf8");
    return this.interceptor?.(note, bytes) ?? false;
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    return {
      next: async () => ({ value: undefined, done: true }),
      [Symbol.asyncIterator]() {
        return this;
      },
    };
  }

  close(): Promise<void> {
    this.closes += 1;
    this.closed = true;
    return Promise.resolve();
  }
}

function defaultResponder(method: string, params: unknown): unknown {
  if (method === "initialize") {
    return {
      userAgent: "codex/0.153.2",
      codexHome: "/owned/codex",
      platformFamily: "unix",
      platformOs: "linux",
    };
  }
  if (method === "account/login/start") {
    const type = (params as { type?: unknown } | undefined)?.type;
    return { type };
  }
  return {};
}

function refreshRequest(
  id: number,
  previousAccountId: string | null = "untrusted-hint",
): CodexNotification {
  return {
    kind: "activity",
    method: "account/chatgptAuthTokens/refresh",
    requestId: id,
    params: { reason: "unauthorized", previousAccountId },
  };
}

function subscription(bridge: CodexSubscriptionRefreshBridge) {
  return createCodexAppServerAuth({ mode: "subscription", initial: INITIAL, bridge });
}

function responses(transport: FakeTransport): RecordedCall[] {
  return transport.calls.filter((call) => call.kind === "respond");
}

async function withTimeout<T>(promise: Promise<T>, ms: number, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}`)), ms);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

describe("Codex app-server auth: pinned startup ordering and modes", () => {
  it("subscription sends initialize, initialized, then server-verified chatgptAuthTokens login", async () => {
    const transport = new FakeTransport();
    const auth = subscription({ refresh: async () => REFRESHED });

    await auth.authenticate(transport);

    assert.deepEqual(transport.calls, [
      {
        kind: "request",
        method: "initialize",
        params: {
          clientInfo: { name: "uzi", title: "uzi", version: "0.1.0-m4" },
          capabilities: { experimentalApi: true, requestAttestation: false },
        },
      },
      { kind: "notify", method: "initialized", params: undefined },
      {
        kind: "request",
        method: "account/login/start",
        params: {
          type: "chatgptAuthTokens",
          accessToken: INITIAL.accessToken,
          chatgptAccountId: INITIAL.accountId,
          chatgptPlanType: null,
        },
      },
    ]);
  });

  it("API-key sends the pinned apiKey login and carries no subscription bridge", async () => {
    const transport = new FakeTransport();
    const auth = createCodexAppServerAuth({ mode: "api_key", apiKey: "test-api-key" });

    await auth.authenticate(transport);

    assert.deepEqual(transport.calls.at(-1), {
      kind: "request",
      method: "account/login/start",
      params: { type: "apiKey", apiKey: "test-api-key" },
    });
    assert.equal(auth.mode, "api_key");
  });

  it("a wrong login response poisons the session and never falls back", async () => {
    const transport = new FakeTransport((method, params) => {
      if (method === "account/login/start") return { type: "apiKey" };
      return defaultResponder(method, params);
    });
    const auth = subscription({ refresh: async () => REFRESHED });

    await assert.rejects(auth.authenticate(transport), (error: unknown) => {
      assert.ok(error instanceof CodexAppServerAuthError);
      assert.equal(error.poisoned, true);
      assert.match(error.message, /unexpected authentication mode/);
      return true;
    });
    await assert.rejects(auth.authenticate(transport), /session is poisoned/);
  });
});

describe("Codex app-server auth: subscription refresh", () => {
  it("coalesces duplicates, ignores previousAccountId as authority, and returns planType null", async () => {
    let release!: (value: ServerVerifiedCodexAuthTokens) => void;
    const held = new Promise<ServerVerifiedCodexAuthTokens>((resolve) => {
      release = resolve;
    });
    const refreshes: Array<{ operationId: string; signal: AbortSignal }> = [];
    const auth = subscription({
      refresh: (request) => {
        refreshes.push(request);
        return held;
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    const first = auth.handleServerRequest(transport, refreshRequest(1, "hint-a"));
    const second = auth.handleServerRequest(transport, refreshRequest(2, "hint-b"));
    assert.equal(refreshes.length, 1, "two concurrent callbacks share one API refresh");
    assert.deepEqual(Object.keys(refreshes[0]!).sort(), ["operationId", "signal"], "the hint is not API authority");

    release(REFRESHED);
    assert.deepEqual(await Promise.all([first, second]), [true, true]);
    assert.deepEqual(responses(transport), [1, 2].map((requestId) => ({
      kind: "respond",
      requestId,
      response: {
        result: {
          accessToken: REFRESHED.accessToken,
          chatgptAccountId: REFRESHED.accountId,
          chatgptPlanType: null,
        },
      },
    })));
  });

  it("retains one operation id across a failed attempt, then mints a new id after delivered success", async () => {
    const operationIds: string[] = [];
    let attempts = 0;
    const auth = subscription({
      refresh: async ({ operationId }) => {
        operationIds.push(operationId);
        attempts += 1;
        if (attempts === 1) throw new Error("private upstream failure");
        return REFRESHED;
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    assert.equal(await auth.handleServerRequest(transport, refreshRequest(1)), true);
    assert.equal(await auth.handleServerRequest(transport, refreshRequest(2)), true);
    assert.equal(operationIds[0], operationIds[1], "retry reconciles the same logical operation");
    assert.deepEqual((responses(transport)[0]!.response as { error: unknown }).error, {
      code: -32001,
      message: "codex authentication refresh unavailable",
    });

    assert.equal(await auth.handleServerRequest(transport, refreshRequest(3)), true);
    assert.notEqual(operationIds[2], operationIds[1], "a later logical refresh gets a new operation id");
  });

  it("enforces a hard bridge budget below pinned app-server's 10-second deadline", async () => {
    let bridgeSignal: AbortSignal | undefined;
    const auth = subscription({
      refresh: ({ signal }) => {
        bridgeSignal = signal;
        return new Promise<ServerVerifiedCodexAuthTokens>((_resolve, reject) => {
          signal.addEventListener("abort", () => reject(new Error("cancelled")), { once: true });
        });
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    assert.ok(CODEX_REFRESH_BRIDGE_BUDGET_MS < 10_000);
    const started = Date.now();
    assert.equal(await auth.handleServerRequest(transport, refreshRequest(1)), true);
    const elapsed = Date.now() - started;
    assert.ok(elapsed >= CODEX_REFRESH_BRIDGE_BUDGET_MS - 100, `deadline fired too early (${elapsed}ms)`);
    assert.ok(elapsed < 10_000, `refresh response missed the app-server budget (${elapsed}ms)`);
    assert.equal(bridgeSignal?.aborted, true, "the timed-out API bridge is cancelled");
    assert.equal((responses(transport)[0]!.response as { error?: unknown }).error !== undefined, true);
  });

  it("poisons on a malformed server-verified bridge response", async () => {
    const auth = subscription({ refresh: async () => ({ accessToken: "", accountId: "" }) });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    await assert.rejects(auth.handleServerRequest(transport, refreshRequest(1)), (error: unknown) => {
      assert.ok(error instanceof CodexAppServerAuthError);
      assert.equal(error.poisoned, true);
      assert.match(error.message, /bridge returned an invalid response/);
      return true;
    });
    await assert.rejects(auth.authenticate(transport), /session is poisoned/);
  });
});

describe("Codex app-server auth: API-key refresh denial", () => {
  it("answers the unexpected subscription callback with an error and poisons permanently", async () => {
    const auth = createCodexAppServerAuth({ mode: "api_key", apiKey: "test-api-key" });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    await assert.rejects(auth.handleServerRequest(transport, refreshRequest(7)), (error: unknown) => {
      assert.ok(error instanceof CodexAppServerAuthError);
      assert.equal(error.poisoned, true);
      assert.match(error.message, /api-key session received a subscription refresh/);
      return true;
    });
    assert.deepEqual(responses(transport), [{
      kind: "respond",
      requestId: 7,
      response: { error: { code: -32600, message: "codex authentication protocol violation" } },
    }]);
    await assert.rejects(auth.authenticate(transport), /session is poisoned/);
  });
});

describe("Codex app-server auth: intercepted request ownership", () => {
  it("fails closed and drains promptly when held refresh requests exceed the count ceiling", async () => {
    let entered!: () => void;
    const bridgeEntered = new Promise<void>((resolve) => {
      entered = resolve;
    });
    let bridgeCalls = 0;
    const auth = subscription({
      refresh: ({ signal }) => {
        bridgeCalls += 1;
        entered();
        return new Promise<ServerVerifiedCodexAuthTokens>((_resolve, reject) => {
          signal.addEventListener("abort", () => reject(new Error("cancelled")), { once: true });
        });
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    assert.equal(transport.emitServerRequest(refreshRequest(1)), true);
    await bridgeEntered;
    for (let id = 2; id <= 9; id += 1) {
      assert.equal(transport.emitServerRequest(refreshRequest(id)), true);
    }

    try {
      await assert.rejects(
        withTimeout(auth.drainInterceptedRequests(), 500, "auth flood drain"),
        /capacity exceeded/,
      );
    } finally {
      auth.closeAdmissionAndCancel();
      await transport.close();
      await auth.drainInterceptedRequests().catch(() => {});
    }
    assert.equal(bridgeCalls, 1, "duplicates coalesced before the overflow poisoned admission");
    assert.ok(transport.closes >= 1, "overflow closes the poisoned transport");
  });

  it("fails closed on the intercepted byte ceiling independently of request count", async () => {
    let bridgeCalls = 0;
    const auth = subscription({
      refresh: async () => {
        bridgeCalls += 1;
        return REFRESHED;
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    assert.equal(transport.emitServerRequest(refreshRequest(1), 20 * 1024), true);
    await assert.rejects(auth.drainInterceptedRequests(), /capacity exceeded/);
    assert.equal(bridgeCalls, 0);
  });

  it("rejects an oversized previousAccountId hint before any async retention", async () => {
    let bridgeCalls = 0;
    const auth = subscription({
      refresh: async () => {
        bridgeCalls += 1;
        return REFRESHED;
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);

    assert.equal(transport.emitServerRequest(refreshRequest(1, "h".repeat(257))), true);
    await assert.rejects(auth.drainInterceptedRequests(), /refresh request is malformed/);
    assert.equal(bridgeCalls, 0);
  });

  it("retains poison when an abort-ignoring bridge races close", async () => {
    let markEntered!: () => void;
    const entered = new Promise<void>((resolve) => {
      markEntered = resolve;
    });
    const auth = subscription({
      refresh: () => {
        markEntered();
        // Deliberately ignores AbortSignal. The 250ms cancellation-settlement guard must
        // poison this accepted operation, and shutdown must retain that poison.
        return new Promise<ServerVerifiedCodexAuthTokens>(() => {});
      },
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);
    assert.equal(transport.emitServerRequest(refreshRequest(1)), true);
    await entered;

    auth.closeAdmissionAndCancel();
    await transport.close();
    await assert.rejects(
      withTimeout(auth.drainInterceptedRequests(), 500, "uncooperative bridge poison"),
      (error: unknown) => {
        assert.ok(error instanceof CodexAppServerAuthError);
        assert.equal(error.poisoned, true);
        assert.match(error.message, /did not settle after cancellation/);
        return true;
      },
    );
  });

  it("retains malformed-response poison accepted immediately before close", async () => {
    const auth = subscription({
      refresh: async () => ({ accessToken: "", accountId: "" }),
    });
    const transport = new FakeTransport();
    await auth.authenticate(transport);
    assert.equal(transport.emitServerRequest(refreshRequest(1)), true);

    // Close in the same synchronous turn, before the intercepted task resumes after its
    // startup await. The later invalid response is still a protocol poison, not an
    // expected transport/startup cancellation.
    auth.closeAdmissionAndCancel();
    await transport.close();
    await assert.rejects(auth.drainInterceptedRequests(), (error: unknown) => {
      assert.ok(error instanceof CodexAppServerAuthError);
      assert.equal(error.poisoned, true);
      assert.match(error.message, /bridge returned an invalid response/);
      return true;
    });
  });
});
