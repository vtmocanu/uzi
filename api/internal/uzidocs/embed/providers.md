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

Selecting a provider as your Claude source arrives in a later release step. Until then you can
create, edit, test and retire providers, and nothing runs through them.

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

## Retire or delete

**Retire** (`uzi provider retire <name|id>`, or in Settings) takes a provider out of use but
keeps it in your list, where it still counts toward the 20-provider limit. A retired provider
cannot be edited or tested. **Delete** removes it for good and is web-only.

## From the CLI

`uzi provider list` shows NAME, URL, AUTH, MODELS, KEY (`yes`, `no` or `disabled`), TEST
(`<auth>/<tools>` from the last test, or `-`) and RETIRED. `<name|id>` matches an id exactly or
a name ignoring case. Refusals print the server's machine-readable `reason` after the message.
See [the CLI reference](./cli.md#model-providers-uzi-provider).
