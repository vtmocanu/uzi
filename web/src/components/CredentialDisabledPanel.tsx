// CredentialDisabledPanel — the run view's panel for a run HELD on `credential_disabled`
// (PRD #1732 D14): status `paused`, parked by the server because a credential it needs is
// disabled. It never spends another credential on its own (D2), and it resumes by itself
// once that credential is enabled (or, where the lane allows, once the owner points it at
// another token). A plain Resume would only park it again, so this panel replaces the
// owner-pause PausedPanel for the hold.
//
// Which credential? The run DTO carries the hold reason but not the credential it waits
// on, so the panel resolves it from what the run names, in the server's own order: a Codex
// run's frozen alias (D6), else the per-run override's pinned token (PRD #1247), else the
// worker's pinned binding, else the token the run last spent. None of them disabled (for
// example work that spends the default while the slot has no default) sends the owner to
// Settings, where the no-default notice and "Enable and make default" live.
//
// D16: "Run with another token" appears only for a lane that accepts a per-run override:
// never for chat, the Judge, self-improve or task review (isCredentialSwitchRefusedLane),
// and never for a Codex run (the override is Anthropic-only). Those get Enable plus the way
// to change the default or binding in Settings.

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { api, type Run, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { isCredentialSwitchRefusedLane } from "../lib/credentialOverride";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import { SwitchTokenAction } from "./RunCredentialOverride";
import { Alert, Button } from "./ui";

export function isCredentialDisabledHold(run: Pick<Run, "status" | "hold_reason">): boolean {
  return run.status === "paused" && run.hold_reason === "credential_disabled";
}

// heldCredential picks the disabled credential this run waits on, or null when none of
// the credentials it names is disabled (the slot-has-no-default case).
export function heldCredential(
  run: Pick<Run, "harness" | "codex_secret_id" | "credential_override" | "anthropic_secret_id">,
  secrets: SecretMeta[],
  workerSecretId: string | null,
): SecretMeta | null {
  const disabled = (s: SecretMeta | undefined) => (s && s.enabled === false ? s : null);
  if (run.harness === "codex") return disabled(secrets.find((s) => s.id === run.codex_secret_id));
  const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
  const override = run.credential_override;
  if (override?.mode === "pinned" && override.label) {
    return disabled(anthropic.find((s) => s.label === override.label));
  }
  return (
    disabled(anthropic.find((s) => s.id === workerSecretId)) ??
    disabled(anthropic.find((s) => s.id === run.anthropic_secret_id))
  );
}

// laneLabel names a lane with no per-run token switch, for the "why no switch" line.
function laneLabel(run: Pick<Run, "kind" | "trigger_source" | "harness">): string {
  if (run.harness === "codex") return "A Codex run";
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

export function CredentialDisabledPanel({
  run,
  canSteer,
  onChanged,
}: {
  run: Run;
  canSteer: boolean;
  // Refetch the run after an Enable or a token switch.
  onChanged: () => void | Promise<void>;
}) {
  const held = isCredentialDisabledHold(run);
  const [secrets, setSecrets] = useState<SecretMeta[] | null>(null);
  const [workerSecretId, setWorkerSecretId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [enabledLabel, setEnabledLabel] = useState("");

  useEffect(() => {
    if (!held || !canSteer) return;
    let cancelled = false;
    void (async () => {
      try {
        const [{ secrets: rows }, workers] = await Promise.all([
          api.listSecrets(),
          run.worker_id
            ? api.listWorkers().then((r) => r.workers).catch(() => [])
            : Promise.resolve([]),
        ]);
        if (cancelled) return;
        setSecrets(rows);
        setWorkerSecretId(workers.find((w) => w.id === run.worker_id)?.anthropic_secret_id ?? null);
      } catch {
        if (!cancelled) setSecrets([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [held, canSteer, run.worker_id]);

  if (!held) return null;

  const target = secrets ? heldCredential(run, secrets, workerSecretId) : null;
  const safe = target ? sanitizeLabel(target.label) : "";
  const canSwitch = !isCredentialSwitchRefusedLane(run) && run.harness !== "codex";

  const enable = async () => {
    if (!target) return;
    setBusy(true);
    setError("");
    try {
      await api.setSecretEnabled(target.kind, target.id, true);
      setEnabledLabel(safe);
      emitSidebarTokensChanged();
      await onChanged();
    } catch (err) {
      setError(errorMessage(err, `Failed to enable “${safe}”`));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="rounded-xl border border-warn/40 bg-warn/10 p-4" data-testid="credential-disabled-panel">
      <p className="text-sm font-semibold text-warn">Waiting: credential disabled</p>
      <p className="mt-1 text-sm text-fg">
        {target
          ? `This run needs “${safe}”, which is disabled.`
          : "A credential this run needs is disabled, or your default slot has none enabled."}{" "}
        uzi never switches it to another credential on its own. It resumes by itself once
        {target ? " you enable it" : " the credential is enabled"}
        {canSwitch ? " or run it with another token" : ""}, and its time budget does not run down
        while it waits.
      </p>
      {enabledLabel && (
        <p className="mt-2 text-xs text-ok" role="status">
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
          {target && (
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
            Manage tokens in Settings
          </Link>
        </div>
      ) : (
        <p className="mt-2 text-xs text-muted">Only the run&rsquo;s owner can enable the credential it needs.</p>
      )}
      {canSteer && !canSwitch && (
        <p className="mt-2 text-xs text-muted" data-testid="no-reassign-note">
          {laneLabel(run)} can&rsquo;t switch to another token. Enable the credential, or change
          your default or binding in Settings.
        </p>
      )}
    </div>
  );
}
