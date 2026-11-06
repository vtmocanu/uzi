import { CODEX_ROLE_MODELS } from "../../lib/codexModels";
import type { Harness, ModelSettingCell } from "../../lib/apiTypes";

// The mock's summary-model resolver (PRD #3248 §2): a deliberate second implementation
// of the server's agenttmpl.ResolveSummaryModel, pinned to it by the shared cases in
// fixtures/model-settings/summary.json (mockApi.summaryModels.test.ts). Same walk as the
// judge resolver, but the Codex lane accepts any Codex ROLE id (the private advice model
// included), not just the curated set.

// The compiled-in summary built-ins per harness, pinned to summary.json `builtins`.
export const MOCK_SUMMARY_BUILTINS = { claude: "haiku", codex: "gpt-6-luna" } as const;

export interface MockSummaryResolution {
  model: string;
  source: string;
  default_model: string;
  default_source: string;
  dropped: string[];
}

const TIER_SOURCES = ["user_default", "instance_default", "builtin"] as const;

// resolveMockSummaryModel walks user > instance > built-in, trimming each tier; the first
// non-blank value the harness accepts wins. Claude (and any unknown harness) accepts
// anything that is not a Codex role id; Codex accepts only role ids. The default_* pair is
// the same walk without the user tier, and `dropped` lists each rejected non-blank
// candidate of the full walk in tier order.
export function resolveMockSummaryModel(
  harness: string,
  user: string | null | undefined,
  instance: string | null | undefined,
): MockSummaryResolution {
  const codex = harness === "codex";
  const accept = codex
    ? (m: string) => CODEX_ROLE_MODELS.includes(m)
    : (m: string) => !CODEX_ROLE_MODELS.includes(m);
  const tiers = [user ?? "", instance ?? "", codex ? MOCK_SUMMARY_BUILTINS.codex : MOCK_SUMMARY_BUILTINS.claude];
  const walk = (skipUser: boolean) => {
    const dropped: string[] = [];
    for (let i = skipUser ? 1 : 0; i < tiers.length; i++) {
      const v = tiers[i].trim();
      if (v === "") continue;
      if (accept(v)) return { model: v, source: TIER_SOURCES[i], dropped };
      dropped.push(v);
    }
    return { model: "", source: "", dropped };
  };
  const full = walk(false);
  const def = walk(true);
  return { model: full.model, source: full.source, default_model: def.model, default_source: def.source, dropped: full.dropped };
}

// mockSummaryModelCells builds the read-only summary_models cells, ordered claude, codex.
// The instance Codex value is optional until the admin key exists in the mock: an absent
// one is treated as unset.
export function mockSummaryModelCells(
  user: { claude: string | null | undefined; codex: string | null | undefined },
  instance: { claude: string | null | undefined; codex: string | null | undefined },
): ModelSettingCell[] {
  return (["claude", "codex"] as const satisfies readonly Harness[]).map((harness) => {
    const r = resolveMockSummaryModel(harness, user[harness], instance[harness]);
    return {
      harness,
      model: user[harness] ?? null,
      default_model: r.default_model,
      default_source: r.default_source,
      resolved_model: r.model,
      source: r.source,
    };
  });
}
