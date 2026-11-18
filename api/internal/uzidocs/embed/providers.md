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

> **Runs through a provider are currently blocked.** You can create, edit, test, retire and
> select providers, but a run that is bound to one **stays queued** and shows "no worker that
> supports model providers is online; waiting for a worker roll". Workers do not yet advertise
> the capability that provider runs need, because the pinned Claude CLI follows redirects from a
> gateway, which could re-send your key or request body to a location the gateway names. A
> loopback relay that removes that risk is planned as a separate change. Until it lands, do not
> select a provider as your Claude source unless you want your Claude runs to queue; clear it to
> go back to your Anthropic token. Details: [ADR-4320](../adr/4320-custom-model-providers.md).

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
  window. Without it, the Claude CLI assumes a 200k window for model names it does not
  recognize and auto-compacts about 33k tokens below it, which is wrong for a smaller or larger
  model.

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

A run card names the provider the run used (or "(name unavailable)" when the name is not known for that view, for example after the provider was deleted), shows no
Anthropic token for it, and hides the credential override. Usage and cost show as **unknown** for provider runs: the model names are the gateway's, so the
vendor price table does not apply. Token counts are still recorded, and run, time and
concurrency limits still apply.

## Gateways behind a private CA

The Test probe trusts the operator's `workers.extraCA` bundle (the chart also mounts it into the
api pod). Under the chart's `workers.uidSplit`, the Claude CLI runs as a different uid that
cannot read that bundle, so `workers.extraCA` reaches only the worker process there, not the CLI.
A private-CA gateway can therefore pass **Test** and still fail from a run on a uid-split worker.
Workers without `uidSplit` and isolated-lane workers are covered. Ask your operator which applies.

## Retire or delete

**Retire** (`uzi provider retire <name|id>`, or in Settings) takes a provider out of use but
keeps it in your list, where it still counts toward the 20-provider limit. A retired provider
cannot be edited or tested. **Delete** removes it for good and is web-only.

## From the CLI

`uzi provider list` shows NAME, URL, AUTH, MODELS, KEY (`yes`, `no` or `disabled`), TEST
(`<auth>/<tools>` from the last test, or `-`) and RETIRED. `<name|id>` matches an id exactly or
a name ignoring case. Refusals print the server's machine-readable `reason` after the message.
See [the CLI reference](./cli.md#model-providers-uzi-provider).
