---
title: PR description editor evaluation
audience: contributor
---

# PR description editor evaluation

Milestone B compares the editor's baseline and revised system prompts against five frozen inputs in [the fixture directory](../fixtures/pr-description-editor/). The baseline is the exact DELIVERY_SYSTEM_PROMPT string from commit 57585dd6, without a trailing newline; its SHA-256 is 2891a46208957ff7f2f706de2c6d6730e697d99775b0aed52ebffd4a75d7bdfc. The evaluator checks that hash before each pass. Both modes use the same DeliveryContext, model, code availability, lead verification stamped at a 40-character SHA, completion and body context. Each request gets production fence nonces.

The revised prompt asks for a compact diagram when the visible diff establishes an order-dependent flow, fallback chain, or interactions among at least three components. Truncated input qualifies only when every depicted step is visible. Evidence and hostile-input rules, graph counts and byte caps remain in force. Trivial documentation changes should omit diagrams; the hostile fixture tests unsupported steps and unsafe label requests.

## Prerequisites and invocation

Use the repository's Node 24 runtime and installed agent dependencies, including the pinned Claude Agent SDK 0.3.287, Go compatible with the API module, and the production pinned Codex executable at /opt/uzi-codex/0.160.0/bin/codex. The Go command must have the API module dependencies available through its normal tool caches. Do not install a second dependency tree to run this evaluation.

Claude reads only CLAUDE_CODE_OAUTH_TOKEN for provider authentication and uses the unchanged runReadOnlyModelPass, buildSdkEnv and ClaudeAdviceHarness SDK query. Codex reads only OPENAI_API_KEY and uses the sanctioned evaluator factory, actual CodexAdviceHarness and production api_key login RPC. Supply credentials through the process environment; never include them in arguments or result attachments. This does not change production model selection.

Run live evaluations on Linux. The Claude pass reuses production HOME cleanup, which removes the SDK HOME through a descriptor-pinned helper and refuses other hosts; on macOS, run the commands inside the worker image (it carries Node 24, Go and the pinned Codex executable) with the checkout mounted and the credentials passed as environment variables. The Go helper reuses the caller's effective GOPATH, GOCACHE and GOMODCACHE, resolved before its HOME is replaced.

From the agent directory, maintainers run these four comparisons:

```sh
npm run eval:pr-description -- --harness claude --model haiku --prompt-mode baseline
npm run eval:pr-description -- --harness claude --model haiku --prompt-mode revised
npm run eval:pr-description -- --harness codex --model gpt-6-sol --prompt-mode baseline
npm run eval:pr-description -- --harness codex --model gpt-6-sol --prompt-mode revised
```

All three flags are required. Invalid arguments produce a fixed invalid_cli error without echoing them. Missing authentication produces missing_auth for each fixture. Model identities are limited to one safe token of at most 100 UTF-8 bytes and exclude provider token patterns and caller credential values. The requested model is explicitly labeled requested_alias: the production advice results do not expose a resolved provider model, so the evaluator does not invent one.

One tool-less pass runs per fixture, with no retries. Each Codex pass owns a fresh temporary 0700 root, private CODEX_HOME and isolated working directory inside this checkout's .uzi/scratch. The child has a minimal environment; the API key travels through the existing login RPC. The real app-server may persist authentication inside this temporary CODEX_HOME. Signal handlers for SIGINT, SIGTERM and SIGHUP are installed before startup, abort active passes and remain until cleanup settles. Factory close waits for startup ownership, child exit and temporary-root removal, including late startup and failure paths. Claude retains its existing SDK HOME cleanup, rooted under .uzi/scratch.

Existing output behavior is retained: Codex's existing 8 MiB response bound and Claude's existing append behavior apply. The evaluator adds no 64 KiB model-response cap. Credentials, prompts, responses, diagram labels and raw exceptions are never evaluation output or logger fields.

## Results and PR attachment

Each fixture yields one JSON row with fixture_id, harness, named_editor_model, model_identity, prompt_mode, emitted, parsed, would_publish and a fixed error/rejection class (or null).

- emitted means the editor supplied a diagram JSON member, including null or a rejected value.
- parsed means the real production full-summary parser accepted both a usable summary and a diagram.
- would_publish means the diagram survives known-zero-code suppression, production generated-field assembly with lead verification, the real Go sanitizer, the WorkerClient strict branded-field decoder, renderRegion's source and region caps, and capBody's selection using fixture body context.

The evaluator calls the real sanitizer helper with bounded stdin (4 MiB), bounded captured output (128 KiB) and a replacement tool-only Go environment. Its instance-local transport accepts only the exact offline POST stage operation and returns the complete pending-version envelope to the production decoder. It performs no database or forge publication. would_publish describes pipeline eligibility, not an observed publication. Provider/startup/timeout failures are fixed errors, never interpreted as diagram omissions.

Attach only the JSON rows to the PR and record runtime versions and invocation model aliases. Summarize them in a matrix:

| Fixture | Claude baseline | Claude revised | Codex baseline | Codex revised |
| --- | --- | --- | --- | --- |
| fallbackchain | pending | pending | pending | pending |
| components | pending | pending | pending | pending |
| trivial | pending | pending | pending | pending |
| truncation | pending | pending | pending | pending |
| hostile | pending | pending | pending | pending |

For each cell record emitted / parsed / would_publish plus any fixed failure or rejection class. Review whether qualifying visible flows improve and trivial/hostile inputs remain grounded; sanitizer acceptance alone cannot prove grounding. Keep prompts, diff contents, model responses and labels out of the attachment.

Live evaluation is deferred to the maintainer before merge and is nonblocking for implementation completion. No live evaluation was performed during implementation. The command is opt-in, registered in package.json and knip, and is not a CI or gate target.

## Accepted production behavior

Accepted changes 1–3 retain API-local diagnostic correlation, without a new request header or protocol. Editor and parser diagnostics carry the known claim generation; API diagnostics correlate the stage/version at the existing service boundary. No new diagram publication event is added. Durable final-stage region_has_diagram is surfaced as diagram_published through the existing published-version behavior; a stored diagram field alone does not establish publication.

The diagram policy, sanitizer and whole-graph clipping remain production-owned. The evaluator factors only the real prompt/full-summary parser, generated-field assembly and per-instance fetch transport seams. It adds no production model-pass parameters, auth adapters, model selection changes or new output limits.
