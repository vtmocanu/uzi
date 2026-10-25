import type { AdminMaintenance, MaintenanceReadout, MaintenanceRunItem, MaintenanceState } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { minsAgo } from "../data/time";
import { delay, mockScenario, requireAdmin, requireSession } from "./shared";

// Instance maintenance mode (PRD #3454). GET /api/maintenance is all-user; GET/PUT
// /api/admin/maintenance is admin. The scenario seeds the starting state:
//
//   ?mock=maintenance → on, with a reason; a not-yet-clear readout (running, held with
//                       needs-owner and progressing tags, a custody hold, queued runs and an
//                       idle bound worker); the queued fixtures carry queue_reason (store.ts)
//   (default)         → off, clear readout
//
// The toggle and reason are then live for the session (module state; a reload resets them).
// See docs/dev-conventions.md's demo-scenarios section for the registered names.

const REASON_MAX = 280;

type MockMaintenance = MaintenanceState & { by_label: string | null };

let current: MockMaintenance | null = null;

function scenarioActive(): boolean {
  return mockScenario() === "maintenance";
}

function maintenanceState(): MockMaintenance {
  if (current === null) {
    current = scenarioActive()
      ? { active: true, since: minsAgo(25), reason: "Node pool upgrade, back by 15:00", by_label: "Robin Diaz" }
      : { active: false, since: null, reason: null, by_label: null };
  }
  return current;
}

function item(over: Partial<MaintenanceRunItem> & Pick<MaintenanceRunItem, "run_id" | "status">): MaintenanceRunItem {
  return { kind: "issue", owner_label: "Robin Diaz", repo: "acme/uzi", since: minsAgo(12), tag: null, hold_id: null, ...over };
}

const EMPTY = { count: 0, items: [] };

// The readout. In the maintenance scenario it stays not-clear (the demo shows the blockers
// an admin must chase); turning maintenance off and on again keeps the same snapshot, since
// the mock does not model the drain itself. Out of the scenario it is clear.
function readout(): MaintenanceReadout {
  const as_of = new Date().toISOString();
  if (!scenarioActive()) {
    return { as_of, clear: true, active: EMPTY, held: EMPTY, custody: EMPTY, queued: EMPTY, idle_bound_workers: EMPTY };
  }
  const held = [
    item({ run_id: "run-awaiting", status: "awaiting_approval", tag: "needs_owner", since: minsAgo(140) }),
    item({ run_id: "run-limit-wait", status: "limit_wait", tag: "progressing", since: minsAgo(48), owner_label: "Sam Chen" }),
    item({ run_id: "run-pool-wait", status: "pool_wait", tag: "progressing", since: minsAgo(9), owner_label: "Priya Nair", repo: "acme/web" }),
  ];
  const queued = [
    item({ run_id: "run-queued", status: "queued", since: minsAgo(6) }),
    item({ run_id: "run-expedited", status: "queued", since: minsAgo(3) }),
    item({ run_id: "run-job-queued", kind: "job", status: "queued", since: minsAgo(2), repo: null, owner_label: "Jordan Kim" }),
  ];
  return {
    as_of,
    clear: false,
    active: {
      count: 2,
      items: [
        item({ run_id: "run-busy", status: "running", since: minsAgo(34) }),
        item({ run_id: "run-live", status: "running", since: minsAgo(7), owner_label: "Alex Rivera" }),
      ],
    },
    held: { count: held.length, items: held },
    custody: {
      count: 1,
      items: [
        item({ run_id: "run-failed", status: "failed", tag: "needs_owner", since: minsAgo(60 * 26), hold_id: "hold-7f3a" }),
      ],
    },
    queued: { count: queued.length, items: queued },
    idle_bound_workers: {
      count: 1,
      items: [{ worker_id: "eph-robin-4c1d", owner_label: "Robin Diaz", run_id: "run-queued", since: minsAgo(6) }],
    },
  };
}

function adminDoc(): AdminMaintenance {
  return { state: { ...maintenanceState() }, readout: readout() };
}

export const maintenanceApi = {
  getMaintenance: async (): Promise<MaintenanceState> => {
    requireSession();
    const { active, since, reason } = maintenanceState();
    return delay({ active, since, reason });
  },

  getAdminMaintenance: async (): Promise<AdminMaintenance> => {
    requireAdmin();
    return delay(adminDoc());
  },

  // PUT: idempotent. Enabling while on updates the reason only and keeps `since`; disabling
  // clears since/reason. The reason is trimmed and capped exactly as the server validates it.
  setMaintenance: async (active: boolean, reason: string): Promise<AdminMaintenance> => {
    const me = requireAdmin();
    const text = reason.trim();
    if ([...text].length > REASON_MAX) throw new ApiError(400, `reason must be at most ${REASON_MAX} characters`);
    const prev = maintenanceState();
    current = active
      ? {
          active: true,
          since: prev.active ? prev.since : new Date().toISOString(),
          reason: text === "" ? null : text,
          by_label: prev.active ? prev.by_label : (me.display_name ?? me.email),
        }
      : { active: false, since: null, reason: null, by_label: null };
    return delay(adminDoc());
  },
};
