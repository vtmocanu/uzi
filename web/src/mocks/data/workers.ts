import type {
  AdminWorker,
  Worker,
} from "../../lib/api";
import { daysAgo, minsAgo } from "./time";
import { mockAdmin } from "./users";

// ── Workers ──────────────────────────────────────────────────────────────────

export const mockWorkers: Worker[] = [
  {
    id: "w-laptop",
    name: "laptop",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: true,
    active_runs: 1,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.2),
    online_since: minsAgo(192), // online ~3h 12m
    created_at: daysAgo(14),
    // cgroup sample with a limit → CPU bar + "used / limit · %" memory bar (ok tone).
    stats_cpu_pct: 34.2,
    stats_mem_bytes: 2254857830, // 2.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~52%
    stats_source: "cgroup",
    // Both volumes reported at a normal level → two "Disk /nix" + "Disk /data" bars.
    stats_disk_nix_bytes: 7623566950, // 7.1 GiB
    stats_disk_nix_total_bytes: 21474836480, // 20 GiB → ~35%
    stats_disk_data_bytes: 3221225472, // 3 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 30%
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
  },
  {
    // Declared jvm at issuance but the running image is base → drift badge demo.
    id: "w-ci",
    name: "ci-runner-1",
    status: "offline",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "jvm",
    template_reported: "base",
    version: "0.4.1",
    upgrade_status: "outdated",
    upgrade_detail: "running 0.4.1, target 0.4.2",
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: daysAgo(2),
    online_since: null, // offline → no uptime anchor
    created_at: daysAgo(21),
    // Offline → its last-known cgroup sample renders dimmed, never live-looking.
    stats_cpu_pct: 12,
    stats_mem_bytes: 1610612736, // 1.5 GiB
    stats_mem_limit_bytes: 2147483648, // 2 GiB → 75%
    stats_source: "cgroup",
    // /nix nearly full (danger tone, ≥85%), /data mid — both dimmed (offline).
    stats_disk_nix_bytes: 19327352832, // 18 GiB
    stats_disk_nix_total_bytes: 21474836480, // 20 GiB → 90% (danger)
    stats_disk_data_bytes: 4294967296, // 4 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 40% (warn)
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
  },
  {
    // PRD #113 M5: the FAILED upgrade. Present so the demo can show the failed-worker
    // strip, the likely-cause copy and the copy-kubectl-command button — a state the
    // product ships and the demo could not previously reach, which meant a browser pass
    // could only ever validate the healthy path.
    //
    // The shape is the v0.11.0 incident: an init container wedged reseeding the nix
    // store. Fictional ids and no registry path, deliberately.
    id: "w-stuck",
    name: "stuck-roller",
    status: "offline",
    kind: "hosted",
    hosted_size: "m",
    docker: false,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    // Still reporting the OLD version: a worker whose new pod never became Ready is
    // offline, so its stored version cannot move. That is the whole reason roll health
    // has to come from the controller rather than from the worker.
    version: "0.4.1",
    upgrade_status: "upgrade_failed",
    upgrade_detail: "seed-nix: CrashLoopBackOff (6 restarts, last exit 2)",
    // Target BELOW the control plane's 0.4.2, so this one worker also renders the Fleet
    // panel's B-1 divergence line. Coherent rather than contrived: the controller is
    // rolling this worker to the PINNED tag 0.4.1 and the pod is wedged getting there.
    // One worker carrying both states is what keeps PRD #58's quota headroom intact —
    // see the note in mockApi/workers.ts.
    upgrade_target: "0.4.1",
    upgrade_blocking_container: "seed-nix",
    upgrade_blocking_reason: "CrashLoopBackOff",
    // The incident's exit code, so the strip's cause line can discriminate a permissions
    // failure from the volume filling up rather than naming both.
    upgrade_last_exit_code: 2,
    last_heartbeat_at: minsAgo(14),
    online_since: null, // offline → no uptime anchor
    created_at: daysAgo(11),
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    // No sample at all → no disk either (all four null → no disk section renders).
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
  },
  {
    // Un-quota'd / cgroup-v1 host → process fallback: no known limit (absolute mem,
    // no percentage bar) and the "worker process only" label.
    id: "w-nas",
    name: "nas-runner",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.4),
    online_since: daysAgo(1), // online ~1d
    created_at: daysAgo(6),
    stats_cpu_pct: 8.3,
    stats_mem_bytes: 503316480, // 480 MiB
    stats_mem_limit_bytes: null, // unlimited/unknown → no bar
    stats_source: "process",
    // Process-source host reports only its writable /data volume (single-volume layout:
    // one "Disk /data" bar, no "Disk /nix"). /nix left null → not rendered.
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: 6442450944, // 6 GiB
    stats_disk_data_total_bytes: 17179869184, // 16 GiB → ~37%
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
  },
  {
    // A hosted worker (PRD #58): the controller runs this one in the cluster. Seeded
    // ONLINE with a live sample so the hosted + docker badges are seen on a realistic
    // row rather than on a permanently-pending one — and so the demo starts at 1 of
    // its quota of 2, one provision away from the at-quota state. docker:true (PRD #83
    // M3) exercises the docker badge; the other seeded hosted rows leave it undefined.
    id: "w-hosted-eu",
    name: "base.m-1a2b", // derived by the server from template + size (AWS-style base.m-<hex>); the form sends no name
    status: "online",
    kind: "hosted",
    hosted_size: "m",
    docker: true,
    // PRD #84 M1/M4: the server-authoritative capability set. This docker-capable hosted
    // worker advertises "docker", so the Workers page shows the capability chip AND — were a
    // docker-requiring run assigned here — the plan-gate readiness summary would read it MET.
    capabilities: ["docker"],
    busy: false,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.3),
    online_since: minsAgo(27), // online ~27m
    created_at: daysAgo(3),
    stats_cpu_pct: 21.5,
    stats_mem_bytes: 1181116006, // 1.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~27%
    stats_source: "cgroup",
    // Both volumes reported at a comfortable level.
    stats_disk_nix_bytes: 12884901888, // 12 GiB
    stats_disk_nix_total_bytes: 21474836480, // 20 GiB → 60%
    stats_disk_data_bytes: 2147483648, // 2 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 20%
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
  },
  {
    // The #365 scenario (PRD #496): online, one free slot (1/2), cordoned for a roll
    // (so also `outdated`), so it claims nothing new. It shows the `draining` label
    // beside the amber outdated badge — demonstrating the pill is visually distinct
    // (dashed neutral, not solid amber) and keyed on draining_since, NOT upgrade_status.
    id: "w-cordon-eu",
    name: "base.m-9f3c",
    status: "online",
    kind: "hosted",
    hosted_size: "m",
    docker: true,
    capabilities: ["docker"],
    busy: true,
    active_runs: 1,
    max_concurrent_runs: 2,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.1",
    upgrade_status: "outdated",
    upgrade_detail: "running 0.4.1, target 0.4.2",
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.3),
    online_since: minsAgo(48),
    created_at: daysAgo(4),
    stats_cpu_pct: 21.5,
    stats_mem_bytes: 1181116006, // 1.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~27%
    stats_source: "cgroup",
    // Both volumes reported, /nix warm.
    stats_disk_nix_bytes: 9663676416, // 9 GiB
    stats_disk_nix_total_bytes: 21474836480, // 20 GiB → 45%
    stats_disk_data_bytes: 5368709120, // 5 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 50%
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: minsAgo(6),
  },
  {
    // An idle cordoned worker (no runs in flight) → shows the `cordoned` label.
    id: "w-cordon-idle",
    name: "base.s-4d7a",
    status: "online",
    kind: "hosted",
    hosted_size: "s",
    docker: false,
    busy: false,
    active_runs: 0,
    max_concurrent_runs: 2,
    template_declared: "base",
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.5),
    online_since: minsAgo(90),
    created_at: daysAgo(5),
    stats_cpu_pct: 21.5,
    stats_mem_bytes: 1181116006, // 1.1 GiB
    stats_mem_limit_bytes: 4294967296, // 4 GiB → ~27%
    stats_source: "cgroup",
    // Both volumes reported at a low level.
    stats_disk_nix_bytes: 5368709120, // 5 GiB
    stats_disk_nix_total_bytes: 17179869184, // 16 GiB → ~31%
    stats_disk_data_bytes: 3221225472, // 3 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 30%
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: minsAgo(20),
  },
];

export const mockAdminWorkers: AdminWorker[] = [
  { ...mockWorkers[0], owner_email: mockAdmin.email },
  { ...mockWorkers[1], owner_email: mockAdmin.email },
  { ...mockWorkers[2], owner_email: mockAdmin.email },
  { ...mockWorkers[3], owner_email: mockAdmin.email },
  {
    // A cap-2 worker running both slots → "2/2 runs" badge demo (PRD #42), and a
    // near-limit cgroup sample → danger-tone CPU + memory bars (≥95%).
    id: "w-mira",
    name: "mira-desktop",
    status: "online",
    kind: "external",
    hosted_size: null,
    busy: true,
    active_runs: 2,
    max_concurrent_runs: 2,
    template_declared: "jvm",
    template_reported: "jvm",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: minsAgo(0.5),
    online_since: daysAgo(2), // online ~2d
    created_at: daysAgo(9),
    stats_cpu_pct: 96.4,
    stats_mem_bytes: 8160437862, // 7.6 GiB
    stats_mem_limit_bytes: 8589934592, // 8 GiB → 95%
    stats_source: "cgroup",
    // Near-full /nix (danger tone, ≥85%) alongside a busy /data → matches the tight box.
    stats_disk_nix_bytes: 19864223744, // 18.5 GiB
    stats_disk_nix_total_bytes: 21474836480, // 20 GiB → ~93% (danger)
    stats_disk_data_bytes: 8589934592, // 8 GiB
    stats_disk_data_total_bytes: 10737418240, // 10 GiB → 80% (warn)
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
    owner_email: "mira@uzi.local",
  },
];
