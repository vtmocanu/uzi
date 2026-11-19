import { describe, it, expect } from "vitest";
import type { EffectiveSettings, StartPreview } from "./apiTypes";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { DEFAULT_RUN_START_OPTIONS, type RunStartFacts, type RunStartOptions } from "./startRun";
import {
  compactSummary,
  defaultChoiceLabels,
  startPreviewKey,
  startPreviewRequest,
  startPreviewView,
  startRefusalLabel,
} from "./startPreview";

const SCENARIO = RECORDED_EFFECTIVE_SCENARIOS[0];
const CODEX_ONLY = RECORDED_EFFECTIVE_SCENARIOS[1];
const GET: EffectiveSettings = SCENARIO.get;

function recorded(name: string, scenario = SCENARIO): StartPreview {
  const r = scenario.start_previews.find((p) => p.name === name);
  if (!r) throw new Error(`no recorded preview ${name}`);
  return structuredClone(r.response);
}

const BOTH: RunStartFacts = { claudeUsable: true, codexUsable: true, defaultHarness: null };
const opts = (o: Partial<RunStartOptions>): RunStartOptions => ({ ...DEFAULT_RUN_START_OPTIONS, ...o });

describe("startPreviewRequest", () => {
  it("is the default choice for default options", () => {
    expect(startPreviewRequest(DEFAULT_RUN_START_OPTIONS, BOTH)).toEqual({ harness: null, credential_override: null });
  });

  it("matches what Start would send for a visible harness, model and effort", () => {
    expect(startPreviewRequest(opts({ harness: "claude", model: "opus", effort: "xhigh" }), BOTH)).toEqual({
      harness: "claude",
      credential_override: null,
      model: "opus",
      effort: "xhigh",
    });
  });

  it("drops the token on Codex and the harness for a single-harness user", () => {
    const token = { mode: "pinned" as const, secret_id: "s1" };
    expect(startPreviewRequest(opts({ harness: "codex", credential: token }), BOTH)).toEqual({
      harness: "codex",
      credential_override: null,
    });
    const claudeOnly: RunStartFacts = { claudeUsable: true, codexUsable: false, defaultHarness: null };
    expect(startPreviewRequest(opts({ harness: "codex", credential: token }), claudeOnly)).toEqual({
      harness: null,
      credential_override: { mode: "pinned", secret_id: "s1" },
    });
  });

  it("keeps a pinned secret id and omits Default model and effort", () => {
    const req = startPreviewRequest(opts({ credential: { mode: "pinned", secret_id: "abc" } }), BOTH);
    expect(req).toEqual({ harness: null, credential_override: { mode: "pinned", secret_id: "abc" } });
    expect("model" in req).toBe(false);
    expect("effort" in req).toBe(false);
  });
});

describe("startPreviewKey", () => {
  it("normalizes nulls and absent members to one key", () => {
    expect(startPreviewKey({ harness: null, credential_override: null })).toBe(
      startPreviewKey({ harness: null, credential_override: null, model: null, effort: null }),
    );
  });

  it("separates choices including the secret id", () => {
    const a = startPreviewKey({ harness: null, credential_override: { mode: "pinned", secret_id: "a" } });
    const b = startPreviewKey({ harness: null, credential_override: { mode: "pinned", secret_id: "b" } });
    const none = startPreviewKey({ harness: null, credential_override: { mode: "pinned" } });
    expect(new Set([a, b, none]).size).toBe(3);
    expect(startPreviewKey({ harness: "claude", credential_override: null, model: "opus" })).not.toBe(
      startPreviewKey({ harness: "claude", credential_override: null, effort: "opus" }),
    );
  });
});

describe("startPreviewView", () => {
  it("joins a default preview with no override cells to the GET defaults", () => {
    const v = startPreviewView(recorded("auto token"), GET);
    expect(v.harness).toBe("claude");
    expect(v.model).toEqual({ text: "opus", short: "opus", source: "lead template" });
    expect(v.effort).toEqual({ text: "medium", short: "medium", source: "built-in" });
    expect(v.credential).toBe("automatic selection");
    expect(compactSummary(v)).toBe("Claude · opus · medium");
    expect(v.fixed).toEqual({ claim: true, override: false });
  });

  it("a create-fixed credential alone is not a choice made here", () => {
    const v = startPreviewView(recorded("default start"), GET);
    expect(v.fixed.override).toBe(false);
    expect(startPreviewView(recorded("enabled pinned token"), GET).fixed.override).toBe(true);
  });

  it("counts an explicit harness choice as fixed on the run now", () => {
    const v = startPreviewView(recorded("claude"), GET);
    expect(v.fixed.override).toBe(true);
    expect(startPreviewView(recorded("auto token"), GET).fixed.override).toBe(false);
  });

  it("uses an explicit model alone and falls back to the GET effort", () => {
    const v = startPreviewView(recorded("claude with a model"), GET);
    expect(v.model).toEqual({ text: "opus", short: "opus", source: "this run" });
    expect(v.effort?.source).toBe("built-in");
    expect(v.fixed.override).toBe(true);
  });

  it("uses an explicit effort alone and falls back to the GET model", () => {
    const v = startPreviewView(recorded("effort only on claude"), GET);
    expect(v.effort).toEqual({ text: "high", short: "high", source: "this run" });
    expect(v.model?.source).toBe("lead template");
  });

  it("lists each alternative for a chosen-at-create harness without inventing one", () => {
    const v = startPreviewView(recorded("default start"), GET);
    expect(v.chosenAtCreate).toBe(true);
    expect(v.harness).toBeNull();
    expect(compactSummary(v)).toBe("Chosen when started");
    expect(v.model).toBeNull();
    expect(v.alternatives.map((a) => a.harness)).toEqual(["claude", "codex"]);
    expect(v.alternatives[0].model?.text).toBe("opus");
    expect(v.alternatives[1].model?.text).toBe("gpt-6-sol");
    expect(v.alternatives[0].effort?.text).toBe("medium");
    expect(v.notes).toEqual(["chosen by the soonest weekly reset"]);
  });

  it("takes an alternative's own effort cell when the request carried one", () => {
    const v = startPreviewView(recorded("effort only without a harness"), GET);
    expect(v.alternatives.map((a) => a.effort?.source)).toEqual(["this run", "this run"]);
  });

  it("shows only the refusal for a refused alternative", () => {
    const p = recorded("default start");
    p.alternatives![1].refusal = "no_credential_for_harness";
    p.alternatives![1].credential = null;
    const v = startPreviewView(p, GET);
    expect(v.alternatives[1]).toEqual({
      harness: "codex",
      refusal: startRefusalLabel("no_credential_for_harness"),
      credential: null,
      model: null,
      effort: null,
    });
    expect(v.alternatives[0].refusal).toBeNull();
  });

  it("strips bidi and zero-width characters from a server model id", () => {
    const get = structuredClone(GET);
    get.models.find((c) => c.setting === "worker_model" && c.harness === "claude")!.resolved_model = "op‮us​";
    const model = startPreviewView(recorded("claude"), get).model;
    expect(model?.text).toBe("opus");
    expect(model?.short).toBe("opus");
  });

  it("shows the Claude SDK default when the resolved model is empty", () => {
    const get = structuredClone(GET);
    get.models.find((c) => c.setting === "worker_model" && c.harness === "claude")!.resolved_model = "";
    expect(startPreviewView(recorded("claude"), get).model?.text).toBe("the SDK decides");
  });

  it("describes a Codex runtime-default effort with the known catalog default", () => {
    const get = structuredClone(GET);
    const cell = get.efforts.find((c) => c.setting === "worker_effort" && c.harness === "codex")!;
    cell.mode = "runtime_default";
    cell.resolved_effort = null;
    const v = startPreviewView(recorded("codex"), get);
    expect(v.effort?.short).toBe("Runtime default");
    expect(v.effort?.text).toContain("catalog default: low (gpt-6-sol, Codex 0.164.0)");
  });

  it("reads a Codex runtime default with an unknown catalog as unknown", () => {
    const get = CODEX_ONLY.get;
    const cell = structuredClone(get);
    const e = cell.efforts.find((c) => c.setting === "worker_effort" && c.harness === "codex")!;
    e.mode = "runtime_default";
    e.resolved_effort = null;
    const v = startPreviewView(recorded("codex", CODEX_ONLY), cell);
    expect(v.effort?.text).toContain("unknown until a Codex run reports it");
  });

  it("describes a Claude runtime-default effort without a catalog sentence", () => {
    const get = structuredClone(GET);
    const e = get.efforts.find((c) => c.setting === "worker_effort" && c.harness === "claude")!;
    e.mode = "runtime_default";
    e.resolved_effort = null;
    expect(startPreviewView(recorded("claude"), get).effort?.text).toBe("Runtime default");
  });

  it("labels sources from the server token and says 'your choice' only for user_default", () => {
    const get = structuredClone(GET);
    const m = get.models.find((c) => c.setting === "worker_model" && c.harness === "claude")!;
    const sources: Array<[string, string]> = [
      ["template", "lead template"],
      ["builtin", "built-in"],
      ["instance_default", "instance default"],
      ["user_default", "your choice"],
    ];
    for (const [token, label] of sources) {
      m.source = token as typeof m.source;
      expect(startPreviewView(recorded("claude"), get).model?.source).toBe(label);
    }
    for (const token of ["template", "builtin", "instance_default"] as const) {
      m.source = token;
      expect(startPreviewView(recorded("claude"), get).model?.source).not.toBe("your choice");
    }
  });

  it("renders a top-level refusal with no model or effort rows", () => {
    const v = startPreviewView(recorded("claude with an incompatible model"), GET);
    expect(v.refusal).toBe(startRefusalLabel("model_incompatible_with_harness"));
    expect(v.credential).toBe("chosen when the run starts");
    expect(v.model).toBeNull();
    expect(v.effort).toBeNull();
    expect(v.alternatives).toEqual([]);
    expect(compactSummary(v)).toBe(v.refusal);
    // The explicit harness choice is still fixed on the run, and a credential fixed at claim shows.
    expect(v.fixed.override).toBe(true);
    const p = recorded("claude with an incompatible model");
    p.credential!.fixed_at = "claim";
    expect(startPreviewView(p, GET).fixed.claim).toBe(true);
  });

  it("keeps the response's harness and credential and reads defaults as unknown when the GET failed", () => {
    const v = startPreviewView(recorded("claude"), null);
    expect(v.harness).toBe("claude");
    expect(v.credential).toBe("chosen when the run starts");
    expect(v.model?.text).toBe("unknown");
    expect(v.effort?.text).toBe("unknown");
    expect(compactSummary(v)).toBe("Claude · unknown · unknown");
  });

  it("keeps response override cells when the GET failed", () => {
    const v = startPreviewView(recorded("model and effort on claude"), null);
    expect(compactSummary(v)).toBe("Claude · opus · xhigh");
  });
});

describe("defaultChoiceLabels", () => {
  it("words the harness Default from the default-choice prediction", () => {
    const decided = recorded("auto token"); // decided claude, source automatic
    const labels = defaultChoiceLabels(GET, decided, recorded("claude"));
    expect(labels.harness).toBe("Default · Claude (automatic)");
  });

  it("keeps the harness Default independent of the selected harness (undecided default)", () => {
    const labels = defaultChoiceLabels(GET, recorded("default start"), recorded("codex"));
    expect(labels.harness).toBe("Default · chosen when started");
    expect(labels.model).toBe("Default · gpt-6-sol (lead template)");
    expect(labels.effort).toBe("Default · medium (built-in)");
  });

  it("keeps the harness Default on Claude when explicit Codex is selected", () => {
    const labels = defaultChoiceLabels(GET, recorded("auto token"), recorded("codex"));
    expect(labels.harness).toBe("Default · Claude (automatic)");
    expect(labels.model).toBe("Default · gpt-6-sol (lead template)");
  });

  it("falls back to the GET start entry when there is no default preview", () => {
    expect(defaultChoiceLabels(GET, null, null).harness).toBe("Default · chosen when started");
    expect(defaultChoiceLabels(CODEX_ONLY.get, null, null).harness).toBe("Default · Codex (automatic)");
  });

  it("describes model and effort Defaults for the harness the selection decided", () => {
    const labels = defaultChoiceLabels(GET, recorded("default start"), recorded("claude"));
    expect(labels.model).toBe("Default · opus (lead template)");
    expect(labels.effort).toBe("Default · medium (built-in)");
  });

  it("says model and effort depend on the harness when the selection is undecided", () => {
    const labels = defaultChoiceLabels(GET, recorded("default start"), recorded("default start"));
    expect(labels.model).toBe("Default · depends on the harness chosen when started");
    expect(labels.effort).toBe("Default · depends on the harness chosen when started");
  });

  it("is plain Default without the GET and never says 'your settings'", () => {
    const labels = defaultChoiceLabels(null, null, null);
    expect(labels).toEqual({ harness: "Default", model: "Default", effort: "Default" });
    const withPreview = defaultChoiceLabels(null, recorded("claude"), recorded("claude"));
    expect(withPreview.model).toBe("Default");
    expect(JSON.stringify(withPreview)).not.toMatch(/your settings/);
  });
});
