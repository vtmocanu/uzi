import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { pathToFileURL } from "node:url";
import { liveArgs, offlineProof, probeError } from "../../agent/test/phase-model-live.js";
import { PhaseCodex, phaseSettings } from "./phase-model-driver.js";
import { assertBinaryVersion, resolveCodexBin } from "./provision.js";
import { MACOS_LINUX_RUNNER_REF } from "./macos-linux-runner.js";

async function main(): Promise<void> {
  const raw = process.argv.slice(2);
  const effortOnly = raw.includes("--effort-check-only");
  if (raw.filter(v => v === "--effort-check-only").length > 1) throw new Error("duplicate effort-check-only flag");
  const args = liveArgs(raw.filter(v => v !== "--effort-check-only"), process.env);
  const startedAt = Date.now();
  if (args.variable !== "CODEX_HOME") throw new Error("Codex probe requires --credential-env CODEX_HOME");
  const home = path.resolve(args.credential);
  const mtime = () => fs.statSync(path.join(home, "auth.json")).mtimeMs;
  const before = mtime(); // Metadata only. Auth is read/refreshed exclusively by the pinned app-server.
  if (process.platform !== "linux") {
    const root = path.resolve(process.cwd(), "..");
    const name = `cdx3246-live-${process.pid}-${randomUUID().slice(0, 8)}`;
    const receiptDir = fs.mkdtempSync(path.join(os.tmpdir(), "cdx3246-container-"));
    const cidfile = path.join(receiptDir, "created.cid");
    try {
      const run = spawnSync("docker", ["run", "--rm", "--init", "--name", name, "--cidfile", cidfile, "--cpus=2",
        "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", `${process.getuid!()}:${process.getgid!()}`,
        "-v", `${root}/agent:/work/agent:ro`, "-v", `${root}/e2e:/work/e2e:ro`,
        "-v", "cdx3246-deps:/work/agent/node_modules:ro", "-v", "cdx3246-codex:/opt/uzi-codex:ro",
        "-v", `${home}:/phase-home:rw`, "--tmpfs", `/phase-work:uid=${process.getuid!()},gid=${process.getgid!()},mode=0700`,
        "--tmpfs", `/work/.uzi/scratch:uid=${process.getuid!()},gid=${process.getgid!()},mode=0700`,
        "-e", "UZI_PHASE_MODEL_LIVE=1", "-e", "CODEX_HOME=/phase-home", "-e", "TMPDIR=/phase-work",
        "-w", "/work/agent", MACOS_LINUX_RUNNER_REF, "node", "--import", "tsx",
        "../e2e/codex-m4/phase-model-live-codex.probe.ts", ...process.argv.slice(2)],
      { encoding: "utf8", timeout: 290_000, maxBuffer: 128 * 1024 });
      // Child output consists only of bounded probe summaries. Never print Docker/app-server stderr.
      if (run.stdout) process.stdout.write(run.stdout);
      if (run.status !== 0) throw new Error("pinned Linux Codex probe failed; see its redacted summary");
    } finally {
      // A name collision or pre-launch failure grants no cleanup authority over an existing container.
      if (fs.existsSync(cidfile)) {
        const created = fs.readFileSync(cidfile, "utf8").trim();
        const actual = spawnSync("docker", ["inspect", "--format", "{{.Id}}", name], { encoding: "utf8", timeout: 2_000 });
        if (/^[0-9a-f]{64}$/.test(created) && actual.status === 0 && actual.stdout.trim() === created)
          spawnSync("docker", ["rm", "-f", name], { stdio: "ignore", timeout: 5_000 });
      }
      fs.rmSync(receiptDir, { recursive: true, force: true });
      console.log(JSON.stringify({ kind: "credential-file-observation", authJsonMtimeChanged: mtime() !== before }));
    }
    return;
  }
  const bin = resolveCodexBin().codexBin;
  assertBinaryVersion(bin, "0.169.0");
  const accounting = await import(pathToFileURL(path.resolve(process.env.CODEX_M4_SRC ?? "src", "codex/token-accounting.ts")).href) as typeof import("../../agent/src/codex/token-accounting.js");
  const accountant = new accounting.CodexUsageAccountant();
  const nonce = `nonce-${randomUUID()}`;
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cdx3246-live-"));
  let warm: PhaseCodex | undefined;
  let cold: PhaseCodex | undefined;
  let stopped = false;
  const check = () => { if (stopped) throw new Error("probe stopped"); };
  const soft = setTimeout(() => stop(), Math.max(1, 295_000 - (Date.now() - startedAt)));
  const hard = setTimeout(() => process.exit(124), Math.max(1, 300_000 - (Date.now() - startedAt)));
  const stop = () => { stopped = true; void warm?.close(); void cold?.close(); };
  process.once("SIGINT", stop); process.once("SIGTERM", stop);
  const groups: Array<{ name: string; thread: string; omission?: null; turns: unknown[]; count: number }> = [];
  let failure: string | undefined;
  try {
    console.log(JSON.stringify({ kind: "preflight", harness: "codex", evidence:
      offlineProof("../e2e/codex-m4/phase-model-switch.test.ts", "PHASE_CODEX_EVIDENCE ") }));
    const active = warm = await PhaseCodex.open(bin, home, root, undefined, undefined, args.modelA);
    check();
    for (const name of effortOnly ? ["omitted-effort", "null-effort"] : ["switch-reclaim-replan", "omitted-effort", "null-effort"]) {
      const started = await active.thread(args.modelA);
      const group = { name, thread: started.thread.id, ...(name === "null-effort" ? { omission: null } : {}), turns: [] as unknown[], count: 0 };
      groups.push(group);
      accountant.registerThread(group.thread, args.modelA, false);
      const drive = async (model: string, effort?: string | null) => {
        check();
        if (++group.count > 4) throw new Error("probe turn budget exceeded");
        const result = await active.turn(group.thread, model, group.count === 1
          ? `Think carefully about 137 times 293 internally, remember ${nonce}, and reply only with that nonce.`
          : "Repeat the remembered nonce exactly.", effort);
        const observation = observe(result, group.thread, model, effort);
        group.turns.push(observation);
        if (!observation.nonceObserved) throw new Error("provider did not complete the nonce control");
      };
      await drive(args.modelA, args.effortA);
      const report = await active.thread(args.modelB, group.thread);
      group.turns.push({ resumeModel: report.model, resumeEffortPresent: Object.hasOwn(report, "reasoningEffort"),
        resumeEffort: Object.hasOwn(report, "reasoningEffort") ? report.reasoningEffort : "absent", role: "warm" });
      await drive(args.modelB, name === "switch-reclaim-replan" ? args.effortB : group.omission);
      await active.thread(args.modelA, group.thread);
      await drive(args.modelA, args.effortA);
    }
    await active.close();
    check();
    cold = await PhaseCodex.open(bin, home, root, undefined, undefined, args.modelA);
    check();
    for (const group of groups) {
      const report = await cold.thread(args.modelB, group.thread);
      accountant.registerThread(group.thread, args.modelB, true);
      group.turns.push({ resumeModel: report.model, resumeEffortPresent: Object.hasOwn(report, "reasoningEffort"),
        resumeEffort: Object.hasOwn(report, "reasoningEffort") ? report.reasoningEffort : "absent", role: "cold" });
      if (++group.count > 4) throw new Error("probe turn budget exceeded");
      const effort = group.name === "switch-reclaim-replan" ? args.effortB : group.omission;
      check();
      const model = group.name === "switch-reclaim-replan" ? undefined : args.modelB;
      const observation = observe(await cold.turn(group.thread, model, "Repeat the remembered nonce exactly.", effort), group.thread, model, effort);
      group.turns.push(observation);
      if (!observation.nonceObserved) throw new Error("provider did not complete the cold nonce control");
    }
  } catch (error) { failure = probeError(error); process.exitCode = 1; }
  finally {
    await cold?.close(); await warm?.close();
    clearTimeout(soft); clearTimeout(hard); process.off("SIGINT", stop); process.off("SIGTERM", stop);
    fs.rmSync(root, { recursive: true, force: true });
    const summary = JSON.stringify({ kind: "phase-model-live", harness: "codex", pin: "0.169.0", scope: effortOnly ? "effort-only" : "full", failure,
      groups: groups.map(({ name, turns, count }) => ({ name, turns, count })),
      usage: accountant.aggregateByModel(), authJsonMtimeChanged: mtime() !== before,
      modelOmission: "not applicable: buildRunRequest uses leadRequestFor/resolveLeadRequest provider fallback",
      acceptanceNote: "prior reasoning acceptance requires nonzero reasoning tokens before the switch; model reports are runtime metadata, not independent provider attestations" });
    if (Buffer.byteLength(summary) > 64 * 1024) { console.error("probe report byte budget exceeded"); process.exitCode = 1; }
    else console.log(summary);
  }
  function observe(result: Awaited<ReturnType<PhaseCodex["turn"]>>, thread: string, model: string | undefined, effort?: string | null) {
    let replay = true;
    const usage: unknown[] = [];
    for (const note of result.notes) {
      if (note.kind === "turn_started") replay = false;
      if (note.kind === "token_usage_updated" && note.threadId === thread) {
        const attributed = accountant.record(thread, note.usage, replay);
        usage.push({ total: note.usage.total, last: note.usage.last, replay,
          pricingEvidenceComplete: note.usage.pricingEvidenceComplete ?? null, attributedModel: attributed?.model });
      }
    }
    return { requestedModel: model ?? "omitted-resume-effectiveness-control", requestedEffort: effort === undefined ? "omitted" : effort,
      status: "completed", settings: phaseSettings(result.notes, thread, result.turnId), usage, nonceObserved: result.text.includes(nonce) };
  }
}
void main().catch(error => { console.error(probeError(error)); process.exitCode = 1; });
