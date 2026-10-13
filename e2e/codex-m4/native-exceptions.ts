import type { ResponsesBody } from "./fake-provider.js";

/** #1566: catalog send_user_message_async is DirectModelOnly; no nested async call is assumed.
 * The schema observed from pinned 0.160.0 requires questions[].title, with optional string options. */
export const ASYNC_QUESTIONS = [{ title: "Which validation should run?", options: ["Focused checks", "Defer"] }];
export const ASYNC_ARGS = { questions: ASYNC_QUESTIONS };
export const EXCEPTIONS_TITLE = "codex P #1566: pinned async questions and UTC reads complete without worker authority";
export const INVENTORY_TITLE = "codex #1566 inventory helper includes namespace and nested code-mode tools";

/** Read the actual namespace inventory (additional_tools is in Responses input), including
 * nested exec declarations; the historical CODEX_NATIVE_TOOL_NAMES list is not exhaustive. */
export function runtimeInventory(requests: readonly ResponsesBody[]): { names: string[]; descriptions: string } {
  const names: string[] = [];
  const descriptions: string[] = [];
  function visit(value: unknown): void {
    if (Array.isArray(value)) { for (const item of value) visit(item); return; }
    if (value === null || typeof value !== "object") return;
    const item = value as Record<string, unknown>;
    if (typeof item.name === "string") names.push(item.name);
    if (typeof item.description === "string") descriptions.push(item.description);
    if (item.tools !== undefined) visit(item.tools);
  }
  for (const request of requests) {
    visit(request.tools);
    for (const item of request.input ?? []) {
      if (item !== null && typeof item === "object" && (item as Record<string, unknown>).type === "additional_tools") visit(item);
    }
  }
  return { names: [...new Set(names)], descriptions: descriptions.join("\n") };
}
