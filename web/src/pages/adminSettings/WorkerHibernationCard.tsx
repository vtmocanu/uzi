import { useId, useState, type FormEvent } from "react";
import { api, type AppSettings, type SettingsResponse, type UpdateSettingsPayload } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { Alert, Button, Card, Field, Input, SectionTitle } from "../../components/ui";

const DEFAULT_IDLE_AFTER = "336h";
const DEFAULT_SURPLUS_IDLE_AFTER = "2h";

// durationHours reads the Go-duration subset the admin is likely to type (h/m/s
// components, decimals allowed) into hours, or null when it does not parse. It only
// drives the plain-language readout beside the threshold; the server stays the
// validator, so an unparsed value is still sent and its 400 is shown.
function durationHours(v: string): number | null {
  if (!/^(?:(?:\d+(?:\.\d*)?|\.\d+)[hms])+$/.test(v)) return null;
  let seconds = 0;
  for (const m of v.matchAll(/(\d+(?:\.\d*)?|\.\d+)([hms])/g)) {
    seconds += Number(m[1]) * (m[2] === "h" ? 3600 : m[2] === "m" ? 60 : 1);
  }
  return seconds / 3600;
}

function readout(v: string): string {
  const h = durationHours(v);
  if (h === null || h <= 0) return "";
  if (h >= 24 && Number.isInteger(h / 24)) {
    const d = h / 24;
    return `${d} ${d === 1 ? "day" : "days"} without run activity`;
  }
  const rounded = Math.round(h * 10) / 10;
  return `${rounded} ${rounded === 1 ? "hour" : "hours"} without run activity`;
}

// WorkerHibernationCard is the admin surface for worker hibernation: the instance
// switch (PRD #4205) plus automatic hibernation of idle workers and its idle threshold
// (PRD #4207) and the warm-reserve surplus switch (PRD #4395). One card, one Save: the five keys go in a single updateSettings call so
// the automatic policy can never be stored half-edited. All round-trip through
// the Admin → Workers page (the settings API is still /admin/settings).
export function WorkerHibernationCard({
  settings,
  onSaved,
}: {
  settings: AppSettings;
  onSaved: (resp: SettingsResponse, sent: UpdateSettingsPayload) => void;
}) {
  const currentEnabled = settings.worker_hibernation_enabled ?? "false";
  const currentAuto = settings.worker_hibernation_auto ?? "false";
  const currentIdle = settings.worker_hibernation_idle_after ?? DEFAULT_IDLE_AFTER;
  const [enabled, setEnabled] = useState(currentEnabled === "true");
  const [auto, setAuto] = useState(currentAuto === "true");
  const currentSurplus = settings.worker_hibernation_surplus ?? "false";
  const currentSurplusIdle = settings.worker_hibernation_surplus_idle_after ?? DEFAULT_SURPLUS_IDLE_AFTER;
  const [idleAfter, setIdleAfter] = useState(currentIdle);
  const [surplus, setSurplus] = useState(currentSurplus === "true");
  const [surplusIdleAfter, setSurplusIdleAfter] = useState(currentSurplusIdle);
  const surplusIdleId = useId();
  const surplusIdleHintId = useId();
  const surplusHintId = useId();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const idleId = useId();
  const idleHintId = useId();
  const autoHintId = useId();

  const dirty =
    (enabled ? "true" : "false") !== currentEnabled ||
    (auto ? "true" : "false") !== currentAuto ||
    idleAfter !== currentIdle ||
    (surplus ? "true" : "false") !== currentSurplus ||
    surplusIdleAfter !== currentSurplusIdle;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setNotice("");
    setBusy(true);
    try {
      const payload: UpdateSettingsPayload = {
        worker_hibernation_enabled: String(enabled),
        worker_hibernation_auto: String(auto),
        worker_hibernation_idle_after: idleAfter,
        worker_hibernation_surplus: String(surplus),
        worker_hibernation_surplus_idle_after: surplusIdleAfter,
      };
      const resp = await api.updateSettings(payload);
      onSaved(resp, payload);
      setEnabled(resp.settings.worker_hibernation_enabled === "true");
      setAuto(resp.settings.worker_hibernation_auto === "true");
      setIdleAfter(resp.settings.worker_hibernation_idle_after ?? DEFAULT_IDLE_AFTER);
      setSurplus(resp.settings.worker_hibernation_surplus === "true");
      setSurplusIdleAfter(resp.settings.worker_hibernation_surplus_idle_after ?? DEFAULT_SURPLUS_IDLE_AFTER);
      setNotice("Worker hibernation settings saved.");
    } catch (err) {
      // The typed values stay in the form so the admin can correct them.
      setError(errorMessage(err, "Failed to save worker hibernation settings"));
    } finally {
      setBusy(false);
    }
  };

  const spelled = readout(idleAfter);
  const surplusActive = enabled && auto;
  const surplusH = durationHours(surplusIdleAfter);
  const idleH = durationHours(idleAfter);
  // The server uses min(surplus, idle_after) at read time; this is only a hint.
  const surplusClamped = surplusH !== null && idleH !== null && surplusH > idleH;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Worker hibernation</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          When on, an admin can hibernate an idle persistent hosted worker: its
          pod is scaled down and its storage is kept, which releases the pod's
          CPU and memory requests while the storage (and its cost) stays. A
          hibernated worker takes no new runs and does not count as available
          capacity. Turning this off wakes{" "}
          <strong className="text-fg">
            every hibernated or hibernating worker
          </strong>{" "}
          (at most 10 per pass, so a large fleet wakes over several passes).
          Waking a worker never depends on this setting. Off by default.
        </p>
      </div>

      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <form onSubmit={save} className="space-y-5">
        <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 rounded border-edge accent-brand"
          />
          Allow hibernating persistent hosted workers
        </label>

        <fieldset className="space-y-4 border-l-2 border-edge pl-4">
          <legend className="sr-only">Automatic hibernation</legend>
          <div className="space-y-1.5">
            <label className="flex cursor-pointer select-none items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={auto}
                onChange={(e) => setAuto(e.target.checked)}
                aria-describedby={autoHintId}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Automatically hibernate idle workers
            </label>
            <div id={autoHintId} className="space-y-1 pl-6 text-sm text-muted">
              <p>
                Hibernates every idle persistent hosted worker, including a
                user's last one, once it has had no run activity of any kind
                for the idle threshold. At most 10 workers per minute. Off by
                default.
              </p>
              {!enabled && (
                <p className="font-medium">
                  Takes effect only while hibernation is allowed above.
                </p>
              )}
            </div>
          </div>

          <Field label="Idle threshold" htmlFor={idleId}>
            <Input
              id={idleId}
              value={idleAfter}
              onChange={(e) => setIdleAfter(e.target.value)}
              aria-describedby={idleHintId}
              spellCheck={false}
              autoComplete="off"
              inputMode="text"
              className="max-w-40 font-mono"
            />
            <p id={idleHintId} className="text-xs text-muted">
              Go duration, e.g. 336h = 14 days; 1h–2160h
              {spelled && (
                <>
                  <br />
                  <span className="text-fg">{spelled}</span>
                </>
              )}
            </p>
          </Field>

          <div className="space-y-1.5">
            <label
              className={`flex select-none items-center gap-2 text-sm ${surplusActive ? "cursor-pointer" : "cursor-not-allowed opacity-60"}`}
            >
              <input
                type="checkbox"
                checked={surplus}
                disabled={!surplusActive}
                onChange={(e) => setSurplus(e.target.checked)}
                aria-describedby={surplusHintId}
                className="h-4 w-4 rounded border-edge accent-brand"
              />
              Keep one warm worker per pool, hibernate the surplus
            </label>
            <div id={surplusHintId} className="space-y-1 pl-6 text-sm text-muted">
              <p>
                Hibernates a pool's extra idle workers after the shorter surplus
                threshold below, keeping one awake so new runs start without a
                cold start. Off by default.
              </p>
              {!surplusActive && (
                <p className="font-medium">
                  Needs both "Allow hibernating" and "Automatically hibernate"
                  switched on.
                </p>
              )}
            </div>
          </div>

          <Field label="Surplus idle threshold" htmlFor={surplusIdleId}>
            <Input
              id={surplusIdleId}
              value={surplusIdleAfter}
              onChange={(e) => setSurplusIdleAfter(e.target.value)}
              disabled={!surplusActive}
              aria-describedby={surplusIdleHintId}
              spellCheck={false}
              autoComplete="off"
              inputMode="text"
              className="max-w-40 font-mono"
            />
            <p id={surplusIdleHintId} className="text-xs text-muted">
              Go duration, e.g. 2h; 15m–336h
              {surplusClamped && (
                <>
                  <br />
                  <span className="text-fg">
                    Longer than the idle threshold above, so the shorter idle
                    threshold ({idleAfter}) is used.
                  </span>
                </>
              )}
            </p>
          </Field>
        </fieldset>

        <Button type="submit" disabled={busy || !dirty}>
          {busy ? "Saving…" : "Save worker hibernation settings"}
        </Button>
      </form>
    </Card>
  );
}
