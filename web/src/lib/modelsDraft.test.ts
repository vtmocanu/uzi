import { describe, expect, it } from "vitest";
import type { UserSettings } from "./apiTypes";
import {
  baselineFromSettings,
  buildModelsPatch,
  dirtyFields,
  emptyCustomFields,
  fieldLabel,
  localFieldWarnings,
  storedFieldWarnings,
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
  it("describes every Models cell in grid order, each named <Row> — <Family>", () => {
    expect(MODELS_FIELDS.map((f) => f.key)).toEqual([
      "default_harness", "default_claude_model", "default_codex_model", "default_effort", "default_codex_effort",
      "planning_claude_model", "planning_codex_model", "planning_claude_effort", "planning_codex_effort",
      "judge_runs_on", "judge_model", "codex_judge_model", "summary_model", "codex_summary_model",
      "task_review_model", "codex_task_review_model", "task_review_effort", "codex_task_review_effort",
      "cross_check_pins:plan/claude/model", "cross_check_pins:plan/codex/model",
      "cross_check_pins:plan/claude/effort", "cross_check_pins:plan/codex/effort",
      "cross_check_pins:code/claude/model", "cross_check_pins:code/codex/model",
      "cross_check_pins:code/claude/effort", "cross_check_pins:code/codex/effort",
    ]);
    expect(MODELS_FIELDS.map(fieldLabel)).toEqual([
      "Default harness", "Worker model — Claude", "Worker model — Codex", "Worker effort — Claude", "Worker effort — Codex",
      "Planning model — Claude", "Planning model — Codex", "Planning effort — Claude", "Planning effort — Codex",
      "Judge runs on", "Judge model — Claude", "Judge model — Codex", "Summary model — Claude", "Summary model — Codex",
      "Task review model — Claude", "Task review model — Codex", "Task review effort — Claude", "Task review effort — Codex",
      "Plan cross-check model — Claude", "Plan cross-check model — Codex", "Plan cross-check effort — Claude", "Plan cross-check effort — Codex",
      "Code cross-check model — Claude", "Code cross-check model — Codex", "Code cross-check effort — Claude", "Code cross-check effort — Codex",
    ]);
  });

  it("sends a mixed helper, worker and pin draft as one patch with exactly the changed keys", () => {
    const base = baselineFromSettings(settings({
      cross_check_pins: [{ stage: "code", harness: "codex", model: "gpt-6-sol", effort: "max" } as never],
    }));
    const draft = {
      ...base,
      default_codex_model: "gpt-6-sol",
      judge_runs_on: "claude",
      codex_judge_model: "gpt-6-astra",
      task_review_effort: "runtime",
      "cross_check_pins:plan/claude/effort": "low",
      "cross_check_pins:code/codex/model": "",
    };
    expect(buildModelsPatch(base, draft)).toEqual({
      default_codex_model: "gpt-6-sol",
      judge_runs_on: "claude",
      codex_judge_model: "gpt-6-astra",
      task_review_effort: "runtime",
      cross_check_pins: [
        { stage: "plan", harness: "claude", effort: "low" },
        { stage: "code", harness: "codex", model: null },
      ],
    });
  });
});

describe("localFieldWarnings", () => {
  it("flags only dirty values its own validator refuses, dormant judge lanes included", () => {
    const base = baselineFromSettings(settings({ summary_model: "gpt-6-sol" }));
    // A stored refused value is not flagged: only an edit can be blocked.
    expect(localFieldWarnings(base, base)).toEqual({});
    const draft = {
      ...base,
      judge_runs_on: "codex",
      judge_model: "gpt-6.1-sol", // the dormant Claude lane is still validated
      task_review_model: "two words",
      "cross_check_pins:plan/claude/model": "gpt-6-sol",
      "cross_check_pins:code/codex/model": "sonnet",
      "cross_check_pins:plan/codex/model": "custom-codex-checker",
      default_codex_model: "custom-codex-id", // a custom id passes the Codex lane
    };
    const warnings = localFieldWarnings(base, draft);
    expect(Object.keys(warnings)).toEqual([
      "judge_model", "task_review_model", "cross_check_pins:plan/claude/model", "cross_check_pins:code/codex/model",
    ]);
    expect(warnings.judge_model).toBe("Not a Claude model: gpt-6.1-sol is a Codex model, so Claude judges skip it. Choose Default to clear it.");
    expect(warnings.task_review_model).toBe("Model must be a single token with no spaces.");
    expect(warnings["cross_check_pins:code/codex/model"]).toBe("Choose a model from the checker's model family.");
  });

  it("trims a checker pin as the server does (Unicode White_Space), so U+FEFF is refused, not trimmed away", () => {
    const base = baselineFromSettings(settings());
    const key = "cross_check_pins:plan/codex/model";
    expect(localFieldWarnings(base, { ...base, [key]: "bad\uFEFF" })).toEqual({ [key]: "Invalid model ID." });
    expect(buildModelsPatch(base, { ...base, [key]: "\u2003custom-id\u2003" })).toEqual({
      cross_check_pins: [{ stage: "plan", harness: "codex", model: "custom-id" }],
    });
  });
});

describe("storedFieldWarnings", () => {
  it("flags only unchanged stored values its validator refuses, and drops the flag once the cell is edited", () => {
    const base = baselineFromSettings(settings({
      summary_model: "gpt-6-sol",
      planning_codex_model: "claude-opus-4-8", // a full Claude id is not an alias, so it passes
      cross_check_pins: [{ stage: "code", harness: "claude", model: "gpt-6.1-sol", effort: null }] as UserSettings["cross_check_pins"],
    }));
    expect(storedFieldWarnings(base, base)).toEqual({
      summary_model: "The saved value is not valid here. Not a Claude model: gpt-6-sol is a Codex model, so Claude summaries skip it. Choose Default to clear it.",
      "cross_check_pins:code/claude/model": "The saved value is not valid here. Choose a model from the checker's model family.",
    });
    const edited = storedFieldWarnings(base, { ...base, summary_model: "" });
    expect(Object.keys(edited)).toEqual(["cross_check_pins:code/claude/model"]);
    // Stored warnings are not blocking ones.
    expect(localFieldWarnings(base, base)).toEqual({});
  });
});

describe("worker and planning lane guards (mirror the server decoders)", () => {
  const claudeWorker = "Not a Claude model: gpt-6-sol is a Codex model, so Claude runs skip it. Choose Default to clear it.";
  const claudePlanning = "Not a Claude model: gpt-6-astra is a Codex model, so Claude planning skips it. Choose Same as the run to clear it.";
  const codexWorker = "Not a Codex model: opus is a Claude model alias, so the Codex lane cannot use it. Choose Default to clear it.";
  const codexPlanning = "Not a Codex model: sonnet is a Claude model alias, so the Codex lane cannot use it. Choose Same as the run to clear it.";

  it("blocks a dirty Codex id in a Claude lane and a dirty Claude alias in a Codex lane", () => {
    const base = baselineFromSettings(settings());
    const draft = {
      ...base,
      default_claude_model: " gpt-6-sol ",
      default_codex_model: "opus",
      planning_claude_model: "gpt-6-astra",
      planning_codex_model: "sonnet",
    };
    expect(localFieldWarnings(base, draft)).toEqual({
      default_claude_model: claudeWorker,
      default_codex_model: codexWorker,
      planning_claude_model: claudePlanning,
      planning_codex_model: codexPlanning,
    });
  });

  it("passes custom ids and the lane's own family, and applies the shared syntax rules", () => {
    const base = baselineFromSettings(settings());
    expect(localFieldWarnings(base, {
      ...base,
      default_claude_model: "claude-opus-4-8",
      default_codex_model: "claude-opus-4-8", // only the closed alias list is refused
      planning_claude_model: "opus",
      planning_codex_model: "gpt-6-sol",
    })).toEqual({});
    expect(localFieldWarnings(base, { ...base, default_codex_model: "two words" })).toEqual({
      default_codex_model: "Model must be a single token with no spaces.",
    });
  });

  it("reports a stored refused value as information, not as a blocking warning", () => {
    const base = baselineFromSettings(settings({
      default_claude_model: "gpt-6-sol",
      default_codex_model: "opus",
      planning_claude_model: "gpt-6-astra",
      planning_codex_model: "sonnet",
    }));
    expect(localFieldWarnings(base, base)).toEqual({});
    expect(storedFieldWarnings(base, base)).toEqual({
      default_claude_model: `The saved value is not valid here. ${claudeWorker}`,
      default_codex_model: `The saved value is not valid here. ${codexWorker}`,
      planning_claude_model: `The saved value is not valid here. ${claudePlanning}`,
      planning_codex_model: `The saved value is not valid here. ${codexPlanning}`,
    });
    // Choosing Default clears the stored flag and is not itself blocked.
    const cleared = { ...base, default_claude_model: "" };
    expect(storedFieldWarnings(base, cleared).default_claude_model).toBeUndefined();
    expect(localFieldWarnings(base, cleared)).toEqual({});
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

describe("emptyCustomFields", () => {
  it("flags a model cell in custom mode with an empty ID whether or not it is dirty", () => {
    const base = baselineFromSettings(settings({ default_codex_model: "gpt-6-sol" }));
    // Stored Default, Other with an empty box: not dirty, still flagged.
    const draft = { ...base, default_codex_model: "  " };
    expect(dirtyFields(base, base).length).toBe(0);
    const custom = { default_claude_model: true, default_codex_model: true, planning_claude_model: false };
    expect(emptyCustomFields(draft, custom).map((f) => f.key)).toEqual(["default_claude_model", "default_codex_model"]);
    expect(emptyCustomFields({ ...draft, default_claude_model: "claude-x" }, custom).map((f) => f.key)).toEqual(["default_codex_model"]);
    expect(emptyCustomFields(draft, {})).toEqual([]);
  });
});

it("flags a raw format character that JavaScript trim would drop", () => {
  expect(localFieldWarnings({ default_codex_model: "" }, { default_codex_model: "\uFEFF" }))
    .toHaveProperty("default_codex_model", expect.stringContaining("format character"));
});
