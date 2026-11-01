import { describe, expect, it } from "vitest";
import cases from "../../../fixtures/recovery-prerequisite-closure/cases.json";
import type { RecoveryArchive } from "./apiTypes";
import { prerequisiteView, recoveryExportCommand } from "./recovery";
const RUN_UUID = "00000000-0000-0000-0000-000000000002";
const CAP_UUID = "00000000-0000-0000-0000-000000000001";
const EXPORT_CMD = `uzi run export ${RUN_UUID} --output <path> --capture ${CAP_UUID}`;
function archive(over: Partial<RecoveryArchive> = {}): RecoveryArchive {
  return { id: CAP_UUID, run_id: RUN_UUID, hold_id: "hold", state: "available", source_sha: cases.pin_tip,
    created_at: cases.updated_at, ...over };
}
describe("bounded prerequisite view and safe commands", () => {
  it.each(cases.cases)("$name maps owner DTO semantics independently", c => {
    const view = prerequisiteView(archive({ state: c.input.capture_state,
      prerequisite_shas: c.input.thin ? [cases.pin_tip] : [],
      ...(c.expected.closure ? { prerequisite_closure: c.expected.closure } : {}),
    }));
    expect(view.protection).toBe(c.expected.protection);
    expect(view.cleanup).toBe(c.expected.cleanup);
    expect(!!view.fetchCommand).toBe(c.expected.protection === "protected");
  });
  it.each([
    { absence_unconfirmed: "false" }, { cleanup_exhausted: "false" },
    { updated_at: "yesterday" }, { updated_at: "0001-01-01T00:00:00Z" }, { reason: {} }, { updated_at: "2026-02-30T12:00:00Z" },
    { updated_at: cases.updated_at + "\n" }, { pin_tip: undefined },
    { pin_tip: cases.pin_tip + "\n" }, { pin_ref: cases.pin_ref + ";echo bad" },
    { pin_tip: "f".repeat(41) }, { pin_tip: "0".repeat(40) }, { pin_tip: "f".repeat(64) }, { pin_tip: "A".repeat(40) }, { state: "__proto__" }, { state: "future" },
  ])("does not promote malformed runtime metadata %j", override => {
    const capture = archive({ id: CAP_UUID, prerequisite_shas: [cases.pin_tip],
      prerequisite_closure: { ...cases.cases[3].expected.closure, ...override } as RecoveryArchive["prerequisite_closure"],
    });
    expect(prerequisiteView(capture).protection).toBe("uncertain");
    expect(prerequisiteView(capture).fetchCommand).toBeNull();
  });

  it("keeps exact valid refs and tips and generates only the fixed UUID namespace", () => {
    const view = prerequisiteView(archive({ id: CAP_UUID, prerequisite_shas: [cases.pin_tip],
      prerequisite_closure: cases.cases[3].expected.closure!,
    }));
    expect(view.ref).toBe(cases.pin_ref);
    expect(view.tip).toBe(cases.pin_tip);
    expect(view.fetchCommand).toBe(`git fetch origin '${cases.pin_ref}:${cases.pin_ref}'`);
    expect(recoveryExportCommand(RUN_UUID, CAP_UUID)).toBe(EXPORT_CMD);
  });


  it("preserves self-contained behavior when the optional prerequisite list is omitted", () => {
    const view = prerequisiteView(archive());
    expect(view.protection).toBe("self_contained");
    expect(view.description).toContain("no prerequisite commits are recorded");
    expect(view.fetchCommand).toBeNull();
  });

  it.each(["$(echo bad)", "a;echo bad", CAP_UUID + "\n", "--help", "a".repeat(10000)])("suppresses unsafe identifiers %s", id => {
    expect(recoveryExportCommand(id, CAP_UUID)).toBeNull();
    expect(recoveryExportCommand(RUN_UUID, id)).toBeNull();
  });
});
