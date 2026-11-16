import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { DOCKER_DAEMON_PRUNE_GAP } from "../src/hibernation-inventory.js";
import { hibernationDockerScope, hibernationParticipates } from "../src/main.js";

// #4369: which workers have Docker daemon storage in their hibernation inventory scope.
describe("hibernationDockerScope", () => {
  it("names the dind_prune gap", () => {
    assert.equal(DOCKER_DAEMON_PRUNE_GAP, "dind_prune");
  });

  it("is in scope when a sidecar is expected but no daemon is wired (degraded), without participating", () => {
    for (const env of [{ DOCKER_HOST: "tcp://x:2375" }, { UZI_DIND_SOCKET: "/run/dind/docker.sock" }]) {
      assert.equal(hibernationDockerScope({ dockerWiring: {} }, env), true, JSON.stringify(env));
      assert.equal(hibernationParticipates({ dockerWiring: {} }, "shared"), false);
    }
  });

  it("is in scope when a daemon is wired, even with no env hint", () => {
    assert.equal(hibernationDockerScope({ dockerWiring: { dockerHost: "tcp://dind:2375" } }, {}), true);
  });

  it("is out of scope with no daemon and no sidecar expected", () => {
    assert.equal(hibernationDockerScope({ dockerWiring: {} }, {}), false);
  });

  it("is out of scope on the isolated lane even with a daemon wired", () => {
    const isolated = { dockerWiring: { dockerHost: "tcp://dind:2375" }, fetcherUrl: "https://f", fetcherCaFile: "/ca" };
    assert.equal(hibernationDockerScope(isolated, { DOCKER_HOST: "tcp://x:2375" }), false);
  });

  it("is in scope but not participating when epoch ownership is unsupported or absent", () => {
    const dind = { dockerWiring: { dockerHost: "tcp://dind:2375" } };
    assert.equal(hibernationDockerScope(dind, {}), true);
    assert.equal(hibernationParticipates(dind, "unsupported"), false);
    assert.equal(hibernationParticipates(dind, undefined), false);
  });
});
