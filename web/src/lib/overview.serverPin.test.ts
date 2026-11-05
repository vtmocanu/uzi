import { describe, expect, it } from "vitest";
import type { RunStatus, RunSummaryItem } from "./api";
import { parkDisposition } from "./overview";

// Pins parkDisposition against the server's needs_owner / progressing tag in
// ListMaintenanceHeldRuns (PRD #3878). The `?raw` import, not node:fs, for the reasons in
// workerSizes.test.ts: web/Dockerfile copies web/ and docs/ only, tsc never resolves the
// path, and vitest runs from a full checkout so the assertion has teeth. Moving the file
// shows up as an ENOENT from `npm test`, not from the compiler.
import sql from "../../../api/internal/store/queries/intake_pause.sql?raw";

function heldRunsBlock(src: string): string {
  const start = src.indexOf("-- name: ListMaintenanceHeldRuns");
  if (start < 0) throw new Error("ListMaintenanceHeldRuns not found in intake_pause.sql");
  const rest = src.slice(start + 1);
  const next = rest.indexOf("-- name:");
  return (next < 0 ? rest : rest.slice(0, next)).replace(/\s+/g, " ");
}

const QUOTED_LIST = /\(((?:'[a-z_]+'(?:, )?)+)\)/;
function parseList(list: string): Set<string> {
  return new Set([...list.matchAll(/'([a-z_]+)'/g)].map((m) => m[1]));
}

// What the server pins, extracted from the SQL text. The extracted shape is only for the checks below.
function serverShape(block: string) {
  const status = block.match(/r\.status IN \('awaiting_input', 'paused'\)/);
  const owner = block.match(
    /r\.recovery_wait_cause IN (\('worker_requeue_exhausted', 'worker_memory_pressure', 'retained_recovery_adoption_failed'\))/,
  );
  const stamped = block.match(
    /r\.recovery_wait_cause IN (\('retained_recovery_exhausted', 'codex_idle_stall'\)) AND r\.recovery_retry_not_before IS NULL/,
  );
  return {
    statusOwner: status != null,
    alwaysOwnerCauses: owner ? parseList(owner[1].match(QUOTED_LIST)![1]) : null,
    nullStampOwnerCauses: stamped ? parseList(stamped[1].match(QUOTED_LIST)![1]) : null,
  };
}

const STAMP = "2026-07-05T12:30:00Z";
const recoveryRun = (cause: string, stamp?: string | null) =>
  ({ id: "r", status: "recovery_wait", health: "ok", recovery_wait_cause: cause, ...(stamp === undefined ? {} : { recovery_retry_not_before: stamp }) }) as unknown as RunSummaryItem;
const pausedRun = (over: Partial<RunSummaryItem> = {}) =>
  ({ id: "r", status: "paused" as RunStatus, health: "ok", ...over }) as RunSummaryItem;

// Where parkDisposition deliberately differs from the server tag, and why.
const DIVERGENCES = [
  {
    name: "weekly-reserve pause",
    reason: "The server tags every paused run needs_owner; the reserve pause releases itself, so the card does not ask the owner to Resume.",
    run: pausedRun({ hold_reason: "weekly_reserve" }),
    server: "owner",
    here: "automatic",
  },
  {
    name: "plain recovery_wait with no retry stamp",
    reason: "Server: progressing. Nothing is scheduled, so the card fails closed to owner.",
    run: recoveryRun("forge_unreachable", null),
    server: "automatic",
    here: "owner",
  },
  {
    name: "provider_outage with no retry stamp",
    reason: "Server: progressing. No scheduled retry, so owner here.",
    run: recoveryRun("provider_outage", null),
    server: "automatic",
    here: "owner",
  },
  {
    name: "retained_recovery_exhausted with an omitted stamp (*_unknown)",
    reason: "Server only treats a NULL stamp as owner; an omitted or invalid one is unknown and fails closed to owner here.",
    run: recoveryRun("retained_recovery_exhausted"),
    server: "automatic",
    here: "owner",
  },
  {
    name: "codex_idle_stall with an invalid stamp (*_unknown)",
    reason: "Same as retained_recovery_exhausted: unknown timing is owner here.",
    run: recoveryRun("codex_idle_stall", "garbage"),
    server: "automatic",
    here: "owner",
  },
] as const;

describe("parkDisposition against ListMaintenanceHeldRuns", () => {
  const shape = serverShape(heldRunsBlock(sql));

  it("finds the server's owner conditions in the SQL", () => {
    expect(shape.statusOwner).toBe(true);
    expect(shape.alwaysOwnerCauses).toEqual(
      new Set(["worker_requeue_exhausted", "worker_memory_pressure", "retained_recovery_adoption_failed"]),
    );
    expect(shape.nullStampOwnerCauses).toEqual(new Set(["retained_recovery_exhausted", "codex_idle_stall"]));
  });

  it("agrees on the always-owner recovery causes, whatever the stamp", () => {
    for (const cause of shape.alwaysOwnerCauses!) {
      for (const stamp of [undefined, null, STAMP]) {
        expect(parkDisposition(recoveryRun(cause, stamp)), `${cause} / ${String(stamp)}`).toBe("owner");
      }
    }
  });

  it("agrees on the causes the server holds only while the retry stamp is NULL", () => {
    for (const cause of shape.nullStampOwnerCauses!) {
      expect(parkDisposition(recoveryRun(cause, null)), cause).toBe("owner");
      expect(parkDisposition(recoveryRun(cause, STAMP)), cause).toBe("automatic");
    }
  });

  it("agrees that an owner pause and a plain pause are owner", () => {
    expect(parkDisposition(pausedRun())).toBe("owner");
    expect(parkDisposition(pausedRun({ hold_reason: "budget_exhausted" }))).toBe("owner");
  });

  it("requires the null-stamp cause list to be followed directly by the NULL-stamp predicate", () => {
    const block = heldRunsBlock(sql);
    expect(block).toMatch(
      /\('retained_recovery_exhausted', 'codex_idle_stall'\) AND r\.recovery_retry_not_before IS NULL/,
    );
    // The check has teeth: the same regex rejects a predicate that dropped the stamp condition.
    const mutated = block.replace(" AND r.recovery_retry_not_before IS NULL", "");
    expect(mutated).not.toMatch(
      /\('retained_recovery_exhausted', 'codex_idle_stall'\) AND r\.recovery_retry_not_before IS NULL/,
    );
    expect(serverShape(mutated).nullStampOwnerCauses).toBeNull();
  });

  it("behaves as documented on every divergence", () => {
    for (const d of DIVERGENCES) {
      expect(d.reason.length, d.name).toBeGreaterThan(0);
      expect(parkDisposition(d.run), d.name).toBe(d.here);
      expect(d.here, d.name).not.toBe(d.server);
    }
  });
});
