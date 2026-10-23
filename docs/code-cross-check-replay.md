---
title: Code cross-check replay
audience: contributor
---

# Code cross-check replay

A maintainer-run, offline replay of today's unchanged Code cross-check checkers (Codex and Claude) on a frozen, pinned, disclosed-synthetic corpus. It baselines detection, unadjudicated findings, cost, latency and the timeout/incomplete rate per family before any brief change (PRD #3167 M1, see [the PRD](../prds/3167-code-cross-check-coverage.md)). A uzi run never executes it, and it is not a gate or a CI target. The corpus provenance, case list and pins are in [the corpus README](../fixtures/code-cross-check-replay/README.md).

## Prerequisites

Run everything from the `agent/` directory with the repository's installed dependencies. Supply credentials through the process environment, never as arguments.

- **Codex:** a uid-split worker container. The replay launches the pinned Codex binary (`/opt/uzi-codex/0.163.0/bin/codex`) through `/usr/local/bin/uzi-codex-supervisor` and the cross-check helpers `/usr/local/bin/uzi-codex-command-sandbox` and `/usr/local/bin/uzi-codex-fileop`. The launcher refuses without `UZI_UID_SPLIT=1`: the worker identity is uid 10001 and the command identity is uid 10003. Codex replay supports API-key auth only (`OPENAI_API_KEY`); subscription auth is not supported.
- **Claude:** Linux, preferably the worker image, with `CLAUDE_CODE_OAUTH_TOKEN`.

## Authorization

Real-model calls cost money. They need the maintainer's own authorization for paid calls and the explicit `--authorize-real-model-calls` flag. Without the flag, `replay` refuses with `authorization_required` before it loads the corpus or builds a checker. `verify` and `score` make no model calls.

## Run a baseline

1. Check the pins and materialize every case offline:

   ```sh
   npm run eval:code-cross-check -- verify
   ```

2. Replay each family. `--family` (`claude` or `codex`), `--model`, `--effort` (`low`, `medium`, `high`, `xhigh`, `max`) and `--repeats` (1 to 20) are required; `--timeout-ms` is optional (at most 15 minutes). Five repeats is the recommended baseline. Record the model and effort you used; the sanitized aggregate carries both.

   ```sh
   npm run eval:code-cross-check -- replay --family claude --model <model> --effort <effort> --repeats 5 --authorize-real-model-calls
   npm run eval:code-cross-check -- replay --family codex --model <model> --effort <effort> --repeats 5 --authorize-real-model-calls
   ```

Stdout is JSON lines of fixed codes, ids, counts and pins. Invalid arguments give `{"error":"invalid_cli"}` without echoing them. `replay` prints the absolute path of the records file, then the sanitized aggregate. It exits 0 only when every replay completed, otherwise 1. The raw records go to an owner-only `.uzi/scratch/code-cross-check-replay-<timestamp>.jsonl` file in the checkout. They hold model output about repository code and are never printed.

Each record carries the checker version (uzi head, agent version, corpus version), the brief SHA-256, model, effort, reader implementation, credential mode (`oauth_token` for Claude, `api_key` for Codex), the outcome, the latency of the checker call only, the cost (`null` means unknown) and the findings.

## Score

`score` is offline. It resolves a relative path against the checkout root:

```sh
npm run eval:code-cross-check -- score <records.jsonl> [--adjudications <file>]
```

The aggregate is grouped by family and input provenance. Reconstructed-input and recorded-input cases are reported separately. A group reports:

- `matched`, `match_rate_completed` and `match_rate_attempted` for read-only defects. Execution-only defects are excluded and counted as `execution_only_excluded`.
- `unmatched_findings` (on defect cases), `control_findings` (on control cases) and `unadjudicated_findings`.
- `verified_false_positives`, only when `--adjudications` is supplied.
- The `outcomes` per code, `incomplete_rate`, the cost sum, median and `cost_unknown` count, and latency median, p90 and max.

Matching an expected defect (path, line range, keyword patterns) is a scoring proxy, not proof that a finding correctly diagnosed the defect, and the expected list is not exhaustive. Findings that match no expected defect, and every finding on a control case, are reported as unadjudicated, never as confirmed false positives.

To publish false-positive counts, the maintainer reads the private records file and writes an adjudication file: a JSON array with exactly these three keys per entry. `record_key` is `<case_id>/<family>/<repeat>`, `finding_id` is the finding's `id` in that record, and `verdict` is `false_positive`, `valid` or `duplicate`.

```json
[
  { "record_key": "c-2904/claude/1", "finding_id": "<finding id>", "verdict": "false_positive" }
]
```

Any adjudicated finding leaves `unadjudicated_findings`; only `false_positive` verdicts count in `verified_false_positives`. A malformed file gives `adjudication_invalid`, a repeated pair `adjudication_duplicate` and an unknown record or finding `adjudication_unknown_finding`. A stale or foreign records file gives `records_invalid`.

## Publish

Post only the sanitized aggregate (the stdout of `replay` or `score`) as a comment on issue #3167. Keep the records file and the adjudications private and local.

## Changing the corpus

A maintainer may replace a case with recorded artifacts and re-pin before the baseline. Replacing inputs after a baseline requires rerunning the old baseline for those cases. See [Replacing a case](../fixtures/code-cross-check-replay/README.md#replacing-a-case).

CI's agent test job checks out shallow, so only the corpus test's hermetic layer runs there. The pin layer runs in full clones and fails instead of skipping when `UZI_REPLAY_CORPUS_REQUIRE_OBJECTS=1` is set.

## Maintainer follow-up

Outside issue #3357 and unnumbered: running the authorized baseline and posting the aggregate on #3167 completes PRD #3167 M1. That unblocks its M2 and M3a and PRD #3168 M2.
