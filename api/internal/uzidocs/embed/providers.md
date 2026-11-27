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
unless the provider is yours (or an [admin-shared definition](#admin-shared-providers)), not retired, on the operator's allowlist and has an enabled key of yours.

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
- **The judge runs on your provider.** A Claude judge for an owner whose Claude source is set runs
  on that provider, frozen when the judge is created, with your provider key and no Anthropic token.
  It never falls back to a vendor token. The one exception is a judge whose token you pinned to a
  specific Anthropic token in Run defaults, which stays a vendor judge. It needs `opus` (the built-in
  judge model) or your chosen judge model in the provider's map; otherwise the judge is skipped as
  `provider_model_unmapped`, and a source that cannot serve it is skipped as `provider_source_unusable`
  (**Run judge** refuses with the same typed 409). Details, failure behavior and data destinations:
  [A judge on a model provider](./judge.md#a-judge-on-a-model-provider). Before PRD #4322 M1a, this
  lane refused every owner whose Claude source was set with `provider_lane_unsupported`, so older
  runs can still show that skip.
- **Jobs run on your provider.** A [job](./jobs.md#jobs-on-a-model-provider) created through
  `/api/v1/jobs` for an owner whose Claude source is a provider runs on that provider, frozen when
  the job is created, with your provider key and no Anthropic token. A job that names a site list
  (`egress_profile`) is still refused with `provider_lane_unsupported`, and site-list jobs on a
  provider are not shipped yet. A job never parks: revocation (a removed allowlist entry or a replaced, disabled or deleted key) or a gateway rate limit fails it. The
  same redirect risk applies, and the job's agent can see your provider key in its environment. The
  worker scrubs the key from what the job publishes and withholds output files that carry it, but
  matching covers only the key's raw, JSON-escaped, `\/`, `\u`-escaped and percent-encoded forms (other
  encodings such as base64 are not detected), so this guards against accidental
  publication only, not a malicious or prompt-injected job; see the linked section.
- **Other lanes that cannot use a provider refuse.** The cross-check lane refuses an
  owner whose Claude source is set, with `provider_lane_unsupported`, including that owner's runs
  that use an Anthropic token. A plan cross-check whose
  checker would be refused sends the plan to the human plan gate instead; for a Codex lead that
  means a Codex lead with plan cross-check, owned by someone with a Claude source, waits for you
  to approve the plan. The isolated research lane refuses provider runs.
- **Where judged traces go.** A provider judge sends the judged run's trace to your gateway, including
  the trace of a run that used an Anthropic token. A pinned vendor judge sends a provider run's trace to
  Anthropic. Because the Claude CLI follows gateway redirects (the warning above), judged trace bodies
  can reach redirect destinations under either auth style, and the key can reach other paths on the
  same host, or other hosts with `x_api_key`. Bearer is recommended.
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
immediate stop. If the operator removes the URL from the allowlist, the run (a job excepted, below)
pauses as `provider_destination_not_allowed` and resumes by itself once the URL is allowed again. If the
key was replaced, disabled or deleted, the run pauses as `provider_binding_stale`: it cannot
continue, so cancel it and start a new run, which uses the current key. A job never pauses: a
removed allowlist entry, or a replaced, disabled or deleted key, fails it with the same reason.

### Gateway rate limits

If the gateway keeps answering 429 or overloaded on an **issue run**, the run parks and retries
1 minute later, then 5, then 15, then waits for you to Resume (see
[the schedule](./run-limit-wait.md#a-custom-providers-gateway-rate-limit)). A **chat** turn that
hits a gateway limit ends with an error naming `provider_gateway_limit`; ask again in a moment.
It does not park or retry. A provider **judge** that hits one ends failed with a fixed gateway-limit
reason; it never parks or waits ([details](./judge.md#a-judge-on-a-model-provider)).

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
  - **A model uzi picks** (the default model, the planning default, the task-review model and the
    Codex judge model) resolves through its own key when the map has one, otherwise through `default`.
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

### The Codex judge on a provider

A Codex judge for an owner whose **Codex** source is a provider (their own; shared Codex definitions are
#4633) runs on that provider, frozen when the judge is created, with no vendor
fallback. A selected Codex source always wins; there is no judge pin that keeps it a vendor judge. An
owner with only a Claude source gets a Codex judge on the vendor Codex credential, ignoring the Claude
source. The judge model resolves through the provider's model map: an exact key, else the required
`default` slot. A map without `default` is an unusable source, skipped as `provider_source_unusable`,
never `provider_model_unmapped`. Failure behavior, the `codex_provider_judge_v1` worker capability and the
trace destination: [A Codex judge on a model provider](./judge.md#a-codex-judge-on-a-model-provider). Older
`provider_lane_unsupported` skip rows for a Codex judge are historical.

### What a Codex provider run does not do

- **Cross-check.** Plan and code cross-check refuse when either side would be bound to a Codex
  provider, with `provider_lane_unsupported`. For the plan stage this means a human plan gate: a
  Claude lead with plan cross-check, owned by someone with a Codex source, has its Codex checker
  refused and waits for you to approve the plan. The code stage records the checker as
  unavailable with no verdict.
- **Jobs and chat** are Claude-only and unchanged by a Codex source. A Codex source does not
  affect them. A job follows your **Claude** source (see
  [Use a provider as your Claude source](#use-a-provider-as-your-claude-source)).

Codex providers on the cross-check lane are tracked in #4574.

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

## Admin-shared providers

An admin can define a **Claude** or a **Codex** gateway once and share it with everyone on the
instance. You see it in Settings, **Custom providers**, next to your own, marked **Managed by your
admin**, and in `uzi provider list` with `admin` in the MANAGED BY column. The admin fixes its
engine, name, base URL, auth style and model map. **You supply and control your own key**: you attach
it, test it and select the definition as your Claude or Codex source (matching its engine) exactly as
for your own provider. The admin UI and API never show your key (the key is still sent to the gateway
endpoint, so whoever operates that gateway receives it), and you never see anyone else's key or Test
results.

- **Before you attach a key or select it**, Settings shows a disclosure. It says what the admin
  decides (the endpoint and auth style, fixed when the definition was created, so your admin cannot
  repoint your key to another gateway; the model map, which decides which gateway model and context
  window your own key pays for on your next fresh run, and which an admin edit changes without asking
  you) and what you hold (your own key, stored encrypted for your account only, which you can rotate,
  disable or delete at any time). It also repeats the redirect warning above: your key is sent to that
  endpoint on every request, a redirecting gateway can resend your prompts and code to another host
  and your key to another path on the same host. For a Claude definition with `x_api_key` auth it can
  also forward the key itself to another host. A shared Codex definition always uses Bearer, and the
  Codex runtime can forward the Bearer header to another host.
- **The key and Test are yours.** **Test** uses your key and shows only your own result ("Latest test
  of your key"). Replacing, rotating or disabling your key, or an admin model-map edit, makes your old
  result read as not tested again. Another user's pass or failure never shows on your row.
- **Select and clear.** **Use as Claude source** (or **Use as Codex source** for a Codex definition)
  opens the disclosure first; confirm to select. `uzi provider use <name|id>` selects it too (the CLI
  says "(shared by your admin)"). Clear it like any source (**Stop using**, or
  `uzi provider use --none --engine claude` / `--engine codex`). Without a key of your own, selecting
  is refused with `provider_source_unusable` and leaves your source unchanged. If your source for that
  engine is unset, new runs keep using your vendor credential when available. If an already-selected
  shared definition loses your key, new runs on that engine are refused without falling back to the
  vendor credential.
- **You cannot change the definition.** Rename, model map, endpoint, retire and delete are not
  offered for it. `uzi provider edit` and `uzi provider retire` refuse a shared definition ("managed by
  your admin") before sending any change. You can still disable or delete your own key on it.
- **When your admin changes it.** A model-map edit makes a new revision for your next fresh run;
  runs already started keep the one they froze. **Retiring** stops new selections and fresh runs. If
  you had it selected, it stays selected but is unusable until you choose another source, and Settings
  says so. **Deleting** is refused while any user has it selected (`provider_in_use_as_source`); once
  nobody does, it also deletes every user's attached key and Test results, and a run still
  bound to it stops at its next claim as `provider_binding_stale`.
- **Codex.** A shared Codex definition uses the `openai_responses` protocol with Bearer auth and a
  model map that has a `default` slot, and the key you attach to it is your own `openai_compatible_key`.
  It follows the same rules as a Claude one: a fresh Codex run freezes the current revision and your
  key, an admin edit makes a new revision for fresh runs only, and rotating, disabling or deleting your
  key makes the next claim of a bound run fail as `provider_binding_stale`. Retiring it refuses fresh
  runs while runs already started, including reclaimed ones, keep their binding. The key and Test
  results are yours alone; the admin sees counts only. Your Codex runs never fall back to the vendor
  Codex credential while it is your selected source.
- **The accepted redirect risk applies to a shared definition under either auth style** (owner
  decision, 2026-11-20), and the operator allowlist still governs where its URL may point.

### Admin: manage shared providers

In the web app, **Admin → Shared providers** (admin only). Create a definition (engine, name, one of
the allowed base URLs, auth style, model map; it has no key), edit its name and model map, retire it,
or delete it. **The engine is chosen at creation**: Claude is the default and Codex the alternative.
A Codex definition uses `openai_responses` with Bearer auth and needs a `default` model slot. When a
create request omits the protocol, it defaults to `anthropic_messages` for Claude and
`openai_responses` for Codex (creating your own provider does not default a Codex protocol). The
engine, base URL and auth style are fixed once created: to change any of them, create a new
definition, and each user must then select it and attach a key again. An edit checks the new model map
against the definition's own engine. Each row shows how many users
selected it and how many personal keys are attached. Delete is refused while anyone has it selected
and otherwise says how many keys go with it, then reports the count it removed. Selection counts cover both engines. Up to 50 shared
definitions can exist, retired ones included. Every change writes a structured `provider_audit` log
line with the actor and provider ids (never a key). All writes are browser-only (cookie and CSRF), and
a `uza_` token returns 401 on them.

**Codex redirect exposure.** The admin controls the base URL of a shared Codex definition, and that URL
receives every recipient's key as a Bearer token on every request. The pinned Codex runtime follows
redirects and can forward request bodies and the Bearer header beyond the approved URL, and the
allowlist is admission control, not confinement to that host (see the Codex warning at the top of
this page). Share only a gateway you trust not to redirect; the create form says so. A mistake here
exposes every recipient who attached a key, not one user.

`uzi admin provider list` and `uzi admin provider get <id|name>`
are the read-only terminal view and need an admin-read token. Design and the lock and rollback
details: [ADR-4324](../adr/4324-admin-shared-providers.md).

## Gateways behind a private CA

For a Codex provider, the worker's extra-CA bundle is delivered to the Codex runtime with
TLS verification left on, so a private-CA gateway works there, but only on workers the controller recorded as
created with it (see the `workers.extraCA` upgrade note in [Configuration](./configuration.md)); a worker without
that record fails TLS until it is reprovisioned. A stalled connection fails with a
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
(`<auth>/<tools>` from the last test, or `-`), RETIRED and MANAGED BY (`you`, or `admin` for an
[admin-shared definition](#admin-shared-providers)). `<name|id>` matches an id exactly or
a name ignoring case; when an owned and a shared definition have the same name, `use` and `test`
exit 2 and ask for an id. Refusals print the server's machine-readable `reason` after the message.
See [the CLI reference](./cli.md#model-providers-uzi-provider).
