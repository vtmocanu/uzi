---
title: Code cross-check replay
audience: contributor
---

# Code cross-check replay

A maintainer-run, offline replay of Code cross-check checkers (Codex and Claude) on a frozen, pinned, disclosed-synthetic corpus, with explicit executing-brief profiles. It baselines detection, unadjudicated findings, cost, latency and the timeout/incomplete rate per family before any brief change (PRD #3167 M1, see [the PRD](../prds/3167-code-cross-check-coverage.md)). A uzi run never executes it, and it is not a gate or a CI target. The corpus provenance, case list and pins are in [the corpus README](../fixtures/code-cross-check-replay/README.md).

## Prerequisites

Run everything from the `agent/` directory with the repository's installed dependencies. Supply credentials through the process environment, never as arguments.

- **Codex:** a uid-split worker container. The replay launches the pinned Codex binary (`/opt/uzi-codex/0.163.0/bin/codex`) through `/usr/local/bin/uzi-codex-supervisor` and the cross-check helpers `/usr/local/bin/uzi-codex-command-sandbox` and `/usr/local/bin/uzi-codex-fileop`. The launcher refuses without `UZI_UID_SPLIT=1`: the worker identity is uid 10001 and the command identity is uid 10003. Codex replay supports API-key auth only (`OPENAI_API_KEY`); subscription auth is not supported. Under the split the replay opens only its own run directory (search only), the snapshot inside it (read only) and each case's per-case HOME (group read-write, the Codex HOME contract) to the runner group; the records file, credentials, the work repository and the rest of the run directory stay private. `.uzi/scratch` and every directory above it must be searchable by the runner (10002) and command (10003) uids, and the path must not contain a symlink. Otherwise `replay` stops before any case or provider call with `{"error":"snapshot_untraversable"}`; fix the directory permissions yourself, the replay never changes a directory it did not create.
- **Claude:** `CLAUDE_CODE_OAUTH_TOKEN`. Running inside the worker image is recommended: the checker's native cleanup certifies its SDK process group with POSIX process-group signals, and its path guard denies the worker's credential paths.

## Authorization

Real-model calls cost money. They need the maintainer's own authorization for paid calls and the explicit `--authorize-real-model-calls` flag. Without the flag, `replay` refuses with `authorization_required` before it loads the corpus or builds a checker. `verify` and `score` make no model calls.

## Select the executing brief

The default profile is `baseline`. It retains `brief_drift` rejection when
the executing brief differs from the original pinned brief. Run the M1 baseline
from commit `4d018dbd69175cc7d60f5acefd7ae122311a1ba1`, using that
checkout's lockfile dependencies and matching pinned worker binary, as described
in the maintainer follow-up below.

On the M2 checkout, select `--profile revised` explicitly for `verify`,
`replay` and `score`. For example:

```sh
npm run --silent eval:code-cross-check -- verify --profile revised
npm run --silent eval:code-cross-check -- score <records.jsonl> --profile revised
```

For authorized model calls, add the same `--profile revised` to the replay
command below; it does not replace the separate spend-authorization flag.
The executing brief must match the selected source-pinned profile. Records and
aggregates identify that profile and the actual brief hash; mixed or stale
records are rejected. The original corpus manifest, brief pins and reconstructed
artifacts remain unchanged. Profiles do not let this checkout execute the old
brief under the new hash, or override prompts arbitrarily.

M2 tests profile integrity and findings-plus-coverage decoding offline with fake
checkers. Baseline and comparative detection, coverage, false-positive, cost and
latency measurements still require the separately authorized maintainer runs.
No measurement improvement is claimed by profile compatibility alone.

## Run a baseline

1. Check the pins and materialize every case offline:

   ```sh
   npm run --silent eval:code-cross-check -- verify
   ```

2. Replay each family. `--family` (`claude` or `codex`), `--model`, `--effort` (`low`, `medium`, `high`, `xhigh`, `max`) and `--repeats` (1 to 20) are required; `--timeout-ms` is optional (default and maximum 15 minutes). The approved plan suggests five repeats, which is 30 paid checker calls per family for the six cases. A missing credential does not stop the run up front: every record is `missing_auth` and the command exits 1. Record the model and effort you used; the sanitized aggregate carries both.

   ```sh
   npm run --silent eval:code-cross-check -- replay --family claude --model <model> --effort <effort> --repeats 5 --authorize-real-model-calls
   npm run --silent eval:code-cross-check -- replay --family codex --model <model> --effort <effort> --repeats 5 --authorize-real-model-calls
   ```

Stdout is JSON lines of fixed codes, ids, counts and pins. Invalid arguments give `{"error":"invalid_cli"}` without echoing them. `replay` prints only the sanitized aggregate on stdout; the absolute path of the records file (a `{"records_file":...}` locator) goes to stderr, because it can carry your username or checkout location. Do not merge stderr into published output (no `2>&1`). Use `npm run --silent`: plain `npm run` echoes the command line, arguments included, to stdout. If a checker cannot confirm that its processes stopped, `replay` records that attempt with its classified outcome (for example `confinement_failed` or `model_timeout`), starts no further checker call and stops with `{"error":"cleanup_unconfirmed"}`. It then leaves the run directory in place and names it on stderr (`retained_run_dir`); confirm no checker process is left before removing it. `replay` exits 0 only when every replay completed, otherwise 1. The raw records go to an owner-only `.uzi/scratch/code-cross-check-replay-<timestamp>.jsonl` file in the checkout. They hold model output about repository code and are never printed.

Each record carries the checker version (uzi head, agent version, corpus version), the brief SHA-256, model, effort, reader implementation, credential mode (`oauth_token` for Claude, `api_key` for Codex), the outcome, the latency of the checker call only, the cost (`null` means unknown) and the findings.

## Score

`score` is offline. It resolves a relative path against the checkout root, so pass the checkout-relative `.uzi/scratch/<file>` path rather than the absolute one:

```sh
npm run --silent eval:code-cross-check -- score <records.jsonl> [--adjudications <file>]
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

Any adjudicated unmatched or control finding leaves `unadjudicated_findings` (an adjudication of a finding that matched an expected defect is accepted but has no effect); only `false_positive` verdicts count in `verified_false_positives`. A malformed file gives `adjudication_invalid`, a repeated pair `adjudication_duplicate` and an unknown record or finding `adjudication_unknown_finding`. A stale or foreign records file gives `records_invalid`, or `score_unknown_case` when a record names a case id that is not in this corpus.

## Publish

Post only the JSON lines from stdout of `replay` or `score` (the sanitized aggregate), never stderr, as a comment on issue #3167. Keep the records file and the adjudications private and local.

## Changing the corpus

A maintainer may replace a case with recorded artifacts and re-pin before the baseline. Replacing inputs after a baseline requires rerunning the old baseline for those cases. See [Replacing a case](../fixtures/code-cross-check-replay/README.md#replacing-a-case).

CI's agent test job checks out shallow, so only the corpus test's hermetic layer runs there. The pin layer runs in full clones and fails instead of skipping when `UZI_REPLAY_CORPUS_REQUIRE_OBJECTS=1` is set.

## Maintainer follow-up

Outside issue #3357 and unnumbered: running the authorized baseline, adjudicating every unmatched and control finding, and posting the `score --adjudications` aggregate (with `unadjudicated_findings` at 0) on #3167 completes PRD #3167 M1, whose baseline includes false positives. Run it from a checkout at the baseline commit `4d018dbd69175cc7d60f5acefd7ae122311a1ba1`, with `agent/` dependencies installed from that commit's lockfile, so it measures the code brief from before PRD #3167 M2 and M3a change it. Record the full baseline SHA and the worker-image digest with the aggregate. The aggregate unblocks PRD #3167 M3b and PRD #3168 M2.
