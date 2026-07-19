import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  resolveDockerWiring,
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

/** No DOCKER_HOST, no default socket present, unless the case overrides. */
function opts(over: Partial<ResolveDockerWiringOptions> = {}): ResolveDockerWiringOptions {
  return { probe: okProbe, socketExists: () => false, ...over };
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
