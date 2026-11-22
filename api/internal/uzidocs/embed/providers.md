---
title: Model providers
order: 44
audience: user
---

# Model providers

A **model provider** is a named, Anthropic-compatible gateway (a company LLM proxy, for
example) that you register with its own API key and a map from Claude's model names to the
gateway's models. Settings has a **Custom providers** card for them, and `uzi provider` covers
the same ground from a terminal.

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
instead of a table.

## Create a provider

In Settings, open **Custom providers** and add one, or from the CLI:

```sh
uzi provider create --name corp-gw \
  --base-url https://gw.example.com/anthropic \
  --model haiku=corp-haiku:200000 \
  --model sonnet=corp-sonnet:200000
```

- **Name.** Unique among your providers, ignoring case. You can have up to 20, retired ones
  included.
- **Auth style.** `bearer` (the default) sends the key as a bearer token; `x_api_key` sends it
  in the `x-api-key` header. Use whichever your gateway expects.
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
  speak the Messages protocol?
- **tools**: `passed`, `not_supported`, `inconclusive` or `not_run` (Settings words them as plain sentences). Does the gateway return a
  tool call when asked for one? It is `not_run` when auth did not succeed.

`inconclusive` means the gateway could not be judged (a network error, a timeout, a redirect or
an unexpected reply), which is not a failure of the key. **Neither result certifies full
compatibility with Claude.** A gateway can pass both and still differ on streaming, long
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
uzi provider use --none      # back to your Anthropic token
```

If the operator turns custom providers off while one is selected, Settings still offers
**Stop using**. You can clear the source there or with `uzi provider use --none` to use
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
  owner whose source is set, with `provider_lane_unsupported`, including that owner's runs that
  use an Anthropic token. The isolated research lane refuses provider runs.
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

## Gateways behind a private CA

The Test probe trusts the operator's `workers.extraCA` bundle (the chart also mounts it into the
api pod). Under the chart's `workers.uidSplit`, the Claude CLI runs as a different uid that
cannot read that bundle, so `workers.extraCA` reaches only the worker process there, not the CLI.
A private-CA gateway can therefore pass **Test** and still fail from a run on a uid-split worker.
Workers without `uidSplit` are covered. (The isolated research lane refuses provider runs, so it
never reaches a gateway.) Ask your operator which applies.

## Retire or delete

**Retire** (`uzi provider retire <name|id>`, or in Settings) takes a provider out of use but
keeps it in your list, where it still counts toward the 20-provider limit. A retired provider
cannot be edited or tested. **Delete** removes it for good and is web-only.

## From the CLI

`uzi provider list` shows NAME, URL, AUTH, MODELS, KEY (`yes`, `no` or `disabled`), TEST
(`<auth>/<tools>` from the last test, or `-`) and RETIRED. `<name|id>` matches an id exactly or
a name ignoring case. Refusals print the server's machine-readable `reason` after the message.
See [the CLI reference](./cli.md#model-providers-uzi-provider).
