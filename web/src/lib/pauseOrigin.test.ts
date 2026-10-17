import { describe, expect, it } from "vitest";
import type { Run, RunMessage } from "./api";
import { pauseOrigin } from "./pauseOrigin";

const since = "2026-10-16T12:00:00.123456789Z";
const run: Pick<Run, "status" | "status_since" | "hold_reason"> = { status: "paused", status_since: since };
const marker = (over: Partial<RunMessage> = {}): RunMessage => ({
  seq: 1, kind: "paused", created_at: since, payload: { origin: "lead_park" },
  agent: null, agent_instance: null, agent_label: null, ...over,
});
describe("pauseOrigin", () => {
  it.each(["lead_park", "owner"] as const)("accepts exact %s at equality", (origin) => {
    expect(pauseOrigin(run, [marker({ payload: { origin } })], true)).toBe(origin);
  });
  it.each([null, {}, [], "owner", { origin: "unknown" }, { origin: "OWNER" },
    { origin: { name: "owner" } }, { text: "Paused by you", person: "owner" },
    { origin: "<img src=x>", pending_request: "pause" }])("rejects hostile/unknown payload %j", (payload) => {
    expect(pauseOrigin(run, [marker({ payload })], true)).toBe("");
  });
  it.each(["", "bad", "2026-02-30T12:00:00Z", "2026-10-16", "0001-01-01T00:00:00Z",
    "0001-01-01T01:00:00+01:00", "2026-10-16T24:00:00Z", "2026-10-16T12:00:00+24:00",
    "2026-10-16T12:00:00.123456788Z"])("rejects invalid/zero/stale marker %s", (created_at) => {
    expect(pauseOrigin(run, [marker({ created_at })], true)).toBe("");
  });
  it.each([null, undefined, "", "bad", "0001-01-01T00:00:00Z", "2026-02-30T00:00:00Z"])("rejects status_since %s", (status_since) => {
    expect(pauseOrigin({ ...run, status_since }, [marker()], true)).toBe("");
  });
  it("compares offsets and keeps nanoseconds", () => {
    expect(pauseOrigin(run, [marker({ created_at: "2026-10-16T13:00:00.123456789+01:00" })], true)).toBe("lead_park");
    expect(pauseOrigin(run, [marker({ created_at: "2026-10-16T12:00:00.123456790Z" })], true)).toBe("lead_park");
  });
  it("validates the highest paused seq before considering its evidence", () => {
    for (const invalid of [marker({ seq: 3, payload: {} }), marker({ seq: 3, created_at: "" })]) {
      expect(pauseOrigin(run, [invalid, marker({ seq: 2 })], true)).toBe("");
    }
    expect(pauseOrigin(run, [marker(), marker({ seq: 2, kind: "text", payload: {} })], true)).toBe("lead_park");
    expect(pauseOrigin(run, [marker({ seq: 0 })], true)).toBe("");
  });
  it("requires loaded evidence, current paused epoch and no hold", () => {
    expect(pauseOrigin(run, [marker()])).toBe("");
    expect(pauseOrigin(run, [], true)).toBe("");
    expect(pauseOrigin({ ...run, status: "running" }, [marker()], true)).toBe("");
    expect(pauseOrigin({ ...run, status_since: "2026-10-16T12:01:00Z" }, [marker()], true)).toBe("");
    for (const hold_reason of ["completion_blocked", "credential_disabled", "provider_unavailable", "unknown"]) {
      expect(pauseOrigin({ ...run, hold_reason }, [marker()], true)).toBe("");
    }
  });
});
