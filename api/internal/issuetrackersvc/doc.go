// Package issuetrackersvc holds the decision logic of external issue tracker
// connections (PRD #4173): connect (verify and store a token), list, read and
// delete a connection, and open the stored token to build a driver for project
// and state discovery.
//
// The rules live here so every caller shares them: the base URL must be https
// scheme and host only and on the issue tracker allowlist, the token is sealed
// with the user, kind and base URL bound in as additional authenticated data,
// and no method ever returns or logs the token. Failures are typed *Error;
// each caller maps them to its own wire format.
package issuetrackersvc
