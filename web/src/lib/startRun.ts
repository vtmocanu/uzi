// Shared start-run flow for the two create sites (IssueView, Board), so the
// credential-override plumbing and the open-MR force-retry live in ONE place
// (PRD #1247 M7). The two pages keep their own state/settle behaviour via the
// handler callbacks; this owns only the create → open-MR confirm → force-retry
// control flow.
//
// The force-retry preservation is structural: the chosen override is captured once
// in the `createAndOpen` closure below, so the retry after the user confirms the
// open-MR overwrite re-sends the SAME override rather than dropping it.

import { api, ApiError, isOpenMRConflict, openMRConflictMRIID } from "./api";
import { errorMessage } from "./apiError";
import { type CredentialSelection, runCredentialBody } from "./credentialOverride";

export interface StartRunHandlers {
  // Navigate to the created run (each site encodeURIComponent-guards the id itself).
  onCreated: (runId: string) => void;
  // Surface a start error on the page banner.
  onError: (message: string) => void;
  // Runs on every non-navigating outcome (declined confirm, forced-retry failure,
  // plain failure). Each site clears its own `starting` flag and reloads here — and
  // IssueView additionally clears the error, matching its pre-#1247 behaviour.
  onSettled: () => void;
}

// startRunWithCredential creates a run for (repoId, issueIid) carrying the chosen
// credential override, and — on an open-MR conflict (issue #856) — confirms the
// overwrite and retries with force AND the same override. inherit sends no override
// (runCredentialBody returns undefined), so an inherit start is byte-identical to the
// pre-#1247 body and the run follows the worker binding.
export async function startRunWithCredential(
  repoId: string,
  issueIid: number,
  selection: CredentialSelection,
  handlers: StartRunHandlers,
): Promise<void> {
  const override = runCredentialBody(selection);
  const createAndOpen = async (force?: boolean) => {
    // `override` is captured here, so the force-retry below re-sends it unchanged. For an
    // inherit choice (override undefined) call the 3-arg form, so an inherit start is
    // byte-identical to the pre-#1247 call (and the wire body omits the field either way).
    const { run } =
      override === undefined
        ? await api.createRun(repoId, issueIid, force)
        : await api.createRun(repoId, issueIid, force, override);
    handlers.onCreated(run.id);
  };
  try {
    await createAndOpen();
  } catch (err) {
    // issue_has_open_mr: a completed prior run still owns an open MR. Compose a
    // web-specific confirm naming the MR (no --force jargon); confirm, then retry with
    // force — preserving the override.
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
