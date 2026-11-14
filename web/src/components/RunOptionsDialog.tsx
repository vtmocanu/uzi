// RunOptionsDialog (PRD #3250 M2, module 6): the optional dialog beside one-click Start
// run, carrying the choices one issue run can be started with: harness, Anthropic token,
// model and reasoning effort. Start run on a card or the issue page stays one click with
// defaults (D8); this dialog is the secondary "Options" path.
//
// It is mounted ONCE at the page level (Board, IssueView), keyed by issue iid, never
// inside the draggable IssueCard (D9). The page owns the create flow (startRun.ts's
// startRunWithOptions through `onStart`), so the open-MR confirm and force retry are the
// same code one-click Start runs.
//
// Rows, in order: Harness (only when both harnesses are usable), Anthropic token (hidden
// when the run will start on Codex), Model (curated per effective harness; custom entry
// for Claude only), Reasoning effort. A user harness change resets Model to Default and
// keeps effort; a precondition refresh (the board's 10 s poll) re-evaluates visibility
// but resets nothing, because runStartBody drops a now-hidden value.

import { useEffect, useId, useRef, useState } from "react";
import type { Harness, SecretMeta } from "../lib/api";
import { CODEX_CURATED_MODELS, CODEX_ROLE_MODELS } from "../lib/codexModels";
import { modelFieldWarning } from "../lib/agentTemplates";
import type { HarnessSelection } from "../lib/harnessSelection";
import type { StartRunGate } from "../lib/runStream";
import {
  DEFAULT_RUN_START_OPTIONS,
  startRunRows,
  type RunStartFacts,
  type RunStartOptions,
} from "../lib/startRun";
import { stripUnsafeChars } from "../lib/safeText";
import { EffortSelect } from "./EffortSelect";
import { HarnessPicker } from "./HarnessPicker";
import { ModelSelect } from "./ModelSelect";
import { Modal } from "./Modal";
import { TokenPicker } from "./TokenPicker";
import { XIcon } from "./icons";
import { Button, Field } from "./ui";

// Both selects name where a Default value really comes from: the caller's own Settings,
// read when a worker claims the run. U+00B7 is the agreed separator.
const DEFAULT_LABEL = "Default · your settings";
const MODEL_MAX_BYTES = 100;

// modelProblem is the inline check that disables Start run. The server is authoritative
// (it answers 400 or 422 and the dialog shows that text), but these are the refusals a
// person can see coming: an empty or whitespace-bearing "Other" entry (the server does not
// trim a start-time model), a control or format character, a model over the byte bound,
// a Codex role model on Claude (C's guard), and a non-curated model on Codex. The "start
// the run on Codex" remedy is offered only when the Harness row is shown (canSwitchHarness).
function modelProblem(model: string, custom: boolean, harness: Harness, canSwitchHarness: boolean): string {
  if (model === "") return custom ? "Enter a model ID, or choose Default." : "";
  if (/\s/u.test(model)) return "A model ID is a single word with no spaces.";
  const shape = modelFieldWarning(model);
  if (shape) return shape;
  if (new TextEncoder().encode(model).length > MODEL_MAX_BYTES) {
    return `Model is too long (max ${MODEL_MAX_BYTES} bytes); use a shorter alias or model ID.`;
  }
  if (harness === "claude" && CODEX_ROLE_MODELS.includes(model)) {
    return canSwitchHarness
      ? `${model} is a Codex model. Choose a Claude model, or start the run on Codex.`
      : `${model} is a Codex model. Choose a Claude model.`;
  }
  if (harness === "codex" && !CODEX_CURATED_MODELS.includes(model)) {
    return `${model} is not a Codex model. Choose one from the list, or Default.`;
  }
  return "";
}

export interface RunOptionsStartCallbacks {
  // Show a server refusal in the dialog's alert region; the dialog stays open.
  onError: (message: string) => void;
  // Every non-navigating outcome (a refusal, a declined open-MR confirm): submitting ends.
  onSettled: () => void;
}

export function RunOptionsDialog({
  issueIid,
  issueTitle,
  facts,
  gate,
  tokens,
  onStart,
  onClose,
}: {
  issueIid: number;
  issueTitle?: string;
  // The page's harness-availability facts. They are live: the board's precondition poll
  // updates them while the dialog is open, and row visibility follows.
  facts: RunStartFacts;
  // The page's start gate for the harness the dialog would start on.
  gate: (harness: HarnessSelection) => StartRunGate;
  // The caller's Anthropic tokens, already loaded by the page.
  tokens: SecretMeta[];
  onStart: (options: RunStartOptions, callbacks: RunOptionsStartCallbacks) => void | Promise<void>;
  onClose: () => void;
}) {
  const [options, setOptions] = useState<RunStartOptions>(DEFAULT_RUN_START_OPTIONS);
  const [modelCustom, setModelCustom] = useState(false);
  // Bumped on a USER harness change so the model picker remounts on Default, including
  // from an empty "Other" entry (which emits the same "" a Default pick does).
  const [modelKey, setModelKey] = useState(0);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const ids = useId();
  const startId = `${ids}-start`;
  const gateId = `${ids}-gate`;
  const modelErrorId = `${ids}-model-error`;
  const defaultsHintId = `${ids}-defaults`;
  // Disabling the focused Start button while submitting drops focus to <body>, outside
  // the dialog's Tab trap. When a start settles without navigating, return focus to Start
  // so a keyboard user lands back in the dialog, next to the refusal it announces.
  const wasSubmitting = useRef(false);
  useEffect(() => {
    if (wasSubmitting.current && !submitting) {
      const active = document.activeElement;
      if (!active || active === document.body) document.getElementById(startId)?.focus();
    }
    wasSubmitting.current = submitting;
  }, [submitting, startId]);

  const rows = startRunRows(options, facts);
  const startGate = gate(rows.harness);
  const modelError = modelProblem(options.model, modelCustom, rows.effectiveHarness, rows.showHarness);
  const canStart = startGate.enabled && modelError === "" && !submitting;

  const patch = (next: Partial<RunStartOptions>) => setOptions((prev) => ({ ...prev, ...next }));

  const onHarnessChange = (harness: HarnessSelection) => {
    // The model vocabularies differ per harness, so a user harness change returns Model
    // to Default; the effort levels are shared and stay.
    setOptions((prev) => ({ ...prev, harness, model: "" }));
    setModelCustom(false);
    setModelKey((k) => k + 1);
  };

  const start = async () => {
    if (!canStart) return;
    setError("");
    setSubmitting(true);
    await onStart(options, {
      onError: (message) => setError(message),
      onSettled: () => setSubmitting(false),
    });
  };

  // While a start is in flight neither Escape, the close button nor a backdrop click can
  // dismiss the dialog, so its outcome always lands somewhere visible.
  const close = submitting ? () => {} : onClose;
  const describedBy = [!startGate.enabled ? gateId : "", modelError ? modelErrorId : ""].filter(Boolean).join(" ");

  return (
    <Modal label={`Run options for #${issueIid}`} onClose={close} closeOnBackdrop={!submitting}>
      <div className="my-8 w-full max-w-md overflow-hidden rounded-2xl border border-edge-strong bg-surface shadow-2xl">
        <div className="flex items-start justify-between gap-3 border-b border-edge px-5 py-4">
          <div className="min-w-0">
            <h2 className="text-base font-semibold">Run options</h2>
            <p className="mt-0.5 truncate text-xs text-muted">
              #{issueIid}
              {issueTitle ? ` ${stripUnsafeChars(issueTitle)}` : ""}
            </p>
          </div>
          <button
            type="button"
            onClick={close}
            disabled={submitting}
            aria-label="Close"
            className="rounded-md p-1 text-muted hover:bg-raised hover:text-fg disabled:opacity-50"
          >
            <XIcon />
          </button>
        </div>

        <form
          className="space-y-4 px-5 py-5"
          onSubmit={(e) => {
            e.preventDefault();
            void start();
          }}
        >
          {rows.showHarness && (
            <Field label="Harness" htmlFor={`${ids}-harness`}>
              <HarnessPicker
                id={`${ids}-harness`}
                label="Harness"
                value={options.harness}
                onChange={onHarnessChange}
                disabled={submitting}
              />
            </Field>
          )}
          {rows.showToken && (
            <Field label="Anthropic token" htmlFor={`${ids}-token`}>
              <TokenPicker
                id={`${ids}-token`}
                label="Anthropic token"
                value={options.credential}
                onChange={(credential) => patch({ credential })}
                tokens={tokens}
                disabled={submitting}
              />
            </Field>
          )}
          <Field label="Model" htmlFor={`${ids}-model`}>
            <ModelSelect
              key={modelKey}
              id={`${ids}-model`}
              value={options.model}
              onChange={(model) => patch({ model })}
              harness={rows.effectiveHarness}
              defaultLabel={DEFAULT_LABEL}
              customAriaLabel="Custom model ID"
              ariaDescribedBy={[modelError ? modelErrorId : "", defaultsHintId].filter(Boolean).join(" ")}
              onCustomModeChange={setModelCustom}
            />
            {modelError && (
              <p id={modelErrorId} className="text-xs text-danger">
                {modelError}
              </p>
            )}
          </Field>
          <Field label="Reasoning effort" htmlFor={`${ids}-effort`}>
            <EffortSelect
              id={`${ids}-effort`}
              value={options.effort}
              onChange={(effort) => patch({ effort })}
              defaultLabel={DEFAULT_LABEL}
            />
          </Field>
          <p id={defaultsHintId} className="text-xs text-faint">
            A setting left on Default follows your Settings when a worker picks the run up. A choice made here stays
            with this run, including after a resume.
          </p>

          {/* Server refusals (a 422 naming the harness and remedies, or a 400 from an
              older api during a roll) land here; the dialog stays open, choices kept. */}
          <div role="alert" aria-live="assertive">
            {error && (
              <div className="rounded-lg border border-danger/40 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>
            )}
          </div>

          <div className="flex flex-wrap items-center justify-end gap-2 border-t border-edge pt-4">
            {!startGate.enabled && (
              <p id={gateId} className="mr-auto text-xs text-faint">
                {startGate.reason}
              </p>
            )}
            <Button type="button" variant="ghost" onClick={close} disabled={submitting}>
              Cancel
            </Button>
            <Button id={startId} type="submit" disabled={!canStart} aria-describedby={describedBy || undefined}>
              {submitting ? "Starting…" : "Start run"}
            </Button>
          </div>
        </form>
      </div>
    </Modal>
  );
}
