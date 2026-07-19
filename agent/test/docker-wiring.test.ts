import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  resolveDockerWiring,
  dockerSidecarExpected,
  DEFAULT_DIND_SOCKET,
  type ResolveDockerWiringOptions,
} from "../src/docker-wiring.js";

// The keystone resolver (PRD #83 M1). Three consumers — the register capability report
// (Q1), the guardrail's allow-when-wired decision (Q3), and DOCKER_HOST into the SDK env
// (Q4) — all read its single `dockerHost` result. "Resolved" is NOT "reachable": the
// field is set ONLY when a bounded liveness probe passes. The probe + socket-existence
// are injected so every branch is provable with NO daemon (M1 has none anyway).

const okProbe = async (): Promise<boolean> => true;
const failProbe = async (): Promise<boolean> => false;

/** No DOCKER_HOST, no default socket present, unless the case overrides. `readyTimeoutMs: 0`
 *  + a no-op sleep keep these single-probe (the readiness WAIT has its own tests below), so
 *  a failing-probe case returns immediately instead of blocking the real 30s budget. */
function opts(over: Partial<ResolveDockerWiringOptions> = {}): ResolveDockerWiringOptions {
  return { probe: okProbe, socketExists: () => false, readyTimeoutMs: 0, sleep: async () => {}, ...over };
}

describe("resolveDockerWiring", () => {
  it("honors an explicit DOCKER_HOST verbatim when the probe passes (the k8s path)", async () => {
    const w = await resolveDockerWiring({ DOCKER_HOST: "tcp://dind:2375" }, opts());
    assert.strictEqual(w.dockerHost, "tcp://dind:2375");
  });

  it("falls back to the default sidecar socket when present + reachable (the compose path)", async () => {
    const seen: string[] = [];
    const w = await resolveDockerWiring(
      {},
      opts({
        socketExists: (p) => {
          seen.push(p);
          return p === DEFAULT_DIND_SOCKET;
        },
      }),
    );
    assert.strictEqual(w.dockerHost, `unix://${DEFAULT_DIND_SOCKET}`);
    assert.deepStrictEqual(seen, [DEFAULT_DIND_SOCKET]);
  });

  it("returns {} when neither DOCKER_HOST nor a socket is present (no daemon → inert, capability absent)", async () => {
    const w = await resolveDockerWiring({}, opts());
    assert.deepStrictEqual(w, {});
    assert.strictEqual(w.dockerHost, undefined);
  });

  it("returns {} when a candidate RESOLVES but the liveness probe FAILS (reachable ≠ configured)", async () => {
    // DOCKER_HOST set but nothing answers: capability absent, guardrail denies, DOCKER_HOST
    // NOT injected — this is the M1 reality (a path may be configured, no daemon exists).
    const viaHost = await resolveDockerWiring({ DOCKER_HOST: "tcp://dind:2375" }, opts({ probe: failProbe }));
    assert.deepStrictEqual(viaHost, {});
    // Same for the socket path: it exists on disk but no daemon listens.
    const viaSocket = await resolveDockerWiring({}, opts({ probe: failProbe, socketExists: () => true }));
    assert.deepStrictEqual(viaSocket, {});
  });

  it("treats a throwing probe as unreachable (never fails worker startup)", async () => {
    const w = await resolveDockerWiring(
      { DOCKER_HOST: "tcp://dind:2375" },
      opts({ probe: async () => { throw new Error("boom"); } }),
    );
    assert.deepStrictEqual(w, {});
  });

  it("honors UZI_DIND_SOCKET as the probed socket path override", async () => {
    const custom = "/var/run/my-dind.sock";
    const w = await resolveDockerWiring(
      { UZI_DIND_SOCKET: custom },
      opts({ socketExists: (p) => p === custom }),
    );
    assert.strictEqual(w.dockerHost, `unix://${custom}`);
  });

  it("prefers an explicit DOCKER_HOST over the socket probe (does not stat the socket)", async () => {
    let statted = false;
    const w = await resolveDockerWiring(
      { DOCKER_HOST: "unix:///run/dind/docker.sock" },
      opts({ socketExists: () => { statted = true; return true; } }),
    );
    assert.strictEqual(w.dockerHost, "unix:///run/dind/docker.sock");
    assert.strictEqual(statted, false, "an explicit DOCKER_HOST short-circuits the socket stat");
  });

  it("ignores a blank/whitespace DOCKER_HOST and falls through to the socket probe", async () => {
    const w = await resolveDockerWiring({ DOCKER_HOST: "   " }, opts({ socketExists: () => true }));
    assert.strictEqual(w.dockerHost, `unix://${DEFAULT_DIND_SOCKET}`);
  });
});

// M2 follow-up: the bounded readiness wait. Only an EXPECTED sidecar (DOCKER_HOST or
// UZI_DIND_SOCKET set) is waited for; a non-docker worker never blocks. Timeout ⇒ unwired.
describe("resolveDockerWiring readiness wait (PRD #83 M2)", () => {
  it("dockerSidecarExpected keys on DOCKER_HOST / UZI_DIND_SOCKET only", () => {
    assert.strictEqual(dockerSidecarExpected({ DOCKER_HOST: "tcp://dind:2375" }), true);
    assert.strictEqual(dockerSidecarExpected({ UZI_DIND_SOCKET: "/run/dind/docker.sock" }), true);
    assert.strictEqual(dockerSidecarExpected({}), false);
    assert.strictEqual(dockerSidecarExpected({ DOCKER_HOST: "  " }), false); // blank ⇒ not expected
  });

  it("RETRIES an expected sidecar's probe until the daemon comes up (compose/k8s start race)", async () => {
    let calls = 0;
    const flaky = async (): Promise<boolean> => { calls++; return calls >= 3; }; // up on the 3rd probe
    const w = await resolveDockerWiring(
      { UZI_DIND_SOCKET: "/run/dind/docker.sock" },
      { probe: flaky, sleep: async () => {}, readyIntervalMs: 1, readyTimeoutMs: 10_000 },
    );
    assert.strictEqual(w.dockerHost, "unix:///run/dind/docker.sock");
    assert.strictEqual(calls, 3, "should have retried until reachable");
  });

  it("forms the candidate from UZI_DIND_SOCKET even before the socket file exists (wait bridges it)", async () => {
    // socketExists is NEVER consulted for an explicit UZI_DIND_SOCKET — the probe/retry owns
    // liveness, so the wait can span the window before the daemon writes the socket.
    let statted = false;
    let calls = 0;
    const w = await resolveDockerWiring(
      { UZI_DIND_SOCKET: "/run/dind/docker.sock" },
      {
        socketExists: () => { statted = true; return false; },
        probe: async () => { calls++; return calls >= 2; },
        sleep: async () => {}, readyIntervalMs: 1, readyTimeoutMs: 10_000,
      },
    );
    assert.strictEqual(w.dockerHost, "unix:///run/dind/docker.sock");
    assert.strictEqual(statted, false, "an explicit UZI_DIND_SOCKET must not gate on socket existence");
  });

  it("does NOT wait when no sidecar is expected — exactly ONE probe even with a generous budget", async () => {
    let calls = 0;
    const w = await resolveDockerWiring(
      {}, // neither DOCKER_HOST nor UZI_DIND_SOCKET → not expected
      {
        socketExists: () => true, // default socket present ⇒ a candidate forms…
        probe: async () => { calls++; return false; }, // …but the probe fails
        readyIntervalMs: 1, readyTimeoutMs: 30_000, sleep: async () => {},
      },
    );
    assert.deepStrictEqual(w, {}, "a failed single probe on a non-expected worker yields unwired");
    assert.strictEqual(calls, 1, "a non-docker worker must NEVER block: exactly one probe");
  });

  it("degrades to unwired after the readiness timeout when an expected daemon never comes up", async () => {
    let calls = 0;
    const w = await resolveDockerWiring(
      { DOCKER_HOST: "tcp://dind:2375" },
      { probe: async () => { calls++; return false; }, readyIntervalMs: 5, readyTimeoutMs: 30 },
    );
    assert.deepStrictEqual(w, {}, "timeout ⇒ degrade to unwired");
    assert.ok(calls >= 2, `expected multiple probe attempts before the timeout, got ${calls}`);
  });
});
