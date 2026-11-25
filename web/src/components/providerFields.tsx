// Shared provider form pieces: the model-map rows, base-URL and auth-style pickers and their
// client-side validation. Used by Settings → Custom providers (Providers.tsx) and by
// Admin → Shared providers (pages/AdminSharedProviders.tsx), so a user-owned and an
// admin-shared definition are described and validated with the same words.
//
// Every string here (URLs, model names) is untrusted and rendered as React text only.

import type { ProviderAuthStyle, ProviderDTO, ProviderModelMap, ProviderModelRole } from "../lib/api";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Field, Input, Select } from "./ui";

type Engine = ProviderDTO["engine"];

const ROLES: { role: ProviderModelRole; label: string; required: boolean }[] = [
  { role: "haiku", label: "Haiku", required: true },
  { role: "sonnet", label: "Sonnet", required: false },
  { role: "opus", label: "Opus", required: false },
  { role: "fable", label: "Fable", required: false },
];

const CODEX_ROLES: typeof ROLES = [
  { role: "default", label: "Default", required: true },
  ...(["gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"] as const).map(role => ({ role, label: role, required: false })),
];
export const rolesFor = (engine: Engine) => engine === "codex" ? CODEX_ROLES : ROLES;

export const AUTH_LABELS: Record<ProviderAuthStyle, string> = {
  bearer: "Authorization: Bearer",
  x_api_key: "x-api-key",
};

const MODEL_RE = /^[A-Za-z0-9._:/@-]{1,256}$/;
const CTX_MIN = 8192;
const CTX_MAX = 10_000_000;

const NO_CONTEXT_WARNING =
  "No context window set. The smallest window configured on any model applies to all of them, and uzi assumes 200k tokens if none is configured; set the gateway model's real window.";

const CATALOG_CONTEXT_WARNING =
  "The Claude CLI may apply its own window to model names it recognizes as Anthropic models (for example claude-sonnet-4-5), ignoring both a configured window and uzi's 200k default (measured at Claude Code 2.1.325).";

interface ModelRow {
  model: string;
  ctx: string;
}
export type ModelRows = Record<ProviderModelRole, ModelRow>;

export const emptyRows = (): ModelRows => Object.fromEntries([...ROLES, ...CODEX_ROLES].map(({ role }) => [role, { model: "", ctx: "" }])) as ModelRows;

export function rowsFromMap(map: ProviderModelMap): ModelRows {
  const rows = emptyRows();
  for (const { role } of [...ROLES, ...CODEX_ROLES]) {
    const e = map[role];
    if (e) rows[role] = { model: e.model, ctx: e.context_window ? String(e.context_window) : "" };
  }
  return rows;
}

// buildModelMap validates the rows the way the server does (the server stays
// authoritative) and returns either the map or the first problem found.
export function buildModelMap(rows: ModelRows, engine: Engine): { map: ProviderModelMap } | { error: string } {
  const out: Partial<Record<ProviderModelRole, { model: string; context_window?: number }>> = {};
  for (const { role, label, required } of rolesFor(engine)) {
    const model = rows[role].model.trim();
    const ctx = rows[role].ctx.trim();
    if (model === "") {
      if (required) return { error: `${label} model is required.` };
      if (ctx !== "") return { error: `${label} has a context window but no model.` };
      continue;
    }
    if (!MODEL_RE.test(model)) {
      return { error: `${label} model must be 1-256 characters from letters, digits and . _ : / @ -` };
    }
    const entry: { model: string; context_window?: number } = { model };
    if (ctx !== "") {
      const n = Number(ctx);
      if (!/^\d+$/.test(ctx) || n < CTX_MIN || n > CTX_MAX) {
        return { error: `${label} context window must be a whole number from ${CTX_MIN} to ${CTX_MAX}.` };
      }
      entry.context_window = n;
    }
    out[role] = entry;
  }
  return { map: out as ProviderModelMap };
}

export function sameModelMap(a: ProviderModelMap, b: ProviderModelMap): boolean {
  return [...ROLES, ...CODEX_ROLES].every(({ role }) => {
    const x = a[role];
    const y = b[role];
    if (!x || !y) return !x && !y;
    return x.model === y.model && (x.context_window ?? null) === (y.context_window ?? null);
  });
}

export function validName(name: string): string | null {
  const n = name.trim();
  if (n.length < 1 || n.length > 64) return "Name must be 1-64 characters.";
  return null;
}

/** The mapped models of a definition, one line each: "Haiku: gw-small (32,768 tokens)". */
export function MappedModels({ engine, map, blankLabel }: { engine: Engine; map: ProviderModelMap; blankLabel: string }) {
  return (
    <ul>
      {rolesFor(engine).filter(({ role }) => map[role]).map(({ role, label }) => {
        const e = map[role]!;
        return (
          <li key={role}>
            {label}: <code>{sanitizeLabel(e.model)}</code>
            {e.context_window ? ` (${e.context_window.toLocaleString()} tokens)` : blankLabel}
          </li>
        );
      })}
    </ul>
  );
}

export function ModelMapFields({
  rows,
  onChange,
  idPrefix,
  engine,
}: {
  rows: ModelRows;
  onChange: (rows: ModelRows) => void;
  idPrefix: string;
  engine: Engine;
}) {
  const set = (role: ProviderModelRole, part: Partial<ModelRow>) =>
    onChange({ ...rows, [role]: { ...rows[role], ...part } });
  return (
    <fieldset className="space-y-3">
      <legend className="text-sm font-medium text-muted">Model map</legend>
      {rolesFor(engine).map(({ role, label, required }) => {
        const row = rows[role];
        return (
          <div key={role} className="space-y-1">
            <div className="grid gap-2 sm:grid-cols-[1fr_12rem]">
              <Input
                id={`${idPrefix}-${role}-model`}
                aria-label={`${label} model${required ? " (required)" : ""}`}
                placeholder={`${label} model${required ? " (required)" : " (optional)"}`}
                autoComplete="off"
                value={row.model}
                onChange={(e) => set(role, { model: e.target.value })}
              />
              <Input
                id={`${idPrefix}-${role}-ctx`}
                aria-label={`${label} context window`}
                placeholder="Context window"
                inputMode="numeric"
                autoComplete="off"
                value={row.ctx}
                onChange={(e) => set(role, { ctx: e.target.value })}
              />
            </div>
            {row.model.trim() !== "" && row.ctx.trim() === "" && (
              <p className="text-xs text-warn" data-testid={`ctx-warning-${role}`}>
                {label}: {engine === "codex" ? "No context window set. Codex uses its runtime fallback context window, which may differ from the gateway limit." : NO_CONTEXT_WARNING}
              </p>
            )}
            {engine === "claude" && row.model.toLowerCase().includes("claude") && (
              <p className="text-xs text-warn" data-testid={`ctx-unenforced-${role}`}>
                {label}: {CATALOG_CONTEXT_WARNING}
              </p>
            )}
          </div>
        );
      })}
    </fieldset>
  );
}

export function BaseUrlSelect({
  value,
  onChange,
  allowed,
  label = "Base URL",
}: {
  value: string;
  onChange: (v: string) => void;
  allowed: string[];
  label?: string;
}) {
  const options = value !== "" && !allowed.includes(value) ? [value, ...allowed] : allowed;
  return (
    <Field label={label}>
      <Select value={value} onChange={(e) => onChange(e.target.value)} aria-label={label}>
        <option value="">Select an allowed base URL…</option>
        {options.map((u) => (
          <option key={u} value={u}>
            {sanitizeLabel(u)}
            {allowed.includes(u) ? "" : " (no longer allowed)"}
          </option>
        ))}
      </Select>
    </Field>
  );
}

export function AuthStyleSelect({
  value,
  onChange,
  engine,
}: {
  engine: Engine;
  value: ProviderAuthStyle;
  onChange: (v: ProviderAuthStyle) => void;
}) {
  return (
    <Field label="Auth style">
      <Select
        aria-label="Auth style"
        value={value}
        onChange={(e) => onChange(e.target.value as ProviderAuthStyle)}
      >
        <option value="bearer">{AUTH_LABELS.bearer}</option>
        {engine === "claude" && <option value="x_api_key">{AUTH_LABELS.x_api_key}</option>}
      </Select>
    </Field>
  );
}
