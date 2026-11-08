// A deliberately small JSON Schema checker shared by the structured-output tests. It supports
// exactly SUPPORTED_KEYWORDS; claude-structured-output.test.ts proves both verdict schemas use
// nothing else, so a true result from `check` is faithful.

export const SUPPORTED_KEYWORDS: ReadonlySet<string> = new Set([
  "type", "enum", "required", "properties", "additionalProperties", "items",
  "maxItems", "maxLength", "pattern", "minimum", "maximum", "description",
]);

export type Schema = { readonly [key: string]: unknown };

export function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

export function check(schema: Schema, value: unknown): boolean {
  const type = schema["type"];
  if (type === "object" && !isRecord(value)) return false;
  if (type === "array" && !Array.isArray(value)) return false;
  if (type === "string" && typeof value !== "string") return false;
  if (type === "integer" && !(typeof value === "number" && Number.isInteger(value))) return false;
  const enumValues = schema["enum"];
  if (Array.isArray(enumValues) && !enumValues.includes(value)) return false;
  if (isRecord(value)) {
    const properties = (schema["properties"] ?? {}) as Record<string, Schema>;
    for (const key of (schema["required"] ?? []) as string[]) if (!Object.hasOwn(value, key)) return false;
    for (const [key, child] of Object.entries(value)) {
      if (Object.hasOwn(properties, key)) {
        if (!check(properties[key] as Schema, child)) return false;
      } else if (schema["additionalProperties"] === false) return false;
    }
  }
  if (Array.isArray(value)) {
    if (typeof schema["maxItems"] === "number" && value.length > schema["maxItems"]) return false;
    const items = schema["items"];
    if (isRecord(items) && !value.every((v) => check(items, v))) return false;
  }
  if (typeof value === "string") {
    if (typeof schema["maxLength"] === "number" && [...value].length > schema["maxLength"]) return false;
    if (typeof schema["pattern"] === "string" && !new RegExp(schema["pattern"]).test(value)) return false;
  }
  if (typeof value === "number") {
    if (typeof schema["minimum"] === "number" && value < schema["minimum"]) return false;
    if (typeof schema["maximum"] === "number" && value > schema["maximum"]) return false;
  }
  return true;
}
