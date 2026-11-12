import assert from "node:assert/strict";
import http from "node:http";

/**
 * Test-only activation override for runPodEpochShadow's deletion phase. It is deliberately defined here, in
 * a module nothing under agent/src imports: production code can neither import nor construct it by accident,
 * and test/pod-epoch-reclaim.test.ts asserts that no agent/src file references it. The literal is the one
 * pod-epoch-reclaim.ts compares against (Symbol.for, so identity needs no shared import).
 */
export const ENABLE_DELETION_FOR_TEST: symbol = Symbol.for("uzi.test.pod-epoch-reclaim.enable-deletion");

/**
 * Test-only activation override for runPodEpochShadow's runner-scratch phase (issue #4043), passed as
 * `scratch.scratchTestGate`. Defined here, in a module nothing under agent/src imports; the literal is the one
 * pod-epoch-reclaim.ts compares against, and test/pod-epoch-reclaim-scratch.test.ts asserts that no other
 * agent/src file references it.
 */
export const ENABLE_SCRATCH_FOR_TEST: symbol = Symbol.for("uzi.test.pod-epoch-reclaim.scratch-gate");

/** A minimal fake Docker daemon: healthy GET-only probes plus a held /events stream. */
export async function daemon() {
  let events: http.ServerResponse | undefined;
  const requests: string[] = [];
  const server = http.createServer((req, res) => {
    requests.push(req.method + " " + req.url);
    if (req.url?.startsWith("/events")) { events = res; res.writeHead(200); res.flushHeaders(); return; }
    const body = req.url === "/_ping" ? "OK" : JSON.stringify(
      req.url === "/info" ? { Swarm: { LocalNodeState: "inactive" }, LiveRestoreEnabled: false } :
      req.url === "/volumes" ? { Volumes: [] } : []);
    res.end(body);
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return {
    host: "tcp://127.0.0.1:" + address.port,
    requests,
    veto: () => events!.write('{"Type":"container"}\n'),
    close: async () => {
      events?.destroy();
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}
