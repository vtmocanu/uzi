# 1732: Local credential enablement

Status: Approved (2026-09-26)

A credential can be locally disabled without deleting its sealed material, bindings, or historical attribution. `disabled_at` is the source of enablement; each real transition increments `enablement_rev`. Repeating the same request preserves both values.

- D1–D2: Disabled credentials cannot be selected for new work or background use. Existing pins remain and their work waits, including Judge work bound to a disabled credential. A new explicit harness selection with no enabled default refuses creation; a new implicit selection may use the existing harness fallback. Runs with a frozen credential or harness wait rather than switch. An empty automatic pool keeps its ordinary pool wait.
- D3: A flight that already owns a valid claim may finish with its pinned credential. Losing the claim ends that exception.
- D4 (amended): Every default is enabled. Anthropic has one slot; Codex authorization and API key aliases share one slot. Disabling a default requires an explicit enabled replacement when another enabled credential exists. Disabling the last enabled credential clears the slot. Re-enabling into an empty slot makes that credential default. Handoff and enablement change commit atomically under the user secret mutation lock.
- D5: New explicit assignments, make-default, and auto-pool opt-in reject disabled credentials. Stored pins and preferences survive.
- D6: A staged enabled Codex alias can reconcile. An account polls and refreshes in background only while an enabled linked alias resolves to it; one disabled sibling does not stop an enabled sibling.
- D7: Enabling a Codex login first attempts coordinated recovery and refresh, then asks for a fresh paste only when recovery fails. An already started refresh completes durably.
- D8: Enablement, auto eligibility, and sidebar preferences are independent. Effective pool membership and sidebar display require enablement.
- D9: Admin live rate-limit views omit disabled credentials entirely. Historical usage and cost are retained.
- D10–D11: The owner-only cookie session can change enablement via PATCH and inspect current dependents via GET. Dependents include workers, schedules, active runs, Judge, default status, and enabled Codex sibling aliases. Lists are bounded cursor pages with totals.
- D12: No new CLI enablement verbs. Existing read views report state and typed waits.
- D13: Poll writes and notifications are fenced by credential identity, enabled state, and the captured revision. Old readings are not current after re-enable.
- D14: Disabled-credential work parks with `hold_reason = 'credential_disabled'`; the server promotes it after enable, preserves budget and custody, and respects other holds.
- D15: For a new explicit harness selection, no enabled default refuses creation. A new implicit harness selection may fall back to another usable harness. Existing runs retain their frozen harness and credential and wait when that credential is disabled.
- D16: Reassignment is offered only on lanes that accept a per-run override.
