# ADR-3508: Browser vault re-unlock

**Status**: Accepted
**Date**: 2026-10-29
**Issue**: #3508

## Context

Protected vaults lose their cached DEK at API restart. Requiring the owner
to type the password/passphrase for each release interrupts browser use.
Keep the password wrapper and add a browser-held wrapping key so a returning
foreground tab can restore the cache. [ADR-3507](3507-optional-credential-vault.md)
still governs optional protection, conversion and singleton/Recreate operation.

This decision explicitly supersedes the historic OIDC no-auto-unlock
non-goal in frozen specs/ai.md:21983 (and its explanatory statement at
21951). The frozen record is unchanged. The new behavior requires a
previously minted browser grant and an eligible foreground request; an
OIDC callback itself neither unlocks nor mints.

## Decision

Persist a second DEK wrapper in `vault_browser_slots`. Generate a random
256-bit wrapping key per mint, with AES-GCM AAD binding the owner and slot ID
(`browserAAD` in `api/internal/vault/browser.go`). Send the key and slot ID
in `__Host-uzi_vault_slot`, a Secure, HttpOnly, SameSite=Strict, host-only
cookie (no Domain), Path=/. The `__Host-` prefix makes browsers reject the
name with a Domain or a non-root Path, so a same-site sibling origin cannot
plant one that persists beside the grant and drains the shared unlock budget;
the server reads no other name. `Path=/` is the prefix's requirement; nginx
logs no requests (`access_log off`), so static paths do not expose it. The
DB stores the wrapped DEK and metadata, not the cookie key. No server-at-rest material, including `UZI_SECRET_KEY` or the
JWT signing secret, opens this wrapper without the cookie key.

Mint after verified interactive password login, unlock or enrollment for an
eligible protected HTTPS browser. HTTP deployments never mint or emit a
secret keyslot cookie and require manual unlock after restart. CLI, seed,
boot unlock, OIDC callbacks and restore cannot mint. Restore cannot extend
a slot's lifetime.

The Settings label **Re-unlock automatically in this browser** is default-on
**account-wide permission**. Turning it off revokes every browser's slots;
turning it on alone mints nothing until a verified interactive operation.
This is distinct from enabling/disabling vault protection.

Sign browser identity into the JWT. Rolling refresh preserves it; fresh login
creates a new identity. Legacy JWTs remain compatible with normal
authentication; interactive HTTPS unlock can add an identity while preserving
the incoming expiry. Store at most 32 browser slots per owner: mint removes
expired and same-browser rows and trims oldest other rows before insertion
(`api/internal/store/queries/vault_browser_slots.sql`).

The admin database setting `vault_reunlock_days` defaults to 7, accepts 1–30
days, and applies to newly minted slots. Fixed expiry is the earlier of
mint time plus that lifetime and the minting session's expiry. Refresh,
restore and later settings changes do not extend existing slots. Lowering
the TTL affects new slots. Slot expiry does not evict a cached DEK.

## Online admission and lifecycle

Restore requires a valid active session with matching token version and
browser ID, plus the existing **incoming JWT-bound X-CSRF-Token**, even for
GET/HEAD. Authentication and this check precede slot access. Missing/invalid
CSRF leaves normal safe-read behavior unchanged: no slot access, unlock
limiter reservation, restore or promotion. Validation precedes rolling
session refresh (`api/internal/middleware/auth.go`,
`api/internal/handler/browser_session.go`). Eligible failed attempts share
the owner's manual-unlock limiter; successful restore refunds its reservation.
A definitive rejection (malformed or duplicate cookie, missing, expired or
revoked slot, wrong key, stale session, opt-out) also clears the slot cookie
on that response, so a browser whose grant was revoked elsewhere stops
spending the owner's unlock budget; the rejected attempt stays charged.
Transient failures (slot read errors, cancelled requests, uncertain lifecycle
outcomes) keep the cookie. Requests already in flight with the old cookie can
each still consume one attempt, and a clear racing a fresh unlock in the same
browser can drop the new slot, costing one manual unlock after a restart.

The client sends the readable CSRF cookie on foreground reads and marks
hidden-tab requests passive. Passive requests, WebSocket upgrades and
/api/ws do not restore. Lifecycle endpoints, logout and settings writes
also bypass restore. A tab opening/focus status read can restore, clear
the lock-notice episode on a best-effort basis and promote that owner's
`vault_locked` recovery parks through `vaultReady`; scheduled retry remains
the fallback and workers use normal claim admission.

Serialize mint, restore and revocation under the owner lifecycle mutex and
transactional owner fences. Preserve the original authentication snapshot
across waits. Compare owner, browser ID, token version, mode epoch, slot
generation and actual expiry after waiting; publish the DEK after an
acknowledged commit. The monotonic `vault_slot_generation` is separate from
`vault_epoch`: explicit lock revokes without changing the ciphertext-mode
epoch. Migration 00379's users trigger advances generation and deletes slots
on relevant auth/mode changes, including storage-level password updates.

Explicit lock durably revokes slots and evicts the cached DEK. Global logout
revokes sessions and slots while retaining an already cached DEK on
acknowledged success. Password changes, automatic-re-unlock opt-out,
switching Off, vault recreation and deactivation revoke slots. Opt-out alone
does not evict a cached DEK. Slot expiry and session expiry do not impose
an idle lock on the API cache.

## Security analysis and limits

a stolen keyslot cookie plus a database snapshot containing its wrapped DEK permits offline DEK recovery, without a valid session.

This is a new bearer unlock capability. Theft of the keyslot and a valid
session permits online restoration while authorized, with that session's CSRF
token. Server-side expiry and revocation prevent future online restores but
cannot invalidate copied key material.

Online authentication, expiry, revocation and limiter checks cannot protect
a stolen snapshot against that offline attack. Strict protects across sites,
not sibling origins on the same site; the `__Host-` prefix only stops those
origins from setting this cookie. Cookies are not isolated by port, so another
HTTPS service on the same hostname receives and can replace it; deployments
must not run untrusted services on uzi's hostname. HttpOnly prevents ordinary script
reads of the cookie but cannot stop an active same-origin script from
issuing authenticated requests. Live-server compromise, cached-DEK memory
dumps, password cracking and retained master-sealed backups remain residuals.
See [the threat model](../docs/vault-threat-model.md#browser-re-unlock).

Runs of a user who keeps the vault still wait after a restart until that user opens or focuses a uzi tab.

The CLI does not get a keyslot here.
Unattended continuity is outside scope. Seed boot-unlock retains its existing
separate behavior; it does not mint slots.

A forgotten password/passphrase does not destroy usable remaining key
material: a cached DEK or usable browser grant may preserve access. Protected
contents become unrecoverable after losing both the password/passphrase and
remaining usable key material. This adds no password-reset, passphrase-change
or recovery feature.

This threat-model review is design analysis. Executed checks and independent
security review evidence belong in the implementation PR; this ADR does not
certify those checks.

## Deployment and rollback

Keep the API singleton with coordinated Recreate replacement; mixed versions
remain unsupported. Migration **00379** adds constant-default users columns,
immediately validated CHECK constraints, slot storage and the revocation
trigger. CHECK validation and ACCESS EXCLUSIVE lock duration/availability
impact are production-dependent and unmeasured. Plan maintenance and backups;
there is no measured production migration-duration claim here.

**Delete or invalidate all browser slots before starting a pre-keyslot
binary, in this order:** stop every keyslot-aware API instance and let
in-flight requests drain, then delete the slots (for example by running
migration 00379 Down, which drops `vault_browser_slots`), and only then
start the older binary. While a keyslot-aware API still serves requests, an
interactive unlock can mint a slot after the deletion. Such a binary cannot
maintain slot revocations during subsequent lock/logout/password
operations. A compatible re-upgrade must not resurrect
pre-rollback grants: discard those rows before downgrade, rather than rely
on the older binary to invalidate them. Migration 00379 Down drops slot
storage; 00373's independent mode/ciphertext rollback restrictions in
[ADR-3507](3507-optional-credential-vault.md) still apply.

## Implementation references

- `api/internal/vault/browser.go`: mint, owner/slot AAD, snapshot checks and restore.
- `api/internal/vault/auth_lifecycle.go` and `api/internal/vault/lifecycle.go`: serialized revocation and cache boundaries.
- `api/internal/handler/browser_session.go`: cookie attributes, CSRF admission and interactive session preparation.
- `api/internal/store/migrations/00379_vault_browser_slots.sql`: storage and revocation trigger.
- `web/src/lib/api.ts` and `web/src/auth/AuthContext.tsx`: foreground/passive request handling and focus refresh.
