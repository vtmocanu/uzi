import { describe, expect, it } from "vitest";
import type { UserSettings } from "./apiTypes";
import {
  baselineFromSettings,
  buildModelsPatch,
  dirtyFields,
  fieldLabel,
  mapFieldErrors,
  MODELS_FIELDS,
  normalizeValue,
  pinField,
} from "./modelsDraft";

const settings = (over: Partial<UserSettings> = {}): UserSettings =>
  ({
    default_harness: null, default_claude_model: null, default_codex_model: null, default_effort: null,
    default_codex_effort: null, planning_claude_model: null, planning_claude_effort: null,
    planning_codex_model: null, planning_codex_effort: null, ...over,
  }) as UserSettings;

describe("MODELS_FIELDS", () => {
  it("describes exactly the nine M1 cells, each named <Row> — <Family>", () => {
    expect(MODELS_FIELDS.map((f) => f.key)).toEqual([
      "default_harness", "default_claude_model", "default_codex_model", "default_effort", "default_codex_effort",
      "planning_claude_model", "planning_codex_model", "planning_claude_effort", "planning_codex_effort",
    ]);
    expect(MODELS_FIELDS.map(fieldLabel)).toEqual([
      "Default harness", "Worker model — Claude", "Worker model — Codex", "Worker effort — Claude", "Worker effort — Codex",
      "Planning model — Claude", "Planning model — Codex", "Planning effort — Claude", "Planning effort — Codex",
    ]);
  });
});

describe("baseline and dirty set", () => {
  it("reads null, absent and blank as Default and trims stored values", () => {
    const base = baselineFromSettings(settings({ default_claude_model: "  opus ", planning_codex_effort: undefined }));
    expect(base.default_claude_model).toBe("opus");
    expect(base.default_codex_model).toBe("");
    expect(base.planning_codex_effort).toBe("");
    expect(normalizeValue(null)).toBe("");
  });

  it("counts semantic changes only: whitespace is not a change, runtime is distinct from Default", () => {
    const base = baselineFromSettings(settings({ default_claude_model: "opus", default_effort: null }));
    expect(dirtyFields(base, { ...base, default_claude_model: " opus  " })).toEqual([]);
    const runtime = dirtyFields(base, { ...base, default_effort: "runtime" });
    expect(runtime.map((f) => f.key)).toEqual(["default_effort"]);
    const both = dirtyFields(base, { ...base, default_harness: "codex", default_claude_model: "" });
    expect(both.map((f) => f.key)).toEqual(["default_harness", "default_claude_model"]);
  });
});

describe("buildModelsPatch", () => {
  it("emits only dirty keys, trimmed, with Default as null and runtime kept as runtime", () => {
    const base = baselineFromSettings(settings({ default_claude_model: "opus", default_codex_effort: "high" }));
    const draft = { ...base, default_harness: "codex", default_claude_model: "", default_codex_effort: "runtime", planning_claude_model: " sonnet " };
    expect(buildModelsPatch(base, draft)).toEqual({
      default_harness: "codex",
      default_claude_model: null,
      default_codex_effort: "runtime",
      planning_claude_model: "sonnet",
    });
    expect(buildModelsPatch(base, base)).toEqual({});
  });

  it("groups cross-check pin members per stage/harness cell (the M2 descriptor shape)", () => {
    const fields = [
      ...MODELS_FIELDS,
      pinField("Plan check model", "plan", "codex", "model"),
      pinField("Plan check effort", "plan", "codex", "effort"),
      pinField("Code check effort", "code", "claude", "effort"),
    ];
    const base = baselineFromSettings(
      settings({ cross_check_pins: [{ stage: "plan", harness: "codex", model: "gpt-6-sol", effort: null } as never] }),
      fields,
    );
    expect(base["cross_check_pins:plan/codex/model"]).toBe("gpt-6-sol");
    const draft = { ...base, "cross_check_pins:plan/codex/model": "", "cross_check_pins:plan/codex/effort": "max", "cross_check_pins:code/claude/effort": "low" };
    expect(buildModelsPatch(base, draft, fields)).toEqual({
      cross_check_pins: [
        { stage: "plan", harness: "codex", model: null, effort: "max" },
        { stage: "code", harness: "claude", effort: "low" },
      ],
    });
  });
});

describe("mapFieldErrors", () => {
  it("pins scalar errors by field and pin errors by stage/harness/member; the rest is unmatched", () => {
    const fields = [...MODELS_FIELDS, pinField("Plan check effort", "plan", "codex", "effort")];
    const { byKey, unmatched } = mapFieldErrors(
      [
        { field: "default_codex_model", message: "default_codex_model: a Claude model is not valid for the Codex lane" },
        { field: "default_codex_model", message: "a second message for the same cell" },
        { field: "planning_claude_effort", message: "planning_claude_effort: effort must be one of low, medium, high, xhigh, max" },
        { field: "cross_check_pins", message: "invalid effort", stage: "plan", harness: "codex", member: "effort" },
        { field: "cross_check_pins", message: "cross_check_pins: expected at most four cells" },
        { field: "default_model", message: "unknown field default_model" },
      ],
      fields,
    );
    expect(byKey).toEqual({
      default_codex_model: "default_codex_model: a Claude model is not valid for the Codex lane",
      planning_claude_effort: "planning_claude_effort: effort must be one of low, medium, high, xhigh, max",
      "cross_check_pins:plan/codex/effort": "invalid effort",
    });
    expect(unmatched).toEqual(["cross_check_pins: expected at most four cells", "unknown field default_model"]);
  });
});

describe("modelSettingsFieldErrors / modelSettingsFailureReason (lib/api)", () => {
  it("prefers the additive errors[] and falls back to the flat field/error pair", async () => {
    const { ApiError } = await import("./apiError");
    const { modelSettingsFieldErrors, modelSettingsFailureReason } = await import("./api");
    const errors = [{ field: "default_effort", message: "bad effort" }, { field: "default_codex_effort", message: "bad too" }];
    expect(modelSettingsFieldErrors(new ApiError(400, "bad effort", { error: "bad effort", field: "default_effort", errors }))).toEqual(errors);
    expect(modelSettingsFieldErrors(new ApiError(400, "nope", { error: "nope", field: "default_harness" }))).toEqual([
      { field: "default_harness", message: "nope" },
    ]);
    expect(modelSettingsFieldErrors(new ApiError(400, "invalid request body", { error: "invalid request body" }))).toEqual([
      { field: "", message: "invalid request body" },
    ]);
    expect(modelSettingsFieldErrors(new ApiError(500, "internal error"))).toEqual([]);
    expect(modelSettingsFailureReason(new ApiError(500, "x", { reason: "settings_commit_unknown" }))).toBe("settings_commit_unknown");
    expect(modelSettingsFailureReason(new ApiError(500, "x", { reason: "settings_saved_readback_failed" }))).toBe("settings_saved_readback_failed");
    expect(modelSettingsFailureReason(new ApiError(500, "internal error", { error: "internal error" }))).toBeNull();
    expect(modelSettingsFailureReason(new Error("network"))).toBeNull();
  });
});
