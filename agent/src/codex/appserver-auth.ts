// PRD #1171: pinned Codex 0.153.2 app-server authentication.
//
// The wire contract comes from upstream commit 657a993c:
//   - initialize with experimentalApi, then the initialized notification;
//   - account/login/start with apiKey or experimental chatgptAuthTokens;
//   - account/chatgptAuthTokens/refresh as a server-initiated request.
//
// This module is deliberately narrower than the run callback broker. It receives only
// one immutable credential mode and, for subscriptions, one run-bound refresh function.
// It never receives a workspace, tool registry, model prompt or logger, so neither the
// credential nor its refresh authority can become model-visible by construction.

import { randomUUID } from "node:crypto";

import type { HarnessError } from "../harness.js";
import type { CodexNotification, CodexTransport } from "./transport.js";

/** Outer bridge budget: above WorkerClient's 8s cap, below app-server's hard 10s cap. */
export const CODEX_REFRESH_BRIDGE_BUDGET_MS = 9_000;

const INITIALIZE_METHOD = "initialize";
const INITIALIZED_METHOD = "initialized";
const LOGIN_METHOD = "account/login/start";
const REFRESH_METHOD = "account/chatgptAuthTokens/refresh";

const REFRESH_FAILED_CODE = -32_001;
const PROTOCOL_POISON_CODE = -32_600;
const MAX_INTERCEPTED_AUTH_REQUESTS = 8;
const MAX_INTERCEPTED_AUTH_BYTES = 16 * 1024;
const MAX_PREVIOUS_ACCOUNT_ID_BYTES = 256;
const BRIDGE_CANCEL_SETTLEMENT_MS = 250;

export type CodexAppServerAuthMode = "api_key" | "subscription";

/**
 * Access-token/account identity already verified by the uzi API. The adapter must not
 * decode a JWT and call that verification: pinned Codex only parses claims, while the
 * API owns canonical credential identity and durable refresh state.
 */
export interface ServerVerifiedCodexAuthTokens {
  readonly accessToken: string;
  readonly accountId: string;
}

/**
 * The only authority exposed to the app-server auth owner. `operationId` identifies one
 * logical refresh and is retained after timeout/failure so a retry reconciles the same
 * API-owned operation. `previousAccountId` is intentionally absent: the app-server value
 * is an untrusted hint and cannot select credential/run/generation authority.
 */
export interface CodexSubscriptionRefreshBridge {
  refresh(request: { readonly operationId: string; readonly signal: AbortSignal }): Promise<ServerVerifiedCodexAuthTokens>;
}

export type CodexAppServerAuthConfig =
  | {
      readonly mode: "api_key";
      readonly apiKey: string;
    }
  | {
      readonly mode: "subscription";
      readonly initial: ServerVerifiedCodexAuthTokens;
      readonly bridge: CodexSubscriptionRefreshBridge;
    };

/** Minimal composition seam consumed by run and advice harnesses. */
export interface CodexAppServerAuthSession {
  readonly mode: CodexAppServerAuthMode;
  /** Initialize and authenticate exactly this app-server connection. */
  authenticate(transport: CodexTransport, signal?: AbortSignal): Promise<void>;
  /** Handle only the pinned subscription-refresh server request. */
  handleServerRequest(
    transport: CodexTransport,
    note: CodexNotification,
    signal?: AbortSignal,
  ): Promise<boolean>;
  /** Wait for every auth request already accepted by the read-path interceptor. */
  drainInterceptedRequests(): Promise<void>;
  /** Permanently close auth admission and cancel an active refresh. Drain separately,
   *  after the owner closes transport and rejects any pending startup RPC. */
  closeAdmissionAndCancel(): void;
}

/** Static, secret-free app-server authentication failure. */
export class CodexAppServerAuthError extends Error {
  readonly failure: HarnessError;
  readonly poisoned: boolean;

  constructor(failure: HarnessError, poisoned: boolean) {
    super(failure.message);
    this.name = "CodexAppServerAuthError";
    this.failure = failure;
    this.poisoned = poisoned;
  }
}

interface ActiveRefresh {
  readonly operationId: string;
  promise: Promise<ServerVerifiedCodexAuthTokens>;
  waiters: number;
  bridgeSucceeded: boolean;
  deliveryFailed: boolean;
  abort(): void;
}

function asObject(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

function isNonEmptyString(value: unknown): value is string {
  return typeof value === "string" && value.length > 0;
}

function validateVerifiedTokens(value: unknown): ServerVerifiedCodexAuthTokens | undefined {
  const record = asObject(value);
  if (!record || !isNonEmptyString(record.accessToken) || !isNonEmptyString(record.accountId)) return undefined;
  return { accessToken: record.accessToken, accountId: record.accountId };
}

function protocolError(message: string): CodexAppServerAuthError {
  return new CodexAppServerAuthError({ category: "protocol", message }, true);
}

function transportError(message: string): CodexAppServerAuthError {
  return new CodexAppServerAuthError({ category: "transport", message }, false);
}

class AppServerAuthSession implements CodexAppServerAuthSession {
  readonly mode: CodexAppServerAuthMode;

  private readonly config: CodexAppServerAuthConfig;
  private state: "new" | "starting" | "ready" | "poisoned" = "new";
  private startup?: Promise<void>;
  private transport?: CodexTransport;
  private retainedOperationId?: string;
  private activeRefresh?: ActiveRefresh;
  private readonly interceptedTasks = new Set<Promise<void>>();
  private interceptedCount = 0;
  private interceptedBytes = 0;
  private interceptedClosed = false;
  private interceptedFailure?: CodexAppServerAuthError;

  constructor(config: CodexAppServerAuthConfig) {
    // Snapshot the discriminant and secret values. A caller retaining and mutating its
    // input object cannot switch this root to another credential mode after construction.
    this.config = config.mode === "api_key"
      ? { mode: "api_key", apiKey: config.apiKey }
      : {
          mode: "subscription",
          initial: { accessToken: config.initial.accessToken, accountId: config.initial.accountId },
          bridge: config.bridge,
        };
    this.mode = config.mode;
  }

  authenticate(transport: CodexTransport, signal?: AbortSignal): Promise<void> {
    if (this.interceptedClosed) {
      return Promise.reject(transportError("codex authentication request admission is closed"));
    }
    if (this.state === "poisoned") return Promise.reject(protocolError("codex authentication session is poisoned"));
    if (this.transport !== undefined && this.transport !== transport) {
      this.poison();
      return Promise.reject(protocolError("codex authentication session cannot move between app-server connections"));
    }
    if (this.state === "ready") return Promise.resolve();
    if (this.startup !== undefined) return this.startup;

    this.transport = transport;
    this.state = "starting";
    const startup = this.start(transport, signal).then(
      () => {
        if (this.state === "poisoned") throw protocolError("codex authentication session is poisoned");
        this.state = "ready";
      },
      (error: unknown) => {
        this.poison();
        if (error instanceof CodexAppServerAuthError) throw error;
        throw transportError("codex authentication startup failed");
      },
    );
    this.startup = startup;
    return startup;
  }

  async handleServerRequest(
    transport: CodexTransport,
    note: CodexNotification,
    signal?: AbortSignal,
  ): Promise<boolean> {
    if (note.kind !== "activity" || note.method !== REFRESH_METHOD || note.requestId === undefined) return false;

    if (this.state !== "ready" || this.transport !== transport) {
      this.poisonAndRespond(transport, note.requestId, "codex authentication refresh arrived outside the active session");
    }
    if (this.config.mode === "api_key") {
      this.poisonAndRespond(transport, note.requestId, "codex api-key session received a subscription refresh request");
    }
    this.validateRefreshParams(transport, note.requestId, note.params);
    await this.handleRefresh(transport, note.requestId, signal);
    return true;
  }

  async drainInterceptedRequests(): Promise<void> {
    // Loop because settlement callbacks and a same-chunk decoder can add/remove owners in
    // adjacent microtasks. A terminal may return only after the observed owner set is empty.
    while (this.interceptedTasks.size > 0) {
      await Promise.all(this.interceptedTasks);
    }
    if (this.interceptedFailure !== undefined) throw this.interceptedFailure;
  }

  closeAdmissionAndCancel(): void {
    this.interceptedClosed = true;
    this.activeRefresh?.abort();
  }

  private validateRefreshParams(
    transport: CodexTransport,
    requestId: number | string,
    value: unknown,
  ): void {
    const params = asObject(value);
    const previousAccountId = params?.previousAccountId;
    if (
      params === undefined
      || params.reason !== "unauthorized"
      || (previousAccountId !== undefined && previousAccountId !== null && typeof previousAccountId !== "string")
      || (typeof previousAccountId === "string"
        && Buffer.byteLength(previousAccountId, "utf8") > MAX_PREVIOUS_ACCOUNT_ID_BYTES)
    ) {
      this.poisonAndRespond(transport, requestId, "codex authentication refresh request is malformed");
    }
    // The bounded hint is deliberately discarded before any await. It never selects API
    // authority and cannot be retained behind a held refresh operation.
  }

  private async handleRefresh(
    transport: CodexTransport,
    requestId: number | string,
    signal?: AbortSignal,
  ): Promise<void> {
    const active = this.getOrStartRefresh(signal);
    active.waiters += 1;
    let delivered = false;
    try {
      let refreshed: ServerVerifiedCodexAuthTokens;
      try {
        refreshed = await active.promise;
      } catch (error) {
        if (error instanceof CodexAppServerAuthError && error.poisoned) {
          this.safeErrorResponse(transport, requestId, PROTOCOL_POISON_CODE, "codex authentication protocol violation");
          throw error;
        }
        delivered = this.safeErrorResponse(
          transport,
          requestId,
          REFRESH_FAILED_CODE,
          "codex authentication refresh unavailable",
        );
        return;
      }

      try {
        transport.respond(requestId, {
          result: {
            accessToken: refreshed.accessToken,
            chatgptAccountId: refreshed.accountId,
            chatgptPlanType: null,
          },
        });
        delivered = true;
      } catch {
        throw transportError("codex authentication response could not be delivered");
      }
      return;
    } finally {
      if (!delivered) active.deliveryFailed = true;
      active.waiters -= 1;
      if (active.waiters === 0 && this.activeRefresh === active) {
        this.activeRefresh = undefined;
        // A successful bridge result is a completed logical operation only after every
        // coalesced app-server request received it. Any bridge or delivery failure retains
        // the id so the next callback reconciles the same API-owned operation.
        if (active.bridgeSucceeded && !active.deliveryFailed) this.retainedOperationId = undefined;
      }
    }
  }

  private async start(transport: CodexTransport, signal?: AbortSignal): Promise<void> {
    const initialized = await transport.request<unknown>(
      INITIALIZE_METHOD,
      {
        clientInfo: { name: "uzi", title: "uzi", version: "0.1.0-m4" },
        capabilities: { experimentalApi: true, requestAttestation: false },
      },
      { signal },
    );
    const init = asObject(initialized);
    if (
      !init
      || !isNonEmptyString(init.userAgent)
      || !isNonEmptyString(init.codexHome)
      || !isNonEmptyString(init.platformFamily)
      || !isNonEmptyString(init.platformOs)
    ) {
      throw protocolError("codex initialize returned an invalid response");
    }

    // Pinned 0.153.2 serializes this notification with no params field.
    transport.notify(INITIALIZED_METHOD);

    // Install the auth-only read-path pump before sending login. Normally it becomes
    // relevant only after login succeeds, but registering here closes the response-batch
    // race: a login response and the first refresh request may arrive in one stdout chunk,
    // whose lines the transport decodes synchronously before promise continuations run.
    // The pump waits for `startup` (and therefore validated login) before answering.
    this.installRefreshPump(transport);

    const loginParams = this.config.mode === "api_key"
      ? { type: "apiKey", apiKey: this.config.apiKey }
      : {
          type: "chatgptAuthTokens",
          accessToken: this.config.initial.accessToken,
          chatgptAccountId: this.config.initial.accountId,
          chatgptPlanType: null,
        };
    const response = await transport.request<unknown>(LOGIN_METHOD, loginParams, { signal });
    const expected = this.config.mode === "api_key" ? "apiKey" : "chatgptAuthTokens";
    if (asObject(response)?.type !== expected) {
      throw protocolError("codex account/login/start returned an unexpected authentication mode");
    }
  }

  private installRefreshPump(transport: CodexTransport): void {
    const install = transport.installServerRequestInterceptor;
    if (install === undefined) {
      throw protocolError("codex transport lacks the authentication request interceptor");
    }
    try {
      install.call(transport, (note, frameBytes): boolean => {
        if (note.kind !== "activity" || note.method !== REFRESH_METHOD || note.requestId === undefined) return false;
        const requestId = note.requestId;
        try {
          if (this.interceptedClosed) {
            this.poisonAndRespond(transport, requestId, "codex authentication request admission is closed");
          }
          if (this.config.mode === "api_key") {
            this.poisonAndRespond(transport, requestId, "codex api-key session received a subscription refresh request");
          }
          // Validate and discard the untrusted hint synchronously, before an async task
          // can retain the decoded provider frame behind a held refresh.
          this.validateRefreshParams(transport, requestId, note.params);
          if (
            !Number.isSafeInteger(frameBytes)
            || frameBytes <= 0
            || this.interceptedCount >= MAX_INTERCEPTED_AUTH_REQUESTS
            || this.interceptedBytes + frameBytes > MAX_INTERCEPTED_AUTH_BYTES
          ) {
            this.poisonAndRespond(transport, requestId, "codex intercepted authentication capacity exceeded");
          }
        } catch (error) {
          this.recordInterceptedFailure(error);
          void transport.close().catch(() => {});
          return true;
        }

        this.interceptedCount += 1;
        this.interceptedBytes += frameBytes;
        const task = this.handlePumpedRefresh(transport, requestId);
        let owned!: Promise<void>;
        owned = task
          .catch((error: unknown) => {
            // During explicit teardown, transport closure is expected to reject a login
            // RPC that an intercepted refresh may be waiting behind. The owner still
            // drains this task, but that shutdown rejection is not a protocol poison.
            // A genuine auth poison remains sticky even when it races admission close.
            const poisoned = error instanceof CodexAppServerAuthError && error.poisoned;
            if (!this.interceptedClosed || poisoned) this.recordInterceptedFailure(error);
            // A protocol poison or undeliverable reply must reject any setup RPC already
            // waiting on this connection. close() rejects pending requests and ends notes.
            return transport.close().catch(() => {});
          })
          .finally(() => {
            this.interceptedTasks.delete(owned);
            this.interceptedCount -= 1;
            this.interceptedBytes -= frameBytes;
          });
        this.interceptedTasks.add(owned);
        return true;
      });
    } catch {
      throw protocolError("codex authentication request interceptor could not be installed");
    }
  }

  private async handlePumpedRefresh(transport: CodexTransport, requestId: number | string): Promise<void> {
    const startup = this.startup;
    if (startup === undefined) throw protocolError("codex authentication startup is unavailable");
    await startup;
    if (this.state !== "ready" || this.transport !== transport) {
      throw protocolError("codex authentication refresh arrived outside the active session");
    }
    await this.handleRefresh(transport, requestId);
  }

  private getOrStartRefresh(signal?: AbortSignal): ActiveRefresh {
    if (this.activeRefresh !== undefined) return this.activeRefresh;
    if (this.config.mode !== "subscription") {
      // The caller checks this first. Keep a fail-closed guard here so a future refactor
      // cannot accidentally add API-key fallback.
      throw protocolError("codex api-key session cannot refresh subscription credentials");
    }
    const operationId = this.retainedOperationId ?? randomUUID();
    this.retainedOperationId = operationId;
    const controller = new AbortController();
    let active!: ActiveRefresh;
    active = {
      operationId,
      promise: this.runRefresh(this.config.bridge, operationId, controller, signal).then((value) => {
        active.bridgeSucceeded = true;
        return value;
      }),
      waiters: 0,
      bridgeSucceeded: false,
      deliveryFailed: false,
      abort: () => controller.abort(),
    };
    this.activeRefresh = active;
    return active;
  }

  private async runRefresh(
    bridge: CodexSubscriptionRefreshBridge,
    operationId: string,
    controller: AbortController,
    outerSignal?: AbortSignal,
  ): Promise<ServerVerifiedCodexAuthTokens> {
    let timedOut = false;
    const onAbort = (): void => controller.abort();
    if (outerSignal?.aborted) controller.abort();
    else outerSignal?.addEventListener("abort", onAbort, { once: true });

    let onInternalAbort: (() => void) | undefined;
    const aborted = new Promise<never>((_, reject) => {
      onInternalAbort = (): void => reject(transportError("codex authentication refresh aborted"));
      controller.signal.addEventListener("abort", onInternalAbort, { once: true });
    });
    let timer: ReturnType<typeof setTimeout> | undefined;
    const deadline = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        controller.abort();
        reject(transportError("codex authentication refresh deadline exceeded"));
      }, CODEX_REFRESH_BRIDGE_BUDGET_MS);
    });

    let bridgeWork: Promise<ServerVerifiedCodexAuthTokens> | undefined;
    try {
      if (controller.signal.aborted && !timedOut) {
        throw transportError("codex authentication refresh aborted");
      }
      bridgeWork = bridge.refresh({ operationId, signal: controller.signal });
      const value = await Promise.race([
        bridgeWork,
        deadline,
        aborted,
      ]);
      const verified = validateVerifiedTokens(value);
      if (verified === undefined) {
        this.poison();
        throw protocolError("codex authentication bridge returned an invalid response");
      }
      return verified;
    } catch (error) {
      if (
        controller.signal.aborted
        && bridgeWork !== undefined
        && !(await this.awaitBridgeSettlement(bridgeWork, BRIDGE_CANCEL_SETTLEMENT_MS))
      ) {
        this.poison();
        throw protocolError("codex authentication bridge did not settle after cancellation");
      }
      if (error instanceof CodexAppServerAuthError) throw error;
      throw transportError("codex authentication refresh failed");
    } finally {
      if (timer !== undefined) clearTimeout(timer);
      if (onInternalAbort) controller.signal.removeEventListener("abort", onInternalAbort);
      outerSignal?.removeEventListener("abort", onAbort);
    }
  }

  private async awaitBridgeSettlement(
    work: Promise<ServerVerifiedCodexAuthTokens>,
    timeoutMs: number,
  ): Promise<boolean> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      return await Promise.race([
        work.then(() => true, () => true),
        new Promise<false>((resolve) => {
          timer = setTimeout(() => resolve(false), timeoutMs);
        }),
      ]);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  private poisonAndRespond(transport: CodexTransport, requestId: number | string, message: string): never {
    this.poison();
    this.safeErrorResponse(transport, requestId, PROTOCOL_POISON_CODE, "codex authentication protocol violation");
    throw protocolError(message);
  }

  private poison(): void {
    this.state = "poisoned";
    this.activeRefresh?.abort();
  }

  private recordInterceptedFailure(error: unknown): void {
    if (this.interceptedFailure !== undefined) return;
    this.interceptedFailure = error instanceof CodexAppServerAuthError
      ? error
      : protocolError("codex intercepted authentication request failed");
    this.poison();
  }

  private safeErrorResponse(
    transport: CodexTransport,
    requestId: number | string,
    code: number,
    message: string,
  ): boolean {
    try {
      transport.respond(requestId, { error: { code, message } });
      return true;
    } catch {
      return false;
    }
  }
}

/** Create one immutable auth owner for one app-server root. */
export function createCodexAppServerAuth(config: CodexAppServerAuthConfig): CodexAppServerAuthSession {
  if (config.mode === "api_key") {
    if (!isNonEmptyString(config.apiKey)) throw protocolError("codex api-key credential is empty");
  } else if (validateVerifiedTokens(config.initial) === undefined) {
    throw protocolError("codex subscription credential is incomplete");
  }
  return new AppServerAuthSession(config);
}
