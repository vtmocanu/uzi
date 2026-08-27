// Package apitypes holds the JSON wire types the api and the uzi CLI binary share
// (PRD #64 M1).
//
// Mostly RequireUser routes, which is what this sentence used to say outright. It
// stopped being exhaustive with BuildInfoDTO (PRD #175), the response of the
// unauthenticated GET /api/version. The membership rule is "both ends of the wire
// need the shape", not "the route is authenticated".
//
// That is a bigger change for a future author than it looks, so it is stated here
// rather than only on the type. Until BuildInfoDTO, every type in this package sat
// behind auth, so "is this field safe to expose?" had ONE package-level answer. It now
// has two. Someone adding a field to BuildInfoDTO is warned by its own doc; someone
// making a CROSS-CUTTING change — a shared Meta embed, a trace_id on every DTO, a
// linter-driven sweep — reads this page and would not be. One type here is served on
// an UNAUTHENTICATED route, so a change touching every type in this package touches a
// world-readable one.
//
// It is a LEAF: it imports only the standard library. That is the whole point —
// the CLI links these types without dragging pgx, chi, or any handler/service
// dependency into the binary (Success Criterion 8, enforced by a go list -deps
// assertion). DTOs behind cookie-only routes stay in the handler package. Handlers
// own the mappers that build these from store rows; this package owns only the shapes.
//
// Membership is BOTH DIRECTIONS of the wire, not only responses: RunInputRequest and
// JudgeBulkDispositionRequest are bodies the CLI ENCODES and a handler decodes. (This
// sentence used to read "the exact set the uzi CLI binary unmarshals" / "types the CLI
// decodes", which RunInputRequest already contradicted; corrected 2026-07-21 while adding
// the PRD #98 M7 request types.) A shared request type is worth more than a shared
// response type here, because httpx.DecodeJSON runs with DisallowUnknownFields — one
// definition means a client/server key mismatch cannot be written, rather than surfacing
// as a 400 at runtime.
package apitypes
