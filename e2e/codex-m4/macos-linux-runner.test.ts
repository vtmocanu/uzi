// PRD #1287 C1 — self-tests for the macOS Linux-container wrapper (D7 point 3). Asserts the argv
// invariants (pinned @sha256 digest, native arch, cap-drop, no-new-privs, execute-stage
// --network=none, npm ci in the deps prep, install-codex.sh in the codex prep, the codex volume
// mounted READ-ONLY into EXECUTE, NO docker socket, NO HOME mount) and that each hard-prerequisite
// branch throws. The worker does NOT execute a real macOS/container run — the builder + prereq
// logic are exercised alone. (The maintainer owns real macOS execution.)

import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  MACOS_LINUX_RUNNER_DIGEST,
  MACOS_LINUX_RUNNER_REF,
  assertPrerequisites,
  buildMacosLinuxRunPlan,
  executeMacosLinuxRun,
  type MacosLinuxExecDeps,
  type MacosLinuxRunOptions,
} from "./macos-linux-runner.js";
import { CodexProvisionError } from "./provision.js";

const OPTS: MacosLinuxRunOptions = {
  arch: "arm64",
  agentDir: "/Users/dev/uzi/agent",
  e2eDir: "/Users/dev/uzi/e2e",
  depsVolume: "uzi-codex-m4-deps",
  codexVolume: "uzi-codex-m4-codex",
};

/** The image-baked prefix root the codex volume is provisioned at and mounted read-only for
 *  EXECUTE (matches resolveCodexBin's DEFAULT_IMAGE_ROOT). */
const CODEX_PREFIX_MOUNT = "/opt/uzi-codex";

describe("buildMacosLinuxRunPlan argv invariants", () => {
  const plan = buildMacosLinuxRunPlan(OPTS);
  const stages: [string, readonly string[]][] = [
    ["prep", plan.prep],
    ["prepCodex", plan.prepCodex],
    ["execute", plan.execute],
  ];

  for (const [name, argv] of stages) {
    it(`${name}: argv[0] is docker run`, () => {
      assert.equal(argv[0], "docker");
      assert.equal(argv[1], "run");
    });
    it(`${name}: pins the @sha256 digest`, () => {
      assert.ok(argv.includes(MACOS_LINUX_RUNNER_REF), "the image ref carries the pinned digest");
      assert.ok(MACOS_LINUX_RUNNER_REF.includes(`@${MACOS_LINUX_RUNNER_DIGEST}`));
    });
    it(`${name}: selects the native architecture`, () => {
      const i = argv.indexOf("--platform");
      assert.ok(i >= 0 && argv[i + 1] === "linux/arm64");
    });
    it(`${name}: drops all capabilities and sets no-new-privileges`, () => {
      assert.ok(argv.includes("--cap-drop=ALL"));
      assert.ok(argv.includes("--security-opt=no-new-privileges"));
    });
    it(`${name}: mounts NO docker socket and NO HOME`, () => {
      const joined = argv.join(" ");
      assert.ok(!joined.includes("docker.sock"), "no docker socket mount");
      assert.ok(!joined.includes("/var/run/docker.sock"));
      assert.ok(!argv.some((a) => a.includes(":/root")), "no HOME/root mount");
      assert.ok(!argv.some((a) => a.includes(":/home/")), "no HOME mount");
    });
  }

  it("initializes named volumes as root and executes the P suite unprivileged", () => {
    for (const [name, argv] of [["prep", plan.prep], ["prepCodex", plan.prepCodex]] as const) {
      const user = argv.indexOf("--user");
      assert.ok(user >= 0 && argv[user + 1] === "0:0", `${name} initializes its root-owned volume as root`);
    }
    const executeUser = plan.execute.indexOf("--user");
    assert.ok(
      executeUser >= 0 && plan.execute[executeUser + 1] === "1000:1000",
      "execute remains unprivileged",
    );
  });

  it("prep runs npm ci (dependency preparation stage)", () => {
    assert.ok(plan.prep.includes("npm") && plan.prep.includes("ci"), "prep runs npm ci");
    // Prep MUST reach the registry, so it is NOT --network=none.
    assert.ok(!plan.prep.includes("--network=none"), "prep is not offline");
  });

  it("prepCodex provisions the lock-verified codex package into the codex volume", () => {
    // install-codex.sh with the native arch (SHA256 verification retained by the installer).
    assert.ok(
      plan.prepCodex.some((a) => a.includes("install-codex.sh")),
      "prepCodex runs install-codex.sh",
    );
    assert.ok(plan.prepCodex.includes(OPTS.arch), "prepCodex passes the native arch to the installer");
    // The installer writes the package into the codex volume: UZI_CODEX_PREFIX + the RW mount.
    assert.ok(
      plan.prepCodex.includes(`UZI_CODEX_PREFIX=${CODEX_PREFIX_MOUNT}`),
      "prepCodex points UZI_CODEX_PREFIX at the codex volume mountpoint",
    );
    assert.ok(
      plan.prepCodex.some((a) => a === `${OPTS.codexVolume}:${CODEX_PREFIX_MOUNT}`),
      "prepCodex mounts the codex volume read-write so the installer can write it",
    );
    // Codex prep MUST reach the release artifact, so it is NOT --network=none.
    assert.ok(!plan.prepCodex.includes("--network=none"), "prepCodex is not offline");
    // The binaries never join PATH: only the absolute install path is populated.
    assert.ok(!plan.prepCodex.some((a) => a.startsWith("PATH=")), "prepCodex does not export PATH");
  });

  it("execute is offline (--network=none) and runs the strict suite", () => {
    assert.ok(plan.execute.includes("--network=none"), "execute disables external network");
    assert.ok(plan.execute.includes("node") && plan.execute.includes("--test"), "execute runs the node test suite");
    assert.ok(plan.execute.includes("--test-concurrency=1"), "execute runs serially");
    // Execute mounts the prepared deps volume READ-ONLY, never the macOS node_modules.
    assert.ok(plan.execute.some((a) => a === `${OPTS.depsVolume}:/work/agent/node_modules:ro`));
  });

  it("execute mounts the provisioned codex volume READ-ONLY at the image-baked prefix", () => {
    // The offline stage cannot install; the read-only codex volume at /opt/uzi-codex is what
    // makes resolveCodexBin's image-baked branch resolve the package inside --network=none.
    assert.ok(
      plan.execute.some((a) => a === `${OPTS.codexVolume}:${CODEX_PREFIX_MOUNT}:ro`),
      "execute mounts the codex volume read-only at /opt/uzi-codex",
    );
    // Negative invariants restated on the offline stage: still no docker socket, still no HOME.
    assert.ok(plan.execute.includes("--network=none"), "execute is still offline");
    const joined = plan.execute.join(" ");
    assert.ok(!joined.includes("docker.sock"), "execute mounts no docker socket");
    assert.ok(!plan.execute.some((a) => a.includes(":/root")), "execute mounts no HOME/root");
    assert.ok(!plan.execute.some((a) => a.includes(":/home/")), "execute mounts no HOME");
  });

  it("execute runs the complete strict P suite (all three P files)", () => {
    for (const f of ["startup-smoke.test.ts", "policy-real.test.ts", "native-bypass.test.ts"]) {
      assert.ok(
        plan.execute.includes(`../e2e/codex-m4/${f}`),
        `execute runs ${f}`,
      );
    }
  });
  it("execute writes codex/P evidence to the host (env + read-write .evidence mount)", () => {
    assert.ok(
      plan.execute.includes("CODEX_M4_EVIDENCE=/work/e2e/codex-m4/.evidence/current.jsonl"),
      "execute points CODEX_M4_EVIDENCE at the mounted host evidence file",
    );
    assert.ok(
      plan.execute.some((a) => a === `${OPTS.e2eDir}/codex-m4/.evidence:/work/e2e/codex-m4/.evidence`),
      "execute mounts the host .evidence dir read-WRITE so recordEvidence reaches the host file",
    );
  });
});

describe("executeMacosLinuxRun orchestration", () => {
  /** Build injectable deps with a recording runStage over the healthy-host defaults. */
  function makeDeps(overrides: Partial<MacosLinuxExecDeps> = {}): {
    deps: MacosLinuxExecDeps;
    argvs: (readonly string[])[];
  } {
    const argvs: (readonly string[])[] = [];
    const deps: MacosLinuxExecDeps = {
      whichDocker: () => "docker",
      nodeArch: "arm64",
      etcCodexPresent: false,
      agentDir: OPTS.agentDir,
      e2eDir: OPTS.e2eDir,
      runStage: (argv) => {
        argvs.push(argv);
        return 0;
      },
      ...overrides,
    };
    return { deps, argvs };
  }

  it("runs prep → prepCodex → execute in order, each a docker invocation", () => {
    const { deps, argvs } = makeDeps();
    executeMacosLinuxRun(deps);
    assert.equal(argvs.length, 3, "three stages ran");
    for (const argv of argvs) assert.equal(argv[0], "docker", "each stage is a docker invocation");
    assert.ok(!argvs[0].includes("--network=none"), "stage 1 (prep) is not the offline stage");
    assert.ok(!argvs[1].includes("--network=none"), "stage 2 (prepCodex) is not the offline stage");
    assert.ok(argvs[2].includes("--network=none"), "stage 3 is the offline execute stage");
  });

  it("hard-fails when docker is missing (before running any stage)", () => {
    const { deps, argvs } = makeDeps({ whichDocker: () => undefined });
    assert.throws(() => executeMacosLinuxRun(deps), /requires docker/);
    assert.equal(argvs.length, 0, "no stage ran on the prereq failure");
  });

  it("hard-fails on an unsupported architecture (before running any stage)", () => {
    const { deps, argvs } = makeDeps({ nodeArch: "ia32" });
    assert.throws(() => executeMacosLinuxRun(deps), CodexProvisionError);
    assert.equal(argvs.length, 0, "no stage ran on the prereq failure");
  });

  it("throws on the first non-zero stage and does NOT run the execute stage", () => {
    let call = 0;
    const argvs: (readonly string[])[] = [];
    const { deps } = makeDeps({
      runStage: (argv) => {
        argvs.push(argv);
        call += 1;
        return call === 2 ? 1 : 0; // prepCodex fails
      },
    });
    assert.throws(() => executeMacosLinuxRun(deps), /exited 1/);
    assert.equal(argvs.length, 2, "stopped after the failing prepCodex stage");
    assert.ok(!argvs.some((a) => a.includes("--network=none")), "the offline execute stage never ran");
  });
});

describe("assertPrerequisites hard-fail branches", () => {
  const ok = { dockerPath: "/usr/local/bin/docker", nodeArch: "arm64", etcCodexPresent: false };

  it("returns the native arch on a healthy host", () => {
    assert.equal(assertPrerequisites(ok), "arm64");
    assert.equal(assertPrerequisites({ ...ok, nodeArch: "x64" }), "amd64");
  });
  it("hard-fails on missing docker", () => {
    assert.throws(() => assertPrerequisites({ ...ok, dockerPath: undefined }), /requires docker/);
  });
  it("hard-fails on an unsupported architecture", () => {
    assert.throws(() => assertPrerequisites({ ...ok, nodeArch: "ia32" }), CodexProvisionError);
  });
  it("hard-fails on an unexpected /etc/codex", () => {
    assert.throws(() => assertPrerequisites({ ...ok, etcCodexPresent: true }), /unexpected \/etc\/codex/);
  });
});
