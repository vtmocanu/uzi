# Unclaimed run PRs: find their lander

A run PR nobody is landing waits forever: the board shows only claimed PRs. This finds
them and settles who lands each. `S` is `.agents/skills/uzi-lander/scripts/`.

## Rule

- **The dispatching session lands its own run.** It holds the plan, the steering and the
  run's quirks. `uzi-watcher` claims `run-<RUN_ID>` at hand-off; `takeover.sh` stops any
  other session at `NEXT=claimed_by_other` and converts the owner's key to `#<PR>`.
- **An orphan goes to whoever asks first.** A run is an orphan when it has no claim, or
  only a dead owner's (registry-confirmed: an owner the registry cannot verify is not
  dead), and its dispatching session is gone or declines.

## 1. Discover

```sh
S/orphans.sh            # one row per open agent/* or uzi/* PR and per in-flight run without a PR
```

`claimed` and `originator-claimed` rows have a lander: leave them. An `orphan` row needs
step 2. Exit 3 means a lookup failed: re-run it; never treat that as "no orphans".

## 2. Ask, once per round

- **Find the dispatching session.** Read the run's issue and PR comments and the board's
  trail. Ask each live uzi session that may have dispatched it.
- **Address the Claude session by its `[ref]` from `ListAgents`.** A Claude session and
  its Codex buddy can share a name, and a message to the Codex thread reaches the wrong
  session.
- **Send one broadcast per round** naming what you hold and the orphans you see. Ask for a
  one-line reply: `landing: #X, #Y` or `none / hand to lander`.
- **Until it answers, prepare read-only.** Snapshot with `takeover.sh --no-claim` and read
  the plan, diff and CI. Never push or rebase an unclaimed branch.

## 3. Decide

| Answer | Do |
|---|---|
| `landing: #X` | leave it; it claims at its own takeover |
| `none / hand to lander`, or the dispatching session is gone | `takeover.sh` claims it; land it per SKILL.md |
| no reply after about 15 min from a live session | leave it; report it to the user with the session's name |

Name every handover in the trail (`S/trail.sh '#N' 'claimed (orphan)'`).
