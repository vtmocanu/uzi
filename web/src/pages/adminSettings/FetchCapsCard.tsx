import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api, type AppSettings, type SettingsResponse, type UpdateSettingsPayload } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

const MIB = 1024 * 1024;

type CapKey = "fetch_max_file_bytes" | "fetch_max_run_bytes" | "fetch_max_run_files" | "fetch_max_concurrent_per_run";

// The four research fetch caps (PRD #1906 Open question 1). Bounds copy
// api/internal/settings/settings_fetch_caps.go (fetchCapBounds); the server stays the
// source of truth and re-validates every write. The two byte caps are edited in MiB and
// stored in bytes.
const FIELDS: {
  key: CapKey;
  label: string;
  hint: string;
  unit: "mib" | "count";
  max: number; // in the stored unit (bytes or a count); min is 1 for every cap
}[] = [
  {
    key: "fetch_max_file_bytes",
    label: "Largest single download (MiB)",
    hint: "Counted after decoding. Up to 1024 MiB (1 GiB).",
    unit: "mib",
    max: 1 << 30,
  },
  {
    key: "fetch_max_run_bytes",
    label: "Total download per run (MiB)",
    hint: "Up to 10240 MiB (10 GiB).",
    unit: "mib",
    max: 10 * 2 ** 30,
  },
  {
    key: "fetch_max_run_files",
    label: "Downloads per run",
    hint: "1 to 10000.",
    unit: "count",
    max: 10000,
  },
  {
    key: "fetch_max_concurrent_per_run",
    label: "Fetches in flight per run",
    hint: "1 to 32.",
    unit: "count",
    max: 32,
  },
];

// bytesToMib renders a stored byte count as MiB, exact when it is a whole MiB and to three
// decimals otherwise (a value written through the API need not be a MiB multiple). A
// count too small for three decimals (1 to 524 bytes) gets as many decimals as it needs
// to stay non-zero, so a tiny stored cap never reads as "0". Fixed notation, never
// exponent notation, so the MiB validator accepts what it shows. An unparsable stored
// value renders as-is.
function bytesToMib(stored: string): string {
  if (!/^\d+$/.test(stored.trim())) return stored;
  const n = Number(stored.trim());
  if (n % MIB === 0) return String(n / MIB);
  let digits = 3;
  while (digits < 6 && Number((n / MIB).toFixed(digits)) === 0) digits++;
  return (n / MIB).toFixed(digits).replace(/\.?0+$/, "");
}

function toDisplay(f: (typeof FIELDS)[number], stored: string | undefined): string {
  // `?? ""` guards an api pod that predates the keys: empty reads honestly and the field's
  // validator flags it rather than the input going uncontrolled.
  const v = stored ?? "";
  return f.unit === "mib" ? bytesToMib(v) : v;
}

// toStored converts an input value to the stored string, or returns an error message.
// Mirrors validateFetchCap: a whole number in [1, max]; zero and "unlimited" do not exist.
function toStored(f: (typeof FIELDS)[number], raw: string): { value: string } | { error: string } {
  const v = raw.trim();
  if (f.unit === "count") {
    if (!/^\d+$/.test(v)) return { error: "Enter a whole number" };
    const n = Number(v);
    if (n < 1 || n > f.max) return { error: `Must be between 1 and ${f.max}` };
    return { value: String(n) };
  }
  const maxMib = f.max / MIB;
  if (!/^\d+(\.\d+)?$/.test(v)) return { error: "Enter a size in MiB, such as 25 or 0.5" };
  const bytes = Math.round(Number(v) * MIB);
  if (bytes < 1 || bytes > f.max) return { error: `Must be more than 0 and at most ${maxMib} MiB` };
  return { value: String(bytes) };
}

// FetchCapsCard is the admin surface for the research fetch caps (PRD #1906 M1w). It
// saves independently of the other cards and sends only the caps that changed, like
// HealthSettingsCard. No environment variable sets a fetch cap, so every field is editable.
export function FetchCapsCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse) => void;
}) {
  const [values, setValues] = useState<Record<CapKey, string>>(
    () => Object.fromEntries(FIELDS.map((f) => [f.key, toDisplay(f, settings[f.key])])) as Record<CapKey, string>,
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // Touched is judged in the DISPLAY unit: a stored byte count that is not a whole MiB
  // shows rounded, and re-saving that rounded value untouched would silently rewrite it.
  // An untouched field is never validated and never sent, so a stored value the form
  // cannot represent exactly does not block saving the other caps.
  const results = FIELDS.map((f) => {
    const touched = values[f.key].trim() !== toDisplay(f, settings[f.key]);
    return { f, r: touched ? toStored(f, values[f.key]) : null };
  });
  const invalid = results.some(({ r }) => r !== null && "error" in r);
  const changed = results.filter(({ f, r }) => r !== null && "value" in r && r.value !== (settings[f.key] ?? ""));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    if (invalid || changed.length === 0) return;
    const payload: UpdateSettingsPayload = {};
    for (const { f, r } of changed) if (r !== null && "value" in r) payload[f.key] = r.value;
    setBusy(true);
    try {
      const resp = await api.updateSettings(payload);
      onSaved(resp);
      setValues(
        Object.fromEntries(FIELDS.map((f) => [f.key, toDisplay(f, resp.settings[f.key])])) as Record<CapKey, string>,
      );
      setNotice("Fetch caps saved.");
    } catch (err) {
      setError(errorMessage(err, "Failed to save the fetch caps"));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Research fetch caps</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Limits on what one official-sources research run may download through its{" "}
          <Link to="/admin/egress-profiles" className="text-brand hover:underline">
            site list
          </Link>
          . Every cap is required: there is no unlimited setting. Nothing reads these until the research lane is enabled.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-4" noValidate>
        <div className="grid gap-4 sm:grid-cols-2">
          {results.map(({ f, r }) => {
            const err = r !== null && "error" in r ? r.error : null;
            const id = `fetchcap-${f.key}`;
            return (
              <div key={f.key} className="space-y-1">
                <Field label={f.label} htmlFor={id}>
                  <Input
                    id={id}
                    inputMode={f.unit === "mib" ? "decimal" : "numeric"}
                    value={values[f.key]}
                    aria-invalid={err != null}
                    aria-describedby={`${id}-hint${err ? ` ${id}-error` : ""}`}
                    onChange={(e) => setValues((v) => ({ ...v, [f.key]: e.target.value }))}
                  />
                </Field>
                <p id={`${id}-hint`} className="text-xs text-faint">
                  {f.hint}
                </p>
                {err && (
                  <p id={`${id}-error`} className="text-xs text-danger">
                    {err}
                  </p>
                )}
              </div>
            );
          })}
        </div>

        <Button type="submit" disabled={busy || invalid || changed.length === 0}>
          {busy ? "Saving…" : "Save fetch caps"}
        </Button>
      </form>
    </Card>
  );
}
