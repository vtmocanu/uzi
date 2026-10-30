// PRD #3244 section 8: iterate the shared contract fixture in full (every display case and the
// label table), the same file the Go runpresentation test reads.
import { describe, expect, it } from "vitest";
import contract from "../../../fixtures/lead-config/contract.json";
import type { Run } from "./apiTypes";
import { evidenceLabel, leadConfigView, leadConfigViewInput, sourceLabel, type LeadConfigViewInput } from "./leadConfig";

describe("lead-config labels (fixtures/lead-config/contract.json)", () => {
  const labels = contract.labels;
  const harnesses: [string, string][] = [
    ["claude", "Claude"],
    ["codex", "Codex"],
  ];

  it("model labels match for every source and harness", () => {
    const visited: string[] = [];
    for (const [src, tmpl] of Object.entries(labels.model)) {
      for (const [h, name] of harnesses) {
        expect(sourceLabel("model", src, h), `${src}/${h}`).toBe(tmpl.replace("{H}", name));
      }
      visited.push(src);
    }
    expect(visited.sort()).toEqual([...contract.vocabulary.model_sources].sort());
  });

  it("Claude with no model sent reads the SDK default label", () => {
    expect(sourceLabel("model", "builtin", "claude", { sdkDefault: true })).toBe(labels.model_sdk_default);
  });

  it("effort labels match for every source and harness", () => {
    const visited: string[] = [];
    for (const [src, tmpl] of Object.entries(labels.effort)) {
      for (const [h, name] of harnesses) {
        expect(sourceLabel("effort", src, h), `${src}/${h}`).toBe(tmpl.replace("{H}", name));
      }
      visited.push(src);
    }
    expect(visited.sort()).toEqual(["schedule_pin", ...contract.vocabulary.effort_sources].sort());
  });

  it("evidence labels match, and a missing observation is 'not reported yet'", () => {
    const visited: string[] = [];
    for (const [ev, label] of Object.entries(labels.evidence)) {
      expect(evidenceLabel(ev), ev).toBe(label);
      visited.push(ev);
    }
    const vocab = new Set([...contract.vocabulary.model_evidence, ...contract.vocabulary.effort_evidence]);
    expect(visited.sort()).toEqual([...vocab].sort());
    expect(evidenceLabel(null)).toBe(labels.not_observed);
  });

  it("renders the fixed strings the display rules use", () => {
    const view = leadConfigView(otherLevelInput());
    expect(view.lines).toContain(`effort: ${labels.observed_effort_other} (${labels.evidence.codex_runtime_default})`);
    expect(labels.effort_modes.runtime_default).toBe("runtime default");
    expect(labels.effort_not_sent).toBe("not sent");
  });
});

function otherLevelInput(): LeadConfigViewInput {
  return {
    status: "running",
    harness: "codex",
    preClaim: { modelSource: "schedule_pin" },
    leadConfig: {
      applicable: true,
      claim_generation: 1,
      current: true,
      phases: [
        {
          phase: "implement",
          phase_entry: 1,
          seq: 1,
          harness: "codex",
          requested_model: "gpt-6-astra",
          requested_model_source: "user_default",
          requested_model_fixed_at: "claim",
          requested_effort: null,
          requested_effort_mode: "runtime_default",
          requested_effort_source: "builtin",
          requested_effort_fixed_at: "claim",
          observed_model: null,
          observed_model_evidence: null,
          observed_effort: "other",
          observed_effort_evidence: "codex_runtime_default",
          truncated: false,
          reported_at: "2026-10-29T10:00:00Z",
        },
      ],
    },
  };
}

describe("leadConfigView display_cases (fixtures/lead-config/contract.json)", () => {
  it("iterates every display case, each id visited", () => {
    const visited: string[] = [];
    for (const c of contract.display_cases) {
      const got = leadConfigView(c.input as unknown as LeadConfigViewInput);
      expect(got, c.id).toEqual(c.expect);
      visited.push(c.id);
    }
    expect(visited).toEqual(contract.display_cases.map((c) => c.id));
    expect(new Set(visited).size).toBe(visited.length);
  });
});

describe("leadConfigView observed-side branches (rule 4, M2 cases not yet in the fixture)", () => {
  function withObserved(over: Record<string, unknown>): LeadConfigViewInput {
    const base = otherLevelInput();
    const row = { ...base.leadConfig!.phases[0], requested_effort: "xhigh", requested_effort_mode: "explicit" as const, ...over };
    return { ...base, leadConfig: { ...base.leadConfig!, phases: [row as never] } };
  }

  it.each(["claude_init", "claude_get_settings", "codex_runtime_default"])(
    "renders 'high (requested xhigh)' for a differing %s report",
    (evidence) => {
      const v = leadConfigView(withObserved({ observed_effort: "high", observed_effort_evidence: evidence }));
      expect(v.chip).toBe("lead: gpt-6-astra · high (requested xhigh)");
    },
  );

  it("keeps the requested level when the evidence is sent-by-us (codex_sent)", () => {
    const v = leadConfigView(withObserved({ observed_effort: "high", observed_effort_evidence: "codex_sent" }));
    expect(v.chip).toBe("lead: gpt-6-astra · xhigh");
  });

  it("keeps the requested level when the observation matches", () => {
    const v = leadConfigView(withObserved({ observed_effort: "xhigh", observed_effort_evidence: "claude_init" }));
    expect(v.chip).toBe("lead: gpt-6-astra · xhigh");
  });

  it("renders 'runtime default: <observed>' and never an 'other' level", () => {
    const seen = leadConfigView({
      ...otherLevelInput(),
      leadConfig: {
        ...otherLevelInput().leadConfig!,
        phases: [{ ...otherLevelInput().leadConfig!.phases[0], observed_effort: "high" }],
      },
    });
    expect(seen.chip).toBe("lead: gpt-6-astra · runtime default: high");
    const other = leadConfigView(otherLevelInput());
    expect(other.chip).toBe("lead: gpt-6-astra · runtime default");
    expect(other.chip).not.toContain("other");
  });

  it("renders 'not sent' for a Claude run whose init carried a null effort", () => {
    const v = leadConfigView(
      withObserved({ observed_effort: null, observed_effort_evidence: "claude_init", harness: "claude" }),
    );
    expect(v.lines).toContain("effort: not sent");
  });

  it("uses the observed model when the request had none and was built-in", () => {
    const v = leadConfigView(
      withObserved({
        requested_model: null,
        requested_model_source: "builtin",
        observed_model: "claude-opus-5",
        observed_model_evidence: "claude_init",
        observed_effort: null,
      }),
    );
    expect(v.chip.startsWith("lead: claude-opus-5 · ")).toBe(true);
  });

  it("strips format characters from model strings", () => {
    const v = leadConfigView(withObserved({ requested_model: `cl${String.fromCodePoint(0x202e)}aude` }));
    expect(v.chip).toContain("claude");
    expect(v.chip + v.lines.join("")).not.toMatch(/[\p{Cf}]/u);
  });
});

describe("leadConfigViewInput", () => {
  it("maps run.model to the schedule-pin pre-claim model and passes lead_config through", () => {
    const run = { status: "queued", harness: "codex", model: "gpt-6-astra", lead_config: undefined } as unknown as Run;
    expect(leadConfigViewInput(run)).toEqual({
      status: "queued",
      harness: "codex",
      preClaim: { model: "gpt-6-astra", modelSource: "schedule_pin" },
      leadConfig: undefined,
    });
  });
});
