// Settings → Custom providers. A provider is an engine-compatible gateway
// the user can point runs at: a base URL chosen from the
// operator's allowlist, an auth style, a model map and a write-only API key.
//
// Every string here (names, URLs, model names) is untrusted and rendered as React
// text only. The key is never displayed or returned by the API; setting or rotating
// it is a re-paste into a password field that is cleared on save.
//
// Admin-shared definitions (PRD #4324, credential_mode "per_user_key") appear in the same
// list. The admin owns their endpoint, auth style and model map; the user owns only their
// own key. Such a row therefore offers key actions, Test and select/clear only (the server
// answers 404 to the owner routes), and both attaching a key and selecting it as the Claude or
// Codex source (the definition's own engine) first show SharedProviderDisclosure: who
// controls what, and where the key goes.

import { useCallback, useEffect, useId, useRef, useState, type FormEvent, type Ref } from "react";
import {
  api,
  type ProviderAuthStyle,
  type ProviderDTO,
  type ProviderModelMap,
  type ProviderProbe,
  type ProvidersList,
} from "../lib/api";
import { ApiError, errorMessage } from "../lib/apiError";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import {
  AUTH_LABELS,
  AuthStyleSelect,
  BaseUrlSelect,
  buildModelMap,
  emptyRows,
  MappedModels,
  ModelMapFields,
  rolesFor,
  rowsFromMap,
  sameModelMap,
  validName,
  type ModelRows,
} from "./providerFields";
import { Alert, Badge, Button, Card, Field, Input, SectionTitle, Select, Skeleton } from "./ui";

const isShared = (p: ProviderDTO) => p.credential_mode === "per_user_key";

// A personal and a shared definition may carry the same name, so the source line says which.
// A retired source stays selected (nothing falls back on its own), so the line says it is unusable.
function sourceName(p: ProviderDTO | undefined): string {
  if (!p) return "a provider that no longer exists";
  const name = isShared(p) ? `${sanitizeLabel(p.name)} (managed by your admin)` : sanitizeLabel(p.name);
  return p.retired ? `${name}, retired: new runs cannot use it until you stop using it or choose another source` : name;
}

const engineLabel = (engine: ProviderDTO["engine"]) => engine === "codex" ? "Codex" : "Claude";
async function setSource(engine: ProviderDTO["engine"], id: string | null) {
  return engine === "codex" ? (await api.setCodexProviderSource(id)).codex_provider_id : (await api.setClaudeProviderSource(id)).claude_provider_id;
}

const KEY_RE = /^[\x21-\x7E]{16,4096}$/;

const REASON_MESSAGES: Record<string, string> = {
  provider_base_url_not_allowed: "That base URL is not on the operator's allowlist. Pick one from the list.",
  provider_in_use_as_source:
    "This provider is still in use as a model source. Switch what uses it to another source, then delete it.",
  provider_limit_reached: "You have reached the maximum number of providers. Delete one first.",
  provider_retired: "This provider is retired and can no longer be changed.",
  provider_name_taken: "You already have a provider with that name.",
  provider_probe_rate_limited: "Tests are rate limited. Wait a moment, then test again.",
  provider_source_unusable: "This provider cannot be your model source right now. Check that it is not retired and has an enabled key.",
};

// Why a provider cannot be selected as its engine source, or null when it can. Mirrors the
// server's usability rules (the server stays authoritative and answers 409
// provider_source_unusable); the feature-off case never reaches a row.
function sourceBlockedReason(p: ProviderDTO): string | null {
  if (p.retired) return "A retired provider cannot be your model source.";
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

// SharedProviderDisclosure is what a user reads before attaching their key to, or selecting,
// an admin-shared definition (PRD #4324 "At attachment and selection"). It splits the
// definition into what the admin decides and what the user holds, then says where the key
// goes. Indigo (the "plan" token) marks admin-controlled things throughout: the badge, this
// panel's frame and its left column; the user's own column stays in plain foreground.
// It is focusable (tabIndex -1, named by its aria-label) so the select panel can move focus
// onto it, and its id describes the control it gates (the key input, the Confirm button).
function SharedProviderDisclosure({
  provider: p,
  context,
  id,
  ref,
}: {
  provider: ProviderDTO;
  context: "attach" | "select";
  id: string;
  ref?: Ref<HTMLElement>;
}) {
  return (
    <section
      ref={ref}
      tabIndex={-1}
      aria-label={context === "attach" ? "Before you attach your key" : "Before you use this provider"}
      className="space-y-3 rounded-lg border border-plan/30 bg-plan/5 p-3 text-sm outline-hidden focus:ring-2 focus:ring-ring"
      data-testid={`shared-disclosure-${p.id}`}
    >
      {/* The description target carries no aria-label, so assistive tech reads the disclosure text itself. */}
      <div id={id} className="space-y-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1.5 border-l-2 border-plan/60 pl-3">
          <h4 className="font-medium text-plan">Set by your admin</h4>
          <p>
            Endpoint <code className="break-all">{sanitizeLabel(p.base_url)}</code>, fixed when your admin created
            this definition. Your admin cannot repoint your key to a different gateway.
          </p>
          <p>Auth style: your key is sent as {AUTH_LABELS[p.auth_style]}.</p>
          <p>
            Model map: your admin chooses which gateway model serves each uzi model, so it decides which model and
            context window your own key pays for on your next fresh run. An admin edit changes that without asking you;
            runs that already started keep the map they started with.
          </p>
        </div>
        <div className="space-y-1.5 border-l-2 border-edge pl-3">
          <h4 className="font-medium text-fg">Held by you</h4>
          <p>
            Your key is personal and in your own custody. It is stored encrypted for your account only, your admin
            cannot see it, and you can rotate, disable or delete it at any time.
          </p>
        </div>
      </div>
      {p.engine === "codex" ? (
        <p className="text-muted" data-testid={`shared-disclosure-redirect-${p.id}`}>
          Your key is sent to that endpoint on every request. Codex runtime redirects may forward request bodies and
          authentication headers, so a redirecting gateway can resend your prompts, code and key to another host.
          Allowlist admission is not confinement to that host.
        </p>
      ) : (
        <p className="text-muted" data-testid={`shared-disclosure-redirect-${p.id}`}>
          Your key is sent to that endpoint on every request. If the gateway redirects, it can resend your prompts and
          code to another host, and your key to another path on the same host.
        </p>
      )}
      {/* Codex definitions are bearer-only, so the x-api-key warning is Claude's alone. */}
      {p.engine === "claude" && p.auth_style === "x_api_key" && (
        <p className="text-warn" data-testid={`shared-disclosure-xapikey-${p.id}`}>
          With x-api-key auth, a redirect can also forward the key itself to another host. Bearer auth limits this.
        </p>
      )}
      </div>
    </section>
  );
}

type RunFn = (fn: () => Promise<unknown>, notice: string, fallback: string, refresh?: boolean) => Promise<boolean>;

const DELETE_SOURCE_REASON = "This is your model source. Stop using it before you delete it.";

type Panel = "edit" | "endpoint" | "key" | "select" | null;

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
  const disclosureId = `${uid}-disclosure`;
  const disclosureRef = useRef<HTMLElement>(null);

  // Opening the select panel moves focus onto its disclosure, so the next Tab reaches Confirm
  // directly instead of walking past Rotate, Disable, Delete and Test first.
  useEffect(() => {
    if (panel === "select") disclosureRef.current?.focus();
  }, [panel]);

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
    const built = buildModelMap(rows, p.engine);
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
  const mapped = rolesFor(p.engine).filter(({ role }) => p.model_map[role]);
  // Claude only: the worker sets one process-wide window, the smallest configured one across all
  // mapped models, or uzi's 200k fallback when none is configured (agent/src/sdk-env.ts).
  const configured = p.engine === "claude" ? mapped.map(({ role }) => p.model_map[role]!.context_window).filter((w): w is number => !!w) : [];
  // Codex omits model_context_window, so its runtime fallback applies (agent/src/codex/config.ts).
  const blankLabel = p.engine === "codex"
    ? " (no context window: Codex runtime fallback)"
    : configured.length
      ? ` (no context window: uzi requests the smallest configured one, ${Math.min(...configured).toLocaleString()} tokens)`
      : " (no context window: 200k assumed)";
  const rowBusy = busy || testing;
  const blocked = sourceBlockedReason(p);
  const shared = isShared(p);
  const selectAsSource = () =>
    run(
      async () => onSource(await setSource(p.engine, p.id)),
      `This provider is now your ${engineLabel(p.engine)} source.`,
      "Failed to use the provider",
    );

  return (
    <li className="space-y-3 rounded-lg border border-edge bg-raised/40 p-4" data-testid={`provider-${p.id}`}>
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-sm font-semibold text-fg">{sanitizeLabel(p.name)}</h3>
        <Badge tone="neutral">{engineLabel(p.engine)}</Badge>
        {shared && <Badge tone="plan">Managed by your admin</Badge>}
        {isSource && <Badge tone="info">{engineLabel(p.engine)} source</Badge>}
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
        <dt className="text-muted">Protocol</dt>
        <dd>{p.protocol === "openai_responses" ? "OpenAI Responses" : "Anthropic Messages"}</dd>
        <dt className="text-muted">Base URL</dt>
        <dd className="break-all">
          <code>{sanitizeLabel(p.base_url)}</code>
          {shared && <span className="text-xs text-faint"> (fixed by your admin)</span>}
        </dd>
        <dt className="text-muted">Auth style</dt>
        <dd>{AUTH_LABELS[p.auth_style]}</dd>
        <dt className="text-muted">Models</dt>
        <dd>
          <MappedModels engine={p.engine} map={p.model_map} blankLabel={blankLabel} />
        </dd>
        <dt className="text-muted">{shared ? "Latest test of your key" : "Latest test"}</dt>
        <dd>
          <ProbeResults probe={p.probe} />
        </dd>
      </dl>

      <div className="flex flex-wrap gap-2">
        {isSource ? (
          <Button
            size="sm"
            aria-label={`Stop using ${engineLabel(p.engine)} source`}
            disabled={rowBusy}
            onClick={() =>
              void run(
                async () => onSource(await setSource(p.engine, null)),
                `${engineLabel(p.engine)} runs use the native source again.`,
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
            // A shared definition is selected only after its disclosure: the first click opens it.
            aria-expanded={shared ? panel === "select" : undefined}
            onClick={() => (shared ? open("select") : void selectAsSource())}
          >
            Use as {engineLabel(p.engine)} source
          </Button>
        )}
        {!p.retired && !shared && (
          <>
            <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("edit")} aria-expanded={panel === "edit"}>
              Edit name and models
            </Button>
            <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("endpoint")} aria-expanded={panel === "endpoint"}>
              Edit endpoint
            </Button>
          </>
        )}
        {/* A retired shared definition still lets its user disable or delete their own key: they
            cannot delete the definition, so this is the only way to take the key back. */}
        {(!p.retired || shared) && (
          <>
            {!p.retired && (
              <Button size="sm" variant="secondary" disabled={rowBusy} onClick={() => open("key")} aria-expanded={panel === "key"}>
                {p.has_key ? "Rotate key" : "Set key"}
              </Button>
            )}
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
        {!p.retired && !shared && (
          <Button
            size="sm"
            variant="secondary"
            disabled={rowBusy}
            onClick={() => void run(() => api.retireProvider(p.id), "Provider retired.", "Failed to retire the provider")}
          >
            Retire
          </Button>
        )}
        {!shared && (
          <Button
            size="sm"
            variant="danger"
            disabled={rowBusy || isSource}
            title={isSource ? DELETE_SOURCE_REASON : undefined}
            onClick={() => setConfirmDelete(true)}
          >
            Delete
          </Button>
        )}
      </div>
      {shared && (
        <p className="text-xs text-faint" data-testid={`shared-note-${p.id}`}>
          {p.retired && isSource
            ? `Your admin retired this definition, but it is still your ${engineLabel(p.engine)} source, so new runs cannot start on it. Stop using it or choose another source. You can still disable or delete your key.`
            : p.retired
            ? "Your admin retired this definition. It can no longer be selected or tested; you can still disable or delete your key."
            : "Your admin manages this definition's name, endpoint and models. You manage only your own key."}
        </p>
      )}
      {isSource && !shared && (
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
          <ModelMapFields engine={p.engine} rows={rows} onChange={setRows} idPrefix={`${uid}-edit`} />
          {formError && <Alert message={formError} />}
          <Button type="submit" disabled={rowBusy}>Save changes</Button>
        </form>
      )}
      {panel === "endpoint" && (
        <form onSubmit={saveEndpoint} className="space-y-3 border-t border-edge pt-3">
          <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
          <AuthStyleSelect engine={p.engine} value={auth} onChange={setAuth} />
          {formError && <Alert message={formError} />}
          <Button type="submit" disabled={rowBusy}>Save endpoint</Button>
        </form>
      )}
      {panel === "key" && (
        <form onSubmit={saveKey} className="space-y-3 border-t border-edge pt-3">
          {shared && <SharedProviderDisclosure provider={p} context="attach" id={disclosureId} />}
          <Field label={p.has_key ? "New key" : "Key"}>
            <Input
              type="password"
              autoComplete="off"
              aria-label="Provider key"
              aria-describedby={shared ? disclosureId : undefined}
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
      {panel === "select" && shared && !isSource && (
        <div className="space-y-3 border-t border-edge pt-3">
          <SharedProviderDisclosure provider={p} context="select" id={disclosureId} ref={disclosureRef} />
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              aria-describedby={disclosureId}
              disabled={rowBusy || blocked !== null}
              onClick={async () => {
                if (await selectAsSource()) setPanel(null);
              }}
            >
              Confirm and use as {engineLabel(p.engine)} source
            </Button>
            <Button size="sm" variant="secondary" onClick={() => setPanel(null)}>
              Cancel
            </Button>
          </div>
        </div>
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
  const [engine, setEngine] = useState<ProviderDTO["engine"]>("claude");
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [auth, setAuth] = useState<ProviderAuthStyle>("bearer");
  const [rows, setRows] = useState<ModelRows>(emptyRows);
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildModelMap(rows, engine);
    const err =
      validName(name) ?? (baseUrl === "" ? "Pick a base URL." : null) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    setError("");
    const ok = await run(
      () =>
        api.createProvider({
          engine,
          protocol: engine === "codex" ? "openai_responses" : "anthropic_messages",
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
      <Field label="Engine"><Select aria-label="Engine" value={engine} onChange={e => { setEngine(e.target.value as ProviderDTO["engine"]); setRows(emptyRows()); setAuth("bearer"); setError(""); }}>
        <option value="claude">Claude</option><option value="codex">Codex</option>
      </Select></Field>
      <p className="text-xs text-muted">Protocol: {engine === "codex" ? "OpenAI Responses" : "Anthropic Messages"}</p>
      {allowed.length === 0 && (
        <p className="text-sm text-warn">The operator has not allowed any base URLs, so a provider cannot be added yet.</p>
      )}
      <Field label="Name">
        <Input aria-label="Provider name" placeholder="team-gateway" value={name} onChange={(e) => setName(e.target.value)} />
      </Field>
      <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
      <AuthStyleSelect engine={engine} value={auth} onChange={setAuth} />
      <ModelMapFields engine={engine} rows={rows} onChange={setRows} idPrefix={`${uid}-new`} />
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
  // Each engine selection: null = native; undefined = not read yet / unreadable.
  const [sources, setSources] = useState<Partial<Record<ProviderDTO["engine"], string | null>>>({});
  const updateSource = (engine: ProviderDTO["engine"], id: string | null) => setSources(old => ({ ...old, [engine]: id }));

  const reload = useCallback(async () => {
    try {
      const [list, source] = await Promise.all([
        api.listProviders(),
        (async () => {
          try {
            const settings = (await api.getMySettings(true)).settings;
            return { claude: settings.claude_provider_id ?? null, codex: settings.codex_provider_id ?? null };
          } catch {
            return undefined; // the selection is advisory here; the list still renders
          }
        })(),
      ]);
      setData(list);
      if (source !== undefined) setSources(source);
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
          Point Claude or Codex runs at a compatible gateway. Pick an allowed base URL, map models, then save a key and test it. Definitions marked as managed by your admin are shared: attach your own key to use them.
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
          {(["claude", "codex"] as const).map(engine => sources[engine] != null && (
            <div key={engine}>
              <p>A custom provider is still selected as your {engineLabel(engine)} source.</p>
              <Button disabled={busy} aria-label={`Stop using ${engineLabel(engine)} source`} onClick={() => void run(async () => updateSource(engine, await setSource(engine, null)), `${engineLabel(engine)} runs use the native source again.`, "Failed to stop using the provider")}>Stop using</Button>
            </div>
          ))}
        </>
      ) : data ? (
        <>
          {(["claude", "codex"] as const).map(engine => (
            <div key={engine} data-testid={`${engine}-source`}>
              Current {engineLabel(engine)} source: {sources[engine] === undefined ? "unknown" : sources[engine] === null ? (engine === "claude" ? "Anthropic" : "OpenAI") : sourceName(data.providers.find(p => p.id === sources[engine]))}
              <p className="text-xs text-faint">{engine === "claude" ? "Use only a gateway you trust not to redirect: a redirect can resend your prompts and code to another host, and your key to another path on the same host (or, with x-api-key auth, to another host). Bearer auth limits this. See the Model providers docs." : "Use only a gateway you trust: Codex runtime redirects may forward request bodies and authentication headers. Allowlist admission is not confinement to that host. See the Model providers docs."}</p>
            </div>
          ))}
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
                  isSource={sources[p.engine] === p.id}
                  onSource={id => updateSource(p.engine, id)}
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
