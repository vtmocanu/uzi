import path from "node:path";
import fs from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { runReadOnlyModelPass } from "./model-pass.js";
import { defaultQueryFn } from "./sdk-messages.js";
import type { SdkQueryFn } from "./sdk-executor.js";
import type { TeardownTestDeps } from "./rmtree.js";
import { makePrDescriptionEvalCodexFactory } from "./codex/codex-executor.js";
import {
  editorRequest, evaluateEditorResponse, evalLog, loadEditorFixtures,
  type EvalOptions,
} from "./pr-description-eval.js";

type PassRequest = Awaited<ReturnType<typeof editorRequest>>;
export interface EvalCliDeps {
  checkout: string;
  env: NodeJS.ProcessEnv;
  write: (row: string) => void;
  signals: {
    on(name: "SIGINT" | "SIGTERM" | "SIGHUP", handler: () => void): unknown;
    removeListener(name: "SIGINT" | "SIGTERM" | "SIGHUP", handler: () => void): unknown;
  };
  timeoutMs?: number;
  queryFn?: SdkQueryFn;
  /** Test-only teardown seam, passed to runReadOnlyModelPass; production keeps pinned Linux cleanup. */
  teardownTestDeps?: TeardownTestDeps;
  codexFactory?: typeof makePrDescriptionEvalCodexFactory;
  pass?: (request: PassRequest, opts: EvalOptions, signal: AbortSignal) => Promise<string>;
}

/** Reject unsafe identities before constructing a pass. Never echo invalid identifiers. */
export function parseEvalArgs(args: string[], env: NodeJS.ProcessEnv): EvalOptions | null {
  if (args.length !== 6) return null;
  const entries = new Map<string, string>();
  for (let i = 0; i < args.length; i += 2) {
    const flag = args[i]!;
    if (!["--harness", "--model", "--prompt-mode"].includes(flag) || entries.has(flag)) return null;
    entries.set(flag, args[i + 1]!);
  }
  const harness = entries.get("--harness");
  const model = entries.get("--model");
  const promptMode = entries.get("--prompt-mode");
  if ((harness !== "claude" && harness !== "codex") ||
      (promptMode !== "baseline" && promptMode !== "revised") || !model ||
      Buffer.byteLength(model) > 100 || /[\s\p{Cc}\p{Cf}\uFFFD]/u.test(model) ||
      !/^[\p{L}\p{N}][\p{L}\p{N}._:/-]*$/u.test(model) ||
      /(?:sk-|gh[pousr]_|github_pat_|glpat-|xox[baprs]-)/i.test(model)) return null;
  // Exact caller credentials are excluded even when short and not provider-shaped.
  for (const key of Object.keys(env)) {
    if (/(?:TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL)/i.test(key) && env[key] && model.includes(env[key]!)) return null;
  }
  return { harness, model, promptMode };
}

/** Actual production passes. The query wrapper connects external cancellation to the existing
 * SDK abort controller; runReadOnlyModelPass still owns HOME and its unchanged cleanup.
 */
async function runEditorPass(request: PassRequest, opts: EvalOptions, signal: AbortSignal, deps: EvalCliDeps): Promise<string> {
  signal.throwIfAborted();
  const timeoutMs = deps.timeoutMs ?? 60_000;
  if (opts.harness === "codex") {
    const owner = (deps.codexFactory ?? makePrDescriptionEvalCodexFactory)({
      checkoutPath: deps.checkout, apiKey: deps.env.OPENAI_API_KEY!, signal,
    });
    try {
      const result = await owner.run({
        ...request, label: "summary", output: { kind: "text" }, signal, timeoutMs,
      }, { onTerminal(_terminal, { isError }) { if (isError) throw new Error("provider_failed"); } });
      return result.text;
    } finally { await owner.close(); }
  }
  const queryFn: SdkQueryFn = async function* (params) {
    const abort = (): void => { params.options.abortController?.abort(); };
    signal.addEventListener("abort", abort, { once: true });
    try {
      signal.throwIfAborted();
      yield* (deps.queryFn ?? defaultQueryFn)(params);
    } finally { signal.removeEventListener("abort", abort); }
  };
  return runReadOnlyModelPass({
    ...request, token: deps.env.CLAUDE_CODE_OAUTH_TOKEN, homeRoot: path.join(deps.checkout, ".uzi/scratch"),
    homePrefix: "uzi-summary-eval-", label: "summary", timeoutMs, queryFn,
    denyReason: "the summary runner is read-only and runs no tools", log: evalLog,
    teardownTestDeps: deps.teardownTestDeps,
  });
}

/** Five serial passes, one attempt each. One fixture failure does not block siblings.
 * Signal handlers precede startup and remain installed until pass cleanup has settled.
 */
export async function runEvalCli(args: string[], deps: EvalCliDeps): Promise<number> {
  const opts = parseEvalArgs(args, deps.env);
  if (!opts) { deps.write(JSON.stringify({ error: "invalid_cli" })); return 1; }
  const abort = new AbortController();
  const onSignal = (): void => { abort.abort(); };
  const signals = ["SIGINT", "SIGTERM", "SIGHUP"] as const;
  for (const name of signals) deps.signals.on(name, onSignal);
  let failed = false;
  try {
    await fs.mkdir(path.join(deps.checkout, ".uzi/scratch"), { recursive: true });
    const fixtures = await loadEditorFixtures(deps.checkout);
    const auth = opts.harness === "claude" ? deps.env.CLAUDE_CODE_OAUTH_TOKEN : deps.env.OPENAI_API_KEY;
    for (const fixture of fixtures) {
      const identity = { fixture_id: fixture.id, harness: opts.harness,
        named_editor_model: opts.model, model_identity: "requested_alias", prompt_mode: opts.promptMode };
      let result = { emitted: false, parsed: false, would_publish: false, error: null as string | null };
      let timer: NodeJS.Timeout | undefined;
      const passAbort = new AbortController();
      let timedOut = false;
      try {
        if (!auth) result.error = "missing_auth";
        else if (abort.signal.aborted) result.error = "interrupted";
        else {
          const request = await editorRequest(deps.checkout, fixture, opts);
          timer = setTimeout(() => { timedOut = true; passAbort.abort(); }, deps.timeoutMs ?? 60_000);
          const signal = AbortSignal.any([abort.signal, passAbort.signal]);
          const response = await (deps.pass
            ? deps.pass(request, opts, signal)
            : runEditorPass(request, opts, signal, deps));
          clearTimeout(timer);
          timer = undefined;
          if (signal.aborted) result.error = abort.signal.aborted ? "interrupted" : "timeout";
          else result = await evaluateEditorResponse(deps.checkout, fixture, response);
        }
      } catch {
        result.error = abort.signal.aborted ? "interrupted" : timedOut ? "timeout" : "provider_failed";
      } finally { if (timer) clearTimeout(timer); }
      failed ||= result.error !== null && result.error !== "editor_omission";
      deps.write(JSON.stringify({ ...identity, ...result }));
    }
    return failed ? 1 : 0;
  } catch {
    deps.write(JSON.stringify({ error: "startup_failed" }));
    return 1;
  } finally {
    abort.abort();
    for (const name of signals) deps.signals.removeListener(name, onSignal);
  }
}

const checkout = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  runEvalCli(process.argv.slice(2), {
    checkout, env: process.env, signals: process, write: (row) => { process.stdout.write(row + "\n"); },
  }).then((code) => { process.exitCode = code; }).catch(() => {
    process.stdout.write(JSON.stringify({ error: "startup_failed" }) + "\n");
    process.exitCode = 1;
  });
}
