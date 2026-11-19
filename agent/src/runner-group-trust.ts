// Once-per-process, fail-closed probe: is the worker's primary group (gid 10001)
// provably excluded from the untrusted runner identities (runner uid 10002, runner-cmd
// uid 10003)? A later consumer uses a "trusted" result to admit group-writable managed
// bare repos. Anything short of positive proof yields "off"; the probe never throws.
//
// The probe reads `/proc/self/status` of a `/bin/cat` spawned through the same wrappers the
// production runner surfaces use (runnerCommand / commandRootCommand), so what it measures is
// what an untrusted child really gets. The evaluator mirrors evaluateProfile in
// agent/codex/supervisor/profile.go and adds the group-exclusion checks.

import { spawn, type ChildProcess, type SpawnOptions } from "node:child_process";
import {
  CODEX_SESSION_GID,
  COMMAND_UID,
  RUNNER_UID,
  WORKER_UID,
  commandRootCommand,
  killCommandRootPid,
  killRunnerGroup,
  runnerCommand,
  uidSplitActive,
} from "./runner-uid.js";

declare const brand: unique symbol;

/** Branded, frozen result. Only this module builds values (see {@link probeRunnerGroupTrust}). */
export type ManagedBareGroupTrust = Readonly<
  { state: "trusted"; gid: number } | { state: "off"; reason: string }
> & { readonly [brand]: true };

const PROBE_TIMEOUT_MS = 10_000;
const JOIN_GRACE_MS = 5_000;
const STDOUT_CAP_BYTES = 64 * 1024;
/** The inert SETUID|SETGID bounding-set residue of the real setpriv path (profile.go). */
const CAP_BND_RESIDUE = 0xc0n;

type ChildResult =
  | { ok: true; code: number | null; signal: string | null; stdout: string }
  | { ok: false; reason: string };

type Identity = { label: string; uid: number; gid: number };
const RUNNER_ID: Identity = { label: "runner", uid: RUNNER_UID, gid: RUNNER_UID };
const COMMAND_ID: Identity = { label: "runner_cmd", uid: COMMAND_UID, gid: COMMAND_UID };

/** Every value this module issued. The brand is type-only, so a consumer that grants an allowance checks
 *  membership here ({@link isIssuedManagedBareGroupTrust}): a cast object literal is never trusted. */
const issued = new WeakSet<object>();

function build(value: { state: "trusted"; gid: number } | { state: "off"; reason: string }): ManagedBareGroupTrust {
  const out = Object.freeze({ ...value });
  issued.add(out);
  return out as unknown as ManagedBareGroupTrust;
}

/** True only for a value built by this module (the probe or the test factory). */
export function isIssuedManagedBareGroupTrust(value: unknown): value is ManagedBareGroupTrust {
  return typeof value === "object" && value !== null && issued.has(value);
}

function off(reason: string): ManagedBareGroupTrust {
  return build({ state: "off", reason });
}

/** Parse one numeric-list field; every token must be a plain decimal. */
function parseIdList(raw: string): number[] | undefined {
  const tokens = raw.trim().split(/\s+/).filter((t) => t !== "");
  const out: number[] = [];
  for (const t of tokens) {
    if (!/^\d+$/.test(t)) return undefined;
    out.push(Number(t));
  }
  return out;
}

function parseCap(raw: string): bigint | undefined {
  const t = raw.trim();
  if (!/^[0-9a-fA-F]{1,16}$/.test(t)) return undefined;
  return BigInt(`0x${t}`);
}

const STATUS_KEYS = ["Uid", "Gid", "Groups", "CapEff", "CapPrm", "CapInh", "CapAmb", "CapBnd", "NoNewPrivs"] as const;

/** Returns undefined (ok) or the first failed field. */
function checkStatus(text: string, id: Identity, trustedGid: number): string | undefined {
  const seen = new Map<string, string>();
  for (const line of text.split("\n")) {
    const m = /^([A-Za-z]+):[ \t]*(.*)$/.exec(line);
    if (!m) continue;
    const key = m[1] ?? "";
    if (!(STATUS_KEYS as readonly string[]).includes(key)) continue;
    if (seen.has(key)) return `${key}_duplicate`;
    seen.set(key, m[2] ?? "");
  }
  for (const key of STATUS_KEYS) {
    if (!seen.has(key)) return `${key}_missing`;
  }
  const uids = parseIdList(seen.get("Uid")!);
  if (!uids || uids.length !== 4 || uids.some((u) => u !== id.uid)) return "uid";
  const gids = parseIdList(seen.get("Gid")!);
  if (!gids || gids.length !== 4 || gids.some((g) => g !== id.gid)) return "gid";
  const groups = parseIdList(seen.get("Groups")!);
  if (!groups) return "groups_unparsable";
  if (gids.includes(trustedGid) || groups.includes(trustedGid)) return "worker_gid_present";
  for (const key of ["CapEff", "CapPrm", "CapInh", "CapAmb"] as const) {
    const v = parseCap(seen.get(key)!);
    if (v === undefined) return `${key}_unparsable`;
    if (v !== 0n) return key;
  }
  const bnd = parseCap(seen.get("CapBnd")!);
  if (bnd === undefined) return "CapBnd_unparsable";
  if (bnd !== 0n && bnd !== CAP_BND_RESIDUE) return "CapBnd";
  if (seen.get("NoNewPrivs")!.trim() !== "1") return "NoNewPrivs";
  return undefined;
}

type EvalInput = {
  split: boolean;
  uid: number;
  gid: number;
  runner: ChildResult;
  command: ChildResult;
  /** Test factory only: skip the production worker uid/gid identity checks (the status-text checks still apply). */
  anyIdentity?: boolean;
};

/** The pure decision shared by the probe and the test factory. */
function evaluate(input: EvalInput): ManagedBareGroupTrust {
  if (!input.split) return off("single_uid");
  if (!input.anyIdentity && input.uid !== WORKER_UID) return off("worker_uid");
  const trustedGid: number = input.gid;
  const sharedGids: readonly number[] = [RUNNER_UID, COMMAND_UID, CODEX_SESSION_GID];
  if (!input.anyIdentity && (trustedGid !== WORKER_UID || sharedGids.includes(trustedGid))) {
    return off("worker_gid");
  }
  for (const [id, res] of [[RUNNER_ID, input.runner], [COMMAND_ID, input.command]] as const) {
    if (!res.ok) return off(`${id.label}_${res.reason}`);
    if (res.code !== 0 || res.signal !== null) return off(`${id.label}_exit`);
    const bad = checkStatus(res.stdout, id, input.anyIdentity ? WORKER_UID : trustedGid);
    if (bad !== undefined) return off(`${id.label}_${bad}`);
  }
  return build({ state: "trusted", gid: trustedGid });
}

/**
 * Test factory: builds a value from injected status text by running the same evaluator the
 * probe uses. Defaults model a healthy split worker.
 */
export function managedBareGroupTrustForTest(o: {
  runnerStatus: string;
  commandStatus: string;
  split?: boolean;
  uid?: number;
  gid?: number;
  /** A test process is not the production worker (uid/gid 10001): the value carries `gid` (its own group) while the
   *  status-text group-exclusion checks still run against the production worker group (10001). */
  anyIdentity?: boolean;
}): ManagedBareGroupTrust {
  const ok = (stdout: string): ChildResult => ({ ok: true, code: 0, signal: null, stdout });
  return evaluate({
    split: o.split ?? true,
    uid: o.uid ?? WORKER_UID,
    gid: o.gid ?? WORKER_UID,
    runner: ok(o.runnerStatus),
    command: ok(o.commandStatus),
    anyIdentity: o.anyIdentity,
  });
}

export type RunnerGroupTrustDeps = {
  spawn?: (command: string, args: string[], options: SpawnOptions) => ChildProcess;
  split?: boolean;
  getuid?: () => number;
  getgid?: () => number;
  timeoutMs?: number;
  /** Bound on waiting for a killed child to close before resolving off anyway. */
  joinGraceMs?: number;
  /** Uid-aware kill of a timed-out child; the worker cannot signal a runner uid directly. */
  kill?: (which: "runner" | "command", pid: number | undefined) => void;
};

function defaultKill(which: "runner" | "command", pid: number | undefined): void {
  if (which === "runner") killRunnerGroup(pid);
  else killCommandRootPid(pid);
}

function runOne(
  wrapped: { command: string; args: string[] },
  which: "runner" | "command",
  deps: RunnerGroupTrustDeps,
  timeoutMs: number,
): Promise<ChildResult> {
  return new Promise<ChildResult>((resolve) => {
    let child: ChildProcess | undefined;
    let timer: NodeJS.Timeout | undefined;
    let settled = false;
    const chunks: Buffer[] = [];
    let bytes = 0;
    const killChild = (): void => {
      try { (deps.kill ?? defaultKill)(which, child?.pid); } catch { /* best effort */ }
      try { child?.kill("SIGKILL"); } catch { /* best effort */ }
    };
    let closed = false;
    let onClosed: (() => void) | undefined;
    // `join`: after the kill request, wait (bounded) for the child to close so a probe child is
    // not left running when the result is returned. The result is the same either way.
    const settle = (r: ChildResult, kill: boolean, join = false): void => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (!kill) { resolve(r); return; }
      killChild();
      if (!join || closed) { resolve(r); return; }
      const grace = setTimeout(() => {
        onClosed = undefined;
        resolve(r.ok ? r : { ok: false, reason: `${r.reason}_unjoined` });
      }, deps.joinGraceMs ?? JOIN_GRACE_MS);
      onClosed = () => { clearTimeout(grace); onClosed = undefined; resolve(r); };
    };
    try {
      const doSpawn = deps.spawn ?? spawn;
      child = doSpawn(wrapped.command, wrapped.args, {
        env: { PATH: "/usr/bin:/bin" },
        stdio: ["ignore", "pipe", "pipe"],
      });
      timer = setTimeout(() => settle({ ok: false, reason: "timeout" }, true, true), timeoutMs);
      child.on("error", () => settle({ ok: false, reason: "spawn_error" }, true));
      child.stderr?.on("data", () => { /* drained and discarded */ });
      child.stderr?.on("error", () => { /* ignored */ });
      child.stdout?.on("error", () => settle({ ok: false, reason: "stdout_error" }, true, true));
      child.stdout?.on("data", (d: Buffer | string) => {
        const b = typeof d === "string" ? Buffer.from(d) : d;
        bytes += b.length;
        if (bytes > STDOUT_CAP_BYTES) { settle({ ok: false, reason: "oversize" }, true, true); return; }
        chunks.push(b);
      });
      child.on("close", (code: number | null, signal: NodeJS.Signals | null) => {
        closed = true;
        onClosed?.();
        settle({ ok: true, code, signal, stdout: Buffer.concat(chunks).toString("utf8") }, false);
      });
    } catch {
      settle({ ok: false, reason: "spawn_throw" }, true);
    }
  });
}

/** Probe once per process. Never throws or rejects; any doubt yields `{state:"off"}`. */
export async function probeRunnerGroupTrust(deps: RunnerGroupTrustDeps = {}): Promise<ManagedBareGroupTrust> {
  try {
    const split = deps.split ?? uidSplitActive();
    if (!split) return off("single_uid");
    const uid = (deps.getuid ?? (() => process.getuid?.() ?? -1))();
    const gid = (deps.getgid ?? (() => process.getgid?.() ?? -1))();
    // Cheap identity checks first so a wrong worker never spawns anything.
    const pre = evaluate({ split, uid, gid, runner: { ok: false, reason: "pending" }, command: { ok: false, reason: "pending" } });
    if (pre.state === "off" && !pre.reason.endsWith("_pending")) return pre;
    const timeoutMs = deps.timeoutMs ?? PROBE_TIMEOUT_MS;
    const args = ["/proc/self/status"];
    const [runner, command] = await Promise.all([
      runOne(runnerCommand("/bin/cat", args, split), "runner", deps, timeoutMs),
      runOne(commandRootCommand("/bin/cat", args), "command", deps, timeoutMs),
    ]);
    return evaluate({ split, uid, gid, runner, command });
  } catch {
    return off("probe_error");
  }
}
