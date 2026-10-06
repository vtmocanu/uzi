---
title: Cross-check
order: 86
audience: user
---

# Cross-check

Cross-check asks the other model family for a second opinion on a run. Its two stages have different jobs:

- **Plan cross-check** checks an auto-approved plan before implementation. It is a required gate for runs that have it enabled. A check that requests changes, blocks the plan, or cannot finish sends the run to a human plan gate. The owner can then approve, request changes, or reject the plan.
- **Code cross-check** is a planned, separate opt-in stage before publication. It will review the finished code and provide advisory findings. It is not available in this release; enabling Plan cross-check does not enable Code cross-check.

## Plan cross-check in this release

In **Settings → Run defaults → Cross-check**, enable **Plan cross-check** for your account. It is off by default and requires usable Claude and Codex credentials. The setting applies to new auto-approved runs; it does not change runs already created or human-gated runs. It covers issue, prompt, self-improvement, CI fix, and merge request rework runs whose plans go through the auto-approval gate. Gateless tasks and plans supplied at creation are outside this gate.

This release establishes the required plan gate, but its checker is not connected yet. A new run that requires Plan cross-check waits in the queue until a worker advertising `cross_check_v1` can claim it. On a Claude lead, that worker parks the run at the human plan gate with **`plan cross-check: checker unavailable`**. An opted-in Codex lead parks with **`plan cross-check: not yet supported for a Codex lead`**. The run cannot implement an unchecked plan. Review the recorded plan there and approve, request changes, or reject it. Turning the setting off affects future runs only.

The next integration stage will check **Claude-lead plans on Codex**. An exact approval will let implementation proceed; a request for changes, block, timeout, or failed check will park the lead for a human decision. An opted-in **Codex-lead** run will park with **`plan cross-check: not yet supported for a Codex lead`** until the Codex-lead direction is implemented. It will not implement an unchecked plan. Code cross-check is a later feature with its own setting.

For a parked run, open its plan gate in uzi to make the decision. Slack shows the cross-check **reason only** on the gate card, not checker findings. The detailed findings and child-run link arrive with the checker integration; they are not present in this release.
