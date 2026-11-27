// Issue #4416 M3: which Codex launches are granted the run home's Docker client config directory.
// The argv/env shape of a model command is also pinned in codex-executor.test.ts; this file adds the
// negative surfaces (boundary process, cross-check run) and the refusal of a malformed directory.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  boundaryProcessSpawnerForTest,
  makeDefaultSpawnCommand,
  runDockerConfigDir,
} from "../src/codex/codex-executor.js";
import type { CodexEffectLaunchSpec } from "../src/codex/launcher.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";

const recordingLaunch = (specs: CodexEffectLaunchSpec[]) => async (spec: CodexEffectLaunchSpec): Promise<never> => {
  specs.push(spec);
  throw new Error("recorded");
};

const WT = "/data/runner/repo/run-1";

describe("issue #4416: Docker config grant on Codex launches", () => {
  it("a model command spec places --docker-config between the cache and --mode, with DOCKER_CONFIG overridden", async () => {
    const specs: CodexEffectLaunchSpec[] = [];
    const docker = "/data/agent-home/run-1/.docker";
    const spawn = makeDefaultSpawnCommand(new ExecutionRegistry(newLocalExecutionEpoch(1)), recordingLaunch(specs), 1000, WT,
      { DOCKER_CONFIG: "/planted/.docker" }, "required", undefined, undefined, undefined, undefined, undefined, docker);
    await assert.rejects(spawn(["/bin/true"], { cwd: WT }));
    const spec = specs[0]!;
    const flags = spec.args.slice(0, spec.args.indexOf("--"));
    const at = flags.indexOf("--docker-config");
    assert.deepEqual(flags.slice(at, at + 3), ["--docker-config", docker, "--mode"]);
    assert.equal(spec.env.DOCKER_CONFIG, docker);
    assert.equal(spec.env.HOME, spec.env.TMPDIR, "HOME and TMPDIR stay the private tmp");
  });

  it("a malformed docker config dir is refused as a trusted-execution refusal", async () => {
    for (const bad of ["relative/.docker", "/data/x/docker", "/data/x/../x/.docker"]) {
      const specs: CodexEffectLaunchSpec[] = [];
      const spawn = makeDefaultSpawnCommand(new ExecutionRegistry(newLocalExecutionEpoch(1)), recordingLaunch(specs), 1000, WT,
        {}, "required", undefined, undefined, undefined, undefined, undefined, bad);
      await assert.rejects(spawn(["/bin/true"], { cwd: WT }), TrustedExecutionRefusal, bad);
      assert.equal(specs.length, 0, `${bad}: nothing launched`);
    }
  });

  it("a boundary command process carries neither the flag nor the variable", async () => {
    const specs: CodexEffectLaunchSpec[] = [];
    const boundary = boundaryProcessSpawnerForTest("required", recordingLaunch(specs));
    const clone = "/data/runner/repo/issue-1";
    await assert.rejects(boundary({ argv: ["/usr/bin/git", "-C", clone, "status"], cwd: clone, env: { DOCKER_CONFIG: "/planted/.docker" }, identity: "command" }, 1000));
    const spec = specs[0]!;
    assert.ok(!spec.args.includes("--docker-config"));
    assert.equal(spec.env.DOCKER_CONFIG, undefined);
  });

  it("the run's docker dir is the run home's .docker, and never granted to a cross_check run", () => {
    assert.equal(runDockerConfigDir("issue", "/data/agent-home/run-1"), "/data/agent-home/run-1/.docker");
    assert.equal(runDockerConfigDir(undefined, "/data/agent-home/run-1"), "/data/agent-home/run-1/.docker");
    assert.equal(runDockerConfigDir("cross_check", "/data/agent-home/run-1"), undefined);
  });
});
