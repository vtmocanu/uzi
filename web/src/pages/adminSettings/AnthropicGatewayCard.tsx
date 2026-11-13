import { useEffect, useRef, useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle, Select } from "../../components/ui";

const ALIASES = ["opus", "sonnet", "haiku", "fable"] as const;
// The one model-name rule, shared verbatim with the api (settings_gateway.go), the worker
// (anthropic-gateway.ts) and the mock api.
const GATEWAY_MODEL_RE = /^[A-Za-z0-9._:/@-]{1,200}$/;
type Alias = (typeof ALIASES)[number];

// Mirrors ParseGatewayModels in settings_gateway.go (the API rechecks every write).
function parseModels(raw: string): Record<Alias, string> {
  const out: Record<Alias, string> = { opus: "", sonnet: "", haiku: "", fable: "" };
  for (const pair of raw.split(",")) {
    const [alias, model] = pair.split("=").map((s) => s.trim());
    if (alias && model && (ALIASES as readonly string[]).includes(alias)) out[alias as Alias] = model;
  }
  return out;
}

function encodeModels(models: Record<Alias, string>): string {
  return ALIASES.filter((a) => models[a].trim() !== "").map((a) => `${a}=${models[a].trim()}`).join(",");
}

function urlError(value: string): string | null {
  if (value === "") return null;
  try {
    const u = new URL(value);
    if (u.protocol !== "https:") return "must use https";
    if (u.username || u.password) return "must not embed credentials";
    if (u.search || u.hash) return "must not carry a query or fragment";
  } catch {
    return "must be a valid URL";
  }
  return null;
}

function modelError(models: Record<Alias, string>): string | null {
  for (const a of ALIASES) {
    const m = models[a].trim();
    if (m !== "" && !GATEWAY_MODEL_RE.test(m)) return `${a}: 1-200 characters of letters, digits and . _ : / @ -`;
  }
  return null;
}

// AnthropicGatewayCard routes every Claude call through an Anthropic-compatible gateway
// (LiteLLM, Portkey, Cloudflare AI Gateway, a corporate proxy). The operator allowlists the
// host (ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS); this card picks it, how the key is sent and
// which gateway model each Claude alias maps to.
export function AnthropicGatewayCard({ settings, onSaved }: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const savedURL = settings.anthropic_gateway_base_url ?? "";
  const savedHeader = settings.anthropic_gateway_auth_header || "authorization";
  const savedModels = settings.anthropic_gateway_models ?? "";
  const [url, setURL] = useState(savedURL);
  const [header, setHeader] = useState(savedHeader);
  const [models, setModels] = useState(parseModels(savedModels));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // A sibling card's save replaces `settings` with the server's confirmed values. Take the
  // new value for every field the admin has not edited, so a later save here never writes
  // back a stale URL or header; an edited field keeps the edit.
  const confirmed = useRef({ url: savedURL, header: savedHeader, models: parseModels(savedModels) });
  useEffect(() => {
    const prev = confirmed.current;
    const nextModels = parseModels(savedModels);
    setURL((cur) => (cur === prev.url ? savedURL : cur));
    setHeader((cur) => (cur === prev.header ? savedHeader : cur));
    setModels((cur) => {
      const out = { ...cur };
      for (const a of ALIASES) if (cur[a] === prev.models[a]) out[a] = nextModels[a];
      return out;
    });
    confirmed.current = { url: savedURL, header: savedHeader, models: nextModels };
  }, [savedURL, savedHeader, savedModels]);

  const encoded = encodeModels(models);
  const dirty = url !== savedURL || header !== savedHeader || encoded !== encodeModels(parseModels(savedModels));
  const invalid = urlError(url) ?? modelError(models);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !dirty || invalid) return;
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const resp = await api.updateSettings({
        anthropic_gateway_base_url: url,
        anthropic_gateway_auth_header: header,
        anthropic_gateway_models: encoded,
      });
      onSaved(resp);
      setURL(resp.settings.anthropic_gateway_base_url ?? "");
      setHeader(resp.settings.anthropic_gateway_auth_header || "authorization");
      setModels(parseModels(resp.settings.anthropic_gateway_models ?? ""));
      setNotice("Claude gateway settings saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save Claude gateway settings"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Claude gateway</SectionTitle>
        <p id="anthropic-gateway-description" className="mt-2 text-sm text-muted">
          Route every Claude call through an Anthropic-compatible gateway, such as LiteLLM, Portkey or Cloudflare AI Gateway,
          instead of Anthropic directly. Each user&rsquo;s saved Anthropic token is then their key for that gateway.
          The URL must be in the operator&rsquo;s <code>ANTHROPIC_GATEWAY_ALLOWED_BASE_URLS</code>. Token tests ask the
          gateway, and usage meters pause, because a gateway has no Anthropic subscription windows. Turn it on before
          users save gateway keys, or have them press Test afterwards. Leave the URL empty to talk to Anthropic
          directly.
        </p>
      </div>
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}
      <form onSubmit={save} className="space-y-4" noValidate>
        <Field label="Gateway base URL" htmlFor="anthropic-gateway-url">
          <Input
            id="anthropic-gateway-url"
            type="url"
            placeholder="https://gateway.example.com"
            value={url}
            aria-invalid={urlError(url) !== null}
            onChange={(e) => { setURL(e.target.value); setError(""); setNotice(""); }}
          />
        </Field>
        <Field label="Send the key as" htmlFor="anthropic-gateway-header">
          <Select id="anthropic-gateway-header" value={header} onChange={(e) => setHeader(e.target.value)}>
            <option value="authorization">Authorization: Bearer</option>
            <option value="x-api-key">x-api-key</option>
          </Select>
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          {ALIASES.map((a) => (
            <Field key={a} label={`Gateway model for ${a}`} htmlFor={`anthropic-gateway-model-${a}`}>
              <Input
                id={`anthropic-gateway-model-${a}`}
                placeholder="unmapped: sent as Claude Code resolves it"
                value={models[a]}
                onChange={(e) => { setModels({ ...models, [a]: e.target.value }); setError(""); setNotice(""); }}
              />
            </Field>
          ))}
        </div>
        {invalid && <p className="text-xs text-danger">{invalid}</p>}
        <Button type="submit" disabled={busy || !dirty || invalid !== null}>
          {busy ? "Saving…" : "Save Claude gateway"}
        </Button>
      </form>
    </Card>
  );
}
