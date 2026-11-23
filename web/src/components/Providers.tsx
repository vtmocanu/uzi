// Settings → Custom providers (PRD #4320 M3). A provider is an Anthropic-Messages
// compatible gateway the user can point Claude runs at: a base URL chosen from the
// operator's allowlist, an auth style, a model map and a write-only API key.
//
// Every string here (names, URLs, model names) is untrusted and rendered as React
// text only. The key is never displayed or returned by the API; setting or rotating
// it is a re-paste into a password field that is cleared on save.

import { useCallback, useEffect, useId, useState, type FormEvent } from "react";
import {
  api,
  type ProviderAuthStyle,
  type ProviderDTO,
  type ProviderModelMap,
  type ProviderModelRole,
  type ProviderProbe,
  type ProvidersList,
} from "../lib/api";
import { ApiError, errorMessage } from "../lib/apiError";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Alert, Badge, Button, Card, Field, Input, SectionTitle, Select, Skeleton } from "./ui";

const ROLES: { role: ProviderModelRole; label: string; required: boolean }[] = [
  { role: "haiku", label: "Haiku", required: true },
  { role: "sonnet", label: "Sonnet", required: false },
  { role: "opus", label: "Opus", required: false },
  { role: "fable", label: "Fable", required: false },
];

const AUTH_LABELS: Record<ProviderAuthStyle, string> = {
  bearer: "Authorization: Bearer",
  x_api_key: "x-api-key",
};

const MODEL_RE = /^[A-Za-z0-9._:/@-]{1,256}$/;
const KEY_RE = /^[\x21-\x7E]{16,4096}$/;
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
type ModelRows = Record<ProviderModelRole, ModelRow>;

const emptyRows = (): ModelRows => ({
  haiku: { model: "", ctx: "" },
  sonnet: { model: "", ctx: "" },
  opus: { model: "", ctx: "" },
  fable: { model: "", ctx: "" },
});

function rowsFromMap(map: ProviderModelMap): ModelRows {
  const rows = emptyRows();
  for (const { role } of ROLES) {
    const e = map[role];
    if (e) rows[role] = { model: e.model, ctx: e.context_window ? String(e.context_window) : "" };
  }
  return rows;
}

// buildModelMap validates the rows the way the server does (the server stays
// authoritative) and returns either the map or the first problem found.
function buildModelMap(rows: ModelRows): { map: ProviderModelMap } | { error: string } {
  const out: Partial<Record<ProviderModelRole, { model: string; context_window?: number }>> = {};
  for (const { role, label, required } of ROLES) {
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

function sameModelMap(a: ProviderModelMap, b: ProviderModelMap): boolean {
  return ROLES.every(({ role }) => {
    const x = a[role];
    const y = b[role];
    if (!x || !y) return !x && !y;
    return x.model === y.model && (x.context_window ?? null) === (y.context_window ?? null);
  });
}

function validName(name: string): string | null {
  const n = name.trim();
  if (n.length < 1 || n.length > 64) return "Name must be 1-64 characters.";
  return null;
}

const REASON_MESSAGES: Record<string, string> = {
  provider_base_url_not_allowed: "That base URL is not on the operator's allowlist. Pick one from the list.",
  provider_in_use_as_source:
    "This provider is still in use as a model source. Switch what uses it to another source, then delete it.",
  provider_limit_reached: "You have reached the maximum number of providers. Delete one first.",
  provider_retired: "This provider is retired and can no longer be changed.",
  provider_name_taken: "You already have a provider with that name.",
  provider_probe_rate_limited: "Tests are rate limited. Wait a moment, then test again.",
  provider_source_unusable: "This provider cannot be your Claude source right now. Check that it is not retired and has an enabled key.",
};

// Why a provider cannot be selected as the Claude source, or null when it can. Mirrors the
// server's usability rules (the server stays authoritative and answers 409
// provider_source_unusable); the feature-off case never reaches a row.
function sourceBlockedReason(p: ProviderDTO): string | null {
  if (p.retired) return "A retired provider cannot be your Claude source.";
  if (!p.has_key) return "Set a key before using this provider.";
  if (!p.key_enabled) return "Enable the key before using this provider.";
  return null;
}

function providerError(err: unknown, fallback: string): string {
  if (err instanceof ApiError) {
    const reason = (err.body as { reason?: string } | null)?.reason;
    if (reason && REASON_MESSAGES[reason]) return REASON_MESSAGES[reason];
    if (err.status === 429) return REASON_MESSAGES.provider_probe_rate_limited;
  }
  return errorMessage(err, fallback);
}

const AUTH_RESULT_TEXT: Record<ProviderProbe["auth_result"], string> = {
  ok: "Authentication worked",
  rejected: "Authentication rejected by the gateway",
  inconclusive: "Authentication check inconclusive",
};
const TOOL_RESULT_TEXT: Record<NonNullable<ProviderProbe["tool_result"]>, string> = {
  passed: "Tool use passed",
  not_supported: "Tool use not supported by the gateway",
  inconclusive: "Tool use check inconclusive",
  not_run: "Tool use check not run",
};

function ProbeResults({ probe }: { probe: ProviderProbe | null }) {
  if (!probe) return <p className="text-xs text-faint">Not tested yet.</p>;
  return (
    <div className="space-y-0.5 text-xs" data-testid="probe-results">
      <p>{AUTH_RESULT_TEXT[probe.auth_result]}</p>
      <p>{TOOL_RESULT_TEXT[probe.tool_result ?? "not_run"]}</p>
      <p className="text-faint">
        Neither result certifies full compatibility. Last tested {new Date(probe.probed_at).toLocaleString()}.
      </p>
    </div>
  );
}

function ModelMapFields({
  rows,
  onChange,
  idPrefix,
}: {
  rows: ModelRows;
  onChange: (rows: ModelRows) => void;
  idPrefix: string;
}) {
  const set = (role: ProviderModelRole, part: Partial<ModelRow>) =>
    onChange({ ...rows, [role]: { ...rows[role], ...part } });
  return (
    <fieldset className="space-y-3">
      <legend className="text-sm font-medium text-muted">Model map</legend>
      {ROLES.map(({ role, label, required }) => {
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
                {label}: {NO_CONTEXT_WARNING}
              </p>
            )}
            {row.model.toLowerCase().includes("claude") && (
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

function BaseUrlSelect({
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

function AuthStyleSelect({
  value,
  onChange,
}: {
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
        <option value="x_api_key">{AUTH_LABELS.x_api_key}</option>
      </Select>
    </Field>
  );
}

type RunFn = (fn: () => Promise<unknown>, notice: string, fallback: string, refresh?: boolean) => Promise<boolean>;

const DELETE_SOURCE_REASON = "This is your Claude source. Stop using it before you delete it.";

type Panel = "edit" | "endpoint" | "key" | null;

function ProviderRow({
  provider,
  allowed,
  busy,
  run,
  onProbe,
  isSource,
  onSource,
}: {
  provider: ProviderDTO;
  allowed: string[];
  busy: boolean;
  run: RunFn;
  onProbe: (id: string, probe: ProviderProbe) => void;
  isSource: boolean;
  onSource: (id: string | null) => void;
}) {
  const p = provider;
  const uid = useId();
  const [panel, setPanel] = useState<Panel>(null);
  const [name, setName] = useState(p.name);
  const [rows, setRows] = useState<ModelRows>(() => rowsFromMap(p.model_map));
  const [baseUrl, setBaseUrl] = useState(p.base_url);
  const [auth, setAuth] = useState<ProviderAuthStyle>(p.auth_style);
  const [key, setKey] = useState("");
  const [formError, setFormError] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [confirmKeyDelete, setConfirmKeyDelete] = useState(false);
  const [testing, setTesting] = useState(false);

  const open = (next: Panel) => {
    setFormError("");
    setPanel(panel === next ? null : next);
    if (next === "edit") {
      setName(p.name);
      setRows(rowsFromMap(p.model_map));
    }
    if (next === "endpoint") {
      setBaseUrl(p.base_url);
      setAuth(p.auth_style);
    }
    setKey("");
  };

  const saveEdit = async (e: FormEvent) => {
    e.preventDefault();
    const nameErr = validName(name);
    const built = buildModelMap(rows);
    const err = nameErr ?? ("error" in built ? built.error : null);
    if (err) return setFormError(err);
    setFormError("");
    // Send only what changed: a model_map always bumps a revision server-side (wiping the stored
    // Test result and re-checking the allowlist), so a pure rename must not carry one.
    const nextMap = (built as { map: ProviderModelMap }).map;
    const body: { name?: string; model_map?: ProviderModelMap } = {};
    if (name.trim() !== p.name) body.name = name.trim();
    if (!sameModelMap(nextMap, p.model_map)) body.model_map = nextMap;
    if (Object.keys(body).length === 0) {
      setPanel(null);
      return;
    }
    if (await run(() => api.patchProvider(p.id, body), "Provider updated.", "Failed to update the provider")) {
      setPanel(null);
    }
  };

  const saveEndpoint = async (e: FormEvent) => {
    e.preventDefault();
    if (baseUrl === "") return setFormError("Pick a base URL.");
    setFormError("");
    if (await run(() => api.patchProviderEndpoint(p.id, { base_url: baseUrl, auth_style: auth }), "Endpoint updated.", "Failed to update the endpoint")) {
      setPanel(null);
    }
  };

  const saveKey = async (e: FormEvent) => {
    e.preventDefault();
    const value = key.trim();
    if (!KEY_RE.test(value)) {
      return setFormError("Key must be 16-4096 printable ASCII characters with no spaces.");
    }
    setFormError("");
    setKey("");
    if (await run(() => api.putProviderKey(p.id, value), "Key saved. It will not be shown again.", "Failed to save the key")) {
      setPanel(null);
    }
  };

  const test = async () => {
    setTesting(true);
    try {
      await run(
        async () => onProbe(p.id, (await api.testProvider(p.id)).probe),
        "Test finished.",
        "Test failed",
        false,
      );
    } finally {
      setTesting(false);
    }
  };

  const canTest = p.has_key && p.key_enabled && !p.retired;
  const mapped = ROLES.filter(({ role }) => p.model_map[role]);
  // The worker sets one process-wide window: the smallest configured one across all mapped
  // models, or uzi's 200k fallback when none is configured (agent/src/sdk-env.ts).
  const configured = mapped.map(({ role }) => p.model_map[role]!.context_window).filter((w): w is number => !!w);
  const blankLabel = configured.length
    ? ` (no context window: uzi requests the smallest configured one, ${Math.min(...configured).toLocaleString()} tokens)`
    : " (no context window: 200k assumed)";
  const rowBusy = busy || testing;
  const blocked = sourceBlockedReason(p);

  return (
    <li className="space-y-3 rounded-lg border border-edge bg-raised/40 p-4" data-testid={`provider-${p.id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold text-fg">{sanitizeLabel(p.name)}</h3>
        {isSource && <Badge tone="info">Claude source</Badge>}
        {p.retired && <Badge tone="neutral">Retired</Badge>}
        {!p.has_key ? (
          <Badge tone="warning">No key</Badge>
        ) : p.key_enabled ? (
          <Badge tone="ok">Key enabled</Badge>
        ) : (
          <Badge tone="neutral">Key disabled</Badge>
        )}
      </div>

      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[8rem_1fr]">
        <dt className="text-muted">Base URL</dt>
        <dd className="break-all">
          <code>{sanitizeLabel(p.base_url)}</code>
        </dd>
        <dt className="text-muted">Auth style</dt>
        <dd>{AUTH_LABELS[p.auth_style]}</dd>
        <dt className="text-muted">Models</dt>
        <dd>
          <ul>
            {mapped.map(({ role, label }) => {
              const e = p.model_map[role]!;
              return (
                <li key={role}>
                  {label}: <code>{sanitizeLabel(e.model)}</code>
                  {e.context_window ? ` (${e.context_window.toLocaleString()} tokens)` : blankLabel}
                </li>
              );
            })}
          </ul>
        </dd>
        <dt className="text-muted">Latest test</dt>
        <dd>
          <ProbeResults probe={p.probe} />
        </dd>
      </dl>

      <div className="flex flex-wrap gap-2">
        {isSource ? (
          <Button
            size="sm"
            disabled={rowBusy}
            onClick={() =>
              void run(
                async () => onSource((await api.setClaudeProviderSource(null)).claude_provider_id),
                "Claude runs use Anthropic again.",
                "Failed to stop using the provider",
              )
            }
          >
            Stop using
          </Button>
        ) : (
          <Button
            size="sm"
            disabled={rowBusy || blocked !== null}
            title={blocked ?? undefined}
            onClick={() =>
              void run(
                async () => onSource((await api.setClaudeProviderSource(p.id)).claude_provider_id),
                "This provider is now your Claude source.",
                "Failed to use the provider",
              )
            }
          >
            Use as Claude source
          </Button>
        )}
        {!p.retired && (
          <>
            <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("edit")} aria-expanded={panel === "edit"}>
              Edit name and models
            </Button>
            <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("endpoint")} aria-expanded={panel === "endpoint"}>
              Edit endpoint
            </Button>
            <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("key")} aria-expanded={panel === "key"}>
              {p.has_key ? "Rotate key" : "Set key"}
            </Button>
            {p.has_key && (
              <>
                <Button
                  size="sm"
                  variant="secondary"
                  disabled={rowBusy}
                  onClick={() =>
                    void run(
                      () => api.setProviderKeyEnabled(p.id, !p.key_enabled),
                      p.key_enabled ? "Key disabled." : "Key enabled.",
                      "Failed to change the key",
                    )
                  }
                >
                  {p.key_enabled ? "Disable key" : "Enable key"}
                </Button>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={rowBusy}
                  onClick={() => setConfirmKeyDelete(true)}
                >
                  Delete key
                </Button>
              </>
            )}
          </>
        )}
        <Button size="sm" disabled={rowBusy || !canTest} onClick={() => void test()}>
          {testing ? "Testing…" : "Test"}
        </Button>
        {!p.retired && (
          <Button
            size="sm"
            variant="secondary"
            disabled={rowBusy}
            onClick={() => void run(() => api.retireProvider(p.id), "Provider retired.", "Failed to retire the provider")}
          >
            Retire
          </Button>
        )}
        <Button
          size="sm"
          variant="danger"
          disabled={rowBusy || isSource}
          title={isSource ? DELETE_SOURCE_REASON : undefined}
          onClick={() => setConfirmDelete(true)}
        >
          Delete
        </Button>
      </div>
      {isSource && (
        <p className="text-xs text-faint" data-testid={`delete-blocked-${p.id}`}>{DELETE_SOURCE_REASON}</p>
      )}
      {!isSource && blocked && (
        <p className="text-xs text-faint" data-testid={`source-blocked-${p.id}`}>{blocked}</p>
      )}
      {!canTest && !p.retired && (
        <p className="text-xs text-faint">Testing needs a key that is set and enabled.</p>
      )}

      {confirmDelete && (
        <div role="group" aria-label={`Confirm deleting ${sanitizeLabel(p.name)}`} className="space-y-2 rounded-lg border border-danger/40 bg-danger/10 p-3 text-sm">
          <p>Delete “{sanitizeLabel(p.name)}” and its key? This cannot be undone.</p>
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="dangerSolid"
              disabled={rowBusy}
              onClick={async () => {
                await run(() => api.deleteProvider(p.id), "Provider deleted.", "Failed to delete the provider");
                setConfirmDelete(false);
              }}
            >
              Confirm delete
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {confirmKeyDelete && (
        <div role="group" aria-label={`Confirm deleting the key of ${sanitizeLabel(p.name)}`} className="space-y-2 rounded-lg border border-danger/40 bg-danger/10 p-3 text-sm">
          <p>Delete the key of “{sanitizeLabel(p.name)}”? You will need to paste it again to use this provider.</p>
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="dangerSolid"
              disabled={rowBusy}
              onClick={async () => {
                await run(() => api.deleteProviderKey(p.id), "Key deleted.", "Failed to delete the key");
                setConfirmKeyDelete(false);
              }}
            >
              Confirm delete key
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setConfirmKeyDelete(false)}>
              Cancel
            </Button>
          </div>
        </div>
      )}

      {panel === "edit" && (
        <form onSubmit={saveEdit} className="space-y-3 border-t border-edge pt-3">
          <Field label="Name">
            <Input aria-label="Provider name" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <ModelMapFields rows={rows} onChange={setRows} idPrefix={`${uid}-edit`} />
          {formError && <Alert message={formError} />}
          <Button type="submit" disabled={rowBusy}>Save changes</Button>
        </form>
      )}
      {panel === "endpoint" && (
        <form onSubmit={saveEndpoint} className="space-y-3 border-t border-edge pt-3">
          <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
          <AuthStyleSelect value={auth} onChange={setAuth} />
          {formError && <Alert message={formError} />}
          <Button type="submit" disabled={rowBusy}>Save endpoint</Button>
        </form>
      )}
      {panel === "key" && (
        <form onSubmit={saveKey} className="space-y-3 border-t border-edge pt-3">
          <Field label={p.has_key ? "New key" : "Key"}>
            <Input
              type="password"
              autoComplete="off"
              aria-label="Provider key"
              placeholder="Paste the gateway API key"
              value={key}
              onChange={(e) => setKey(e.target.value)}
            />
          </Field>
          <p className="text-xs text-faint">The key is stored encrypted and is never shown again.</p>
          {formError && <Alert message={formError} />}
          <Button type="submit" disabled={rowBusy || key === ""}>Save key</Button>
        </form>
      )}
    </li>
  );
}

function CreateForm({
  allowed,
  busy,
  run,
}: {
  allowed: string[];
  busy: boolean;
  run: RunFn;
}) {
  const uid = useId();
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [auth, setAuth] = useState<ProviderAuthStyle>("bearer");
  const [rows, setRows] = useState<ModelRows>(emptyRows);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildModelMap(rows);
    const err =
      validName(name) ?? (baseUrl === "" ? "Pick a base URL." : null) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    setError("");
    const ok = await run(
      () =>
        api.createProvider({
          name: name.trim(),
          base_url: baseUrl,
          auth_style: auth,
          model_map: (built as { map: ProviderModelMap }).map,
        }),
      "Provider added. Set its key, then test it.",
      "Failed to add the provider",
    );
    if (ok) {
      setName("");
      setBaseUrl("");
      setAuth("bearer");
      setRows(emptyRows());
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3 border-t border-edge pt-5" aria-label="Add a provider">
      <h3 className="text-sm font-semibold text-fg">Add a provider</h3>
      {allowed.length === 0 && (
        <p className="text-sm text-warn">The operator has not allowed any base URLs, so a provider cannot be added yet.</p>
      )}
      <Field label="Name">
        <Input aria-label="Provider name" placeholder="team-gateway" value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
      <AuthStyleSelect value={auth} onChange={setAuth} />
      <ModelMapFields rows={rows} onChange={setRows} idPrefix={`${uid}-new`} />
      {error && <Alert message={error} />}
      <Button type="submit" disabled={busy || allowed.length === 0}>Add provider</Button>
    </form>
  );
}

export function Providers() {
  const [data, setData] = useState<ProvidersList | null>(null);
  const [loadError, setLoadError] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  // UserSettings.claude_provider_id: null = Anthropic; undefined = not read yet / unreadable.
  const [sourceId, setSourceId] = useState<string | null | undefined>(undefined);

  const reload = useCallback(async () => {
    try {
      const [list, source] = await Promise.all([
        api.listProviders(),
        (async () => {
          try {
            return (await api.getMySettings(true)).settings.claude_provider_id ?? null;
          } catch {
            return undefined; // the selection is advisory here; the list still renders
          }
        })(),
      ]);
      setData(list);
      if (source !== undefined) setSourceId(source);
      setLoadError("");
    } catch (err) {
      setLoadError(errorMessage(err, "Failed to load providers"));
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  // run executes one mutation: it reports into this card's own alert slots and
  // refreshes the list on success. Resolves true on success so callers can close forms.
  const run: RunFn = async (fn, okNotice, fallback, refresh = true) => {
    setError("");
    setNotice("");
    setBusy(true);
    try {
      await fn();
      setNotice(okNotice);
      if (refresh) await reload();
      return true;
    } catch (err) {
      setError(providerError(err, fallback));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const onProbe = (id: string, probe: ProviderProbe) =>
    setData((d) => (d ? { ...d, providers: d.providers.map((p) => (p.id === id ? { ...p, probe } : p)) } : d));

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Custom providers</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Point Claude runs at an Anthropic Messages compatible gateway instead of Anthropic. Pick a base URL the
          operator allows, map the Claude model names to the gateway's models, then save a key and test it.
        </p>
      </div>

      {loadError && <Alert message={loadError} />}
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      {!data && !loadError ? (
        <Skeleton className="h-16 w-full" />
      ) : data && !data.enabled ? (
        <>
          <p className="rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm text-muted">
            Custom providers are off. Your operator has not enabled them by setting{" "}
            <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_PROVIDER_ALLOWED_BASE_URLS</code>.
          </p>
          {sourceId !== undefined && sourceId !== null && (
            <div className="space-y-2">
              <p className="text-sm text-muted">A custom provider is still selected as your Claude source.</p>
              <Button
                size="sm"
                disabled={busy}
                onClick={() =>
                  void run(
                    async () => setSourceId((await api.setClaudeProviderSource(null)).claude_provider_id),
                    "Claude runs use Anthropic again.",
                    "Failed to stop using the provider",
                  )
                }
              >
                Stop using
              </Button>
            </div>
          )}
        </>
      ) : data ? (
        <>
          <div className="space-y-1 text-sm" data-testid="claude-source">
            <p>
              <span className="text-muted">Current Claude source: </span>
              <span className="font-medium text-fg">
                {sourceId === undefined
                  ? "unknown"
                  : sourceId === null
                    ? "Anthropic"
                    : data.providers.find((p) => p.id === sourceId)
                      ? sanitizeLabel(data.providers.find((p) => p.id === sourceId)!.name)
                      : "a provider that no longer exists"}
              </span>
            </p>
            <p className="text-xs text-faint">Use only a gateway you trust not to redirect: a redirect can resend your prompts and code to another host, and your key to another path on the same host (or, with x-api-key auth, to another host). Bearer auth limits this. See the Model providers docs.</p>
          </div>
          {data.providers.length === 0 ? (
            <p className="text-sm text-muted">No providers yet.</p>
          ) : (
            <ul className="space-y-3">
              {data.providers.map((p) => (
                <ProviderRow
                  key={p.id}
                  provider={p}
                  allowed={data.allowed_base_urls}
                  busy={busy}
                  run={run}
                  onProbe={onProbe}
                  isSource={sourceId === p.id}
                  onSource={setSourceId}
                />
              ))}
            </ul>
          )}
          <CreateForm allowed={data.allowed_base_urls} busy={busy} run={run} />
        </>
      ) : null}
    </Card>
  );
}
