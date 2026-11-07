// @vitest-environment jsdom
//
// PRD #3254 M2: the demo serves the server's RECORDED effective-settings behavior
// (fixtures/api-contract/effective_settings.behavior.json), per mock scenario.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import behavior from "../../../fixtures/api-contract/effective_settings.behavior.json";
import { MODEL_SETTING_SOURCE_LABELS } from "../lib/modelSettings";
import { RECORDED_EFFECTIVE_SCENARIOS } from "./data/effectiveSettings";
import { mockApi } from "./mockApi";

function installStorage(): void {
  const m = new Map<string, string>();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage,
  });
}

function setScenario(name: string): void {
  window.localStorage.setItem("uzi_mock_scenario", name);
}

beforeEach(installStorage);
afterEach(() => window.localStorage.clear());

describe("recorded effective-settings corpus", () => {
  it("the mock data is a verbatim copy of the Go-recorded corpus", () => {
    expect(RECORDED_EFFECTIVE_SCENARIOS).toEqual(behavior.scenarios);
    expect(RECORDED_EFFECTIVE_SCENARIOS.map((s) => s.name)).toEqual(["default", "codexOnly", "overflowCodexDefault"]);
  });

  it("every harness, credential and note token it serves has a settings-surface label", () => {
    const labelled = new Set(MODEL_SETTING_SOURCE_LABELS.map((r) => `${r.field}/${r.token}`));
    const missing: string[] = [];
    for (const { get, start_previews } of RECORDED_EFFECTIVE_SCENARIOS) {
      const harnesses = [...get.start.map((c) => c.harness), ...start_previews.map((p) => p.response.harness)];
      for (const h of harnesses) {
        if (h) {
          if (!labelled.has(`harness/${h.source}`)) missing.push(`harness/${h.source}`);
          for (const n of h.notes) if (!labelled.has(`note/${n}`)) missing.push(`note/${n}`);
        }
      }
      const credentials = [
        ...get.harnesses.map((h) => h.credential),
        ...start_previews.flatMap((p) => [p.response.credential, ...(p.response.alternatives ?? []).map((a) => a.credential)]),
      ];
      for (const c of credentials) if (c && !labelled.has(`credential/${c.source}`)) missing.push(`credential/${c.source}`);
    }
    expect(missing).toEqual([]);
  });
});

describe("getEffectiveSettings", () => {
  it("serves the default recording: both harnesses usable, 8 model cells, 4 effort cells", async () => {
    const got = await mockApi.getEffectiveSettings();
    expect(got).toEqual(behavior.scenarios[0].get);
    expect(got.harnesses.map((h) => [h.harness, h.usable])).toEqual([["claude", true], ["codex", true]]);
    expect(got.models).toHaveLength(8);
    expect(got.efforts).toHaveLength(4);
    expect(got.cross_check.models).toHaveLength(4);
    expect(got.cross_check.efforts).toHaveLength(4);
  });

  it("serves the codexOnly recording under the codex-only scenario", async () => {
    setScenario("codex-only");
    const got = await mockApi.getEffectiveSettings();
    expect(got).toEqual(behavior.scenarios[1].get);
    expect(got.harnesses[0]).toMatchObject({ harness: "claude", usable: false, refusal: "no_credential_for_harness" });
    expect(got.start.find((c) => c.choice === "claude")).toMatchObject({ harness: null, refusal: "no_credential_for_harness" });
  });

  it("returns a copy, so a caller cannot change the recording", async () => {
    const first = await mockApi.getEffectiveSettings();
    first.harnesses.length = 0;
    expect((await mockApi.getEffectiveSettings()).harnesses).toHaveLength(2);
  });
});

describe("previewStartSettings", () => {
  it("answers every recorded request with its recorded response, in each scenario", async () => {
    const scenarios: Record<string, string> = { default: "", codexOnly: "codex-only", overflowCodexDefault: "overflow-codex-default" };
    for (const s of behavior.scenarios) {
      setScenario(scenarios[s.name]);
      for (const p of s.start_previews) {
        expect(await mockApi.previewStartSettings(p.request), `${s.name}: ${p.name}`).toEqual(p.response);
      }
    }
  });

  it("returns the recorded refused alternative: Codex refused while Claude is valid", async () => {
    setScenario("overflow-codex-default");
    const got = await mockApi.previewStartSettings({ harness: null, credential_override: { mode: "inherit" } });
    expect(got.refusal).toBeNull();
    expect(got.credential).toBeNull();
    expect(got.alternatives).toEqual([
      { harness: "claude", credential: { kind: "anthropic", source: "claim_selection", fixed_at: "claim", auth_mode: null }, refusal: null, refusal_detail: null },
      { harness: "codex", credential: null, refusal: "credential_override_harness_unsupported", refusal_detail: null },
    ]);
  });

  it("treats an absent secret_id and a missing override like the recorded null", async () => {
    const got = await mockApi.previewStartSettings({ harness: "claude", credential_override: null });
    expect(got.harness).toMatchObject({ value: "claude", source: "run_override" });
  });

  it("rejects a request the corpus does not record instead of inventing an answer", async () => {
    await expect(mockApi.previewStartSettings({ harness: "codex", credential_override: { mode: "default" } })).rejects.toMatchObject({ status: 400 });
  });
});
