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
export const DEFAULT_PROBE_TIMEOUT_MS = 2_000;

export interface ResolveDockerWiringOptions {
  /** Reachability probe; injected in tests so the resolver is provable with no daemon.
   *  Resolves true iff a daemon answers on `dockerHost` within the timeout. Default =
   *  a bounded raw socket connect (unix:// or tcp://). */
  probe?: (dockerHost: string, timeoutMs: number) => Promise<boolean>;
  /** Existence check for the sidecar socket path (default = a stat that also verifies
   *  the entry is a socket). Injected in tests. */
  socketExists?: (socketPath: string) => boolean;
  /** Probe timeout override (ms). */
  probeTimeoutMs?: number;
}

/**
 * Resolve this worker's docker wiring ONCE at startup (arch doc §Keystone):
 *   1. DOCKER_HOST set + non-empty → that endpoint verbatim (the k8s path).
 *   2. else the sidecar socket (UZI_DIND_SOCKET, default /run/dind/docker.sock) if it
 *      exists → unix://<path> (the compose auto-detect path).
 *   3. else no candidate → {}.
 * A candidate is then PROBED for liveness; `dockerHost` is returned only when the probe
 * succeeds, so "reachable" (not merely "configured") is what every consumer keys on.
 */
export async function resolveDockerWiring(
  env: NodeJS.ProcessEnv = process.env,
  opts: ResolveDockerWiringOptions = {},
): Promise<DockerWiring> {
  const candidate = resolveCandidate(env, opts);
  if (!candidate) return {};
  const probe = opts.probe ?? defaultProbe;
  const timeoutMs = opts.probeTimeoutMs ?? DEFAULT_PROBE_TIMEOUT_MS;
  let reachable = false;
  try {
    reachable = await probe(candidate, timeoutMs);
  } catch {
    // A probe that throws is treated as unreachable — never let it fail worker
    // startup, and never report a capability we could not confirm.
    reachable = false;
  }
  return reachable ? { dockerHost: candidate } : {};
}

/** Resolve the candidate endpoint (before the liveness probe), or undefined. */
function resolveCandidate(env: NodeJS.ProcessEnv, opts: ResolveDockerWiringOptions): string | undefined {
  const explicit = env.DOCKER_HOST?.trim();
  if (explicit) return explicit; // k8s: the controller sets it explicitly.
  const socketPath = env.UZI_DIND_SOCKET?.trim() || DEFAULT_DIND_SOCKET;
  const exists = opts.socketExists ?? defaultSocketExists;
  if (exists(socketPath)) return `unix://${socketPath}`; // compose: auto-detected.
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
