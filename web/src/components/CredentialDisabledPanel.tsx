// CredentialDisabledPanel — the run view's panel for a run HELD on `credential_disabled`
// (PRD #1732 D14): status `paused`, parked by the server because a credential it needs is
// disabled. It never spends another credential on its own (D2), and it resumes by itself
// once that credential is enabled (or, where the lane allows, once the owner points it at
// another token). A plain Resume would only park it again, so this panel replaces the
// owner-pause PausedPanel for the hold.
//
// Which credential? The run DTO carries the hold reason but not the credential it waits
// on, so heldCredential resolves it from what the run names, in the server's own order
// (workersvc claimSecretID / runOverrideChoice / judgeChoice): a Codex run's frozen alias
// (D6); for the Judge and self-improve, the owner's judge binding; for a chat, the
// default; otherwise the per-run override (PRD #1247), which wins over the worker even
// when it says "default" or "auto"; else the claiming worker's pinned binding. A lane that
// resolves to the DEFAULT waits only when the slot has no default at all (every token
// disabled, D4): the panel then offers the token the run last spent, because enabling any
// token into an empty slot makes it the default. Anything else sends the owner to
// Settings.
//
// D16: "Run with another token" appears only for a lane that accepts a per-run override:
// never for chat, the Judge, self-improve or task review (isCredentialSwitchRefusedLane),
// and never for a Codex run (the override is Anthropic-only). The Judge and self-improve
// get Enable plus the way to change the default or judge token in Settings; a Codex run
// is tied to its login, so it gets Enable (or cancelling the run).

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type Run, type SecretMeta, type Worker } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { isCredentialSwitchRefusedLane } from "../lib/credentialOverride";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import { SwitchTokenAction } from "./RunCredentialOverride";
import { Alert, Button } from "./ui";

export function isCredentialDisabledHold(run: Pick<Run, "status" | "hold_reason">): boolean {
  return run.status === "paused" && run.hold_reason === "credential_disabled";
}

// The bindings a run's lane can resolve through, read by the panel: the claiming worker's
// PINNED token (an unbound or auto worker names none) and the owner's judge token.
export interface HeldBindings {
  workerSecretId: string | null;
  judgeSecretId: string | null;
}

// The credential a held run waits on. viaDefault: the run spends the default and the slot
// has none, so this is the token it last spent, offered because enabling it restores a
// default (not because the run is pinned to it).
export interface HeldCredential {
  secret: SecretMeta;
  viaDefault: boolean;
}

type HeldRun = Pick<
  Run,
  | "kind"
  | "harness"
  | "codex_secret_id"
  | "credential_override"
  | "anthropic_secret_id"
>;

// heldCredential picks the disabled credential this run waits on, or null when none of
// the credentials its lane resolves to is disabled.
export function heldCredential(run: HeldRun, secrets: SecretMeta[], bindings: HeldBindings): HeldCredential | null {
  const isOff = (s: SecretMeta | undefined): s is SecretMeta => !!s && s.enabled === false;
  if (run.harness === "codex") {
    const alias = secrets.find((s) => s.id === run.codex_secret_id);
    return isOff(alias) ? { secret: alias, viaDefault: false } : null;
  }
  const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
  const pinnedTo = (s: SecretMeta | undefined): HeldCredential | null =>
    isOff(s) ? { secret: s, viaDefault: false } : null;
  const byId = (id: string | null) => (id ? anthropic.find((s) => s.id === id) : undefined);
  // The default lane holds only when the slot has no enabled default (D4); then the token
  // the run last spent is the natural one to bring back.
  const defaultLane = (): HeldCredential | null => {
    if (anthropic.some((s) => s.enabled !== false && s.is_default)) return null;
    const spent = byId(run.anthropic_secret_id);
    return isOff(spent) ? { secret: spent, viaDefault: true } : null;
  };

  // The Judge and self-improve follow the judge binding (checked first on the server, so
  // no override or worker binding ever applies to them); chat spends the default.
  if (run.kind === "judge" || run.kind === "self_improve") {
    return bindings.judgeSecretId ? pinnedTo(byId(bindings.judgeSecretId)) : defaultLane();
  }
  if (run.kind === "chat") return defaultLane();

  const override = run.credential_override;
  switch (override?.mode) {
    case "pinned":
      // GetRun resolves the override's label LIVE from the override's secret id
      // (runs_lifecycle.go), so the label always names the token the run is pinned to now
      // (a rename follows it; labels are unique per kind). Match on it among the Anthropic
      // tokens. A pin with no label is a deleted token: the server resolves it as inherit,
      // so fall through.
      if (override.label) return pinnedTo(anthropic.find((s) => s.label === override.label));
      break;
    case "default":
    case "auto":
      // The override decides; the worker binding never applies (runOverrideChoice).
      return defaultLane();
  }
  return bindings.workerSecretId ? pinnedTo(byId(bindings.workerSecretId)) : defaultLane();
}

// workerPin is the token a worker's run-lane claims are PINNED to, mirroring the server's
// workerSecretId: only a pinned bind mode names one.
function workerPin(w: Worker | undefined): string | null {
  if (!w || w.anthropic_bind_mode !== "pinned") return null;
  return w.anthropic_secret_id;
}

// laneLabel names a lane with no per-run token switch, for the "why no switch" line.
function laneLabel(run: Pick<Run, "kind">): string {
  switch (run.kind) {
    case "chat":
      return "A chat";
    case "judge":
      return "A retrospective";
    case "self_improve":
      return "A self-improvement run";
    default:
      return "A task review";
  }
}

// noSwitchNote is the line under the actions for a lane with no per-run token switch:
// what the owner CAN do there instead.
function noSwitchNote(run: Pick<Run, "kind" | "harness">): string {
  if (run.harness === "codex") {
    return "A Codex run stays on the login it started with. Enable it, or cancel the run if you no longer need it.";
  }
  switch (run.kind) {
    case "judge":
    case "self_improve":
      return `${laneLabel(run)} can’t switch to another token. Enable the token, or change your judge token or default in Settings.`;
    case "chat":
      return `${laneLabel(run)} can’t switch to another token. Enable the token, or make another token your default in Settings.`;
    default:
      return `${laneLabel(run)} can’t switch to another token. Enable the token, or change your default or the worker’s binding in Settings.`;
  }
}

export function CredentialDisabledPanel({
  run,
  canSteer,
  onChanged,
  onEnabled,
}: {
  run: Run;
  canSteer: boolean;
  // Refetch the run after an Enable or a token switch.
  onChanged: () => void | Promise<void>;
  // Told after a successful Enable, with the (sanitized) label. The run view announces it
  // from its always-mounted live region and moves focus, because this panel unmounts as
  // soon as the refetched run is no longer held.
  onEnabled?: (label: string) => void;
}) {
  const held = isCredentialDisabledHold(run);
  const [secrets, setSecrets] = useState<SecretMeta[] | null>(null);
  const [bindings, setBindings] = useState<HeldBindings>({ workerSecretId: null, judgeSecretId: null });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [enabledLabel, setEnabledLabel] = useState("");
  const judgeLane = run.kind === "judge" || run.kind === "self_improve";

  useEffect(() => {
    if (!held || !canSteer) return;
    let cancelled = false;
    void (async () => {
      try {
        const [{ secrets: rows }, workers, judgeSecretId] = await Promise.all([
          api.listSecrets(),
          run.worker_id && !judgeLane
            ? api
                .listWorkers()
                .then((r) => r.workers)
                .catch((): Worker[] => [])
            : Promise.resolve([] as Worker[]),
          // The Judge and self-improve spend the owner's judge binding, read fresh.
          judgeLane
            ? api
                .me()
                .then(({ user }) =>
                  user.judge_anthropic_bind_mode === "pinned" ? user.judge_anthropic_secret_id : null,
                )
                .catch(() => null)
            : Promise.resolve(null),
        ]);
        if (cancelled) return;
        setSecrets(rows);
        setBindings({ workerSecretId: workerPin(workers.find((w) => w.id === run.worker_id)), judgeSecretId });
      } catch {
        if (!cancelled) setSecrets([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [held, canSteer, run.worker_id, judgeLane]);

  if (!held) return null;

  const found = secrets ? heldCredential(run, secrets, bindings) : null;
  const target = found?.secret ?? null;
  const safe = target ? sanitizeLabel(target.label) : "";
  const canSwitch = !isCredentialSwitchRefusedLane(run) && run.harness !== "codex";
  const codex = run.harness === "codex";

  const enable = async () => {
    if (!target) return;
    setBusy(true);
    setError("");
    try {
      await api.setSecretEnabled(target.kind, target.id, true);
      setEnabledLabel(safe);
      emitSidebarTokensChanged();
      onEnabled?.(safe);
      await onChanged();
    } catch (err) {
      setError(errorMessage(err, `Failed to enable “${safe}”`));
    } finally {
      setBusy(false);
    }
  };

  const needs = !target
    ? "A credential this run needs is disabled, or your default slot has none enabled."
    : found?.viaDefault
      ? `This run spends your default token, and every token is disabled. Enabling “${safe}” makes it your default again.`
      : codex
        ? `This run is tied to its Codex login “${safe}”, which is disabled.`
        : `This run needs “${safe}”, which is disabled.`;

  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4" data-testid="credential-disabled-panel">
      <p className="text-sm font-semibold text-warn">Waiting: credential disabled</p>
      <p className="mt-1 text-sm text-fg">
        {needs} uzi never switches it to another credential on its own. It resumes by itself once
        {target ? " you enable it" : " the credential is enabled"}
        {canSwitch ? " or run it with another token" : ""}, and its time budget does not run down
        while it waits.
      </p>
      {/* Not a live region: the run view announces the Enable from its always-mounted
          region, since this panel unmounts once the run resumes. */}
      {enabledLabel && (
        <p className="mt-2 text-xs text-ok" data-testid="credential-enabled-note">
          Enabled “{enabledLabel}”. The run resumes in a moment.
        </p>
      )}
      {error && (
        <div className="mt-2">
          <Alert message={error} />
        </div>
      )}
      {canSteer ? (
        <div className="mt-3 flex flex-wrap items-start gap-2">
          {target && !enabledLabel && (
            <Button size="sm" disabled={busy} onClick={() => void enable()}>
              {busy ? "Enabling…" : `Enable ${safe}`}
            </Button>
          )}
          {canSwitch && (
            <SwitchTokenAction run={run} canSteer={canSteer} onSwitched={() => void onChanged()} triggerLabel="Run with another token" />
          )}
          <Link
            to="/settings"
            className="inline-flex h-7 items-center rounded-lg px-2.5 text-xs font-medium text-muted hover:bg-raised hover:text-fg"
          >
            {codex ? "Manage Codex credentials in Settings" : "Manage tokens in Settings"}
          </Link>
        </div>
      ) : (
        <p className="mt-2 text-xs text-muted">Only the run&rsquo;s owner can enable the credential it needs.</p>
      )}
      {canSteer && !canSwitch && (
        <p className="mt-2 text-xs text-muted" data-testid="no-reassign-note">
          {noSwitchNote(run)}
        </p>
      )}
    </div>
  );
}
