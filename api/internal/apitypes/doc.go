// Package apitypes holds the JSON wire types (DTOs) the api serializes on the
// RequireUser routes — the exact set the uzi CLI binary unmarshals (PRD #64 M1).
//
// It is a LEAF: it imports only the standard library. That is the whole point —
// the CLI links these types without dragging pgx, chi, or any handler/service
// dependency into the binary (Success Criterion 8, enforced by a go list -deps
// assertion). Membership is precisely "types the CLI decodes"; DTOs behind
// cookie-only routes stay in the handler package. Handlers own the mappers that
// build these from store rows; this package owns only the shapes.
package apitypes
