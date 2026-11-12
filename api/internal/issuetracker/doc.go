// Package issuetracker holds drivers for external issue trackers: systems that
// keep issues but host no code (PRD #4173 D6).
//
// A Driver implements forge.IssueTracker and never forge.CodeHost (PRD #4173
// S1): an issue tracker cannot answer repository, merge request or pipeline
// questions, so the type system refuses to let one be used as a code host.
// Each driver is constructed through New by Kind.
//
// Credentials are handled per PRD #4173 S3: the token is scrubbed from every
// error a driver returns (see redactor), responses are size-bounded, redirects
// are refused so the token never follows a Location header, and the HTTP
// client has a finite timeout.
package issuetracker
