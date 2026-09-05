import type { BindMode } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { mockAdminWorkers, mockWorkers } from "../data";
import { delay } from "./shared";
// secrets ↔ workers is the one accepted import cycle (PRD #991 D4): setWorkerBindMode
// resolves a token label against the secrets roster, and secrets' deleteAnthropicTokenById
// unbinds workers pinned to a deleted token. Both reads are inside function bodies and no
// module-level initializer crosses the modules, so the cycle cannot throw at import.
import { secrets } from "./secrets";

export let workers = mockWorkers.map((w) => ({ ...w }));
let workerCounter = 0;

export const workersApi = {
  // PRD #113 M6. Computed LIVE from the demo worker list rather than hardcoded, so the
  // badge actually clears when a worker is deleted — web-ux needs to see it appear AND
  // clear, and a constant would only ever show the first half.
  workerUpgradeSummary: async () => {
    const attention = workers.filter(
      (w) => w.upgrade_status === "upgrade_failed" || w.upgrade_status === "outdated",
    ).length;
    return delay({ attention, target_release: "0.4.2" });
  },

  // ── Workers ─────────────────────────────────────────────────────────────────
  listWorkers: async () => delay({ workers: workers.map((w) => ({ ...w })) }),
  createWorker: async (name: string, template?: string) => {
    const w = {
      id: `w-new-${++workerCounter}`,
      name,
      status: "offline",
      busy: false,
      // Hand-run: the user starts this container themselves, so it has no size
      // (PRD #58). provisionHostedWorker below is the other kind.
      kind: "external" as const,
      hosted_size: null,
      // No runs and no advertised cap until the worker registers (PRD #42).
      active_runs: 0,
      max_concurrent_runs: null,
      // Declared at issuance; reported stays null until the worker registers.
      template_declared: template ?? null,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      // No resource sample until the worker heartbeats (PRD #49) → no gauges yet.
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      // No disk sample yet either (PRD #837) → no disk bars until the first heartbeat.
      stats_disk_nix_bytes: null,
      stats_disk_nix_total_bytes: null,
      stats_disk_data_bytes: null,
      stats_disk_data_total_bytes: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    const token = `uzi_wk_${Array.from(crypto.getRandomValues(new Uint8Array(18)), (b) => b.toString(16).padStart(2, "0")).join("")}`;
    return delay({ worker: { ...w }, token });
  },
  deleteWorker: async (id: string) => {
    workers = workers.filter((w) => w.id !== id);
    return delay(null);
  },
  // PRD #104 M3: rebind a worker to a named token, or clear it with null. Mirrors
  // the real route's label→id resolution and its 400 for an unknown label, so the
  // picker's error path is browsable.
  setWorkerBindMode: async (id: string, mode: BindMode, label: string | null) => {
    const w = workers.find((x) => x.id === id);
    if (!w) throw new ApiError(404, "worker not found");
    // Mirrors the server's refusal of a contradictory pair, so the picker's error
    // path is browsable in the mock rather than only in production.
    if (mode !== "pinned" && label !== null && label.trim() !== "") {
      throw new ApiError(400, "anthropic_token must be null when anthropic_bind_mode is default or auto");
    }
    if (mode !== "pinned") {
      w.anthropic_bind_mode = mode;
      w.anthropic_secret_id = null;
      w.anthropic_secret_label = null;
      return delay({ worker: { ...w } });
    }
    if (label === null || label.trim() === "") {
      throw new ApiError(400, "anthropic_bind_mode=pinned requires a token label in anthropic_token");
    }
    const secret = secrets.find(
      (x) => x.kind === "anthropic_token" && x.label.toLowerCase() === label.trim().toLowerCase(),
    );
    if (!secret) throw new ApiError(400, "no Anthropic token with that label");
    w.anthropic_bind_mode = "pinned";
    w.anthropic_secret_id = secret.id;
    w.anthropic_secret_label = secret.label;
    return delay({ worker: { ...w } });
  },

  // Hosted workers (PRD #58). The demo is the only place M5 can be seen working: on a
  // real stack WORKER_HOSTING_ENABLED is off by default, and turning it on gets you a
  // worker that sits offline forever, because the controller that would run its pod is
  // M3's. So hosting is hardcoded ON here — a demo of a feature that renders nothing
  // is not a demo — and quota 2 against one seeded hosted worker puts the whole
  // journey three clicks away: provision → 2 of 2 → the button disables → delete →
  // it enables again.
  // Quota 5 against FOUR seeded hosted workers (PRD #496 added two cordoned demo
  // workers, w-cordon-eu and w-cordon-idle, on top of the two PRD #113 M5 seeded). The
  // load-bearing property is unchanged and is why the numbers moved together: there is
  // exactly ONE slot of headroom, so web-ux can still drive provision -> at quota ->
  // button disables -> delete -> it enables again, which is the only way to prove the
  // client-side gate RELEASES rather than merely starting disabled.
  //
  // The second seeded worker is the failed roller, which the demo previously could not
  // show at all — so a browser pass could only ever validate the healthy path.
  hostedConfig: async () => delay({ enabled: true, quota: 5, ephemeral_enabled: true }),
  provisionHostedWorker: async (template: string, size: string, docker = false, name?: string) => {
    const w = {
      id: `w-hosted-${++workerCounter}`,
      // Empty name → the server derives one from template + size (handler's
      // derivedHostedWorkerName), now AWS-style `base.l-<4-hex>`: dot notation,
      // lowercase t-shirt letter, random hex suffix. The M5 form sends none, so this
      // is the live path. The real suffix is crypto/rand; the mock stands in a
      // counter-derived 4-hex from workerCounter (already ++'d for the id) to stay
      // deterministic-enough for tests — a plausible suffix, not byte-for-byte parity.
      name:
        name?.trim() ||
        `${template}.${size.toLowerCase()}-${(workerCounter & 0xffff).toString(16).padStart(4, "0")}`,
      // Offline until the controller starts the pod and it registers — the same
      // lifecycle a hand-run worker has, just with the controller doing the running.
      status: "offline",
      busy: false,
      active_runs: 0,
      max_concurrent_runs: null,
      kind: "hosted" as const,
      hosted_size: size,
      docker,
      template_declared: template,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      // No disk sample yet either (PRD #837) → no disk bars until the first heartbeat.
      stats_disk_nix_bytes: null,
      stats_disk_nix_total_bytes: null,
      stats_disk_data_bytes: null,
      stats_disk_data_total_bytes: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    // { worker } and NOTHING ELSE. Do not mint a token here the way createWorker does
    // above: the real endpoint's transaction returns none, its response cannot carry
    // one, and a mock that invents one documents a contract the server structurally
    // cannot honor. TypeScript will not catch it (delay() infers its own T, so an
    // extra field type-checks clean) — mockApi.hosted.test.ts is what does.
    return delay({ worker: { ...w } });
  },

  adminListWorkers: async () => delay({ workers: mockAdminWorkers.map((w) => ({ ...w })) }),
};
