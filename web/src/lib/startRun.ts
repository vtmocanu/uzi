// Shared start-run flow for the two create sites (IssueView, Board), so the start-option
// body shaping and the open-MR force-retry live in ONE place (PRD #1247 M7, PRD #3250
// M2). The two pages keep their own state/settle behaviour via the handler callbacks;
// this owns only the options → body → create → open-MR confirm → force-retry flow.
//
// The force-retry preservation is structural: the body is built ONCE in
// startRunWithOptions below and captured in the `createAndOpen` closure, so the retry after
// the user confirms the open-MR overwrite re-sends the SAME harness, token, model and
// effort rather than dropping any of them.

import { api, ApiError, isOpenMRConflict, openMRConflictMRIID, type Harness, type RunStartBody } from "./api";
import { errorMessage } from "./apiError";
import { type CredentialSelection, INHERIT_SELECTION, runCredentialBody } from "./credentialOverride";
import {
  bothHarnessesUsable,
  effectiveHarnessIsCodex,
  INHERIT_HARNESS,
  type HarnessSelection,
  runHarnessBody,
} from "./harnessSelection";

// RunStartOptions is everything a person can choose when starting one issue run (PRD
// #3250 module 6): the Run options dialog edits one of these, and one-click Start run
// sends DEFAULT_RUN_START_OPTIONS. "" for model or effort means Default (the claim-time
// setting applies). #3151 adds its Automation fields here and to runStartBody.
export interface RunStartOptions {
  harness: HarnessSelection;
  credential: CredentialSelection;
  model: string;
  effort: string;
}

export const DEFAULT_RUN_START_OPTIONS: RunStartOptions = {
  harness: INHERIT_HARNESS,
  credential: INHERIT_SELECTION,
  model: "",
  effort: "",
};

// RunStartFacts are the page's harness-availability facts, refreshed by the board's 10 s
// precondition poll. They decide which dialog rows are shown, and so which values the
// builder may send.
export interface RunStartFacts {
  claudeUsable: boolean;
  codexUsable: boolean;
  defaultHarness: Harness | null;
}

// startRunRows answers which rows of the Run options dialog are visible for these options
// and facts, and the harness the run will effectively start on. The harness picker shows
// only when both harnesses are usable (D2); a hidden picker contributes "inherit". The
// Anthropic token row hides when the run will effectively start on Codex.
export function startRunRows(options: RunStartOptions, facts: RunStartFacts) {
  const showHarness = bothHarnessesUsable(facts.claudeUsable, facts.codexUsable);
  const harness = showHarness ? options.harness : INHERIT_HARNESS;
  const startsOnCodex = effectiveHarnessIsCodex(harness, facts.claudeUsable, facts.codexUsable, facts.defaultHarness);
  return {
    showHarness,
    // The selection that is actually in effect (a hidden picker's stale value is not).
    harness,
    showToken: !startsOnCodex,
    effectiveHarness: (startsOnCodex ? "codex" : "claude") as Harness,
  };
}

// runStartBody is the ONE body builder (PRD #3250 module 6). It omits every Default value
// and every value whose row is hidden for the effective harness: the token when the run
// will start on Codex, and the harness when its picker is not shown. A poll that hides a
// row therefore drops its value here instead of resetting the dialog. The default options
// build `{}`, so one-click Start run sends today's body.
export function runStartBody(options: RunStartOptions, facts: RunStartFacts): RunStartBody {
  const rows = startRunRows(options, facts);
  const body: RunStartBody = {};
  const credential = rows.showToken ? runCredentialBody(options.credential) : undefined;
  if (credential) body.credential_override = credential;
  const harness = rows.showHarness ? runHarnessBody(options.harness) : undefined;
  if (harness) body.harness = harness;
  if (options.model !== "") body.model = options.model;
  if (options.effort !== "") body.effort = options.effort;
  return body;
}

export interface StartRunHandlers {
  // Navigate to the created run (each site encodeURIComponent-guards the id itself).
  onCreated: (runId: string) => void;
  // Surface a start error (the page banner for one-click Start, the dialog's alert region
  // when started from Run options).
  onError: (message: string) => void;
  // Runs on every non-navigating outcome (declined confirm, forced-retry failure,
  // plain failure). Each site clears its own `starting` flag and reloads here. It runs
  // AFTER onError on a failure, so it must not clear the error just surfaced (#1727).
  onSettled: () => void;
}

// startRunWithOptions creates a run for (repoId, issueIid) carrying the options' body and,
// on an open-MR conflict (issue #856), confirms the overwrite and retries with force and
// the same body.
export async function startRunWithOptions(
  repoId: string,
  issueIid: number,
  options: RunStartOptions,
  facts: RunStartFacts,
  handlers: StartRunHandlers,
): Promise<void> {
  const body = runStartBody(options, facts);
  const createAndOpen = async (force?: boolean) => {
    // `body` is captured here, so the force-retry below re-sends it unchanged.
    const { run } = force ? await api.createRun(repoId, issueIid, body, true) : await api.createRun(repoId, issueIid, body);
    handlers.onCreated(run.id);
  };
  try {
    await createAndOpen();
  } catch (err) {
    // issue_has_open_mr: a completed prior run still owns an open MR. Compose a
    // web-specific confirm naming the MR (no --force jargon); confirm, then retry with
    // force, preserving the body.
    if (isOpenMRConflict(err) && err instanceof ApiError) {
      const mr = openMRConflictMRIID(err);
      const detail = mr != null ? `an open merge request (!${mr})` : "an open merge request";
      const proceed = window.confirm(
        `This issue already has ${detail} from a completed run. Starting a new run will plan and review it again from scratch. Start a new run anyway?`,
      );
      if (proceed) {
        try {
          await createAndOpen(true);
          return;
        } catch (retryErr) {
          handlers.onError(errorMessage(retryErr, "Could not start run"));
        }
      }
      handlers.onSettled();
      return;
    }
    handlers.onError(errorMessage(err, "Could not start run"));
    handlers.onSettled();
  }
}
