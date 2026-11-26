// Admin → Shared providers (PRD #4324): Claude or Codex gateway definitions every user can
// pick as their source for that engine, with their OWN key. The admin fixes the engine,
// endpoint and auth style at creation and may later change only the name and model map;
// there is no key here at all.
// Every write is a cookie-only admin route, so the CLI can list and show but never write.
//
// The page answers two questions per definition: what is fixed (endpoint, auth style: a
// locked block, never an input once created) and who depends on it (how many users selected
// it, how many personal keys are attached). Delete is guarded on both: its confirmation says
// how many keys go with it and, while anyone has it selected, leads with the refusal instead.
// Every confirmation opens with focus on Cancel.
//
// Server strings (names, URLs, model names) render as React text nodes only; nothing here
// uses dangerouslySetInnerHTML.

import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from "react";
import { api, type AdminSharedProvider, type ProviderAuthStyle, type ProviderDTO, type ProviderModelMap } from "../lib/api";
import { ApiError, errorMessage } from "../lib/apiError";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Badge, Button, Card, EmptyState, Field, Input, ListSkeleton, Select } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { KeyIcon, LockIcon } from "../components/icons";
import {
  AUTH_LABELS,
  AuthStyleSelect,
  BaseUrlSelect,
  buildModelMap,
  emptyRows,
  MappedModels,
  ModelMapFields,
  rowsFromMap,
  sameModelMap,
  validName,
  type ModelRows,
} from "../components/providerFields";

type Engine = ProviderDTO["engine"];

const ENGINE_LABELS: Record<Engine, string> = { claude: "Claude", codex: "Codex" };
const PROTOCOL_LABELS: Record<Engine, string> = { claude: "Anthropic Messages", codex: "OpenAI Responses" };

const REASONS: Record<string, (engine: Engine) => string> = {
  provider_name_taken: () => "A shared provider with that name already exists. Shared names are unique regardless of case.",
  provider_limit_reached: () => "This instance already has 50 shared providers, retired ones included. Delete one first.",
  provider_in_use_as_source: (engine) =>
    `Delete refused: at least one user still has this definition selected as their ${ENGINE_LABELS[engine]} source. They must choose another source first.`,
  provider_retired: () => "This shared provider is retired and can no longer be edited.",
  provider_base_url_not_allowed: () => "That base URL is not on the operator's allowlist. Pick one from the list.",
};

function sharedError(err: unknown, fallback: string, engine: Engine): string {
  const reason = err instanceof ApiError ? (err.body as { reason?: string } | null)?.reason : undefined;
  return (reason && REASONS[reason]?.(engine)) || errorMessage(err, fallback);
}

const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

type Confirm = { kind: "retire" | "delete"; id: string } | null;

interface PageData {
  providers: AdminSharedProvider[];
  // From the admin's own /me/providers read: the operator allowlist. null when it could not be read.
  enabled: boolean | null;
  allowed: string[];
}

export function AdminSharedProviders() {
  const { data, loading, error: loadError, reload } = useAsyncData<PageData>(
    async () => {
      const [shared, mine] = await Promise.all([
        api.adminListSharedProviders(),
        api.listProviders().catch(() => null),
      ]);
      return { providers: shared.providers, enabled: mine?.enabled ?? null, allowed: mine?.allowed_base_urls ?? [] };
    },
    [],
    { fallback: "Failed to load the shared providers" },
  );
  const providers = data?.providers ?? [];
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<Confirm>(null);
  const [notice, setNotice] = useState("");

  const reset = () => {
    setNotice("");
    setCreating(false);
    setEditing(null);
    setConfirm(null);
  };
  const done = async (message: string) => {
    reset();
    setNotice(message);
    await reload();
  };
  const canCreate = data?.enabled !== false && (data?.allowed.length ?? 0) > 0;
  const newButton = (
    <Button onClick={() => { reset(); setCreating(true); }} disabled={!canCreate}>
      New shared provider
    </Button>
  );

  return (
    <AdminShell
      description={
        <>
          Shared providers are Claude or Codex gateways any user can pick as their source for that engine. You fix the
          engine, endpoint and auth style and map the models; each user attaches their own key, which you never see.
        </>
      }
    >
      {loadError && <Alert message={loadError} />}
      {notice && <Alert tone="success" message={notice} />}
      {data?.enabled === false && (
        <p className="rounded-lg border border-edge bg-raised/60 px-4 py-3 text-sm text-muted">
          Custom providers are off, so no definition can be created and users cannot select one. The operator enables
          them by setting <code className="rounded bg-raised px-1 py-0.5 text-fg">UZI_PROVIDER_ALLOWED_BASE_URLS</code>.
        </p>
      )}
      {data && data.enabled === null && (
        <p className="text-sm text-warn">The base URL allowlist could not be read, so a new definition cannot be created right now.</p>
      )}

      {creating ? (
        <CreateForm allowed={data?.allowed ?? []} onCancel={() => setCreating(false)} onCreated={(p) => done(`Shared provider ${sanitizeLabel(p.name)} created. Users can now attach their own key to it.`)} />
      ) : (
        !loading &&
        providers.length > 0 && (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-sm text-muted">{plural(providers.length, "shared provider", "shared providers")}</p>
            {newButton}
          </div>
        )
      )}

      {loading ? (
        <ListSkeleton rows={2} />
      ) : providers.length === 0 ? (
        !creating && (
          <EmptyState
            icon={<KeyIcon />}
            title="No shared providers yet"
            description="Create one so users can run Claude or Codex through your gateway with a key of their own."
            action={newButton}
          />
        )
      ) : (
        <ul className="space-y-3" aria-label="Shared providers">
          {providers.map((p) => (
            <li key={p.id}>
              {editing === p.id ? (
                <EditForm provider={p} onCancel={() => setEditing(null)} onSaved={(n) => done(`Changes to ${sanitizeLabel(n.name)} saved.`)} />
              ) : (
                <ProviderCard
                  provider={p}
                  confirm={confirm?.id === p.id ? confirm.kind : null}
                  onEdit={() => { reset(); setEditing(p.id); }}
                  onAsk={(kind) => { setNotice(""); setConfirm({ kind, id: p.id }); }}
                  onCancel={() => setConfirm(null)}
                  onDone={done}
                  onStale={reload}
                />
              )}
            </li>
          ))}
        </ul>
      )}
    </AdminShell>
  );
}

// FixedFacts is the locked block: endpoint and auth style, which no edit can change.
function FixedFacts({ baseUrl, auth }: { baseUrl: string; auth: ProviderAuthStyle }) {
  return (
    <div className="rounded-lg border border-edge bg-raised/50 px-3 py-2" data-testid="fixed-facts">
      <p className="flex items-center gap-1.5 text-xs text-faint">
        <LockIcon className="h-3.5 w-3.5" aria-hidden="true" />
        Fixed at creation
      </p>
      <dl className="mt-1 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[7rem_1fr]">
        <dt className="text-muted">Endpoint</dt>
        <dd className="break-all"><code>{sanitizeLabel(baseUrl)}</code></dd>
        <dt className="text-muted">Auth style</dt>
        <dd>{AUTH_LABELS[auth]}</dd>
      </dl>
    </div>
  );
}

function ProviderCard({
  provider: p,
  confirm,
  onEdit,
  onAsk,
  onCancel,
  onDone,
  onStale,
}: {
  provider: AdminSharedProvider;
  confirm: "retire" | "delete" | null;
  onEdit: () => void;
  onAsk: (kind: "retire" | "delete") => void;
  onCancel: () => void;
  onDone: (message: string) => Promise<void>;
  onStale: () => Promise<void>;
}) {
  const name = sanitizeLabel(p.name);
  const [working, setWorking] = useState(false);
  const [error, setError] = useState("");
  const confirmRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setError("");
    // Focus the safe choice, never the destructive one: an Enter right after opening must not act.
    if (confirm) confirmRef.current?.querySelector<HTMLButtonElement>("button[data-confirm-cancel]")?.focus();
  }, [confirm]);

  const act = async (fn: () => Promise<string>, fallback: string) => {
    setError("");
    setWorking(true);
    try {
      await onDone(await fn());
    } catch (err) {
      setError(sharedError(err, fallback, p.engine));
      // A 409 means the counts on screen were stale, a 404 that another admin already deleted the
      // definition: either way refresh the list, so no stale count or ghost card stays behind.
      if (err instanceof ApiError && (err.status === 409 || err.status === 404)) await onStale();
    } finally {
      setWorking(false);
    }
  };

  const selected = p.selected_user_count;
  const keys = p.attached_key_count;

  return (
    <Card className="space-y-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <h2 className="[overflow-wrap:anywhere] text-sm font-semibold text-fg">{name}</h2>
          <p className="text-xs text-faint">
            {ENGINE_LABELS[p.engine]}, {PROTOCOL_LABELS[p.engine]}, revision {p.current_revision}
          </p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          {p.retired && <Badge tone="neutral">Retired</Badge>}
          {!p.retired && (
            <>
              <Button variant="secondary" size="sm" onClick={onEdit} aria-label={`Edit ${name}`}>
                Edit
              </Button>
              <Button variant="secondary" size="sm" onClick={() => onAsk("retire")} aria-label={`Retire ${name}`}>
                Retire
              </Button>
            </>
          )}
          <Button variant="danger" size="sm" onClick={() => onAsk("delete")} aria-label={`Delete ${name}`}>
            Delete
          </Button>
        </div>
      </div>

      <ul className="flex flex-wrap gap-2 text-sm" aria-label={`Who uses ${name}`}>
        <li className="rounded-md border border-edge px-2 py-0.5" data-testid={`selected-count-${p.id}`}>
          Selected by {plural(selected, "user", "users")}
        </li>
        <li className="rounded-md border border-edge px-2 py-0.5" data-testid={`key-count-${p.id}`}>
          {plural(keys, "personal key", "personal keys")} attached
        </li>
      </ul>

      <FixedFacts baseUrl={p.base_url} auth={p.auth_style} />

      <div className="text-sm">
        <p className="text-muted">Models</p>
        <MappedModels engine={p.engine} map={p.model_map} blankLabel=" (no context window)" />
      </div>

      {confirm === "retire" && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm retiring ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working} onClick={() => void act(async () => {
                await api.adminRetireSharedProvider(p.id);
                return `${name} retired.`;
              }, "Failed to retire the shared provider")}>
                {working ? "Retiring…" : "Retire definition"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={onCancel} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          <p>
            Retire {name}? No new run can start on it and nobody can newly select it. {plural(selected, "user keeps", "users keep")} it
            selected but cannot use it until they choose another source; runs already started keep their binding. This cannot be undone.
          </p>
        </ConfirmBox>
      )}

      {confirm === "delete" && (
        <ConfirmBox
          refEl={confirmRef}
          label={`Confirm deleting ${name}`}
          error={error}
          actions={
            <>
              <Button variant="dangerSolid" size="sm" disabled={working || selected > 0} onClick={() => void act(async () => {
                const r = await api.adminDeleteSharedProvider(p.id);
                return `${name} deleted, with ${plural(r.deleted_key_count, "attached key", "attached keys")}.`;
              }, "Failed to delete the shared provider")}>
                {working ? "Deleting…" : "Delete definition and keys"}
              </Button>
              <Button variant="ghost" size="sm" disabled={working} onClick={onCancel} data-confirm-cancel>Cancel</Button>
            </>
          }
        >
          {/* While anyone has it selected the server refuses, so the box leads with the refusal
              instead of asking a question it will not act on. */}
          {selected > 0 ? (
            <>
              <p data-testid={`delete-selected-${p.id}`} className="text-warn">
                {`${name} can't be deleted yet: ${plural(selected, "user has", "users have")} it selected as their ${ENGINE_LABELS[p.engine]} source. ${p.retired ? "Ask them" : "Retire it, then ask them"} to choose another source.`}
              </p>
              <p data-testid={`delete-keys-${p.id}`} className="text-muted">
                Once nobody has it selected, deleting it would also delete {plural(keys, "personal key", "personal keys")} users
                attached to it, and their Test results.
              </p>
            </>
          ) : (
            <>
              <p data-testid={`delete-keys-${p.id}`}>
                Delete {name}? This also deletes {plural(keys, "personal key", "personal keys")} users attached to it, and their
                Test results. This cannot be undone.
              </p>
              <p data-testid={`delete-selected-${p.id}`} className="text-muted">
                No user has it selected as their {ENGINE_LABELS[p.engine]} source.
              </p>
            </>
          )}
        </ConfirmBox>
      )}
    </Card>
  );
}

function ConfirmBox({
  refEl,
  label,
  error,
  actions,
  children,
}: {
  refEl: React.RefObject<HTMLDivElement | null>;
  label: string;
  error: string;
  actions: ReactNode;
  children: ReactNode;
}) {
  return (
    <div ref={refEl} role="group" aria-label={label} className="space-y-2 rounded-lg border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-fg">
      {children}
      {error && <Alert message={error} />}
      <div className="flex flex-wrap gap-2">{actions}</div>
    </div>
  );
}

function CreateForm({
  allowed,
  onCancel,
  onCreated,
}: {
  allowed: string[];
  onCancel: () => void;
  onCreated: (p: AdminSharedProvider) => Promise<void>;
}) {
  const uid = useId();
  const [engine, setEngine] = useState<Engine>("claude");
  const [name, setName] = useState("");
  const [baseUrl, setBaseUrl] = useState("");
  const [auth, setAuth] = useState<ProviderAuthStyle>("bearer");
  const [rows, setRows] = useState<ModelRows>(emptyRows);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildModelMap(rows, engine);
    const err = validName(name) ?? (baseUrl === "" ? "Pick a base URL." : null) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    setError("");
    setSaving(true);
    try {
      const { provider } = await api.adminCreateSharedProvider({
        engine,
        name: name.trim(),
        base_url: baseUrl,
        auth_style: auth,
        model_map: (built as { map: ProviderModelMap }).map,
      });
      await onCreated(provider);
    } catch (err) {
      setError(sharedError(err, "Failed to create the shared provider", engine));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <form onSubmit={submit} className="space-y-3" aria-label="New shared provider">
        <h2 className="text-sm font-semibold text-fg">New shared provider</h2>
        <p className="text-sm text-muted">
          No key here: each user attaches their own. The engine, endpoint and auth style cannot be changed after you
          create it; a different gateway needs a new definition, which users select again on purpose.
        </p>
        {/* Switching engine starts the map and auth over: the two engines share no model slots,
            and Codex is bearer-only. */}
        <Field label="Engine">
          <Select
            aria-label="Engine"
            value={engine}
            onChange={(e) => {
              setEngine(e.target.value as Engine);
              setRows(emptyRows());
              setAuth("bearer");
              setError("");
            }}
          >
            <option value="claude">Claude</option>
            <option value="codex">Codex</option>
          </Select>
        </Field>
        <p className="text-xs text-muted" data-testid="engine-protocol">Protocol: {PROTOCOL_LABELS[engine]}</p>
        <Field label="Name">
          <Input aria-label="Provider name" placeholder="platform-gateway" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <BaseUrlSelect value={baseUrl} onChange={setBaseUrl} allowed={allowed} />
        <AuthStyleSelect engine={engine} value={auth} onChange={setAuth} />
        <p className={auth === "x_api_key" ? "text-xs text-warn" : "text-xs text-faint"} data-testid="auth-advice">
          {engine === "codex"
            ? "Codex sends each user's key as a Bearer token. Codex runtime redirects may forward request bodies and authentication headers, so share only a gateway you trust not to redirect: allowlist admission is not confinement to that host."
            : auth === "x_api_key"
              ? "With x-api-key, a redirecting gateway can forward each user's key to another host. Bearer is recommended."
              : "Bearer is recommended: on a redirect the key can reach another path on the same host, but not another host. Request bodies can still follow a redirect."}
        </p>
        <ModelMapFields engine={engine} rows={rows} onChange={setRows} idPrefix={`${uid}-new`} />
        {error && <Alert message={error} />}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={saving}>{saving ? "Creating…" : "Create shared provider"}</Button>
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
        </div>
      </form>
    </Card>
  );
}

function EditForm({
  provider: p,
  onCancel,
  onSaved,
}: {
  provider: AdminSharedProvider;
  onCancel: () => void;
  onSaved: (p: AdminSharedProvider) => Promise<void>;
}) {
  const uid = useId();
  const [name, setName] = useState(p.name);
  const [rows, setRows] = useState<ModelRows>(() => rowsFromMap(p.model_map));
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const built = buildModelMap(rows, p.engine);
    const err = validName(name) ?? ("error" in built ? built.error : null);
    if (err) return setError(err);
    // Send only what changed: a model map is a new revision for every user who selected it.
    const nextMap = (built as { map: ProviderModelMap }).map;
    const body: { name?: string; model_map?: ProviderModelMap } = {};
    if (name.trim() !== p.name) body.name = name.trim();
    if (!sameModelMap(nextMap, p.model_map)) body.model_map = nextMap;
    if (Object.keys(body).length === 0) return onCancel();
    setError("");
    setSaving(true);
    try {
      await onSaved((await api.adminPatchSharedProvider(p.id, body)).provider);
    } catch (err) {
      setError(sharedError(err, "Failed to save the shared provider", p.engine));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card>
      <form onSubmit={submit} className="space-y-3" aria-label={`Edit ${sanitizeLabel(p.name)}`}>
        <h2 className="text-sm font-semibold text-fg">Edit {sanitizeLabel(p.name)}</h2>
        <Field label="Name">
          <Input aria-label="Provider name" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <FixedFacts baseUrl={p.base_url} auth={p.auth_style} />
        <p className="text-xs text-faint">
          {ENGINE_LABELS[p.engine]}, {PROTOCOL_LABELS[p.engine]}. To use a different engine, endpoint or auth style,
          create a new definition.
        </p>
        <ModelMapFields engine={p.engine} rows={rows} onChange={setRows} idPrefix={`${uid}-edit`} />
        <p className="text-xs text-faint">
          A model map change applies to new runs of {plural(p.selected_user_count, "user", "users")} who selected it and resets
          every user's Test result. Runs already started keep their map.
        </p>
        {error && <Alert message={error} />}
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={saving}>{saving ? "Saving…" : "Save changes"}</Button>
          <Button type="button" variant="ghost" onClick={onCancel} disabled={saving}>Cancel</Button>
        </div>
      </form>
    </Card>
  );
}
