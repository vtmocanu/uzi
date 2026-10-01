// Package oauthsrv holds the pure protocol logic of uzi's OAuth authorization server (PRD
// #1910 D1): redirect-URI validation, scope parsing and the client-secret class. It has no HTTP
// and no SQL, so the RFC detail lives in one table-tested place and the handlers stay thin.
package oauthsrv
