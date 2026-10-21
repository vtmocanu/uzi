import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../../lib/codexModels";
import type { Harness, ModelSettingCell } from "../../lib/apiTypes";

// The mock's judge-model resolver (PRD #3245 §2): a deliberate second implementation of
// the server's agenttmpl.ResolveJudgeModel, pinned to it by the shared cases in
// fixtures/model-settings/judge.json (mockApi.judgeModels.test.ts). One function serves
// the judge_models cells and the session's effective_judge_model, so the two never drift.

// The compiled-in judge built-ins per harness, pinned to judge.json `builtins`.
export const MOCK_JUDGE_BUILTINS = { claude: "opus", codex: "gpt-6.1-sol" } as const;

export interface MockJudgeResolution {
  model: string;
  source: string;
  default_model: string;
  default_source: string;
  dropped: string[];
}

const TIER_SOURCES = ["user_default", "instance_default", "builtin"] as const;

// resolveMockJudgeModel walks user > instance > built-in, trimming each tier; the first
// non-blank value the harness accepts wins. Claude (and any unknown harness) accepts
// anything that is not a Codex role id; Codex accepts only curated ids. The default_*
// pair is the same walk without the user tier ("what Default means to this user"), and
// `dropped` lists each rejected non-blank candidate of the full walk in tier order.
export function resolveMockJudgeModel(
  harness: string,
  user: string | null | undefined,
  instance: string | null | undefined,
): MockJudgeResolution {
  const codex = harness === "codex";
  const accept = codex
    ? (m: string) => CODEX_CURATED_MODELS.includes(m)
    : (m: string) => !CODEX_ROLE_MODELS.includes(m);
  const tiers = [user ?? "", instance ?? "", codex ? MOCK_JUDGE_BUILTINS.codex : MOCK_JUDGE_BUILTINS.claude];
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

// mockJudgeModelCells builds the read-only judge_models cells, ordered claude, codex.
export function mockJudgeModelCells(
  user: { claude: string | null | undefined; codex: string | null | undefined },
  instance: { claude: string; codex: string },
): ModelSettingCell[] {
  return (["claude", "codex"] as const satisfies readonly Harness[]).map((harness) => {
    const r = resolveMockJudgeModel(harness, user[harness], instance[harness]);
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
