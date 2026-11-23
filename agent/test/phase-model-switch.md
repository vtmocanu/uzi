# Phase model spike (#3246)

These are maintainer-only probes. They are not gate targets and never run from CI
or uzi. The ordinary characterization tests use dummy credentials and loopback
providers. They pin SDK 0.3.325 (bundled CLI 2.1.325) and Codex 0.169.0.

Run from this worktree's `agent/` directory. The user supplies credentials at run
time; neither command reads or prints a credential file in the calling session.

```sh
UZI_PHASE_MODEL_LIVE=1 node --import tsx test/phase-model-live-claude.probe.ts --credential-env CLAUDE_CODE_OAUTH_TOKEN --model-a opus --model-b sonnet --effort-a high --effort-b medium
UZI_PHASE_MODEL_LIVE=1 CODEX_HOME="$HOME/.config/uzi-probe/codex-home" node --import tsx ../e2e/codex-m4/phase-model-live-codex.probe.ts --credential-env CODEX_HOME --model-a gpt-6-astra --model-b gpt-6.1-sol --effort-a high --effort-b medium
```

Both require the exact opt-in and the named variable. Each group has at most four
turns; the whole probe has a five-minute wall cap. Claude's provider output limit
is 256 tokens, verified by the offline request assertion. Codex interrupts at a
2 KiB combined observed text/reasoning output limit, with a 30-second turn cap;
this is a client output limit, not a provider-side billing quota. Both reports are
bounded to 64 KiB. Errors are redacted. No worker tool callbacks are exposed or
serviced. Codex uses production native-disabled config overrides and empty
execution environments; the accepted authority-free native exceptions remain.

The omitted-model group first measures a fresh default and chooses an explicit
nondefault model from the supplied A/B pair before omitting it on resume. This
avoids mistaking an identical vendor default for session-model retention. Its
fourth turn checks that a fresh session still has the original default.
The cold Codex switch control omits the additional turn-level model override, so
the runtime's resume response is checked without a second override masking it.
This control is separate from production model omission, which is not applicable.

For an effort-reporting rerun, append `--effort-check-only` to the Codex command.
It runs only the omitted/null effort groups, eight live turns total. Resume
effort presence and explicit null are preserved, and each turn reports the
runtime's model/effort settings notifications. Those notifications are observations
after dispatch, not a new pre-provider admission barrier. The older report format
collapsed null and absent resume effort into `unreported`; do not infer the live
omission rule from that older field or from output-token counts.

The macOS Codex entry uses the pinned Linux runner image and the locally prepared
`cdx3246-deps` and `cdx3246-codex` volumes. Preparation uses the unchanged
`install-codex.sh` SHA256/manifest checks, never Homebrew. Container names start
with `cdx3246-`; cleanup addresses only the exact created name. The dedicated
CODEX_HOME is mounted read-write because ChatGPT refresh can rewrite `auth.json`.
Its content is never copied or printed. The summary reports only whether its
mtime changed.

Reports separate credential-free barrier evidence from live observations.
`modelUsage` and Codex totals are cumulative: repeated snapshots must not be
summed. Prior-thinking/reasoning acceptance requires observed prior reasoning,
not merely a successful response. A refusal, missing usage or unknown setting is
reported as such. Live evidence must be reviewed before choosing D1 in #3252 and
#3253; fake responses alone cannot choose a mechanism or authorize dispatch.
