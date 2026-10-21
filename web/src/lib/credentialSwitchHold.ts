// A leaf module (no imports): the mock API imports it, and lib/api imports the mock API, so
// it must not reach lib/api (runBadge does) or the mock surface sees a half-initialized cycle.
//
// credentialSwitchHold names the issue #3181 token-switch hold a run is parked in, or null.
// "settling": the owner switched a retained-recovery run's token, the run's processes could
// not be proven stopped, and its worker is still stopping them; the server refuses Resume and
// another token switch (409) until it finishes, while Cancel always works. "unsettled": the
// worker finished without proving them stopped, so the run is held for its owner with its work
// and custody kept on that worker; Resume requeues it on the newly chosen token.
export function credentialSwitchHold(run: { status: string; hold_reason?: string | null }): "settling" | "unsettled" | null {
  if (run.status !== "paused") return null;
  if (run.hold_reason === "credential_switch_settling") return "settling";
  if (run.hold_reason === "credential_switch_unsettled") return "unsettled";
  return null;
}
