// Disable / Enable for account credentials (PRD #1732 M5), shared by the Anthropic
// token card and the OpenAI / Codex credential card.
//
// Disable is SUSPENSION, not deletion: the sealed value, label, default flag, pool
// opt-in and sidebar preference all survive, and Enable brings them back. The visual
// language says so: a disabled credential keeps its row, drawn as a dashed OUTLINE of
// the live card (no fill, muted name) in a "Disabled (n)" shelf at the bottom of its
// provider card. The shelf is collapsed by default and its expansion is remembered per
// browser (prefs wraps every localStorage access in try/catch).
//
// Pieces:
//   - DisableCredentialDialog: what disabling does, the live "Uses right now" list from
//     GET …/dependents, the REQUIRED replacement default when disabling the default
//     (D4), or the no-default consequence when it is the slot's last enabled credential,
//     plus the Codex shared-account note (D6);
//   - DisabledSection / DisabledCredentialRow: the shelf and its rows ("Disabled since",
//     Enable or "Enable and make default", Delete);
//   - NoDefaultNotice: the top-of-card notice for a slot whose credentials are all
//     disabled, visible even while the shelf is collapsed;
//   - useCheckingUsage: the "checking usage…" state after Enable, held until a FRESH
//     reading lands (a pre-disable reading is never shown as current, D13);
//   - usePendingFocus + the id helpers: where keyboard focus lands after a row moves
//     between the live list and the shelf (the control it came from has unmounted).

import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from "react";
import { api, type SecretDependents, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { prefs } from "../lib/prefs";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { ChevronDownIcon } from "./icons";
import { Modal } from "./Modal";
import { Alert, Badge, Button, cx } from "./ui";

export type CredentialSlot = "anthropic" | "codex";

// isEnabled reads the DTO's `enabled`, treating an ABSENT value as enabled: a pre-#1732
// api pod omits the key, and every credential it lists is usable.
function isEnabled(s: Pick<SecretMeta, "enabled">): boolean {
  return s.enabled !== false;
}

// splitByEnablement is the one place a card partitions its rows: the live list above,
// the Disabled shelf below.
export function splitByEnablement(rows: SecretMeta[]): { active: SecretMeta[]; disabled: SecretMeta[] } {
  return {
    active: rows.filter(isEnabled),
    disabled: rows.filter((s) => !isEnabled(s)),
  };
}

// slotHasDefault: every default is enabled (D4), so a slot has a default exactly when an
// enabled row carries the flag.
export function slotHasDefault(rows: SecretMeta[]): boolean {
  return rows.some((s) => isEnabled(s) && s.is_default);
}

// disabledSince renders "Disabled since 18 Sep 2026". The date is the ORIGINAL disable
// (a repeated disable keeps it, D10), so it answers "how long has this been off".
export function disabledSince(iso: string | null | undefined): string {
  if (!iso) return "Disabled";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "Disabled";
  const date = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", year: "numeric" }).format(d);
  return `Disabled since ${date}`;
}

// useRememberedExpansion is the Disabled shelf's open state, collapsed by default and
// remembered per browser under `key`.
export function useRememberedExpansion(key: string): [boolean, (open: boolean) => void] {
  const [open, setOpen] = useState<boolean>(() => prefs.get<boolean>(key, false) === true);
  const set = useCallback(
    (next: boolean) => {
      setOpen(next);
      prefs.set(key, next);
    },
    [key],
  );
  return [open, set];
}

// ── Focus after a row moves ─────────────────────────────────────────────────

// A Disable or Enable moves the row between the live list and the shelf, so the button
// that was clicked unmounts and focus would fall to <body>. These ids name where it goes
// instead: after Disable the shelf toggle (the row is behind it now), after Enable the
// restored row's own Disable button, after "Show disabled" the first shelf row's action.
export const disableButtonId = (id: string) => `cred-disable-${id}`;
export const enableButtonId = (id: string) => `cred-enable-${id}`;

// How long a focus request waits for its target to render (a reload is one round trip).
const FOCUS_WAIT_MS = 4_000;

// usePendingFocus returns request(id): focus the element with that id as soon as it is
// in the DOM. The target usually appears only after the card's reload re-renders, so the
// request is retried after every render until it lands or FOCUS_WAIT_MS passes; a request
// that never lands is dropped rather than stealing focus later.
export function usePendingFocus(): (id: string) => void {
  const pending = useRef<{ id: string; until: number } | null>(null);
  const [, setTick] = useState(0);
  useEffect(() => {
    const want = pending.current;
    if (!want) return;
    if (Date.now() > want.until) {
      pending.current = null;
      return;
    }
    const el = document.getElementById(want.id);
    if (el) {
      pending.current = null;
      el.focus();
    }
  });
  return useCallback((id: string) => {
    pending.current = { id, until: Date.now() + FOCUS_WAIT_MS };
    setTick((t) => t + 1);
  }, []);
}

// ── The shelf ───────────────────────────────────────────────────────────────

export function DisabledSection({
  count,
  expanded,
  onToggle,
  toggleId,
  children,
}: {
  count: number;
  expanded: boolean;
  onToggle: (open: boolean) => void;
  // The toggle's id, so a Disable can hand focus to it.
  toggleId?: string;
  children: ReactNode;
}) {
  const panelId = useId();
  if (count === 0) return null;
  return (
    <div className="border-t border-dashed border-edge pt-3">
      <button
        id={toggleId}
        type="button"
        aria-expanded={expanded}
        // Only point at the panel while it is mounted (LastRun's disclosure rule).
        aria-controls={expanded ? panelId : undefined}
        onClick={() => onToggle(!expanded)}
        className="inline-flex items-center gap-1.5 rounded text-sm font-medium text-muted transition-colors hover:text-fg"
      >
        <ChevronDownIcon
          aria-hidden="true"
          className={cx("h-4 w-4 transition-transform motion-reduce:transition-none", !expanded && "-rotate-90")}
        />
        Disabled ({count})
      </button>
      {expanded && (
        <div id={panelId} className="mt-3 space-y-2">
          {children}
        </div>
      )}
    </div>
  );
}

// DisabledCredentialRow is one suspended credential: the dashed outline of a live row.
// Auto-select and sidebar checkboxes are not drawn (hidden, not cleared: D8 keeps the
// stored preferences for Enable).
export function DisabledCredentialRow({
  secret,
  kindLabel,
  note,
  enableLabel,
  busy,
  onEnable,
  onDelete,
  testId,
}: {
  secret: SecretMeta;
  // The kind badge (Codex card: "Codex login" / "OpenAI API key"); omitted on Anthropic.
  kindLabel?: string;
  // The status line: "Disabled since <date>", or the Codex shared-account fact.
  note: string;
  enableLabel: string;
  busy: boolean;
  onEnable: () => void;
  onDelete: () => void;
  testId?: string;
}) {
  const safe = sanitizeLabel(secret.label);
  return (
    <div
      className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-dashed border-edge-strong px-4 py-3"
      data-testid={testId}
    >
      <div className="min-w-0">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <span className="truncate font-medium text-muted">{safe}</span>
          {kindLabel && <Badge tone="neutral">{kindLabel}</Badge>}
        </div>
        <p className="mt-0.5 text-xs text-faint">{note}</p>
      </div>
      <div className="flex items-center gap-2">
        <Button
          id={enableButtonId(secret.id)}
          variant="secondary"
          size="sm"
          disabled={busy}
          onClick={onEnable}
          aria-label={`${enableLabel}: ${safe}`}
        >
          {enableLabel}
        </Button>
        <Button variant="danger" size="sm" disabled={busy} onClick={onDelete} aria-label={`Delete ${safe}`}>
          Delete
        </Button>
      </div>
    </div>
  );
}

// NoDefaultNotice sits at the TOP of a provider card whose slot has no default (every
// credential disabled, D4). It renders whatever the shelf's state, and offers to open the
// shelf when it is collapsed, because that is where the fix is.
export function NoDefaultNotice({
  slot,
  expanded,
  onShow,
}: {
  slot: CredentialSlot;
  expanded: boolean;
  onShow: () => void;
}) {
  const noun = slot === "anthropic" ? "token" : "credential";
  return (
    <div
      className="rounded-lg border border-warn/40 bg-warn/10 px-4 py-3 text-sm"
      data-testid={`no-default-${slot}`}
    >
      <p className="font-semibold text-warn">
        {slot === "anthropic" ? "No default Anthropic token" : "No default Codex credential"}
      </p>
      <p className="mt-1 text-fg">
        {slot === "anthropic"
          ? "Every token is disabled, so work that spends your default token waits, and new runs that don't name a harness start on Codex if you have a Codex credential."
          : "Every Codex credential is disabled, so new runs that don't name a harness start on Claude if you have a Claude token. Runs already on a disabled login wait."}{" "}
        Enabling one makes it the default.
      </p>
      {!expanded && (
        <button
          type="button"
          onClick={onShow}
          className="mt-2 text-xs font-medium text-brand hover:text-brand-hover"
        >
          Show disabled {noun}s
        </button>
      )}
    </div>
  );
}

// ── Checking usage after Enable ─────────────────────────────────────────────

const CHECK_INTERVAL_MS = 5_000;
// The server pokes a poll on Enable, so a reading normally lands within seconds. When it
// cannot (vault locked, polling off) the label must not claim "checking" forever: after
// this many probes the row falls back to its ordinary meters state.
const CHECK_MAX_PROBES = 24;

// useCheckingUsage tracks credentials the owner just enabled until a FRESH reading lands.
// `probe` returns which of the given ids now have one.
export function useCheckingUsage(probe: (ids: string[]) => Promise<string[]>): {
  checking: ReadonlySet<string>;
  start: (id: string) => void;
} {
  const [checking, setChecking] = useState<Set<string>>(() => new Set());
  // Bumped after every probe, so an unchanged set still schedules the next one.
  const [tick, setTick] = useState(0);
  const probeRef = useRef(probe);
  useEffect(() => {
    probeRef.current = probe;
  });
  // Probes spent PER credential: enabling a second one must not restart the first one's
  // give-up clock (or it could say "checking" far past CHECK_MAX_PROBES).
  const attempts = useRef(new Map<string, number>());

  const start = useCallback((id: string) => {
    attempts.current.set(id, 0);
    setChecking((prev) => new Set(prev).add(id));
  }, []);

  useEffect(() => {
    if (checking.size === 0) return;
    let cancelled = false;
    const ids = [...checking];
    // A just-started credential gets its first probe soon; the rest keep the interval.
    const firstProbe = ids.some((id) => (attempts.current.get(id) ?? 0) === 0);
    const timer = window.setTimeout(async () => {
      for (const id of ids) attempts.current.set(id, (attempts.current.get(id) ?? 0) + 1);
      let fresh: string[] = [];
      try {
        fresh = await probeRef.current(ids);
      } catch {
        // a failed probe is retried on the next tick
      }
      if (cancelled) return;
      const done = new Set(fresh);
      for (const id of ids) {
        if ((attempts.current.get(id) ?? 0) >= CHECK_MAX_PROBES) done.add(id);
      }
      done.forEach((id) => attempts.current.delete(id));
      setChecking((prev) => {
        const next = new Set(prev);
        done.forEach((id) => next.delete(id));
        return next.size === prev.size ? prev : next;
      });
      setTick((t) => t + 1);
    }, firstProbe ? 1_500 : CHECK_INTERVAL_MS);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
    };
  }, [checking, tick]);

  return { checking, start };
}

// CheckingUsageBadge is the "checking usage…" chip a just-enabled row wears.
export function CheckingUsageBadge() {
  return (
    <Badge tone="info" dot pulse title="Enabled. uzi is taking a fresh usage reading; an older one is never shown as current.">
      checking usage…
    </Badge>
  );
}

// ── The Disable dialog ──────────────────────────────────────────────────────

function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

const MAX_NAMED = 3;
function named<T>(page: { items: T[]; total: number }, name: (t: T) => string): string {
  const shown = page.items.slice(0, MAX_NAMED).map(name);
  const rest = page.total - shown.length;
  const joined =
    shown.length <= 1 ? (shown[0] ?? "") : `${shown.slice(0, -1).join(", ")} and ${shown[shown.length - 1]}`;
  return rest > 0 ? `${joined} and ${rest} more` : joined;
}

// usesRightNow turns the dependents read into plain sentences, one per kind of reliance.
function usesRightNow(d: SecretDependents): string[] {
  const out: string[] = [];
  if (d.workers.total > 0) {
    out.push(
      `${d.workers.total === 1 ? "Worker" : "Workers"} ${named(d.workers, (w) => sanitizeLabel(w.name))} ${
        d.workers.total === 1 ? "is" : "are"
      } bound to it: ${d.workers.total === 1 ? "it waits" : "they wait"} until you enable it or rebind ${
        d.workers.total === 1 ? "it" : "them"
      }.`,
    );
  }
  if (d.schedules.total > 0) {
    out.push(
      `${plural(d.schedules.total, "schedule is", "schedules are")} pinned to it: ${
        d.schedules.total === 1 ? "its next fire is" : "their next fires are"
      } skipped.`,
    );
  }
  if (d.runs.total > 0) {
    out.push(
      `${plural(d.runs.total, "unfinished run uses", "unfinished runs use")} it: a run already working on it keeps it until it finishes; one that has not started waits.`,
    );
  }
  if (d.judge) out.push("Your run judge is pinned to it: retrospectives wait until you enable it or change the judge token.");
  return out;
}

export function DisableCredentialDialog({
  secret,
  slot,
  replacements,
  replacementBadges,
  onClose,
  onConfirm,
}: {
  secret: SecretMeta;
  slot: CredentialSlot;
  // The slot's OTHER enabled credentials: the candidates for the new default, in the
  // order to offer them (the Codex card puts failed logins last).
  replacements: SecretMeta[];
  // Badges beside a candidate's name (the Codex card: its kind and verification status),
  // so the choice is made knowing whether the new default can actually run.
  replacementBadges?: (s: SecretMeta) => ReactNode;
  onClose: () => void;
  // Resolves when the disable landed; rejects with the server's error to show inline.
  onConfirm: (newDefaultId: string | undefined) => Promise<void>;
}) {
  const safe = sanitizeLabel(secret.label);
  const [deps, setDeps] = useState<SecretDependents | null>(null);
  const [depsFailed, setDepsFailed] = useState(false);
  const [choice, setChoice] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const legendId = useId();

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const d = await api.getSecretDependents(secret.kind, secret.id);
        if (!cancelled) setDeps(d);
      } catch {
        if (!cancelled) setDepsFailed(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [secret.kind, secret.id]);

  const needsReplacement = secret.is_default && replacements.length > 0;
  const lastOfSlot = secret.is_default && replacements.length === 0;
  const canConfirm = !busy && (!needsReplacement || choice !== "");
  const noun = slot === "anthropic" ? "token" : secret.kind === "openai_api_key" ? "API key" : "login";

  const submit = async () => {
    if (!canConfirm) return;
    setBusy(true);
    setError("");
    try {
      await onConfirm(needsReplacement ? choice : undefined);
    } catch (err) {
      setError(errorMessage(err, `Failed to disable “${safe}”`));
      setBusy(false);
    }
  };

  const uses = deps ? usesRightNow(deps) : [];
  const siblings = deps?.enabled_siblings;

  return (
    <Modal label={`Disable ${safe}`} onClose={() => !busy && onClose()} closeOnBackdrop={!busy}>
      <div className="my-8 w-full max-w-lg overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
        <div className="border-b border-edge px-5 py-4">
          <h2 className="text-base font-semibold text-fg">Disable “{safe}”?</h2>
          <p className="mt-0.5 text-xs text-muted">You can enable it again at any time. Nothing is deleted.</p>
        </div>
        <div className="space-y-4 px-5 py-5 text-sm">
          {error && <Alert message={error} />}
          <ul className="list-disc space-y-1 pl-5 text-muted">
            <li>uzi stops checking its usage and refreshing it.</li>
            {slot === "anthropic" ? (
              <li>It leaves your sidebar, the token pickers, auto-select and the admin Rate limits view.</li>
            ) : (
              <li>It leaves your sidebar, and new Codex runs cannot start on it.</li>
            )}
            <li>Its name, value and settings are kept, and past run history is unchanged.</li>
            <li>A run already working on it finishes first.</li>
          </ul>

          <section aria-label="Uses right now" className="rounded-lg border border-edge bg-raised/50 p-3">
            <h3 className="text-xs font-semibold text-fg">Uses right now</h3>
            {!deps && !depsFailed && <p className="mt-1 text-xs text-faint">Checking what uses it…</p>}
            {depsFailed && (
              <p className="mt-1 text-xs text-warn">
                Could not check what uses it. Anything bound or pinned to it waits until you enable it again.
              </p>
            )}
            {deps && uses.length === 0 && <p className="mt-1 text-xs text-muted">Nothing uses it right now.</p>}
            {uses.length > 0 && (
              <ul className="mt-1.5 space-y-1 text-xs text-muted">
                {uses.map((u) => (
                  <li key={u}>{u}</li>
                ))}
              </ul>
            )}
            {secret.kind === "codex_auth" && siblings && (
              <p className="mt-2 text-xs text-muted" data-testid="codex-sibling-note">
                {siblings.total > 0
                  ? `The shared ChatGPT account stays live through ${named(siblings, (s) => `“${sanitizeLabel(s.label)}”`)}: uzi keeps checking its usage.`
                  : "No other enabled login uses this ChatGPT account, so uzi stops checking and refreshing it."}
              </p>
            )}
          </section>

          {needsReplacement && (
            <fieldset aria-labelledby={legendId} className="space-y-2">
              <p id={legendId} className="font-medium text-fg">
                “{safe}” is your default. Choose the new default
                <span className="text-danger" aria-hidden="true">
                  {" "}
                  *
                </span>
              </p>
              <div className="space-y-1.5">
                {replacements.map((r) => (
                  <label
                    key={r.id}
                    className={cx(
                      "flex cursor-pointer items-center gap-2.5 rounded-lg border px-3 py-2",
                      choice === r.id ? "border-brand/60 bg-brand/5" : "border-edge hover:border-edge-strong",
                    )}
                  >
                    <input
                      type="radio"
                      name={`replacement-${secret.id}`}
                      value={r.id}
                      required
                      checked={choice === r.id}
                      onChange={() => setChoice(r.id)}
                      className="h-4 w-4 accent-brand"
                    />
                    <span className="text-fg">Make “{sanitizeLabel(r.label)}” the default</span>
                    {replacementBadges && (
                      <span className="ml-auto flex shrink-0 flex-wrap items-center gap-1.5">{replacementBadges(r)}</span>
                    )}
                  </label>
                ))}
              </div>
            </fieldset>
          )}

          {lastOfSlot && (
            <p className="rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-xs text-fg" data-testid="no-default-consequence">
              {slot === "anthropic"
                ? `This is your last enabled token, so you will have no default Anthropic token. Work that spends your default waits until you enable a token, and new runs that don't name a harness start on Codex if you have a Codex credential.`
                : `This is your last enabled Codex credential, so you will have no default. New runs that don't name a harness start on Claude if you have a Claude token; runs already on this ${noun} wait until you enable it.`}
            </p>
          )}
        </div>
        <div className="flex items-center justify-end gap-2 border-t border-edge px-5 py-3.5">
          <Button variant="ghost" size="sm" disabled={busy} onClick={onClose}>
            Cancel
          </Button>
          <Button size="sm" disabled={!canConfirm} onClick={() => void submit()}>
            {busy ? "Disabling…" : `Disable ${noun}`}
          </Button>
        </div>
      </div>
    </Modal>
  );
}
