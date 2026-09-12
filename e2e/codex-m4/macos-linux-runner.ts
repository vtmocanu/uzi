// PRD #1287 C1 — the macOS Linux-container wrapper (D7 point 3), FULLY IMPLEMENTED as a pure
// argv/plan builder. The canonical macOS target runs the SAME strict P suite inside a
// lightweight pinned Linux container (NOT the full worker image). The maintainer owns the real
// macOS execution; the worker only builds and unit-tests the plan.
//
// Two stages, deliberately separated (D7 point 3 "separate image/package/dependency
// preparation from the offline execution stage"):
//   • PREP    — `npm ci` from the COMMITTED lockfile INSIDE the container into a disposable
//               cache volume (NEVER on the macOS host; agent-browser's install hook can clobber
//               a host-wide symlink). This stage needs the registry, so it is NOT --network=none.
//   • EXECUTE — the strict P suite, OFFLINE (--network=none), reusing the prepared Linux
//               node_modules volume.
//
// Both argvs pin the `@sha256:` digest, select the native architecture, drop all capabilities,
// set no-new-privileges, run unprivileged, and mount ONLY disposable fixture/test inputs. NEITHER
// mounts the Docker socket or any HOME. Treat the digest as a hand-reviewed pin (D7 point 3).

import { resolveArch, type Arch } from "./provision.js";

/** The pinned multiarch runner image (verified amd64 + arm64 on 2026-09-12, PRD D7 point 3). */
export const MACOS_LINUX_RUNNER_IMAGE = "docker.io/library/node:24-bookworm";
export const MACOS_LINUX_RUNNER_DIGEST =
  "sha256:6dac556d980b7f0e5498d08f08cee0ca67798b4ad6c23964a9214920e67758d0";
/** The exact `image@sha256:...` reference both stages run. */
export const MACOS_LINUX_RUNNER_REF = `${MACOS_LINUX_RUNNER_IMAGE}@${MACOS_LINUX_RUNNER_DIGEST}`;

/** An unprivileged default (node:bookworm ships the `node` user at uid 1000). */
const DEFAULT_USER = "1000:1000";

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
  /** Unprivileged container user, `uid:gid`. Defaults to 1000:1000. */
  readonly user?: string;
}

/** The two-stage plan. Each stage is a full argv (argv[0] === "docker"). */
export interface MacosLinuxRunPlan {
  readonly prep: readonly string[];
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
 * Build the two-stage macOS Linux-container run plan. Pure: no docker is invoked, no host
 * mutation. {@link assertPrerequisites} is the caller's hard-fail gate; this only shapes argv.
 */
export function buildMacosLinuxRunPlan(opts: MacosLinuxRunOptions): MacosLinuxRunPlan {
  const user = opts.user ?? DEFAULT_USER;
  const prep = [
    ...baseArgs(opts.arch, user),
    // Committed lockfile source (read-only); node_modules is the disposable volume npm ci fills.
    "-v", `${opts.agentDir}:/prep/agent:ro`,
    "-v", `${opts.depsVolume}:/prep/agent/node_modules`,
    "-w", "/prep/agent",
    MACOS_LINUX_RUNNER_REF,
    // `npm ci` from the COMMITTED lockfile, inside the container, into the cache volume.
    "npm", "ci",
  ];
  const execute = [
    ...baseArgs(opts.arch, user),
    // External network disabled during tests (D7 point 3).
    "--network=none",
    // Test inputs read-only; the prepared Linux deps volume mounted read-only.
    "-v", `${opts.agentDir}:/work/agent:ro`,
    "-v", `${opts.e2eDir}:/work/e2e:ro`,
    "-v", `${opts.depsVolume}:/work/agent/node_modules:ro`,
    "-w", "/work/agent",
    MACOS_LINUX_RUNNER_REF,
    // The strict, serial, bounded P suite (mirrors test:codex-m4's node invocation).
    "node", "--import", "tsx", "--test", "--test-concurrency=1", "--test-timeout=120000",
    "../e2e/codex-m4/startup-smoke.test.ts",
  ];
  return { prep, execute };
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
