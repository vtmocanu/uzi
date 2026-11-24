---
title: Model providers
order: 44
audience: user
---

# Model providers

A **model provider** is a named gateway (a company LLM proxy, for example) that you register
with its own API key and a map from an engine's model names to the gateway's models. A
**Claude** provider is Anthropic-compatible; a **Codex** provider is Responses-compatible
(OpenAI Responses protocol, Bearer auth). Settings has a **Custom providers** card for them,
and `uzi provider` covers the same ground from a terminal. Most of this page applies to both
engines; [Use a provider as your Codex source](#use-a-provider-as-your-codex-source) lists
what differs.

> **Runs through a provider are active, and the Claude CLI follows gateway redirects.** The
> pinned Claude CLI follows HTTP redirects from a gateway. A gateway that answers with a redirect
> can make the CLI re-send your key to another path on the same host under **both** auth styles,
> and re-send the request body (your prompts, plus code and tool output the agent read) to any
> host the redirect names. With `x_api_key` auth the key can also go to another host; Bearer auth
> drops the `Authorization` header across hosts. The owner accepted this risk on 2026-11-19 and
> parked the loopback relay that would remove it. The operator allowlist
> (`UZI_PROVIDER_ALLOWED_BASE_URLS`) is therefore the **only control** on where provider traffic
> starts: allowlist only gateways you trust not to redirect, and prefer Bearer auth, which limits
> cross-host key exposure.
>
> **Codex providers carry the same accepted risk.** The pinned Codex runtime follows redirects
> too, and can forward request bodies (your prompts, plus code and tool output) and the Bearer
> header beyond the approved URL. The allowlist is admission control, not confinement; the Test
> probe refuses redirects, the runtime does not (owner decision, 2026-11-19). The runtime also
> puts no byte limit on the model responses it reads from the gateway (HTTP bodies and SSE
> streams), so a hostile or broken gateway can exhaust that worker's memory and interrupt your concurrent
> runs on it (owner decision, 2026-11-22; limiter parked as #4600). Allowlist only gateways you
> trust. See [ADR-4320](../adr/4320-custom-model-providers.md#codex-engine-prd-4323-2026-11-23).
>
> Two operational notes. A worker image that predates providers never claims a provider run:
> the run stays queued with "no worker that supports model providers is online" until the worker
> is updated. And uzi blocks any push to the forge, including checkpoints, whose commits contain
> your provider key, so work that carries it stays on the worker and the run does not push it
> (`push_secret_blocked` / `secret_remediation_pending`). Details:
> [ADR-4320](../adr/4320-custom-model-providers.md#known-accepted-limit-the-cli-follows-redirects-activated-without-the-relay).

## Which URLs you can use

Your operator decides where providers may point, with `UZI_PROVIDER_ALLOWED_BASE_URLS`
(see [Configuration](./configuration.md) for the operator side). A provider's base URL must
equal one entry **exactly, path included**, for example `https://gw.example.com/anthropic`.
Nothing is matched by host or prefix. The Settings form offers the allowed URLs to pick from,
and `uzi provider list --json` prints them as `allowed_base_urls`. If your operator has not
set the list, the feature is off: Settings says so, and `uzi provider list` prints a message
instead of a table. A Codex provider's URL is the one the Responses requests are sent under
(Test posts to `<base_url>/responses`), for example `https://gw.example.com/openai`.

## Create a provider

In Settings, open **Custom providers** and add one, or from the CLI:

```sh
uzi provider create --name corp-gw \
  --base-url https://gw.example.com/anthropic \
  --model haiku=corp-haiku:200000 \
  --model sonnet=corp-sonnet:200000
```

- **Engine.** `--engine claude` (the default) or `--engine codex`. The rest of this list is the
  Claude form; a Codex provider differs as described under
  [Use a provider as your Codex source](#use-a-provider-as-your-codex-source).
- **Name.** Unique among your providers, ignoring case. You can have up to 20, retired ones
  included.
- **Auth style.** `bearer` (the default) sends the key as a bearer token; `x_api_key` sends it
  in the `x-api-key` header. Use whichever your gateway expects. A Codex provider is Bearer only.
- **Model map.** `--model alias=model[:context_window]`, repeatable, with `alias` one of
  `haiku`, `sonnet`, `opus` or `fable`. **`haiku` is required**: Claude's background calls use
  the small/fast slot, which is `haiku`. Set `context_window` to the gateway model's real
  window.
  - **Default.** With no `context_window` on any mapped model, uzi assumes 200k tokens. The
    worker sets that value itself; the Claude CLI's own default for an unrecognized model name
    is now 1M (Claude Code 2.1.325), which would compact far too late on a smaller model.
  - **Configured windows.** The window is process-wide: the smallest `context_window` across
    all mapped models applies to the whole run, to the lead and to subagents alike. The CLI
    auto-compacts about 30k tokens below the window (measured: 50000 compacts at 20k, 400000 at
    370k).
  - **Partly configured map.** If only some models carry a `context_window`, the smallest
    explicit one applies to every model, including those you left blank. This is a known
    limitation.
  - **Measured exceptions.** Measured at Claude Code 2.1.325, the configured window was
    enforced for `corp-sonnet`, `claude-corp-custom` and `corp-claude-x`. It was **not
    honoured** for `claude-sonnet-4-5` and `us.anthropic.claude-sonnet-4-5`, with a configured
    window or with uzi's 200k default: no compaction was seen up to 605k reported tokens. The
    CLI logged no unrecognized-model warning for those ids, so it appears to recognize them; we
    record what was seen, not which window it applied, and other names the CLI recognizes as
    Anthropic models may behave the same. (The Claude Code 2.1.325 changelog says the setting
    now also applies to unrecognized ids starting with `claude-`, which matches
    `claude-corp-custom` being enforced there but not at 2.1.319.) The web form and
    `uzi provider create`/`edit` show a caveat for any model name containing "claude".

## The key

The API key is added in the web Settings only. It is 16 to 4096 printable ASCII characters,
stored encrypted, and **never shown again** once saved: you can replace or remove it, not read
it back. The CLI has no key verb and no flag that accepts one, so a stolen CLI token cannot
mint or swap a credential. A provider without a key cannot be tested or used. Changing a
provider's base URL or auth style, and deleting a provider, are web-only for the same reason.

`uzi provider edit <name|id>` renames a provider and/or replaces its model map. Any `--model`
replaces the whole map, so repeat every alias you want to keep.

## Test

**Test** (Settings, or `uzi provider test <name|id>`) sends two small requests to the gateway
and reports them as two separate lines:

- **auth**: `ok`, `rejected` or `inconclusive`. Does the key authenticate and does the endpoint
  speak the Messages protocol (for a Codex provider, the Responses protocol)?
- **tools**: `passed`, `not_supported`, `inconclusive` or `not_run` (Settings words them as plain sentences). Does the gateway return a
  tool call when asked for one? It is `not_run` when auth did not succeed.

`inconclusive` means the gateway could not be judged (a network error, a timeout, a redirect or
an unexpected reply), which is not a failure of the key. **Neither result certifies full
compatibility with the engine.** A gateway can pass both and still differ on streaming, long
contexts or other features.

The probe is dialed from the api directly, not from a worker, and it ignores proxy
environment variables. It admits private gateway addresses (RFC 1918, carrier-grade NAT,
IPv6 unique-local) and refuses link-local (including cloud metadata), unspecified, multicast
and broadcast addresses. Loopback is refused unless the operator turned on the development
flag. Limits: one test at a time per user, 30 per hour per user, and at most 4 tests running at once across the whole instance; the counts reset when the
api restarts. Past any of these limits the request fails with the reason `provider_probe_rate_limited`.

## Use a provider as your Claude source

Your **Claude source** decides which credential your Claude runs and chats use. Unset (the
default), they use your Anthropic token exactly as before. Set to a provider, they use that
provider's gateway and key, with no Anthropic credential in the run, even if you also hold one.
The source is a separate setting, so your Anthropic token keeps its own default untouched.

In Settings, each provider row has **Use as Claude source** (disabled, with the reason, when the
provider is retired, has no key or has a disabled key); the selected one shows **Stop using** and
Settings names it as your current Claude source. From a terminal:

```sh
uzi provider use corp-gw     # select by name or id
uzi provider use --none --engine claude   # back to your Anthropic token
```

`use` sets the source of the provider's own engine, and clearing needs `--engine claude|codex`
(see [Use a provider as your Codex source](#use-a-provider-as-your-codex-source)). If the
operator turns custom providers off while one is selected, Settings still offers **Stop using**.
You can clear the source there or with `uzi provider use --none --engine claude` to use
Anthropic for new runs.

Both call `PUT /api/me/providers/claude-source` and accept a Bearer token, since selecting reveals no
key and cannot move a destination. The selection applies to runs you start afterwards. A selection is refused (409 `provider_source_unusable`)
unless the provider is yours, not retired, on the operator's allowlist and has an enabled key.

- **A set source is never skipped.** If it becomes unusable (retired, its URL removed from the
  allowlist, its key missing or disabled), new runs are refused with `provider_source_unusable`.
  They do not fall back to your Anthropic token or to Codex.
- **Deleting a selected provider is refused** (`provider_in_use_as_source`). Clear the source first.
  Retiring is allowed and leaves the source set, so the next run is refused as above.
- **No overrides.** A per-run credential override (`--token`) and switching a running run's
  credential are refused for a provider-sourced owner.
- **Automatic harness choice and overflow skip Claude** while a provider is your source: unknown
  usage is never compared with vendor readings, and your run never moves to another engine's
  credential.
- **Lanes that cannot use a provider refuse.** The judge, jobs and cross-check lanes refuse an
  owner whose Claude source is set, with `provider_lane_unsupported`, including that owner's runs
  that use an Anthropic token. The refused judge is a visible skip on the run ("the selected
  source is a model provider, which the judge does not support yet"). A plan cross-check whose
  checker would be refused sends the plan to the human plan gate instead; for a Codex lead that
  means a Codex lead with plan cross-check, owned by someone with a Claude source, waits for you
  to approve the plan. The isolated research lane refuses provider runs.
- **Models.** Every model the run uses (the lead, each agent template, summaries and advice
  passes) must be in the provider's model map. An unmapped alias or a full Anthropic model id is
  refused with `provider_model_unmapped`; uzi never swaps it for another model.

### What a run keeps

A run freezes its provider, revision and key at creation. Continuations of that run (resume,
recovery, restart, automatic rework) and its follow-on rework, review and chat runs keep the
frozen binding, not whatever your source is later. CI-fix and handoff runs have no source run and
use your current source. Editing a provider's model map creates a new revision for later runs
and older ones keep theirs.

Revocation takes effect at the run's **next claim**, not mid-claim, and cancelling is the
immediate stop. If the operator removes the URL from the allowlist, the run pauses as
`provider_destination_not_allowed` and resumes by itself once the URL is allowed again. If the
key was replaced, disabled or deleted, the run pauses as `provider_binding_stale`: it cannot
continue, so cancel it and start a new run, which uses the current key.

### Gateway rate limits

If the gateway keeps answering 429 or overloaded on an **issue run**, the run parks and retries
1 minute later, then 5, then 15, then waits for you to Resume (see
[the schedule](./run-limit-wait.md#a-custom-providers-gateway-rate-limit)). A **chat** turn that
hits a gateway limit ends with an error naming `provider_gateway_limit`; ask again in a moment.
It does not park or retry.

### Usage and cost

A run card names the provider the run used (or "(name unavailable)" when the name is not known
for that view, for example after the provider was deleted; the TUI board's narrow column shows
"(no name)"), shows no Anthropic token for it, and hides the credential override. Usage and cost
show as **unknown** for provider runs: the model names are the gateway's, so the vendor price
table does not apply. Token counts are still recorded, and run, time and
concurrency limits still apply.

## Use a provider as your Codex source

A **Codex provider** (engine `codex`, protocol `openai_responses`) is a gateway that speaks the
OpenAI Responses protocol, at an operator-allowlisted exact base URL, with **Bearer auth only**.
Your **Codex source** decides which credential your Codex runs use. It is separate from your
Claude source: each engine has its own selection. Unset (the default), Codex runs use your Codex
login or OpenAI key exactly as before. Set to a provider, they use that gateway and key only. No
Codex subscription login (`codex_auth`) and no OpenAI vendor key is forwarded, and a run never
falls back to one.

```sh
uzi provider create --engine codex --name code-gw \
  --base-url https://gw.example.com/openai \
  --model default=gw-model-a:200000 \
  --model gpt-6-sol=gw-model-b:200000
uzi provider use code-gw                     # sets your Codex source (the provider's engine)
uzi provider use --none --engine codex       # back to your Codex login or OpenAI key
```

The key is added in the web Settings, as for a Claude provider, and is stored as its own kind
(`openai_compatible_key`): it is never an OpenAI vendor key, and a Claude provider's key cannot be
attached to a Codex provider or the reverse. In Settings, **Custom providers** has an engine
selector, and each row offers **Use as Codex source** or **Stop using**. `uzi provider use <name>`
picks the source from the provider's engine; clearing needs the engine named
(`--none --engine claude|codex`), because the two selections are independent. Both call
`PUT /api/me/providers/codex-source` or `.../claude-source` and accept a Bearer token.

- **Model map.** `default` is required. The optional keys are the Codex model ids uzi can send:
  `gpt-6-astra`, `gpt-5.6-sol`, `gpt-6-sol`, `gpt-6.1-sol`, `gpt-6.2-sol`, `gpt-6.2-astra` and `gpt-6-luna`. Each entry is
  `model[:context_window]` as for Claude (8,192 to 10,000,000 tokens).
  - **An explicit model you pick** (`--model` on a run, or a model pinned on a schedule) resolves
    only through its own key. If the map has no such key the run is refused with
    `provider_model_unmapped` before any credential is sent.
  - **A model uzi picks** (the default model, the planning default and the task-review model)
    resolves through its own key when the map has one, otherwise through `default`.
  - **Effort** values pass through to the gateway unchanged.
  - **Context window.** A map entry's `context_window` sets the runtime's
    `model_context_window` for that model. Without one, the runtime's own fallback applies, which
    may not match the gateway model.
- **A set source is never skipped.** If it becomes unusable (retired, URL removed from the
  allowlist, key missing or disabled), new Codex runs are refused with `provider_source_unusable`,
  even when you hold a usable Codex login, OpenAI key or Claude token. Deleting a selected provider
  is refused (`provider_in_use_as_source`). A set source also turns automatic harness choice off.
- **No account switching or overrides.** A per-run credential override and in-place Codex account
  switching are refused for provider runs, and `codex_auth` is never delivered to them.
- **Usage and cost** are unknown for provider runs, as for Claude, and gateway model names are
  never priced from the vendor table.

### What a Codex run keeps

Codex issue, task, prompt and self-improve runs, including their in-run advice, summary and
task-review passes, use only the provider, revision and key frozen at creation: there is no
vendor fallback. Retries, resume and cold resume keep that binding. Rework (`mr_rework`) and
follow-up runs inherit it. CI-fix and handoff runs have no source run: they use your current
Codex source, and are refused when it is set but unusable.

Rotating or disabling the key, or a revision or allowlist change, takes effect at the run's
**next actual claim**, not mid-claim (cancel is the immediate stop), as the typed
`provider_binding_stale` outcome (cancel and start a new run) or, for a removed allowlist entry,
`provider_destination_not_allowed`. A gateway 429 or overloaded answer parks the run as
`provider_gateway_limit`, retried after 1, 5 and then 15 minutes, then it waits for you to
Resume ([the schedule](./run-limit-wait.md#a-custom-providers-gateway-rate-limit)).

### What a Codex provider run does not do

- **Judge.** A Codex judge is skipped for an owner with a provider source (a Claude or a Codex
  source), with the visible skip "the selected source is a model provider, which the judge does
  not support yet." A Claude judge follows your Claude source as described above.
- **Cross-check.** Plan and code cross-check refuse when either side would be bound to a Codex
  provider, with `provider_lane_unsupported`. For the plan stage this means a human plan gate: a
  Claude lead with plan cross-check, owned by someone with a Codex source, has its Codex checker
  refused and waits for you to approve the plan. The code stage records the checker as
  unavailable with no verdict.
- **Jobs and chat** are Claude-only and unchanged by a Codex source. A job refuses with
  `provider_lane_unsupported` while your **Claude** source is set; a Codex source does not
  affect them.

Codex providers on the judge and cross-check lanes are tracked in #4574.

### Operators: rollout and keys

- **Capability.** Workers advertise `codex_provider_v1` together with the Codex harness
  capabilities. The api admits a Codex provider claim only to a worker advertising
  `codex_provider_v1`, `codex_harness_v1`, `codex_runtime_v2` and `codex_runtime_v3` on the same
  worker; otherwise the run stays queued and its health reads "no worker that supports model
  providers is online" (it counts only workers with all four). Deploy the api before the workers.
  Update worker image overrides and separately managed workers to the current runtime too.
- **Old ephemeral workers.** A registered worker missing v3 can be reaped and replaced when its
  bound Codex provider run is queued and has never been claimed. Missing v3 alone does not
  authorize reaping a previously claimed run's worker; active assignments and open custody also
  prevent reaping. Provision/reap ticks and per-user caps bound rate and concurrency, but
  replacement cycles can continue indefinitely until the provisioned image advertises v3.
- **Credential delivery.** A Codex provider claim carries the gateway key only in its provider
  object, with no vendor Codex credential block, so there is no vendor release or refresh for it
  (`/codex/release` and `/codex/refresh` refuse provider runs).
- **Key protection.** The key is redacted by its exact encodings (raw, JSON-escaped, `\/`, HTML-safe,
  percent-encoded) from model, review and summary output, PR text and forge pushes. Transformed
  encodings are not detected, and the private recovery archive is not scanned, as for Claude
  providers (#4505).
- **Runtime.** The pinned Codex runtime is 0.171.0. The cited maintainer observation that the
  response body limit was unset was made on 2026-11-23 at tag `rust-v0.169.0`; it is historical
  evidence, not a response-size guarantee for the current pin. Updating the pin does not by
  itself remove the owner-accepted model-response ingestion risk above.

## Gateways behind a private CA

For a Codex provider, the worker's existing extra-CA bundle is delivered to the Codex runtime with
TLS verification left on, so a private-CA gateway works there. A stalled connection fails with a
typed timeout (setup through the first model response is bounded at 10 seconds per runtime
root); it is reported as a certificate error only when the runtime reports one. This page does not
separately document the `workers.uidSplit` case for Codex; the paragraph below describes Claude.

The Test probe trusts the operator's `workers.extraCA` bundle (the chart also mounts it into the
api pod). For a Claude provider under the chart's `workers.uidSplit`, the Claude CLI runs as a different uid that
cannot read that bundle, so `workers.extraCA` reaches only the worker process there, not the CLI.
A private-CA gateway can therefore pass **Test** and still fail from a run on a uid-split worker.
Workers without `uidSplit` are covered. (The isolated research lane refuses provider runs, so it
never reaches a gateway.) Ask your operator which applies.

## Retire or delete

**Retire** (`uzi provider retire <name|id>`, or in Settings) takes a provider out of use but
keeps it in your list, where it still counts toward the 20-provider limit. A retired provider
cannot be edited or tested. **Delete** removes it for good and is web-only.

## From the CLI

`uzi provider list` shows NAME, ENGINE, URL, AUTH, MODELS, KEY (`yes`, `no` or `disabled`), TEST
(`<auth>/<tools>` from the last test, or `-`) and RETIRED. `<name|id>` matches an id exactly or
a name ignoring case. Refusals print the server's machine-readable `reason` after the message.
See [the CLI reference](./cli.md#model-providers-uzi-provider).
