import { describe, expect, it } from "vitest";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import type { ScheduleSettingsPreview } from "./apiTypes";
import { modelSettingSourceLabel } from "./modelSettings";
import {
  defaultModelChipSuffix,
  scheduleRefusalLabel,
  schedulePreviewKey,
  schedulePreviewRequest,
  schedulePreviewView,
} from "./schedulePreview";

function fixture(scenario: string, name: string): ScheduleSettingsPreview {
  const s = RECORDED_EFFECTIVE_SCENARIOS.find((x) => x.name === scenario);
  const p = s?.schedule_previews.find((x) => x.name === name);
  if (!p) throw new Error(`missing fixture ${scenario}/${name}`);
  return structuredClone(p.response);
}

const draft = {
  target: "sweep",
  harnessPatch: undefined,
  credentialPatch: undefined,
  model: "",
  overrideSubagentModel: false,
} as const;

describe("schedulePreviewView over every recorded response", () => {
  for (const s of RECORDED_EFFECTIVE_SCENARIOS) {
    for (const p of s.schedule_previews) {
      it(`${s.name}/${p.name}`, () => {
        const v = schedulePreviewView(p.response);
        expect(v.refusal !== null).toBe(p.response.refusal !== null);
        if (p.response.worker_model) {
          expect(v.model?.source).toBe(modelSettingSourceLabel("model", p.response.worker_model.source));
        } else {
          expect(v.model).toBeNull();
        }
        expect(v.alternatives).toHaveLength(p.response.alternatives?.length ?? 0);
        for (const row of [v.model, v.effort, v.credential, v.harness]) {
          if (row) expect(row.timing).toMatch(/^(pinned|predicted)/);
        }
        const c = p.response.credential;
        if (c) expect(JSON.stringify(v.credential)).not.toMatch(/secret|[0-9a-f]{8}-[0-9a-f]{4}/);
      });
    }
  }
});

describe("schedulePreviewView specifics", () => {
  it("unpinned sweep: chosen at create with two alternatives and notes", () => {
    const p = fixture("default", "unpinned sweep");
    const v = schedulePreviewView(p);
    expect(v.harness?.chosenAtCreate).toBe(true);
    expect(v.harness?.value).toBeNull();
    expect(v.model).toBeNull();
    expect(v.alternatives.map((a) => a.harness)).toEqual(["Claude", "Codex"]);
    expect(v.notes).toContain(modelSettingSourceLabel("note", "issue_labels_may_select_harness"));
    expect(v.notes).toContain(modelSettingSourceLabel("note", "automatic_weekly_reset"));
    const claude = v.alternatives[0];
    expect(claude.model?.value).toBe(p.alternatives![0].worker_model!.resolved_model);
    expect(claude.model?.timing).toBe("predicted · read when a worker picks the run up");
    expect(claude.credential?.value).toBe("Anthropic");
    expect(v.alternatives[1].credential?.value).toBe("Codex · subscription");
  });

  it("pinned harness, ignored model pin", () => {
    const p = fixture("default", "sweep pinned to codex with a claude model pin");
    const v = schedulePreviewView(p);
    expect(v.harness?.value).toBe("Codex");
    expect(v.harness?.timing).toBe("pinned · fixed when it fires");
    expect(v.ignoredPins).toHaveLength(1);
    expect(v.ignoredPins[0].pin).toBe("sonnet");
    expect(v.ignoredPins[0].harness).toBe("Codex");
    expect(v.ignoredPins[0].fallback).toBe(p.worker_model!.resolved_model);
    expect(v.ignoredPins[0].fallbackSource).toBe(modelSettingSourceLabel("model", p.worker_model!.source));
    expect(v.model?.source).toBe(modelSettingSourceLabel("model", p.worker_model!.source));
    expect(v.model?.timing).toBe("predicted · read when a worker picks the run up");
  });

  it("automatic harness is predicted and fixed at create; a model pin is pinned", () => {
    const v = schedulePreviewView(fixture("default", "sweep with a model pin and no harness"));
    expect(v.harness?.timing).toBe("predicted · fixed when it fires");
    expect(v.model?.value).toBe("sonnet");
    expect(v.model?.timing).toBe("pinned · fixed when it fires");
    expect(v.ignoredPins).toEqual([]);
  });

  it("an ignored pin inside a chosen-at-create alternative gets that branch's harness and fallback", () => {
    const p = fixture("default", "unpinned sweep");
    p.alternatives![1].worker_model!.ignored = { value: "sonnet", source: "schedule_pin" };
    const v = schedulePreviewView(p);
    expect(v.ignoredPins).toEqual([
      {
        pin: "sonnet",
        harness: "Codex",
        fallback: p.alternatives![1].worker_model!.resolved_model,
        fallbackSource: modelSettingSourceLabel("model", p.alternatives![1].worker_model!.source),
      },
    ]);
  });

  it("codexOnly: single usable harness and ignored pin", () => {
    const v = schedulePreviewView(fixture("codexOnly", "sweep with a claude model pin"));
    expect(v.harness?.value).toBe("Codex");
    expect(v.ignoredPins.map((x) => x.pin)).toEqual(["sonnet"]);
    expect(v.subagents?.mode).toBe("follow the lead");
    const plain = schedulePreviewView(fixture("codexOnly", "unpinned sweep"));
    expect(plain.ignoredPins).toEqual([]);
    expect(plain.effort?.value).toBe("medium");
  });

  it("refusals carry labels and no rows", () => {
    const v = schedulePreviewView(fixture("default", "invalid model"));
    expect(v.refusal).toBe(scheduleRefusalLabel("invalid_model"));
    expect(v.harness).toBeNull();
    expect(v.model).toBeNull();
    const w = schedulePreviewView(fixture("codexOnly", "sweep pinned to claude"));
    expect(w.refusal).toBe(scheduleRefusalLabel("no_credential_for_harness"));
  });

  it("schedule-only refusals use the shared wording", () => {
    expect(scheduleRefusalLabel("invalid_model")).toBe("This model ID is not valid");
    expect(scheduleRefusalLabel("codex_override_conflict")).toBe(
      "Codex harness cannot use an Anthropic credential override",
    );
    expect(scheduleRefusalLabel("codex_pool_unavailable")).toBe("No eligible Codex account in the auto-select pool");
  });

  it("server text in effort, credential kind and harness names is stripped", () => {
    const bidi = "‮";
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    p.worker_effort = { ...p.worker_effort!, mode: "explicit", resolved_effort: `hi${bidi}gh` };
    p.credential = { ...p.credential!, kind: `x${bidi}y` } as typeof p.credential;
    p.harness = { ...p.harness!, value: `cl${bidi}aude` } as typeof p.harness;
    const v = schedulePreviewView(p);
    expect(v.effort?.value).toBe("high");
    expect(v.credential?.value.startsWith("Xy")).toBe(true);
    expect(v.harness?.value).toBe("Claude");
    const ignored = fixture("default", "sweep pinned to codex with a claude model pin");
    ignored.harness = { ...ignored.harness!, value: `co${bidi}dex` } as typeof ignored.harness;
    expect(schedulePreviewView(ignored).ignoredPins[0].harness).toBe("Codex");
    expect(defaultModelChipSuffix(p)).toContain("(Claude, predicted)");
  });

  it("subagents: follow the lead and own pins", () => {
    const follow = schedulePreviewView(fixture("default", "sweep pinned to claude with subagents following the lead"));
    expect(follow.subagents).toMatchObject({ mode: "follow the lead", lead: "opus", pins: [], more: false });
    const p = fixture("default", "sweep pinned to claude with subagents on their own pins");
    p.subagents = {
      mode: "own_pins",
      lead_value: "",
      pinned: [{ name: "reviewer", model: "haiku" }],
      pinned_truncated: true,
      notes: ["repo_agents_not_predicted"],
    };
    const own = schedulePreviewView(p).subagents!;
    expect(own.mode).toBe("own pins, else the lead");
    expect(own.lead).toBe(modelSettingSourceLabel("note", "sdk_default"));
    expect(own.pins).toEqual(["reviewer → haiku"]);
    expect(own.more).toBe(true);
    expect(own.notes).toEqual([modelSettingSourceLabel("note", "repo_agents_not_predicted")]);
  });

  it("empty resolved_model reads as the SDK decides", () => {
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    p.worker_model!.resolved_model = "";
    expect(schedulePreviewView(p).model?.value).toBe(modelSettingSourceLabel("note", "sdk_default"));
  });

  it("an alternative's own refusal", () => {
    const p = fixture("default", "unpinned sweep");
    p.alternatives![1] = {
      harness: "codex",
      worker_model: null,
      worker_effort: null,
      credential: null,
      subagents: null,
      refusal: "codex_pool_unavailable",
      refusal_detail: null,
    };
    const v = schedulePreviewView(p);
    expect(v.alternatives[1].refusal).toBe(scheduleRefusalLabel("codex_pool_unavailable"));
    expect(v.alternatives[1].model).toBeNull();
  });
});

describe("defaultModelChipSuffix", () => {
  it("decided harness", () => {
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    expect(defaultModelChipSuffix(p)).toBe("now opus (Claude, predicted)");
  });

  it("chosen at create joins the surviving branches", () => {
    const p = fixture("default", "unpinned sweep");
    const [a, b] = p.alternatives!;
    expect(defaultModelChipSuffix(p)).toBe(
      `now ${a.worker_model!.resolved_model} (Claude) or ${b.worker_model!.resolved_model} (Codex), predicted`,
    );
    p.alternatives![1] = { ...b, worker_model: null, refusal: "codex_pool_unavailable" };
    expect(defaultModelChipSuffix(p)).toBe(`now ${a.worker_model!.resolved_model} (Claude, predicted)`);
    p.alternatives![0] = { ...a, worker_model: null, refusal: "no_usable_credential" };
    expect(defaultModelChipSuffix(p)).toBeNull();
  });

  it("a schedule-pinned model is not described as the Default", () => {
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    p.worker_model!.source = "schedule_pin";
    expect(defaultModelChipSuffix(p)).toBeNull();

    const q = fixture("default", "unpinned sweep");
    const [a, b] = q.alternatives!;
    q.alternatives![0] = { ...a, worker_model: { ...a.worker_model!, source: "schedule_pin" } };
    expect(b.worker_model!.source).not.toBe("schedule_pin");
    expect(defaultModelChipSuffix(q)).toBeNull();
  });

  it("refused or missing model gives null", () => {
    expect(defaultModelChipSuffix(fixture("default", "invalid model"))).toBeNull();
    const p = fixture("default", "sweep pinned to claude with subagents following the lead");
    p.worker_model = null;
    expect(defaultModelChipSuffix(p)).toBeNull();
  });
});

describe("schedulePreviewRequest", () => {
  it("a new draft omits untouched patches and sends model and subagent flag", () => {
    expect(schedulePreviewRequest(draft)).toEqual({ target: "sweep", model: null, override_subagent_model: false });
  });

  it("an edit carries schedule_id", () => {
    const r = schedulePreviewRequest({ ...draft, scheduleId: "s1", model: " opus ", overrideSubagentModel: true });
    expect(r).toEqual({ target: "sweep", schedule_id: "s1", model: " opus ", override_subagent_model: true });
    expect("harness" in r).toBe(false);
    expect("credential_override" in r).toBe(false);
  });

  it("a cleared harness is null, a pin is the pin", () => {
    expect(schedulePreviewRequest({ ...draft, harnessPatch: null }).harness).toBeNull();
    expect(schedulePreviewRequest({ ...draft, harnessPatch: "codex" }).harness).toBe("codex");
  });

  it("credential: inherit/pinned pass through, self_improve never sends one", () => {
    const inherit = { mode: "inherit" };
    expect(schedulePreviewRequest({ ...draft, credentialPatch: inherit }).credential_override).toEqual(inherit);
    const r = schedulePreviewRequest({ ...draft, target: "self_improve", credentialPatch: { mode: "auto" } });
    expect("credential_override" in r).toBe(false);
  });

  it("blank model is null", () => {
    expect(schedulePreviewRequest({ ...draft, model: "   " }).model).toBeNull();
  });
});

describe("schedulePreviewKey", () => {
  it("distinguishes omitted from null and differing fields", () => {
    const base = { target: "sweep", model: null, override_subagent_model: false };
    const keys = new Set([
      schedulePreviewKey(base),
      schedulePreviewKey({ ...base, harness: null }),
      schedulePreviewKey({ ...base, harness: "claude" }),
      schedulePreviewKey({ ...base, credential_override: null }),
      schedulePreviewKey({ ...base, credential_override: { mode: "pinned", secret_id: "a" } }),
      schedulePreviewKey({ ...base, credential_override: { mode: "pinned", secret_id: "b" } }),
      schedulePreviewKey({ ...base, schedule_id: "x" }),
      schedulePreviewKey({ ...base, model: "opus" }),
      schedulePreviewKey({ ...base, override_subagent_model: true }),
    ]);
    expect(keys.size).toBe(9);
    expect(schedulePreviewKey(base)).toBe(schedulePreviewKey({ ...base }));
  });
});

describe("scheduleRefusalLabel", () => {
  it("every recorded refusal and added token has text; unknown is Refused", () => {
    const tokens = new Set<string>(["invalid_model", "credential_override_lane_not_switchable", "codex_override_conflict", "codex_pool_unavailable"]);
    for (const s of RECORDED_EFFECTIVE_SCENARIOS) {
      for (const p of s.schedule_previews) if (p.response.refusal) tokens.add(p.response.refusal);
    }
    for (const t of tokens) {
      expect(scheduleRefusalLabel(t as never).length).toBeGreaterThan(0);
      expect(scheduleRefusalLabel(t as never)).not.toBe("Refused");
    }
    expect(scheduleRefusalLabel("something_new" as never)).toBe("Refused");
  });
});
