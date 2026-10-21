import { it, mock } from "node:test";
import assert from "node:assert/strict";
import type http from "node:http";
import { FakeApi } from "./fake-api.js";

// A bind failure (EPERM in a sandbox without loopback, EADDRINUSE) must fail the
// caller's setup immediately with the real error. Before the fix, listen() only
// waited for its callback and never settled on a bind error: the error escaped as an
// uncaught 'error' event, attributed to whichever test was running, while the awaiting
// setup could sit out the full --test-timeout. The 5 s cap keeps a regression cheap.
it("listen() rejects with the server's bind error instead of waiting", { timeout: 5_000 }, async () => {
  const api = new FakeApi("tok");
  const server = (api as unknown as { server: http.Server }).server;
  const before = { listening: server.listenerCount("listening"), error: server.listenerCount("error") };
  const bindError = Object.assign(new Error("listen EPERM: operation not permitted 127.0.0.1"), { code: "EPERM" });
  mock.method(server, "listen", function (this: http.Server) {
    process.nextTick(() => this.emit("error", bindError));
    return this;
  });
  await assert.rejects(api.listen(), (err: unknown) => err === bindError);
  assert.deepEqual(
    { listening: server.listenerCount("listening"), error: server.listenerCount("error") },
    before,
    "no listener left behind on the error path",
  );
});

it("listen() still resolves a loopback URL and leaves no stray listeners", async () => {
  const api = new FakeApi("tok");
  const server = (api as unknown as { server: http.Server }).server;
  const errorListeners = server.listenerCount("error");
  const url = await api.listen();
  try {
    assert.match(url, /^http:\/\/127\.0\.0\.1:\d+$/);
    assert.equal(server.listenerCount("error"), errorListeners);
  } finally {
    await api.close();
  }
});
