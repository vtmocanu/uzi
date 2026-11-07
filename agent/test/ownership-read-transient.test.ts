import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { describe, it } from "node:test";
import { isFetchRejection, RequestError, WorkerClient } from "../src/client.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { transientOwnershipReadCause } from "../src/ownership-read-transient.js";
import { nullLogger } from "./helpers.js";

const RUN = "run-own-1";
const OWNERSHIP_PATH = `/api/worker/runs/${RUN}/ownership`;

type Handler = (req: http.IncomingMessage, res: http.ServerResponse) => void;

function makeClient(baseUrl: string, fetchImpl?: typeof fetch): WorkerClient {
  return new WorkerClient(baseUrl, "tok", "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    ...(fetchImpl ? { fetch: fetchImpl } : {}),
  });
}

async function withServer<T>(handler: Handler, body: (baseUrl: string) => Promise<T>): Promise<T> {
  const server = http.createServer(handler);
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  try {
    return await body(`http://127.0.0.1:${port}`);
  } finally {
    server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
  }
}

async function caught(fn: () => Promise<unknown>): Promise<unknown> {
  try {
    await fn();
  } catch (error) {
    return error;
  }
  assert.fail("expected a rejection");
}

const ownership = (client: WorkerClient) => caught(() => client.getRunOwnership(RUN));

function causeOf(error: unknown): { code?: unknown } {
  assert.ok(error instanceof TypeError, "expected TypeError");
  return (error as { cause: { code?: unknown } }).cause;
}

const classify = (error: unknown) => transientOwnershipReadCause(error, RUN);

describe("transientOwnershipReadCause: real transport failures", () => {
  it("classifies a refused connection as econnrefused", async () => {
    const server = http.createServer();
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    const { port } = server.address() as AddressInfo;
    await new Promise<void>(resolve => server.close(() => resolve()));
    const error = await ownership(makeClient(`http://127.0.0.1:${port}`));
    assert.equal(causeOf(error).code, "ECONNREFUSED");
    assert.ok(isFetchRejection(error));
    assert.equal(classify(error), "econnrefused");
  });

  it("classifies a socket destroyed before any response as und_err_socket", async () => {
    await withServer((req) => { req.socket.destroy(); }, async (url) => {
      const error = await ownership(makeClient(url));
      assert.equal(causeOf(error).code, "UND_ERR_SOCKET");
      assert.equal(classify(error), "und_err_socket");
    });
  });

  it("classifies a reset connection as econnreset", async () => {
    await withServer((req) => { req.socket.resetAndDestroy(); }, async (url) => {
      const error = await ownership(makeClient(url));
      assert.equal(causeOf(error).code, "ECONNRESET");
      assert.equal(classify(error), "econnreset");
    });
  });

  it("works through the signal and deadline overload", async () => {
    await withServer((req) => { req.socket.destroy(); }, async (url) => {
      const client = makeClient(url);
      const error = await caught(() => client.getRunOwnership(RUN, { deadline: Date.now() + 5000 }));
      assert.equal(classify(error), "und_err_socket");
    });
  });

  it("classifies a transport rejection inside MaintenanceBudget.runOwned", async () => {
    await withServer((req) => { req.socket.destroy(); }, async (url) => {
      const client = makeClient(url);
      const error = await caught(() => MaintenanceBudget.runOwned(new AbortController().signal, () => client.getRunOwnership(RUN)));
      assert.equal(classify(error), "und_err_socket");
    });
  });

  it("classifies a transport rejection inside a maintenance budget", async () => {
    await withServer((req) => { req.socket.destroy(); }, async (url) => {
      const client = makeClient(url);
      const error = await caught(() => MaintenanceBudget.run(new MaintenanceBudget(5000), () => client.getRunOwnership(RUN)));
      assert.equal(classify(error), "und_err_socket");
    });
  });
});

describe("transientOwnershipReadCause: HTTP statuses", () => {
  for (const status of [502, 503, 504]) {
    it(`classifies ${status} as http_${status}`, async () => {
      await withServer((_req, res) => { res.writeHead(status).end("bad gateway"); }, async (url) => {
        const error = await ownership(makeClient(url));
        assert.ok(error instanceof RequestError);
        assert.equal(error.status, status);
        assert.equal(classify(error), `http_${status}`);
      });
    });
  }

  for (const status of [400, 401, 403, 408, 429, 500]) {
    it(`does not classify ${status}`, async () => {
      await withServer((_req, res) => { res.writeHead(status).end("x"); }, async (url) => {
        const error = await ownership(makeClient(url));
        assert.ok(error instanceof RequestError);
        assert.equal(classify(error), undefined);
      });
    });
  }

  it("does not classify a strict 404 ownership loss", async () => {
    await withServer((_req, res) => {
      res.writeHead(404, { "content-type": "application/json" }).end(JSON.stringify({ error: "run not found for this worker" }));
    }, async (url) => {
      assert.equal(classify(await ownership(makeClient(url))), undefined);
    });
  });

  it("does not classify a 503 on another path or method", () => {
    assert.equal(classify(new RequestError("GET", "/api/worker/runs/other/ownership", 503, "")), undefined);
    assert.equal(classify(new RequestError("GET", `${OWNERSHIP_PATH}?purpose=code_snapshot`, 503, "")), undefined);
    assert.equal(classify(new RequestError("POST", OWNERSHIP_PATH, 503, "")), undefined);
    assert.equal(classify(new RequestError("GET", OWNERSHIP_PATH, 503, "")), "http_503");
  });
});

describe("transientOwnershipReadCause: failures after the transport settled", () => {
  it("does not classify a truncated 200 body (real terminated TypeError)", async () => {
    await withServer((req, res) => {
      res.writeHead(200, { "content-length": "1000", "content-type": "application/json" });
      res.write("{\"status\":", () => { req.socket.destroy(); });
    }, async (url) => {
      const error = await ownership(makeClient(url));
      assert.ok(error instanceof TypeError);
      assert.equal(causeOf(error).code, "UND_ERR_SOCKET");
      assert.equal(isFetchRejection(error), false);
      assert.equal(classify(error), undefined);
    });
  });

  it("does not classify a malformed 200 body", async () => {
    await withServer((_req, res) => { res.writeHead(200).end("{not json"); }, async (url) => {
      const error = await ownership(makeClient(url));
      assert.ok(error instanceof SyntaxError);
      assert.equal(classify(error), undefined);
    });
  });

  it("does not classify an error thrown after the transport resolved", async () => {
    const ac = new AbortController();
    const reason = new Error("late abort");
    const transport = (async () => { ac.abort(reason); return new Response("{}"); }) as typeof fetch;
    const client = makeClient("http://127.0.0.1:1", transport);
    const error = await caught(() => client.getRunOwnership(RUN, ac.signal));
    assert.equal(error, reason);
    assert.equal(isFetchRejection(error), false);
    assert.equal(classify(error), undefined);
  });

  it("does not mark a body-read failure on a custom transport", async () => {
    const body = new ReadableStream({
      start(controller) {
        controller.error(Object.assign(new TypeError("terminated"), { cause: Object.assign(new Error("x"), { code: "UND_ERR_SOCKET" }) }));
      },
    });
    const client = makeClient("http://127.0.0.1:1", (async () => new Response(body, { status: 200 })) as typeof fetch);
    const error = await ownership(client);
    assert.equal(isFetchRejection(error), false);
    assert.equal(classify(error), undefined);
  });

  it("does not classify a pre-request abort", async () => {
    const client = makeClient("http://127.0.0.1:1");
    const error = await caught(() => client.getRunOwnership(RUN, AbortSignal.abort()));
    assert.equal(isFetchRejection(error), false);
    assert.equal(classify(error), undefined);
  });

  const hang: Handler = () => {};
  const resetReason = () => new TypeError("x", { cause: { code: "ECONNRESET" } });

  it("does not classify a caller abort whose reason looks like a transport failure (real fetch)", async () => {
    let onRequest: () => void = () => {};
    const requested = new Promise<void>(resolve => { onRequest = resolve; });
    await withServer((_req, _res) => { onRequest(); }, async (url) => {
      const ac = new AbortController();
      const client = makeClient(url);
      const result = caught(() => client.getRunOwnership(RUN, ac.signal));
      await requested;
      const reason = resetReason();
      ac.abort(reason);
      const error = await result;
      assert.equal(error, reason);
      assert.equal(isFetchRejection(error), false);
      assert.equal(classify(error), undefined);
    });
  });

  it("does not classify an owned-scope abort whose reason looks like a transport failure (real fetch)", async () => {
    let onRequest: () => void = () => {};
    const requested = new Promise<void>(resolve => { onRequest = resolve; });
    await withServer((_req, _res) => { onRequest(); }, async (url) => {
      const ac = new AbortController();
      const client = makeClient(url);
      const result = caught(() => MaintenanceBudget.runOwned(ac.signal, () => client.getRunOwnership(RUN)));
      await requested;
      const reason = resetReason();
      ac.abort(reason);
      const error = await result;
      assert.equal(error, reason);
      assert.equal(isFetchRejection(error), false);
      assert.equal(classify(error), undefined);
    });
  });

  it("does not classify an already-aborted signal reused for a second read (real fetch)", async () => {
    await withServer(hang, async (url) => {
      const ac = new AbortController();
      ac.abort(resetReason());
      const client = makeClient(url);
      for (let i = 0; i < 2; i++) {
        const error = await caught(() => client.getRunOwnership(RUN, ac.signal));
        assert.equal(error, ac.signal.reason);
        assert.equal(isFetchRejection(error), false);
        assert.equal(classify(error), undefined);
      }
    });
  });

  it("does not classify when the budget wins the race against a real fetch", async () => {
    await withServer(hang, async (url) => {
      let requestSignal: AbortSignal | null | undefined;
      const transport: typeof fetch = (input, init) => {
        requestSignal = init?.signal;
        return fetch(input, init);
      };
      const client = makeClient(url, transport);
      const budget = new MaintenanceBudget(50);
      const error = await caught(() => MaintenanceBudget.run(budget, () => client.getRunOwnership(RUN)));
      assert.ok((budget.signal.aborted && error === budget.signal.reason) ||
        (requestSignal?.aborted === true && error === requestSignal.reason));
      assert.equal(isFetchRejection(error), false);
      assert.equal(classify(error), undefined);
    });
  });

  it("does not classify when the budget aborts the race", async () => {
    let requestSignal: AbortSignal | null | undefined;
    const transport: typeof fetch = (_input, init) => {
      requestSignal = init?.signal;
      return new Promise<Response>(() => {});
    };
    const client = makeClient("http://127.0.0.1:1", transport);
    const budget = new MaintenanceBudget(20);
    const error = await caught(() => MaintenanceBudget.run(budget, () => client.getRunOwnership(RUN)));
    assert.ok((budget.signal.aborted && error === budget.signal.reason) ||
      (requestSignal?.aborted === true && error === requestSignal.reason));
    assert.equal(isFetchRejection(error), false);
    assert.equal(classify(error), undefined);
  });
});

describe("transientOwnershipReadCause: marked transport rejection shapes", () => {
  async function rejectWith(value: unknown): Promise<unknown> {
    const client = makeClient("http://127.0.0.1:1", (async () => { throw value; }) as typeof fetch);
    return ownership(client);
  }
  const withCause = (cause: unknown) => new TypeError("fetch failed", { cause });

  it("classifies an allowlisted code on a marked TypeError", async () => {
    const error = await rejectWith(withCause({ code: "ECONNREFUSED" }));
    assert.ok(isFetchRejection(error));
    assert.equal(classify(error), "econnrefused");
  });

  it("does not classify an unmarked synthetic error with an allowlisted cause", () => {
    const error = withCause({ code: "ECONNREFUSED" });
    assert.equal(isFetchRejection(error), false);
    assert.equal(classify(error), undefined);
  });

  const unclassified: Array<[string, () => unknown]> = [
    ["TypeError with no cause", () => new TypeError("fetch failed")],
    ["cause without code", () => withCause({})],
    ["unknown code", () => withCause({ code: "ENOTFOUND" })],
    ["numeric code", () => withCause({ code: 111 })],
    ["null cause", () => withCause(null)],
    ["string cause", () => withCause("ECONNREFUSED")],
    ["non-TypeError with allowlisted cause", () => Object.assign(new Error("x"), { cause: { code: "ECONNREFUSED" } })],
    ["throwing cause getter", () => Object.defineProperty(new TypeError("x"), "cause", { get() { throw new Error("boom"); } })],
    ["throwing code getter", () => withCause({ get code(): string { throw new Error("boom"); } })],
    ["Proxy cause with throwing traps", () => withCause(new Proxy({}, {
      get() { throw new Error("get"); },
      getOwnPropertyDescriptor() { throw new Error("gopd"); },
      has() { throw new Error("has"); },
    }))],
    ["AbortError", () => new DOMException("aborted", "AbortError")],
    ["TimeoutError", () => new DOMException("timed out", "TimeoutError")],
    ["string", () => "ECONNREFUSED"],
    ["null", () => null],
  ];
  for (const [name, make] of unclassified) {
    it(`does not classify: ${name}`, async () => {
      const value = make();
      const error = await rejectWith(value);
      if (typeof value === "object" && value !== null) assert.ok(isFetchRejection(error));
      assert.equal(classify(error), undefined);
    });
  }

  it("marks object rejections only and preserves identity", async () => {
    const original = new DOMException("aborted", "AbortError");
    const error = await rejectWith(original);
    assert.equal(error, original);
    assert.ok(isFetchRejection(error));
    assert.equal(isFetchRejection("str"), false);
    assert.equal(isFetchRejection(null), false);
    assert.equal(isFetchRejection(undefined), false);
  });
});
