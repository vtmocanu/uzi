import { randomUUID } from "node:crypto";
import { PhaseClaude, CLAUDE_PHASE_SDK } from "./phase-model-claude.js";
import { liveArgs, offlineProof, probeError } from "./phase-model-live.js";

async function main(): Promise<void> {
  const args = liveArgs(process.argv.slice(2), process.env);
  if (args.variable !== "CLAUDE_CODE_OAUTH_TOKEN") throw new Error("Claude probe requires --credential-env CLAUDE_CODE_OAUTH_TOKEN");
  const nonce = `nonce-${randomUUID()}`;
  const runtime = new PhaseClaude({ CLAUDE_CODE_OAUTH_TOKEN: args.credential });
  const runtimes = [runtime];
  const models = new Map<string, string>();
  const groups: Array<{ name: string; turns: unknown[] }> = [];
  let failure: string | undefined;
  let stopped = false;
  const soft = setTimeout(() => onSignal(), 295_000);
  const hard = setTimeout(() => process.exit(124), 300_000);
  const onSignal = () => { stopped = true; for (const r of runtimes) void r.close(); };
  process.once("SIGINT", onSignal); process.once("SIGTERM", onSignal);
  try {
    const barrier = offlineProof("test/phase-model-switch.integration.test.ts", "PHASE_CLAUDE_EVIDENCE ");
    console.log(JSON.stringify({ kind: "preflight", harness: "claude", barrier }));
    for (const name of ["switch-reclaim-replan", "omitted-model", "omitted-effort"]) {
      const driver = name === "switch-reclaim-replan" ? runtime : new PhaseClaude(runtime.providerEnv);
      if (driver !== runtime) runtimes.push(driver);
      const turns: Awaited<ReturnType<PhaseClaude["turn"]>>[] = [];
      const group = { name, turns: [] as unknown[] };
      groups.push(group);
      const drive = async (model?: string, effort?: "high" | "medium", fresh = false) => {
        if (stopped) throw new Error("probe stopped");
        if (turns.length >= 4) throw new Error("probe turn budget exceeded");
        const prior = fresh ? undefined : turns.at(-1);
        const turn = await driver.turn({ ...(model ? { model } : {}), ...(effort ? { effort } : {}),
          ...(prior ? { resume: prior.session } : {}),
          prompt: prior ? "Repeat the remembered nonce exactly."
            : `Think carefully about 137 times 293 internally, remember ${nonce}, then reply only with that nonce. Use no tools.` });
        turns.push(turn);
        if (model) models.set(model, turn.model);
        group.turns.push({ requestedModel: model ?? "omitted", requestedEffort: effort ?? "omitted",
          model: turn.model, effort: turn.effort ?? "unreported", usage: turn.usage, thinkingBlocks: turn.thinkingBlocks, cli: turn.cli, resumed: prior !== undefined,
          nonceObserved: turn.text.includes(nonce), failure: turn.failure ? probeError(turn.failure, args.credential) : undefined });
        if (turn.failure) throw new Error(turn.failure);
        if (prior && turn.session !== prior.session) throw new Error("runtime did not resume the same conversation");
        if (!turn.text.includes(nonce)) throw new Error("provider did not complete the nonce control");
      };
      if (name === "omitted-model") {
        await drive(undefined, args.effortB, true);
        const defaultModel = turns[0]!.model;
        const explicit = [args.modelA, args.modelB].find(model => models.has(model) && models.get(model) !== defaultModel);
        if (!explicit) throw new Error("model omission control cannot distinguish explicit model from runtime default");
        await drive(explicit, args.effortA, true);
        await drive(undefined, args.effortB);
        await drive(undefined, args.effortB, true);
        if (turns.at(-1)!.model !== defaultModel) throw new Error("explicit query changed the fresh-session default; omission result is ambiguous");
      } else {
        await drive(args.modelA, args.effortA);
        if (name === "switch-reclaim-replan") { await drive(args.modelB, args.effortB); await drive(args.modelA, args.effortA); }
        else await drive(args.modelA);
      }
    }
  } catch (error) { failure = probeError(error, args.credential); process.exitCode = 1; }
  finally {
    for (const r of runtimes) await r.close();
    clearTimeout(soft); clearTimeout(hard);
    process.off("SIGINT", onSignal); process.off("SIGTERM", onSignal);
    const summary = JSON.stringify({ kind: "phase-model-live", harness: "claude", sdk: CLAUDE_PHASE_SDK,
      groups, usageNote: "modelUsage is cumulative per session; do not sum repeated snapshots", failure,
      acceptanceNote: "provider acceptance of prior thinking is established only when turn 1 thinkingBlocks is nonzero" });
    if (Buffer.byteLength(summary) > 64 * 1024) { console.error("probe report byte budget exceeded"); process.exitCode = 1; }
    else console.log(summary);
  }
}
void main().catch(error => { console.error(probeError(error)); process.exitCode = 1; });
