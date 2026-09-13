// PRD #1287 C1 — the macOS Linux-container wrapper (D7 point 3), FULLY IMPLEMENTED as a pure
// argv/plan builder. The canonical macOS target runs the SAME strict P suite inside a
// lightweight pinned Linux container (NOT the full worker image). The maintainer owns the real
// macOS execution; the worker only builds and unit-tests the plan.
//
// Three stages, deliberately separated (D7 point 3 "separate image/package/dependency
// preparation from the offline execution stage"). The two PREP stages are network-enabled;
// only EXECUTE is offline:
//   • PREP (deps)  — `npm ci` from the COMMITTED lockfile INSIDE the container into a disposable
//                    cache volume (NEVER on the macOS host; agent-browser's install hook can
//                    clobber a host-wide symlink). Needs the registry, so NOT --network=none.
//   • PREP (codex) — provision the lock-verified Codex package into a disposable codex volume by
//                    running `agent/codex/install-codex.sh <arch>` with UZI_CODEX_PREFIX pointed
//                    at that volume. The installer retains fail-closed SHA256 verification and
//                    keeps the binaries OFF PATH. Needs the release artifact, so NOT
//                    --network=none. (D7 point 3 "reuse the lock-verified package"; "Linux CI may
//                    fetch only the lock-pinned release artifact during preparation".)
//   • EXECUTE      — the strict P suite, OFFLINE (--network=none). It mounts the prepared Linux
//                    node_modules volume AND the codex volume, both READ-ONLY.
//
// The codex volume is provisioned at, and mounted read-only into EXECUTE at, the image-baked
// prefix root /opt/uzi-codex (CODEX_PREFIX_MOUNT). This is the SIMPLER of the two options in D7
// point 3: because install-codex.sh lays the package down at ${UZI_CODEX_PREFIX}/<version> and
// resolveCodexBin's DEFAULT_IMAGE_ROOT is also /opt/uzi-codex, the existing image-baked branch
// resolves /opt/uzi-codex/<version>/bin/codex offline with NO env override threaded into the
// test process. EXECUTE runs --network=none with no way to install, so the volume is what makes
// resolveCodexBin succeed inside it.
//
// Every argv pins the `@sha256:` digest, selects the native architecture, drops all capabilities,
// sets no-new-privileges, and mounts ONLY disposable fixture/test inputs. The two preparation stages
// run as root so they can initialize root-owned named volumes; EXECUTE stays unprivileged and sees
// those volumes read-only. NONE mounts the Docker socket or any HOME. Treat the digest as a
// hand-reviewed pin (D7 point 3).

import { resolveArch, type Arch } from "./provision.js";

/** The pinned multiarch runner image (verified amd64 + arm64 on 2026-09-12, PRD D7 point 3). */
export const MACOS_LINUX_RUNNER_IMAGE = "docker.io/library/node:24-bookworm";
export const MACOS_LINUX_RUNNER_DIGEST =
  "sha256:6dac556d980b7f0e5498d08f08cee0ca67798b4ad6c23964a9214920e67758d0";
/** The exact `image@sha256:...` reference both stages run. */
export const MACOS_LINUX_RUNNER_REF = `${MACOS_LINUX_RUNNER_IMAGE}@${MACOS_LINUX_RUNNER_DIGEST}`;

/** An unprivileged default (node:bookworm ships the `node` user at uid 1000). */
const DEFAULT_USER = "1000:1000";

/** The strict P-layer suite (real-binary protocol tests). Single source of truth for the execute
 *  argv AND the maintainer's macOS container run. */
export const STRICT_P_SUITE: readonly string[] = [
  "startup-smoke.test.ts",
  "policy-real.test.ts",
  "native-bypass.test.ts",
];

/** Where the disposable codex volume is provisioned and, read-only, mounted for EXECUTE. It
 *  matches resolveCodexBin's DEFAULT_IMAGE_ROOT so the existing image-baked branch resolves the
 *  package offline (install-codex.sh writes to ${UZI_CODEX_PREFIX}/<version>). */
const CODEX_PREFIX_MOUNT = "/opt/uzi-codex";

export interface MacosLinuxRunOptions {
  /** The native architecture (`amd64` | `arm64`), chosen by the caller after prereq checks. */
  readonly arch: Arch;
  /** Host path to the `agent/` package (source of the committed lockfile + adapter src). */
  readonly agentDir: string;
  /** Host path to the `e2e/` tree (the M4 test inputs). */
  readonly e2eDir: string;
  /** A docker VOLUME name for the prepared Linux node_modules (disposable/cache). NEVER a
   *  bind mount of the macOS host's node_modules. */
  readonly depsVolume: string;
  /** A docker VOLUME name for the lock-verified Codex package (disposable/cache). install-codex.sh
   *  writes ${UZI_CODEX_PREFIX}/<version> here in PREP; EXECUTE mounts it read-only. NEVER a bind
   *  mount of a host codex install, and its binaries never join PATH. */
  readonly codexVolume: string;
  /** Unprivileged EXECUTE-stage user, `uid:gid`. Defaults to 1000:1000. */
  readonly user?: string;
}

/** The three-stage plan. Each stage is a full argv (argv[0] === "docker"). `prep` and `prepCodex`
 *  are the two network-enabled preparation stages; `execute` is the offline P suite. */
export interface MacosLinuxRunPlan {
  readonly prep: readonly string[];
  readonly prepCodex: readonly string[];
  readonly execute: readonly string[];
}

function baseArgs(arch: Arch, user: string): string[] {
  // Shared hardening every stage carries. `--network=none` is added ONLY to EXECUTE.
  return [
    "docker", "run", "--rm",
    "--platform", `linux/${arch}`,
    "--cap-drop=ALL",
    "--security-opt=no-new-privileges",
    "--user", user,
  ];
}

/**
 * Build the three-stage macOS Linux-container run plan (two network-enabled prep stages, one
 * offline execute stage). Pure: no docker is invoked, no host mutation. {@link assertPrerequisites}
 * is the caller's hard-fail gate; this only shapes argv.
 */
export function buildMacosLinuxRunPlan(opts: MacosLinuxRunOptions): MacosLinuxRunPlan {
  const user = opts.user ?? DEFAULT_USER;
  const prep = [
    ...baseArgs(opts.arch, "0:0"),
    // Committed lockfile source (read-only); node_modules is the disposable volume npm ci fills.
    "-v", `${opts.agentDir}:/prep/agent:ro`,
    "-v", `${opts.depsVolume}:/prep/agent/node_modules`,
    "-w", "/prep/agent",
    MACOS_LINUX_RUNNER_REF,
    // `npm ci` from the COMMITTED lockfile, inside the container, into the cache volume.
    "npm", "ci",
  ];
  const prepCodex = [
    ...baseArgs(opts.arch, "0:0"),
    // The installer reads UZI_CODEX_PREFIX from the env; point it at the codex volume mountpoint.
    // UZI_CODEX_LOCK defaults to the sibling lock in the mounted agent tree.
    "-e", `UZI_CODEX_PREFIX=${CODEX_PREFIX_MOUNT}`,
    // agent/ (read-only) supplies install-codex.sh + codex-package.lock; the codex volume is
    // read-WRITE here so the installer can lay the verified package down at <prefix>/<version>.
    "-v", `${opts.agentDir}:/prep/agent:ro`,
    "-v", `${opts.codexVolume}:${CODEX_PREFIX_MOUNT}`,
    "-w", "/prep/agent",
    MACOS_LINUX_RUNNER_REF,
    // The pinned, SHA256-verified Codex package install (binaries stay OFF PATH — D7 point 3).
    "/prep/agent/codex/install-codex.sh", opts.arch,
  ];
  const execute = [
    ...baseArgs(opts.arch, user),
    // External network disabled during tests (D7 point 3).
    "--network=none",
    // Point the P suite's recordEvidence at the mounted host evidence file so the container's
    // codex/P evidence lands on the HOST alongside the native U run's (run-completeness merges them).
    "-e", "CODEX_M4_EVIDENCE=/work/e2e/codex-m4/.evidence/current.jsonl",
    // Test inputs read-only; the prepared Linux deps volume mounted read-only.
    "-v", `${opts.agentDir}:/work/agent:ro`,
    "-v", `${opts.e2eDir}:/work/e2e:ro`,
    // A nested READ-WRITE mount over the read-only e2e parent (docker layers the more-specific
    // path as writable) so recordEvidence can append the P evidence to the HOST .evidence file.
    "-v", `${opts.e2eDir}/codex-m4/.evidence:/work/e2e/codex-m4/.evidence`,
    "-v", `${opts.depsVolume}:/work/agent/node_modules:ro`,
    // The provisioned codex volume mounted READ-ONLY at the image-baked prefix root, so
    // resolveCodexBin's existing image-baked branch finds /opt/uzi-codex/<version>/bin/codex
    // offline (no env override needed). No install is possible under --network=none.
    "-v", `${opts.codexVolume}:${CODEX_PREFIX_MOUNT}:ro`,
    "-w", "/work/agent",
    MACOS_LINUX_RUNNER_REF,
    // The strict, serial, bounded P subset of test:codex-m4: all three P files, writing evidence
    // to the host via the read-write .evidence mount above.
    "node", "--import", "tsx", "--test", "--test-concurrency=1", "--test-timeout=120000",
    ...STRICT_P_SUITE.map((f) => `../e2e/codex-m4/${f}`),
  ];
  return { prep, prepCodex, execute };
}

/** The macOS-host facts the prerequisite gate inspects. */
export interface MacosLinuxPrereqEnv {
  /** The resolved `docker` binary path, or undefined when docker is missing. */
  readonly dockerPath: string | undefined;
  /** The macOS host's `process.arch`. */
  readonly nodeArch: string;
  /** Whether `/etc/codex` exists on the host (an unexpected managed config). */
  readonly etcCodexPresent: boolean;
}

/**
 * HARD-FAIL (throw) on missing docker, wrong/unsupported architecture, or an unexpected
 * /etc/codex (D7 point 3 — never a platform skip). Returns the resolved native {@link Arch} to
 * feed {@link buildMacosLinuxRunPlan}.
 */
export function assertPrerequisites(env: MacosLinuxPrereqEnv): Arch {
  if (env.dockerPath === undefined || env.dockerPath.length === 0) {
    throw new Error("macOS Linux-container runner requires docker; none was found");
  }
  const arch = resolveArch(env.nodeArch); // throws CodexProvisionError on an unsupported arch
  if (env.etcCodexPresent) {
    throw new Error("macOS Linux-container runner refuses to run with an unexpected /etc/codex present");
  }
  return arch;
}

/** Injectable seams so the worker can unit-test the orchestrator without a real macOS/docker run. */
export interface MacosLinuxExecDeps {
  readonly whichDocker: () => string | undefined;
  readonly nodeArch: string;
  readonly etcCodexPresent: boolean;
  readonly agentDir: string;
  readonly e2eDir: string;
  readonly depsVolume?: string;
  readonly codexVolume?: string;
  readonly user?: string;
  /** Run one docker stage argv; returns its exit code (0 = ok). */
  readonly runStage: (argv: readonly string[]) => number;
  readonly log?: (message: string) => void;
}

/**
 * Prereq-gate (HARD-FAIL on missing docker / unsupported arch / unexpected /etc/codex via
 * {@link assertPrerequisites}), build the plan, then run prep → prepCodex → execute in order,
 * throwing on the first non-zero stage. Pure orchestration over injected seams; the entry
 * (run-macos-linux.ts) wires the real docker/child_process seams.
 */
export function executeMacosLinuxRun(deps: MacosLinuxExecDeps): void {
  const arch = assertPrerequisites({
    dockerPath: deps.whichDocker(),
    nodeArch: deps.nodeArch,
    etcCodexPresent: deps.etcCodexPresent,
  });
  const plan = buildMacosLinuxRunPlan({
    arch,
    agentDir: deps.agentDir,
    e2eDir: deps.e2eDir,
    depsVolume: deps.depsVolume ?? "uzi-codex-m4-deps",
    codexVolume: deps.codexVolume ?? "uzi-codex-m4-codex",
    user: deps.user,
  });
  const stages: readonly [string, readonly string[]][] = [
    ["prep (npm ci)", plan.prep],
    ["prepCodex (install-codex.sh)", plan.prepCodex],
    ["execute (strict P suite)", plan.execute],
  ];
  for (const [name, argv] of stages) {
    deps.log?.(`[codex-m4 macos] ${name}: ${argv.join(" ")}`);
    const code = deps.runStage(argv);
    if (code !== 0) throw new Error(`macOS Linux-container stage "${name}" exited ${code}`);
  }
}
