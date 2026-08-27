// The docker-wiring keystone (PRD #83 M1, arch doc §Keystone).
//
// Three of the PRD's questions — the capability self-report (Q1), the guardrail's
// allow-when-wired decision (Q3), and delivering DOCKER_HOST to the SDK env (Q4) —
// all collapse onto ONE worker-side primitive built here in M1. M2 (compose) and M3
// (k8s) only *supply* the socket to it; they never re-implement it.
//
//   - k8s is EXPLICIT: the controller renders DOCKER_HOST, so resolution uses it
//     verbatim.
//   - compose is IMPLICIT: the sidecar shares its socket via a named volume, and the
//     worker auto-detects it by probing UZI_DIND_SOCKET (default /run/dind/docker.sock).
//
// That decoupling is what lets M2 ∥ M3 ship without agreeing on a socket path.
//
// "Resolved" (a candidate DOCKER_HOST exists) is DELIBERATELY NOT the same as
// "reachable". The single `dockerHost` field is set ONLY when a bounded liveness probe
// against the candidate succeeds — so a stale socket path or an unset DOCKER_HOST both
// yield {} (capability absent, guardrail denies, DOCKER_HOST NOT injected). In M1 there
// is no daemon anywhere, so this resolves to {} in practice; the primitive exists so
// M2/M3 wire a real sidecar into it without touching the consumers.
//
// Compute this ONCE at worker startup (main.ts) and carry the result on Config; the
// register capability report, the guardrail hook, and the SDK env all read the same
// resolved value. Never re-probe per run.

import net from "node:net";
import fs from "node:fs";

/** The resolved docker wiring for this worker's lifetime. `dockerHost` is present
 *  ONLY when a daemon is reachable at the resolved endpoint (probe passed); absent
 *  ⇒ no daemon, docker is inert + denied and no capability is reported. */
export interface DockerWiring {
  dockerHost?: string;
}

/** The default sidecar socket the compose track shares into the worker; overridable
 *  via UZI_DIND_SOCKET. The daemon lives in the sidecar's own mount namespace and the
 *  volume carries ONLY the socket (PRD #83 Decision 3). */
export const DEFAULT_DIND_SOCKET = "/run/dind/docker.sock";

/** Bounded liveness probe budget (~2s): a wired daemon answers a socket connect well
 *  within this, and an absent/stale endpoint must not stall worker startup. */
const DEFAULT_PROBE_TIMEOUT_MS = 2_000;

/** Readiness-wait cadence + budget (M2 follow-up). Used ONLY when a sidecar is EXPECTED
 *  (DOCKER_HOST or UZI_DIND_SOCKET set) — the worker retries the probe up to the timeout so
 *  it does not lose the capability just because the daemon container is a few seconds slower
 *  to come up than the worker (the compose start race, and the k8s pod-sidecar race). */
const DEFAULT_READY_INTERVAL_MS = 1_000;
const DEFAULT_READY_TIMEOUT_MS = 30_000;

const realSleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

export interface ResolveDockerWiringOptions {
  /** Reachability probe; injected in tests so the resolver is provable with no daemon.
   *  Resolves true iff a daemon answers on `dockerHost` within the timeout. Default =
   *  a bounded raw socket connect (unix:// or tcp://). */
  probe?: (dockerHost: string, timeoutMs: number) => Promise<boolean>;
  /** Existence check for the sidecar socket path (default = a stat that also verifies
   *  the entry is a socket). Injected in tests. Consulted ONLY for the default-path
   *  auto-detect (a non-expected worker) — an explicit UZI_DIND_SOCKET forms the candidate
   *  regardless, so the readiness wait can span the window before the socket file appears. */
  socketExists?: (socketPath: string) => boolean;
  /** Probe timeout override (ms). */
  probeTimeoutMs?: number;
  /** Readiness-wait cadence (ms) between probes; only used when a sidecar is expected. */
  readyIntervalMs?: number;
  /** Readiness-wait budget (ms) before degrading to unwired; only used when expected. 0 ⇒
   *  exactly one probe, no wait (the non-expected path). */
  readyTimeoutMs?: number;
  /** Injectable sleep (tests drive the wait with no real delay). */
  sleep?: (ms: number) => Promise<void>;
  /** Injectable clock (ms epoch). Paired with `sleep` so tests drive the readiness
   *  deadline deterministically instead of racing real wall time. Default = Date.now. */
  now?: () => number;
}

/** True when the operator EXPECTS a sidecar: DOCKER_HOST (k8s) or UZI_DIND_SOCKET (compose
 *  opt-in) set non-empty. Only then does the worker WAIT for the daemon; an ordinary
 *  non-docker worker (neither set) never blocks. Exported so main.ts warns on the same
 *  signal it degraded on. */
export function dockerSidecarExpected(env: NodeJS.ProcessEnv = process.env): boolean {
  return !!(env.DOCKER_HOST?.trim() || env.UZI_DIND_SOCKET?.trim());
}

/**
 * Resolve this worker's docker wiring ONCE at startup (arch doc §Keystone; M2 follow-up
 * adds the bounded readiness wait):
 *   1. DOCKER_HOST set + non-empty → that endpoint verbatim (the k8s path); EXPECTED.
 *   2. else UZI_DIND_SOCKET set → unix://<path> regardless of whether the file exists yet
 *      (compose opt-in); EXPECTED — the wait spans the window before the socket appears.
 *   3. else the DEFAULT socket if it already exists → unix://<default>; NOT expected.
 *   4. else no candidate → {}.
 * The candidate is PROBED for liveness. When EXPECTED, the probe is retried every
 * `readyIntervalMs` up to `readyTimeoutMs` before concluding unwired, so a slow-to-start
 * daemon does not cost the capability. When NOT expected, exactly ONE fast probe runs and
 * the worker NEVER blocks. `dockerHost` is returned only when a probe succeeds, so
 * "reachable" (not merely "configured") is what every consumer keys on. Timeout ⇒ {}
 * (degrade-to-unwired); main.ts emits the loud warn.
 */
export async function resolveDockerWiring(
  env: NodeJS.ProcessEnv = process.env,
  opts: ResolveDockerWiringOptions = {},
): Promise<DockerWiring> {
  const resolved = resolveCandidate(env, opts);
  if (!resolved) return {};
  const probe = opts.probe ?? defaultProbe;
  const probeTimeoutMs = opts.probeTimeoutMs ?? DEFAULT_PROBE_TIMEOUT_MS;
  const sleep = opts.sleep ?? realSleep;
  const now = opts.now ?? Date.now;
  const readyIntervalMs = opts.readyIntervalMs ?? DEFAULT_READY_INTERVAL_MS;
  // Only an EXPECTED sidecar gets a wait budget; otherwise the deadline is now (one probe).
  const readyTimeoutMs = resolved.expected ? (opts.readyTimeoutMs ?? DEFAULT_READY_TIMEOUT_MS) : 0;
  const deadline = now() + readyTimeoutMs;
  for (;;) {
    let reachable = false;
    try {
      reachable = await probe(resolved.host, probeTimeoutMs);
    } catch {
      // A probe that throws is treated as unreachable — never let it fail worker startup,
      // and never report a capability we could not confirm.
      reachable = false;
    }
    if (reachable) return { dockerHost: resolved.host };
    const remaining = deadline - now();
    if (remaining <= 0) return {}; // not expected ⇒ deadline≈now ⇒ exactly one probe, no wait
    await sleep(Math.min(readyIntervalMs, remaining));
  }
}

/** The resolved candidate endpoint + whether a sidecar was EXPECTED (drives the wait). */
interface ResolvedCandidate {
  host: string;
  expected: boolean;
}

/** Resolve the candidate endpoint (before the liveness probe), or undefined. */
function resolveCandidate(env: NodeJS.ProcessEnv, opts: ResolveDockerWiringOptions): ResolvedCandidate | undefined {
  const explicit = env.DOCKER_HOST?.trim();
  if (explicit) return { host: explicit, expected: true }; // k8s: the controller sets it.
  const configured = env.UZI_DIND_SOCKET?.trim();
  // An explicitly-configured socket path forms the candidate REGARDLESS of current
  // existence, so the readiness wait can bridge the gap until the daemon writes the socket.
  if (configured) return { host: `unix://${configured}`, expected: true };
  // Default path, NOT explicitly configured: only a candidate if the socket is ALREADY
  // present, and never expected — a non-docker worker must not block on a path that may
  // never appear (the socket-share volume always exists; the SOCKET is what matters).
  const exists = opts.socketExists ?? defaultSocketExists;
  if (exists(DEFAULT_DIND_SOCKET)) return { host: `unix://${DEFAULT_DIND_SOCKET}`, expected: false };
  return undefined;
}

/** True when `socketPath` exists and is a socket. Any fs error ⇒ false (absent). */
function defaultSocketExists(socketPath: string): boolean {
  try {
    return fs.statSync(socketPath).isSocket();
  } catch {
    return false;
  }
}

type ConnectTarget = { path: string } | { host: string; port: number };

/** Parse a DOCKER_HOST into a raw-connect target. unix://<path> and a bare /abs path
 *  connect to the unix socket; tcp://host:port connects TCP. Anything else ⇒ undefined
 *  (unprobeable ⇒ unreachable). */
function parseDockerTarget(dockerHost: string): ConnectTarget | undefined {
  if (dockerHost.startsWith("unix://")) return { path: dockerHost.slice("unix://".length) };
  if (dockerHost.startsWith("/")) return { path: dockerHost };
  if (dockerHost.startsWith("tcp://")) {
    const rest = dockerHost.slice("tcp://".length);
    const idx = rest.lastIndexOf(":");
    if (idx < 0) return undefined;
    const host = rest.slice(0, idx);
    const port = Number(rest.slice(idx + 1));
    if (!host || !Number.isInteger(port) || port <= 0) return undefined;
    return { host, port };
  }
  return undefined;
}

/**
 * Default reachability probe: a bounded raw socket connect. A successful connect on a
 * unix socket proves a daemon is actively listening (a socket file with no listener
 * yields ECONNREFUSED), which is a good "a daemon is reachable" signal without needing
 * the docker CLI on PATH at probe time. Never throws — resolves false on any failure.
 */
function defaultProbe(dockerHost: string, timeoutMs: number): Promise<boolean> {
  const target = parseDockerTarget(dockerHost);
  if (!target) return Promise.resolve(false);
  return new Promise((resolve) => {
    let settled = false;
    const finish = (ok: boolean): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      sock.destroy();
      resolve(ok);
    };
    const sock = net.connect(target as net.NetConnectOpts);
    const timer = setTimeout(() => finish(false), timeoutMs);
    timer.unref?.();
    sock.once("connect", () => finish(true));
    sock.once("error", () => finish(false));
  });
}
