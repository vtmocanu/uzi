---
name: issue-triage
description: "Triages one GitHub issue on this repo from backlog to a queued or parked decision. Preflight lists spent one-time issue schedules to delete and open issues from non-maintainers, which go first. Then hunts silently un-sweepable issues (a bug/Planned selector without uzi eligibility, or eligible without a selector), then the un-triaged backlog, then parked brainstorm/Later issues. Explains the issue, recommends a verdict, checks freshness (premise, anchors, referenced PR/PRD merged), and on confirmation applies labels plus a freshness comment. Use when triaging the backlog, finding issues the sweep never fires, or deciding what to send to uzi. Triggers include triage issue, triage the backlog, un-sweepable issues, next issue to implement, should we do this issue, queue an issue for uzi, clean up fired schedules."
---

# Issue triage

One issue per run. GitHub repo `vtmocanu/uzi`: use `gh` only.

Out of scope, read instead:
- Sweep gating and this instance's schedules: `CLAUDE.local.md` → "uzi scheduled jobs", `docs/scheduling.md`, `docs/admin-settings.md#run-eligibility`. Live truth: `uzi schedule list`.
- Dispatching and plan steering: **uzi-watcher**. Landing the PR: **uzi-lander**.

## Step 0: Preflight

Run both checks; report results before Step 1.

**A. Spent one-shot schedules** (fired, still listed as enabled):

```sh
uzi schedule list --json 2>/dev/null | jq -r '.[]
  | select(.timing=="once" and .status=="fired")
  | .id as $id | (.last_fire.started // [])[]
  | "\($id)\t#\(.issue_iid)\t\(.run_id)"'
```

- Per row, check run status (`uzi run get <run_id> --json | jq -r .status`), issue state, and the `agent/issue-<n>` PR.
- Propose delete only if the run is terminal AND the work landed (PR merged or issue closed). Keep the rest.
- On OK: `uzi schedule delete <id>` (run history is preserved).
- Keep stderr out of `jq` pipes (CLI version-skew warning breaks them).

**B. Non-maintainer issues** (triaged before Tier 1; `bot` rows are CI signals, not requests):

```sh
gh issue list --repo vtmocanu/uzi --state open --limit 400 --json number,title,author,labels \
  | jq -r '.[] | select(.author.login != "vtmocanu")
      | (if (.author.login | startswith("app/")) then "bot" else "external" end) as $k
      | "\($k)\t#\(.number)\t\(.author.login)\t[\([.labels[].name]|join(","))]\t\(.title[0:64])"' \
  | sort
```

Empty = maintainer-only backlog. If suspicious, compare against the open-issue total.

## Step 1: Pick

Order: user-named issue → lowest-numbered `external` from 0B → lowest-numbered issue in the highest non-empty tier.

A sweep fires an issue only with BOTH a selector (`Planned`, or `bug`) AND eligibility (`uzi` label OR assigned to the uzi-bot account). Missing either half = looks queued, never runs.

- **1A selector, not eligible**: `bug`/`Planned`, no `uzi`, not bot-assigned.
- **1B eligible, no selector**: `uzi` or bot-assigned, no `bug`/`Planned`.
- **2 untriaged**: no selector, no park label.
- **3 parked** (`brainstorm`/`Later`): propose revisiting only when 1 and 2 are empty.

```sh
# BOT_LOGIN: uzi-bot login from CLAUDE.local.md. Empty = label-only; then verify a 1A hit
# with `gh issue view NNN --json assignees` (bot-assigned = not a gap).
BOT_LOGIN="${BOT_LOGIN:-}"
gh issue list --repo vtmocanu/uzi --state open --json number,title,labels,assignees,body --limit 400 \
  | jq -r --arg bot "$BOT_LOGIN" '
    def park: ["brainstorm","Later","In Progress","Human Review","wontfix","duplicate","invalid"];
    def names: [.labels[].name];
    def has($l): (names | index($l)) != null;
    def selector: (has("bug") or has("Planned"));
    def assigned_to_bot: ($bot != "" and ([.assignees[].login] | index($bot)) != null);
    def fireable: (has("uzi") or assigned_to_bot);
    def parked: ((names) - park) != (names);
    [ .[]
      | (if parked and (has("brainstorm") or has("Later")) then "3:parked"
         elif parked then empty
         elif (selector and (fireable|not)) then "1A:selector-not-eligible"
         elif (fireable and (selector|not)) then "1B:eligible-no-selector"
         elif (selector and fireable) then empty
         else "2:untriaged" end) as $tier
      | select($tier != null)
      | "\($tier)\t#\(.number)\t[\(names|join(","))]\t\(.title[0:64])" ]
    | sort | .[]'
```

Confirm the pick with the user. Gap issues still run Steps 2 to 4: the gap names the missing label, not whether adding it is right.

## Step 2: Explain

- Read issue plus comments: `gh issue view NNN --repo vtmocanu/uzi --json title,body,labels,comments`. A verdict may already be recorded.
- Give a one-paragraph plain summary, plus one line on user-visible change if any.

## Step 3: Recommend

One verdict, one-line reason. Apply only after Step 5 confirmation.

| Verdict | When | Action |
|---|---|---|
| **Send to sweep** | clear value, self-contained, premise holds, no `.github/workflows` | add the missing selector/`uzi`; freshness comment |
| **Do locally** | tiny, must touch `.github/workflows`, or wanted now | in-session or **uzi-watcher**; no sweep labels |
| **Needs design** | open question / competing approaches | `brainstorm`; summarize the fork |
| **Defer** | valid, not now | `Later` |
| **Already done** | premise gone (verified in code) | recommend close; cite code |
| **Not worth it** | duplicate / invalid / out of scope | rationale comment + `wontfix`/`duplicate`/`invalid` |

- Recommend the best-practice option and say why.
- No `prds/*.md` for a spec-in-body issue; `uzi` label suffices.
- Tier 1: add only the missing half (1A → `uzi` or bot assignee; 1B → selector).
- Deliberately deferred gap → **Defer** (`Later`), not sweep completion.

## Step 4: Freshness (sweep/local verdicts only)

Do not trust issue line numbers.

1. **Premise**: grep the target code. Already implemented → **Already done**.
2. **Referenced PR/PRD**: confirm merged (`gh pr view NNN --json state,mergedAt`).
3. **Anchors**: re-grep named symbols; record current locations and omitted/extra sites.
4. **Design forks**: pin a direction with reason; verify any ADR/PRD conflict against code, not the issue's framing.
5. **Workflow scope**: a fix that must touch `.github/workflows/**` cannot go to a sweep (worker PAT lacks `workflow` scope; the whole push is rejected). → **Do locally**, or split into a local-only issue. See `.claude/rules/prds.md`.

## Step 5: Propose, confirm, apply

Labels and comments are public writes: propose first, apply on OK.

```sh
gh issue edit NNN --repo vtmocanu/uzi --add-label "SELECTOR" --add-label "uzi"
gh issue comment NNN --repo vtmocanu/uzi --body "$(cat <<'EOF'
Queued for the nightly SELECTOR sweep (SELECTOR + uzi added; spec-in-body).
[anchor refresh + any pinned design direction from Step 4]
EOF
)"
```

- Bot-assignment path: replace `--add-label "uzi"` with `--add-assignee BOT_LOGIN` (from `CLAUDE.local.md`; never invent it).
- Comment carries Step 4 findings; mirror #525/#509.
- Remind the user: auto-approve runs past the plan gate; a human still merges. For plan review first, use **uzi-watcher** (Auto mode).
