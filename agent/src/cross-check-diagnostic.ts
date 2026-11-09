/** Fixed worker-authored diagnostics. Provider data never participates in classification. */
const DIAGNOSTICS = Object.freeze({
  provider_error: "Cross-check incomplete: provider reported an error.",
  checker_error_result: "Cross-check incomplete: checker returned an unsuccessful result.",
  transport_ended: "Cross-check incomplete: checker event stream ended without a terminal result.",
  no_complete_verdict: "Cross-check incomplete: terminal result contained no complete verdict.",
  tool_budget_exceeded: "Cross-check incomplete: Codex checker exceeded its tool-call budget.",
  unknown_execution_error: "Cross-check incomplete: execution error; specific cause not recorded.",
});
type DiagnosticCode = keyof typeof DIAGNOSTICS;
const trusted = new WeakMap<object, DiagnosticCode>();

/** Only worker-created errors carry a diagnostic; copying properties or prototypes cannot brand one. */
export function crossCheckExecutionError(code: DiagnosticCode): Error {
  // Internal messages are worker-authored; emitted diagnostics use only DIAGNOSTICS.
  const messages: Record<DiagnosticCode, string> = {
    provider_error: "cross-check model error",
    checker_error_result: "cross-check model error: terminal turn unsuccessful",
    transport_ended: "cross-check ended without a result or terminal turn",
    no_complete_verdict: "cross-check terminal turn has no complete verdict",
    tool_budget_exceeded: "cross-check cumulative tool budget exceeded",
    unknown_execution_error: "cross-check execution error",
  };
  const error = new Error(messages[code]);
  trusted.set(error, code);
  return error;
}

/** Read no properties of an untrusted throw. Passing undefined forces the unknown pair. */
export function crossCheckDiagnostic(error: unknown, stage: "plan" | "code", harness: "claude" | "codex") {
  const code = ((typeof error === "object" && error !== null) || typeof error === "function")
    ? trusted.get(error) ?? "unknown_execution_error" : "unknown_execution_error";
  return Object.freeze({
    event: "cross_check_diagnostic" as const, stage, harness, reason_class: "model_error" as const,
    code, text: DIAGNOSTICS[code],
  });
}
