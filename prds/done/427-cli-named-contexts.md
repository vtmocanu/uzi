# PRD #427 — uzi CLI named contexts (multi-token profiles)

- **Issue**: [#427](https://github.com/vtmocanu/uzi/issues/427)
- **Priority**: Medium
- **Status**: Done — all five milestones landed on `agent/issue-427` (M1 resolution core, M2 `context` verbs, M3 per-context writes + `status --all` + `context set`, M4 docs, M5 gate + smoke).
- **Forge scope**: Forge-agnostic. This is a **client-side CLI-only** change (config/credential resolution in `api/cmd/uzi` + `api/internal/uzicli`). No server, API, DTO, route, auth-tier, or migration change. It works identically against a GitLab, Forgejo, or GitHub uzi instance because it touches nothing that talks to a forge.
- **Handoff**: Send to uzi (Auto mode), offline worker. This PRD is entirely codebase-local — every fact below was resolved from this repo, and no milestone needs open-web access. All tests are pure Go unit tests in `api/cmd/uzi` / `api/internal/uzicli` (no live DB, no network, no browser).

---

## Problem

The uzi CLI stores exactly **one** credential slot — the implicit `default` context. Everything that reads or writes a credential hardcodes the string `"default"`:

- `api/cmd/uzi/root.go:231` — `resolveSettings` resolves `env.Store.Resolve("default")`.
- `api/cmd/uzi/auth.go:46,48` — `uzi auth token` writes `creds.Contexts["default"]`.
- `api/cmd/uzi/auth.go:74,81` — `uzi auth status` reports `CONTEXT default`.
- `api/cmd/uzi/login.go:148-164` — `uzi login` writes `cfg.Contexts["default"]` + `creds.Contexts["default"]`; `:196` reads it; `:202` `delete(creds.Contexts, "default")` on logout.

A uzi token carries a **scope** (verified `api/internal/middleware/cli_auth.go:31-38, 85-87`): a default (`uzc_`) token acts as your own user and is masked to `is_admin:false` even for an admin ("effective authority, not your résumé"); an admin (`uza_`, scope `admin_ro`) token is not masked and is what the read-only `uzi admin …` views require. The two scopes are a deliberate least-privilege boundary — an admin write is never reachable from any CLI token (admin writes are cookie-only), and a leaked `uza_` grants only factory-wide **reads**, which is why minting one is admin-gated and audit-logged (`api/internal/handler/cli_tokens.go:129-174`).

Because there is one slot, an admin who legitimately holds **both** tokens — a `uzc_` for owner actions (`run create`/`approve`/`handoff`) and a `uza_` for `uzi admin` reads — cannot keep both stored. They must overwrite one with `uzi auth token`, or juggle `UZI_TOKEN=…` env overrides per invocation (`root.go:239-241` — `$UZI_TOKEN` beats the stored token). That is the whole friction: the model is right, the ergonomics force a choice the tool should not require.

**This is a half-built feature, not a greenfield one.** The config layer was deliberately shaped for named contexts from day one and never finished on the surface (`api/internal/uzicli/config.go:43-49`):

> *"Config is a map of contexts from day one (`[contexts.default]`) so a later `uzi context use` is additive, not a file-format migration."*

`Config` already carries a `Current string` field (`config.go:47`) and a `Contexts map[string]Context`; `Credentials` already carries `Contexts map[string]Credential`; and `Store.Resolve(ctxName string)` (`config.go:154`) already takes a context name (defaulting `""` → `"default"` at `:156`). The scaffolding is done; only the CLI surface hardcodes `"default"` and nothing sets or reads `Current`.

## Solution overview

Finish the named-context surface, entirely client-side:

1. **Resolve an active context name** instead of hardcoding `"default"`. Precedence: `--context/-c <name>` flag > `$UZI_CONTEXT` > `config.Current` > `"default"`. Feed it to the already-parameterized `Store.Resolve(name)`.
2. **Keep the per-invocation overrides on top, unchanged.** `$UZI_TOKEN` still overrides the resolved context's token and `$UZI_URL`/`--url` still override its URL — the headless/CI path (`UZI_URL` + `UZI_TOKEN`, no `$HOME`) is preserved exactly. Context selection only chooses *which stored credential* is the base; the env/flag overrides still layer over it.
3. **Add a `uzi context` verb group**: `list`, `current`, `use <name>`, `rm <name>`.
4. **Let credential writes target a named context**: `uzi auth token --context <name>`, `uzi login --context <name>`, and `uzi logout` operating on the active context. `uzi auth status` reports the active context and gains `--all` to list every stored context.
5. **URL inheritance so the common case needs no per-context URL**: when the active context has no stored `Context.URL`, fall back to the `default` context's URL (then `$UZI_URL`/`--url` still win on top). This makes the primary use case — two tokens against the **same** server — work by storing the URL once; multi-server users can set a per-context URL explicitly.

Net effect for the motivating case:

```
uzi context use default                 # (or just leave it)
uzi auth token                          # store your uzc_ under the active (default) context
uzi auth token --context admin          # store your uza_ under an "admin" context (URL inherited)
uzi run handoff -m "…"                  # uses default → uzc_
uzi --context admin admin runs          # one-off admin read → uza_
uzi context use admin                   # or switch stickily
```

No `UZI_TOKEN` juggling, both tokens stored, the scope split intact, and you always know which identity you are wearing.

## Non-goal (explicit): command→scope auto-selection

We considered having the CLI **auto-pick** the token per command (e.g. `uzi admin …` silently uses the admin token, everything else the user token). It is **out of scope**, deliberately, and this section records why so a future reader does not re-open it as "the obvious improvement":

- **Scope is an identity, not a per-command capability.** The two tokens do not partition commands cleanly. `uzi admin *` is the only group that *unambiguously* requires `uza_` and cannot run under `uzc_`. Almost everything else (`whoami`, `run list`, and owner writes like `run create`/`approve`) works under **both** tokens — a large overlap set. For that set "pick the token it needs" is undefined; resolving it by preferring the more powerful token silently runs every command at admin identity, which inverts least-privilege and makes the effective identity (`is_admin` true vs false) invisible and command-dependent.
- **It reimplements the server's authorization map on the client.** The server enforces the scope ceiling in exactly one place (`cli_auth.go`) and explicitly refuses "a second mechanism that can drift from the masking". A client-side command→scope table is that second mechanism, one layer further out; the day a command's required scope changes server-side, the client's guess is silently wrong (confusing 403s, or worse a wrong-identity success).
- **Debuggability.** Explicit switching gives crisp failures ("you used the admin context for a write"); auto-selection turns every failure into "which token did it pick, and why".

If auto-selection is ever wanted, the only defensible bounded form is sugar layered **on top of** explicit contexts — `uzi admin …` transparently using a configured admin context, because admin verbs have zero user-scope meaning — and that is a separate, later follow-up, not this PRD.

## Resolved codebase facts (baked in for the offline worker)

Exact anchors so the implementer need not rediscover them. Line numbers are the state at authoring time — re-derive them at implementation; the symbols are stable.

- **Config/credential types (already context-shaped)** — `api/internal/uzicli/config.go`: `Config{ Current string \`toml:"current,omitempty"\`; Contexts map[string]Context }` (:46-49); `Context{ URL string }` (:51-54); `Credentials{ Contexts map[string]Credential }` (:56-59); `Credential{ Token string }` (:61-64). `Settings{ URL, Token string }` (:66-71) is the resolved pair before env/flag overrides.
- **`Store.Resolve(ctxName string)`** (`config.go:154-174`): empty `ctxName` → `"default"` (:155-157); reads `cfg.Contexts[ctxName].URL` and `creds.Contexts[ctxName].Token`; applies **no** env/flag overrides (that is the caller's layer). **This function already does what M1 needs — do not rewrite it; call it with the resolved name.**
- **File I/O + safety invariants to preserve** — `LoadCredentials` (`config.go:107-136`) refuses to read a group/world-accessible `credentials.toml` (perm `&0o077 != 0`, :116-120) and drops the underlying toml error so a bearer token never leaks on parse failure (:125-131). `SaveCredentials` writes `0600` (:140-149), `SaveConfig` `0644` (:93-102), both via `writeFileAtomic` (:187-207). The store dir is **not** XDG (`DefaultStore`, :20-32 — hardcoded `~/.config/uzi`, deliberately, do not "fix" to read `$XDG_CONFIG_HOME`). Named contexts share this one Store, so all of these invariants apply to every context for free — do not add a second code path.
- **The single resolution site** — `api/cmd/uzi/root.go`: `resolveSettings(env, gf)` (:227-246) is the one place settings are resolved. It calls `env.Store.Resolve("default")` (:231), then layers `$UZI_URL` (:236-238), `$UZI_TOKEN` (:239-241), `--url` (:242-244). There is deliberately **no `--token`** flag (a credential must never land on argv — :225-226). `globalFlags{ json, url, quiet, noColor }` (:118-123); persistent flags registered at :195-199 on `root.PersistentFlags()`. `env.client(gf)` (:249-255) and `env.printer(gf)` (:258-260) both flow through `resolveSettings`, so fixing that one function reaches every command.
- **Credential-write sites that hardcode `"default"`** (the full set the move must cover):
  - `api/cmd/uzi/auth.go` — `uzi auth token` writes `creds.Contexts["default"]` (:46-48); `uzi auth status` prints `context: "default"` / `CONTEXT default` (:73-84). `readToken` reads stdin (:95-108), `--with-token` forces stdin on a TTY (:58). `maskToken`/`tokenPrefix` (:112-124) render `uzc_`/`uza_` prefixes.
  - `api/cmd/uzi/login.go` — `uzi login` writes `cfg.Contexts["default"]` (:148-150) and `creds.Contexts["default"]` with the brokered token (:162-165); the `uzi logout` stored-token guard reads `creds.Contexts["default"]` (:196); `uzi logout` does `delete(creds.Contexts, "default")` (:202). All four must target the **active** context.
- **`Resolve`'s empty-string default** (`config.go:156`) stays as the last-resort fallback; the new precedence chain resolves to `"default"` before calling `Resolve`, so `Resolve("")` should never be hit from the CLI but remains correct if it is.
- **Command tree** — `api/cmd/uzi/root.go:201-219` `root.AddCommand(...)`; add `newContextCmd(env, gf)` there beside `newAuthCmd`. `newAuthCmd`/`newWhoamiCmd` live in `auth.go`.
- **Existing tests to extend, not break** — `api/cmd/uzi/auth_test.go` asserts `creds.Contexts["default"].Token` after `auth token` (:38); `api/internal/uzicli/config_test.go` round-trips `Contexts["default"].URL` (:35-36); `api/cmd/uzi/login_test.go` has four `store.Resolve("default")` assertions (:88, :137, :173, :207 — the last two assert logout empties the token), which D4/D8 must keep green. Back-compat means these keep passing unchanged (the active context resolves to `"default"` when nothing is set).

## Design decisions (bake these into the implementation)

- **D1 — Active-context precedence**: `--context/-c <name>` flag > `$UZI_CONTEXT` > `config.Current` > `"default"`. An **empty** `$UZI_CONTEXT` or `-c ""` is treated as **unset** and falls through to `config.Current`/`"default"`, matching the `os.Getenv(...) != ""` "set" test `resolveSettings` already uses (add a one-line test). Implement as one helper — `resolveContextName(env, gf) (name string, explicit bool, err error)` — used by `resolveSettings` and the write commands, so there is a single definition of "the active context". It **resolves the NAME only and does NOT validate existence** — the caller decides (read/run callers validate per D9, write callers create per D4). Return an `error` (not a bare string) so a `LoadConfig` failure surfaces instead of being swallowed; thread the loaded `*Config` through to avoid a second load in `Resolve`. The `explicit` bit — true when the name came from `--context`/`$UZI_CONTEXT`/`config.Current`, false for the implicit `"default"` fallback — is what D9's existence check keys on.
- **D2 — Per-invocation overrides still win**: keep `$UZI_TOKEN` overriding the resolved token and `$UZI_URL`/`--url` overriding the URL, applied **after** context resolution (unchanged order in `resolveSettings`). The headless `UZI_URL`+`UZI_TOKEN` path must behave byte-identically whether or not contexts exist.
- **D3 — URL inheritance**: resolved URL = active context's `Context.URL`, else the `default` context's `Context.URL`; then `$UZI_URL`/`--url` layer on top. This lets the two-tokens-one-server case store the URL once. (Token does **not** inherit — a context with no token resolves to an empty token and the command fails auth, which is correct; you would not want the admin context silently borrowing the default token.) **Multi-server caveat**: inheriting `default`'s URL is right for the same-server motivating case, but a URL-less context aimed at a *different* server would send its token to the wrong host — mitigated by setting an explicit per-context URL (D7 `context set --url`); the docs (M4) must note this.
- **D4 — Where writes land**: `uzi auth token` / `uzi login` write to the **active** context (so they compose with `use` and `--context`); `--context <name>` on them is the explicit override. `uzi logout` removes the **active** context's stored token (and, if the context becomes empty, may drop the context entry). Document this clearly so `auth token` after a `context use admin` is understood to target `admin`. **The `--context` asymmetry, stated once (the single most likely trap)**: on a **write** command (`auth token`, `login`, `context set`) an unknown context name is **created**; on a **read/run** command it is a D9 error. The shared `resolveContextName` helper (D1) never decides this — write callers create, read/run callers validate. Do not fold existence-validation into the helper.
- **D5 — `context use`/`rm` validation**: `use <name>` sets `config.Current = name`; it requires the context to already exist (present in `config.Contexts` **or** `credentials.Contexts`), else a clear error telling the user to create it first (`uzi auth token --context <name>` / `uzi login --context <name>`). `rm <name>` deletes the entry from both config and credentials; if `Current == name`, reset `Current` to `"default"`. Removing `default` clears its stored entries but the implicit `default` still resolves (empty token) — that is fine.
- **D6 — Security unchanged, stated in the docs**: contexts are pure client-side credential *selection*. No server/API/DTO/route/scope change; `cli_auth.go` masking and the RequireAuth/RequireUser tiers are untouched. Authority is still the token's scope, enforced server-side — a context does not grant capability, it only picks which already-minted credential is sent. The `0600` creds-file rule and the no-token-on-argv rule extend to every context automatically (one Store). The docs must say this so nobody reads "contexts" as "the CLI can now switch privilege".

## Milestones

1. **Context resolution core.** Add the `--context`/`-c` persistent flag (`root.go` PersistentFlags) and read `$UZI_CONTEXT`. Add `resolveContextName(env, gf)` implementing D1. Rewrite `resolveSettings` to resolve the active name, call `Store.Resolve(name)`, and apply D3 URL inheritance, keeping the existing `$UZI_URL`/`$UZI_TOKEN`/`--url` layering (D2) in the same order. **Enforce D9 here**: when the resolved name is **explicit** (D1's `explicit` bit) and present in neither `config.Contexts` nor `credentials.Contexts`, fail with `unknown context <name>` (exit 2, usage) before building the client; the implicit `"default"` fallback must **never** error on absence (a fresh user's first `uzi --url … login` has no `default` yet). Unit tests: precedence (flag > env > Current > default), URL inheritance from `default`, an unknown **explicit** context → `unknown context` error while a fresh-user implicit `default` still resolves, empty `$UZI_CONTEXT`/`-c ""` treated as unset, and **back-compat** — with nothing set, resolution is byte-identical to today (the existing `auth_test.go`/`config_test.go`/`login_test.go` still pass, plus a new explicit "no context configured behaves as default" test).
2. **`uzi context` verb group.** New `context.go` with `list` (table: NAME, URL — the **stored** URL, blank when the context inherits `default`'s per D3, so the table never implies every context stores its own — TOKEN stored?/prefix, a CURRENT marker; `--json`), `current` (print the sticky `Current`, or `default`; document that `--context`/`$UZI_CONTEXT` override per-invocation), `use <name>` (set `Current`, D5 validation), `rm <name>` (delete + reset `Current` if needed). Wire into `root.AddCommand`. Unit tests for each verb incl. the unknown-context error paths and the `Current`-reset on `rm`.
3. **Per-context credential writes + status.** `uzi auth token --context <name>` and `uzi login --context <name>` write to the active/named context; `uzi logout` operates on the active context (D4). `uzi auth status` reports the active context, and `--all` lists every stored context (name, url, token-stored — never the value). **Includes `uzi context set <name> --url <url>`** (firm, per D7 — the only way to create a URL-only context, which D3 inheritance and D8 re-login rely on). Unit tests: write-then-resolve round-trips under a named context, `--all` output, logout removes only the active context's token, a confirmation line names the target context (so an accidental overwrite is visible), and a named context's write also lands `0600` and is refused when made group-readable.
4. **Docs + skill (per the repo's "fix the doc in the same change" rule).** Update `api/internal/uzicli/skill/SKILL.md` (the CLI source of truth): add the `uzi context` verbs and `uzi auth token`/`login --context` to the synopsis + reference, add `--context`/`-c` to the global-flags line and `$UZI_CONTEXT` to the env list, and expand the **Configuration and credentials** section to describe named contexts (it currently describes a single implicit slot). Note: the two existing "stored credential" sentences — `SKILL.md:79` ("`UZI_TOKEN` … overrides the stored credential") and `docs/cli.md:353` ("`uzi logout` … removes the stored credential") — are **already correct** under multi-context (they refer to the *active* context), so clarify them to "the active context's credential" rather than treating them as bugs; there is **no** "default context" wording to hunt (verified: `git grep -iF 'default context'` is empty across both docs). Update `docs/cli.md` similarly and add the multi-server URL-inheritance caution (D3). Add a one-line note to `ARCHITECTURE.md`'s CLI map if warranted (the `api/cmd/uzi/` "new functionality ⇒ check the CLI doc" convention applied to itself).
5. **Quality gate + resolution smoke.** `task gate:api` green (fmt-check, vet, build, lint ratchet, deadcode, `-race` tests). Add a pure-local end-to-end smoke (no server): store two contexts with distinct placeholder tokens (`uzc_…`, `uza_…`), then assert `Store.Resolve`/`resolveSettings` returns the right token under `config.Current`, under `--context`, and under `$UZI_CONTEXT`, and that `$UZI_TOKEN` still overrides all of them. (Resolution is client-side, so this needs no network and proves the whole feature.)

## Success criteria

1. A user can store two tokens under two named contexts and switch between them with `uzi context use <name>`, a `--context/-c <name>` flag, or `$UZI_CONTEXT`, with no `UZI_TOKEN` juggling.
2. `uzi context list` shows every stored context, which is current, and whether each has a token — never a token value. `uzi context current` prints the sticky context; `uzi context rm` removes one and resets `current` if it pointed there.
3. `uzi auth token --context <name>` and `uzi login --context <name>` store under that context; `uzi auth status` shows the active context and `--all` lists all of them.
4. **Back-compat is byte-identical**: a user who never touches contexts, and every existing script/CI using `UZI_URL`+`UZI_TOKEN`, behaves exactly as before; no config/credentials file migration is required (the on-disk format is already `[contexts.default]`).
5. The scope/security model is unchanged: no server, API, route, auth-tier, or scope change; a context selects a stored credential, it never grants capability. The `0600` credentials-file refusal and the no-token-on-argv rule hold for every context.
6. `task gate:api` (incl. `-race`, deadcode, lint ratchet) is green; the resolution precedence, URL inheritance, back-compat, and every new verb are covered by pure Go unit tests.
7. No doc still implies exactly one credential slot: the **Configuration and credentials** sections of `SKILL.md` and `docs/cli.md` describe named contexts, both list the `uzi context` verbs and the `--context`/`-c` flag and `$UZI_CONTEXT`, and the two existing "stored credential" sentences read as "the active context's credential". (Judge by meaning, not a literal grep-clean gate — the notion appears in several correct phrasings.)

## Risks and mitigations

- **Accidental token overwrite** (`auth token` after a `context use` targets the switched context) → the write commands print a confirmation line naming the target context (masked prefix), so an overwrite is visible; `--context` makes the target explicit.
- **URL fails to resolve for a context with no stored URL and no `$UZI_URL`/`--url`** → D3 inherits the `default` context's URL; if none resolves, the command errors clearly ("no URL for context <name>; set one with `uzi context set <name> --url …` or `$UZI_URL`/`--url`") rather than sending to an empty host.
- **Back-compat regression** → M1's dedicated "no context configured == today" test plus the untouched `auth_test.go`/`config_test.go` pin identical default behavior; the change adds a resolution step in front of `Store.Resolve` without altering `Resolve` itself.
- **Users misread contexts as privilege switching** → D6 docs state plainly that a context selects a credential and authority is still the token's server-enforced scope; the Non-goal section rules out auto-selection.
- **Credential-file safety must extend to all contexts** → it does automatically (one Store, one `LoadCredentials`/`SaveCredentials` with the `0600` refusal); a test asserts a named context's write also lands `0600` and is refused when made group-readable.
- **Concurrent config/credential writes** (two `uzi` processes writing at once) → `writeFileAtomic` prevents a torn file but not a lost update on the load-modify-save path. Pre-existing for the single slot and not worsened by contexts; acceptable for a local single-user CLI, noted here as a known limitation.

## Dependencies

- Existing seams only: `uzicli.Store` (`config.go`), `resolveSettings` (`root.go`), and the `auth`/`login`/`logout` commands. No new migration, no server change, no new third-party dependency, no cross-repo change.

## Out of scope

- **Command→scope auto-selection** — see the Non-goal section (scope is an identity; explicit switching keeps effective identity legible; the only defensible form is later sugar on top of contexts).
- **Any server, API, DTO, route, auth-tier, or scope change.** Minting/rotating/deleting tokens stays web-only (PRD #104 D8); this PRD does not touch it.
- **Syncing contexts across machines or into a secrets manager** — the credentials file stays local and `0600`, as today.
- **A per-context default output format / non-secret preferences** beyond URL — `Context` stays `{URL}` for this PRD; extending it is a later change if wanted.

## Open questions / Decision log

- **D1–D6**: resolved above (precedence, override order, URL inheritance, write targeting, `use`/`rm` validation, security framing).
- **D7 — Include `uzi context set <name> --url <url>`?**: RESOLVED — **yes, firm M3 deliverable.** It is the only way to create a URL-only context (both `auth token` and `login` require a token), which D3's inheritance and D8's re-login-after-logout both depend on. Cheap since `Context.URL` already exists.
- **D8 — Should `uzi logout` delete the context entry or just its token?**: RESOLVED — clear the active context's **token**; drop the context entry only if it becomes empty (no URL either). Leaves a URL-only context intact for a re-login.
- **D9 — Auto-create a context on `--context <name>` for a name that does not exist?**: No — a read/run with an unknown `--context` should error (unknown context), not silently create an empty one; creation is an explicit act (`auth token`/`login --context`, or `context set --url`). This mirrors `use`'s D5 validation.
