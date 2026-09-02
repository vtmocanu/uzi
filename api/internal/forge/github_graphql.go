package forge

// github_graphql.go holds the GitHub GraphQL transport shared by github_mr.go's
// review-thread lookups and projectsync.go's ProjectBoardSyncer methods.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// --- Raw GraphQL client for Projects v2 (PRD #364 M1) ----------------------
//
// Projects v2 has NO REST surface (F1), so the driver POSTs raw GraphQL to the
// forge's /graphql endpoint. Two transport preconditions are load-bearing and
// both are satisfied here:
//
//   - AUTH: the POST goes through g.client.Client(), the go-github *http.Client
//     whose transport injects "Authorization: Bearer <token>" (built in newGitHub) and
//     carries the per-call timeout. The struct's logClient is deliberately no-auth
//     and redirect-refusing and is NEVER used for Projects v2.
//   - ENDPOINT: derived from g.client.BaseURL() (never a hardcoded
//     api.github.com/graphql), so the httptest harness — which reaches the driver
//     via WithEnterpriseURLs and thus serves under <srv>/api/v3 — can mount and
//     intercept the GraphQL route. See graphqlEndpoint.

// graphqlEndpoint maps go-github's REST base URL onto the matching GraphQL
// endpoint. go-github's BaseURL() is "https://api.github.com/" for the default
// host and "<X>/api/v3/" for an enterprise/httptest base set via
// WithEnterpriseURLs. The default host's GraphQL lives at
// https://api.github.com/graphql; an enterprise/httptest base "<X>/api/v3" maps
// to "<X>/api/graphql" (GitHub's documented GHES convention), which is exactly
// what lets the mock mux intercept it.
func graphqlEndpoint(restBase string) string {
	base := strings.TrimRight(strings.TrimSpace(restBase), "/")
	if base == "" || base == "https://api.github.com" {
		return "https://api.github.com/graphql"
	}
	base = strings.TrimSuffix(base, "/api/v3")
	return base + "/api/graphql"
}

// graphqlURL is the driver's GraphQL POST target, derived from the authenticated
// client's configured REST base so tests can intercept it.
func (g *github) graphqlURL() string {
	return graphqlEndpoint(g.client.BaseURL())
}

// graphqlError is one entry of a GraphQL response's top-level errors array. A
// non-empty array means the operation failed even on an HTTP 200 (GraphQL's
// error model), so graphqlDo surfaces these as a redacted Go error.
type graphqlError struct {
	Message string `json:"message"`
	// Type is GitHub's machine-readable error classification (e.g. "NOT_FOUND",
	// "FORBIDDEN"). graphqlDo inspects it so a caller can distinguish a
	// non-existent login (NOT_FOUND) from a permission/transient failure without
	// parsing the redacted message; see ErrGitHubUserNotFound in projectsync.go.
	Type string `json:"type"`
}

// graphqlResponse is the standard GraphQL envelope: data plus an optional
// top-level errors array.
type graphqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphqlError  `json:"errors"`
}

// graphqlDo POSTs {"query":…, "variables":…} to the driver's GraphQL endpoint
// using the authenticated client's transport (auth header + timeout), then
// decodes the JSON "data" object into out. It returns a REDACTED error on a
// transport failure, a non-2xx status, an undecodable body, OR a non-empty
// GraphQL errors array (whose messages it surfaces, redacted so a reflected PAT
// never escapes). out may be nil when the caller does not need the data.
func (g *github) graphqlDo(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return g.wrapErr("graphql: encode request", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.graphqlURL(), bytes.NewReader(payload))
	if err != nil {
		return g.wrapErr("graphql: build request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// g.client.Client() carries the Bearer-token transport and the per-call
	// timeout; NEVER g.logClient (no auth, refuses redirects).
	resp, err := g.client.Client().Do(req)
	if err != nil {
		return g.wrapErr("graphql", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTraceBytes+1))
	if err != nil {
		return g.wrapErr("graphql: read response", err)
	}
	if resp.StatusCode/100 != 2 {
		return g.wrapErr("graphql", fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
	}
	var envelope graphqlResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		return g.wrapErr("graphql: decode response", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		notFound := false
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
			if e.Type == "NOT_FOUND" {
				notFound = true
			}
		}
		// The joined message is still redacted so a reflected PAT never escapes.
		redactedErr := g.wrapErr("graphql", fmt.Errorf("%s", strings.Join(msgs, "; ")))
		if notFound {
			// Wrap the (already redacted) error so errors.Is(err,
			// ErrGitHubUserNotFound) is true for the caller while the scrubbed
			// message stays authoritative. Only a NOT_FOUND type is wrapped, so a
			// permission/transient error stays a plain redacted error and maps to
			// 500 rather than "bad username".
			return fmt.Errorf("%w: %w", ErrGitHubUserNotFound, redactedErr)
		}
		return redactedErr
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return g.wrapErr("graphql: decode data", err)
		}
	}
	return nil
}
