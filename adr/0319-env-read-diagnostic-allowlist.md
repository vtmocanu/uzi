# ADR-319: An env-read allowlist lets a run read PATH/TMPDIR by name without opening enumeration

**Status**: Accepted
**Date**: 2026-08-15
**Deciders**: Vlad + agent team

## Context

The Bash guardrail (`agent/src/guardrails.ts`) denies `env` and `printenv`
outright: `printenv` reaches `analyzeSimple`, which returned
`deny(REASON_ENV)` for either base, and a bare `env` is additionally denied in
`analyzeSegment` after its assignment/flag peel. The reason the deny exists is
enumeration — a bare `env`/`printenv` dumps the WHOLE process environment, and
the SDK subprocess env carries `CLAUDE_CODE_OAUTH_TOKEN` (the user's Anthropic
credential), so a dump leaks it.

But an agent legitimately needs to answer diagnostic questions like "what is my
`PATH`?" or "where is my `TMPDIR`?" while debugging a failing check. The flat
deny made `printenv PATH` — a targeted, single-variable read of a value that is
not a secret — indistinguishable from `printenv` — a full dump. The blunt deny
cost real diagnostic ergonomics to close a hole that only the enumerating form
actually opens.

The real containment here is NOT the screener: `buildSdkEnv`
(`agent/src/sdk-env.ts`) hands the SDK subprocess a REPLACEMENT env (it is not
merged with `process.env`), and that env emits only `CLAUDE_CODE_OAUTH_TOKEN`,
`HOME`, `PATH`, an optional `TMPDIR`/`DOCKER_HOST`, and provisioned tool keys.
`PATH` is the runner image PATH and `TMPDIR` is the runner's private 0700
scratch dir; neither carries a secret by construction.

## Decision

Allowlist exactly `{PATH, TMPDIR}` for a NAME-read via `env`/`printenv`.

In `analyzeSimple`, when the base is `printenv` or `env`, allow the command IFF
it has at least one positional argument AND every positional argument is in the
allowlist `ENV_READ_ALLOWLIST = {PATH, TMPDIR}`; otherwise deny with
`REASON_ENV`. The rule uses `.every()` deliberately: a `.some()` would let
`printenv PATH ANTHROPIC_API_KEY` through, exfiltrating a secret alongside an
allowlisted var. That is the specific hole this ADR exists to avoid.

Bare `env`/`printenv` (no positional) fails the `≥1 arg` half and stays denied —
it is the enumerating form that dumps `CLAUDE_CODE_OAUTH_TOKEN`.

### Why the scope is exactly PATH + TMPDIR, and no wider

`buildSdkEnv`'s sparse env REPLACEMENT is the real containment, not this
screener. The agent subprocess env is a fixed, small set, and only `PATH` and
`TMPDIR` are both (a) present in it and (b) non-secret diagnostic values an
agent would reasonably read. `HOME` and the provisioned tool keys are present
but deliberately NOT allowlisted — there is no diagnostic need to widen the
read surface past the two that debugging a check actually asks for. Worker-only
variables such as `UZI_RUNNER_PATH` / `UZI_UID_SPLIT` are absent from the agent
env by construction (they live in the worker process, not the SDK subprocess),
so allowlisting them would only add vacuous tests that pass because the value is
never there to read — not because a control admits it.

### The two deny sites

- `analyzeSimple` — the load-bearing site. `printenv` is not a wrapper, so
  `printenv PATH` always reaches here; this is where the allowlist decision
  lives.
- `analyzeSegment`'s `env` branch — peels `-u`/`-i`/`--`/`VAR=val` and denies a
  bare `env` unconditionally. A non-assignment positional (`env PATH`) breaks
  that peel and rides down to `analyzeSimple` as command word `PATH` — NOT as
  `env PATH`, so the allowlist rule does not govern it. That is correct, because
  `env`'s own semantics treat `env PATH` as "run a command named PATH", not a
  variable read: `env NAME` never prints the value (so `env HOME` is allowed as
  an unknown command, and it leaks nothing). The read spelling that the allowlist
  actually governs is `printenv <var>`. The bare-`env` deny stays unconditional;
  no allowlist logic is folded into it.

## Consequences

- A run can answer `printenv PATH` / `printenv TMPDIR` / `printenv PATH TMPDIR`
  while every enumerating or secret-bearing form (`printenv`, `env`,
  `printenv HOME`, `printenv ANTHROPIC_API_KEY`,
  `printenv PATH ANTHROPIC_API_KEY`) stays denied.
- The allowlist is evaluated PER SEGMENT. A compound like
  `printenv PATH; printenv CLAUDE_CODE_OAUTH_TOKEN` is denied as a whole,
  because the second segment fails the allowlist and the PreToolUse hook returns
  a single deny for the entire Bash call.
- Per-segment PARTIAL success on a denial was considered and REJECTED as
  structurally impossible: the PreToolUse hook returns one allow/deny decision
  for the whole Bash tool call, so there is no mechanism to run the allowlisted
  segment and drop only the denied one. A compound that contains any denied
  segment is denied outright.
- This loosening is scoped and defense-in-depth-preserving: the containment that
  actually keeps `CLAUDE_CODE_OAUTH_TOKEN` out of a dump is the sparse env
  REPLACEMENT in `sdk-env.ts`, and this change never widens what that env
  contains — it only lets two of its non-secret values be read by name.
