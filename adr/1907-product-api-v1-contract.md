# ADR-1907: `/api/v1` is the stable external contract; only `RequireV1Caller` resolves a product token

**Status**: Accepted (PRD #1907 M1-M6 implemented; M7, hosted k8s acceptance, is maintainer-owned and not done)
**Date**: 2026-09-29
**Issue**: [vtmocanu/uzi#1907](https://github.com/vtmocanu/uzi/issues/1907)
**PRD**: [prds/1907-product-tokens-api-v1.md](../prds/1907-product-tokens-api-v1.md)

## Decision (summary)

> `/api/v1` is uzi's stable, versioned external API, described by a checked-in
> OpenAPI 3.1 document and parity-tested against the router. Changes are
> additive only, with a deprecation window for anything breaking. Product
> tokens (`uzp_`) are resolved by exactly one piece of code,
> `RequireV1Caller`, and `/api/v1` never carries admin authority. The internal
> `/api/*` stays unversioned and free to change.

## Context

External products call uzi on behalf of their users. The only Bearer
credentials that existed were CLI tokens, which carry the owner's full
authority on every route under `RequireUser`, and the only API was the
unversioned `/api/*` tree that the web app and CLI consume and that changes
freely. A product built on it breaks on an ordinary refactor, and a product
holding a `uzc_` holds the whole account (PRD #1907, Problem).

## Decision

1. **The contract.** `/api/v1` is described by `api/openapi/v1.yaml` (OpenAPI
   3.1), owned by PRD #1907 and extended by later PRDs. It ships one endpoint,
   `GET /api/v1/whoami`. The internal `/api/*` routes are not part of the
   contract and products must not call them.
2. **The spec is the source, and it is tested.** `TestV1OpenAPIRouteParity`
   asserts that the spec's operations and the production router's `/api/v1`
   routes are the same set, in both directions (removing `whoami` from either
   side reddens it), and `TestV1OpenAPISchemasMatchDTOs` that response schema
   properties match the Go DTO's JSON fields. DTO shapes are pinned by recorded fixtures as in PRD #982.
3. **Additive only.** New paths, new optional request fields and new response
   fields are compatible. Removing or renaming a path, field or enum value, or
   making an optional request field required, is breaking.
4. **Deprecation window.** A breaking change keeps the old shape served for at
   least two minor releases **and** at least 90 days, whichever is later,
   announced in the changelog; or it ships under a new `/api/v2` prefix. The
   release cadence makes a release count alone too short (PRD #1907, Q1).
5. **Only `RequireV1Caller` resolves `uzp_` (risk R1).** Product tokens live in
   their own `product_tokens` table, and `RequireUser` resolves Bearer values
   against `cli_tokens` only, so a `uzp_` token fails closed on every internal
   route, current and future. A future route must never mount its own Bearer
   auth that reads `product_tokens`. `TestV1IsolationLiveDB` walks both
   production routers and compares a `uzp_` request with an unknown-Bearer
   request on every route outside `/api/v1`.
6. **`/api/v1` never carries admin authority.** `RequireV1Caller` clears
   `IsAdmin` on its copy of the user row for both accepted token kinds, so a
   handler reused from the internal API (which takes admin-ness from the
   context user) cannot act as admin because an admin minted the token. Admin
   views stay on the non-v1 routes.
7. **Dispatch is deterministic.** `RequireV1Caller` is Bearer only (no cookie,
   no CSRF surface) and picks the table from the token prefix, never by trying
   one and falling back: `uzp_` reads `product_tokens`; `uzc_` reads
   `cli_tokens` and the row's scope must be `user` (the prefix is a label,
   authority comes from the row); anything else, including `uza_`, is 401.
   Every failure is the same 401.
   *Amended 2026-10-02 (#1992):* every credential refusal is the same 401,
   and a token-store lookup failure (any error other than "no such row") is a
   fail-closed 503 `auth_unavailable` with no `Retry-After`, so an outage no
   longer reads as a revoked token. A full outage gives the same 503 for every
   `uzp_` or `uzc_` token (any other class never reaches the store and stays
   401, which reveals nothing: the caller chose the prefix). A partial fault (the first lookup succeeds, the later
   user lookup fails) lets the holder of that exact token tell it passed the
   first lookup's checks from an unknown one; for `uzc_` the owner's active
   flag is not yet checked at that point. We do not claim a caller cannot
   induce the fault. Accepted: it reaches only the token's holder, 256-bit
   tokens make it useless for enumeration, and a healthy 200 tells that
   holder more. See the PRD #1907 Decision Log (2026-10-02).

## Consequences

- Products get a surface that does not move under them; the cost is that every
  `/api/v1` change is a spec change and a fixture change.
- The automated OpenAPI breaking-change gate is **not built**. It arrives with
  the first real endpoints in PRD #1908, as a pinned upstream tool behind a
  wrapper script; PRD #1907 deliberately wrote no diff checker for one
  endpoint. Until then additive-only is enforced by review and by the parity
  test, not by an automated diff.
  *Amended 2026-10-02 (#1992):* it is built: `task check:api-v1-compat`
  (PRD #1908, Decision 11), part of `gate:repo`.
- PRD #1908 and later mount their endpoints under the existing
  `RequireV1Caller` and add no auth of their own.
- Scopes (`jobs:run`, `jobs:read`) exist, but no endpoint enforces them yet;
  `whoami` requires none.
  *Amended 2026-10-02 (#1992):* the job endpoints enforce them through
  `RequireScope` in `api/internal/handler/routes_v1.go` (PRD #1908, Decision
  10); `whoami` still requires none.
- A `uzp_` token is a bearer credential: "bound to one product" labels the row
  and does not prove which application presents it. PRD #1910's client
  credentials address that.

## References

- [PRD #1907](../prds/1907-product-tokens-api-v1.md): D1-D5, D12, R1, Decision Log.
- `api/internal/middleware/v1_auth.go`, `api/internal/handler/routes_v1.go`, `api/openapi/v1.yaml`.
- [Product tokens](../docs/product-tokens.md): the user-facing description.
