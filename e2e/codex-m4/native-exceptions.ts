import { parseSync } from "../../agent/test/codex-inventory-parser.js";
import type { ResponsesBody } from "./fake-provider.js";

/** #1566: catalog send_user_message_async is DirectModelOnly; no nested async call is assumed.
 * The schema observed from pinned 0.160.0 requires questions[].title, with optional string options. */
export const ASYNC_QUESTIONS = [{ title: "Which validation should run?", options: ["Focused checks", "Defer"] }];
export const ASYNC_ARGS = { questions: ASYNC_QUESTIONS };
export const EXCEPTIONS_TITLE = "codex P #1566: pinned async questions and UTC reads complete without worker authority";
export const ASYNC_MESSAGE_TITLES: Readonly<Record<string, string>> = {
  "gpt-6.2-sol": "codex P native async message has no worker authority: gpt-6.2-sol",
  "gpt-6.2-astra": "codex P native async message has no worker authority: gpt-6.2-astra",
};
export const INVENTORY_TITLE = "codex #1566 inventory helper includes namespace and nested code-mode tools";

/** Evidence only: callers must reject unparsed evidence and missing observations before
 * comparing structuredNames (qualified namespace paths) and nestedNames (exec members).
 * At most 20,000 inventory nodes, depth 32 and 256 KiB per description are processed.
 * A failure is recorded while independent sibling evidence remains observable. */
export function runtimeInventory(requests: readonly ResponsesBody[]) {
  const names = new Set<string>(), structuredNames = new Set<string>(), nestedNames = new Set<string>();
  const descriptions: string[] = [];
  const unparsed = new Set<string>();
  let nodes = 0, observed = false;

  function declarations(description: string): void {
    // Discover declaration-bearing code broadly; Oxc validates the declaration shape.
    const marker = /\bdeclare\b/g;
    const count = (s: string) => [...s.matchAll(marker)].length;
    const expected = count(description);
    if (!expected) return;
    const fences = [...description.matchAll(/```[^\n]*\n([\s\S]*?)```/g)];
    const sources = fences.length ? fences.map(m => m[1]!).filter(s => count(s) > 0) : [description];
    let consumed = 0;
    for (const source of sources) {
      const result = parseSync("inventory.ts", source);
      if (result.errors.length) { unparsed.add("malformed tools declaration"); continue; }
      for (const statement of result.program.body) {
        if (statement.type !== "VariableDeclaration" || !statement.declare || statement.kind !== "const") {
          unparsed.add("unconsumed declaration content"); continue;
        }
        for (const declaration of statement.declarations) {
          const id = declaration.id;
          const type = id.type === "Identifier" ? id.typeAnnotation?.typeAnnotation : undefined;
          if (id.type !== "Identifier" || id.name !== "tools" || type?.type !== "TSTypeLiteral" || declaration.init) {
            unparsed.add("unsupported tools declaration"); continue;
          }
          consumed++;
          for (const member of type.members) {
            if ((member.type !== "TSMethodSignature" && member.type !== "TSPropertySignature") ||
                member.computed || (member.type === "TSMethodSignature" && member.kind !== "method") ||
                (member.type === "TSPropertySignature" && member.typeAnnotation?.typeAnnotation.type !== "TSFunctionType")) {
              unparsed.add("unconsumed tools member"); continue;
            }
            const key = member.key;
            if (key.type !== "Identifier" && !(key.type === "Literal" && typeof key.value === "string")) {
              unparsed.add("unsupported tools member name"); continue;
            }
            const name = key.type === "Identifier" ? key.name : String(key.value);
            names.add(name); nestedNames.add(name);
          }
        }
      }
    }
    if (consumed !== expected) unparsed.add("unparsed tools declaration");
  }

  function visit(value: unknown, parents: string[] = [], depth = 0): void {
    if (++nodes > 20000 || depth > 32) { unparsed.add("inventory traversal limit"); return; }
    if (Array.isArray(value)) {
      for (const child of value) {
        if (nodes >= 20000) { unparsed.add("inventory traversal limit"); break; }
        visit(child, parents, depth + 1);
      }
      return;
    }
    if (!value || typeof value !== "object") { unparsed.add("malformed inventory entry"); return; }
    const item = value as Record<string, unknown>;
    let path = parents;
    if ("name" in item) {
      if (typeof item.name !== "string" || !item.name.length) unparsed.add("malformed tool name");
      else {
        observed = true; names.add(item.name);
        path = [...parents, item.name]; structuredNames.add(path.join("."));
      }
    }
    if ("type" in item && !["function", "custom", "namespace", "additional_tools"].includes(item.type as string)) unparsed.add("unknown inventory type");
    if ("description" in item) {
      if (typeof item.description !== "string") unparsed.add("malformed description");
      else if (item.description.length > 262144) unparsed.add("description size limit");
      else { descriptions.push(item.description); declarations(item.description); }
    }
    const keys = ["tools", "function", "namespaces", "additional_tools"].filter(key => key in item);
    if (!("name" in item) && keys.length === 0) unparsed.add("unrecognized inventory entry");
    if (item.type === "namespace" && !keys.length) unparsed.add("missing namespace tools");
    for (const key of keys) visit(item[key], path, depth + 1);
  }
  for (const request of requests) {
    for (const key of ["tools", "additional_tools"]) if (key in request) visit(request[key]);
    for (const item of request.input ?? []) {
      if (item && typeof item === "object" && (item as Record<string, unknown>).type === "additional_tools") visit(item);
    }
  }
  return { names: [...names], descriptions: descriptions.join("\n"),
    structuredNames: [...structuredNames], nestedNames: [...nestedNames], observed, unparsed: [...unparsed] };
}
