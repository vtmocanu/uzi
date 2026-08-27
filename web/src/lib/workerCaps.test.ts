import { describe, expect, it } from "vitest";

import { effectiveWorkerCaps } from "./workerCaps";

// effectiveWorkerCaps is the web mirror of SQL fn_effective_worker_caps /
// capability.EffectiveWorkerCaps (single source since #512 M5): capabilities ∪ {docker if
// dockerEnabled}, returned as a Set.
describe("effectiveWorkerCaps", () => {
  it("returns an empty set for undefined caps with docker off", () => {
    const caps = effectiveWorkerCaps(undefined, false);
    expect(caps.size).toBe(0);
    expect(caps.has("docker")).toBe(false);
  });

  it("folds docker in when dockerEnabled, even with no stored caps", () => {
    const caps = effectiveWorkerCaps(undefined, true);
    expect(caps.has("docker")).toBe(true);
    expect(caps.size).toBe(1);
  });

  it("preserves stored caps without adding docker when docker is off", () => {
    const caps = effectiveWorkerCaps(["jvm"], false);
    expect(caps.has("jvm")).toBe(true);
    expect(caps.has("docker")).toBe(false);
    expect(caps.size).toBe(1);
  });

  it("unions stored caps with docker when dockerEnabled", () => {
    const caps = effectiveWorkerCaps(["jvm"], true);
    expect(caps.has("jvm")).toBe(true);
    expect(caps.has("docker")).toBe(true);
    expect(caps.size).toBe(2);
  });

  it("collapses a docker already present in caps (Set dedup)", () => {
    const caps = effectiveWorkerCaps(["docker"], true);
    expect(caps.has("docker")).toBe(true);
    expect(caps.size).toBe(1);
  });
});
