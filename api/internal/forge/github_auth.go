package forge

// github_auth.go is the GitHub token-verification / identity seam: VerifyToken
// (classic-PAT identity + admin detection) and TokenInfo/parseGitHubScopes
// (header-derived scope and expiry introspection).

import (
	"context"
	"errors"
	"net/textproto"
	"strings"
	"time"
)

func (g *github) VerifyToken(ctx context.Context) (BotIdentity, error) {
	// Enforce classic-only UP FRONT (D3/H4). A fine-grained token (github_pat_
	// prefix) cannot be introspected — no X-OAuth-Scopes header, a buggy expiry
	// header (go-github #3708) — so accepting it would silently save a token whose
	// scopes uzi can never check, defeating the over-privilege guard for exactly the
	// token type whose breadth is unknown. Refuse it here (fail-safe) rather than
	// fall through to TokenInfo's introspection-unsupported WARNING path, which is
	// the right disposition for GitLab's known-coarse token but wrong for an
	// un-introspectable one. The message carries no secret.
	if strings.HasPrefix(g.token, "github_pat_") {
		return BotIdentity{}, errors.New("github: fine-grained tokens are not supported yet; use a classic PAT (ghp_…)")
	}
	// No version gate (D4). GET /user (empty user = the authenticated user).
	u, _, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return BotIdentity{}, g.wrapErr("verify token", err)
	}
	// site_admin==true is an instance-admin (god-mode) PAT; the privilege checker
	// treats it as a violation exactly as GitLab/Forgejo instance-admin.
	return BotIdentity{ForgeUserID: u.GetID(), Username: u.GetLogin(), IsAdmin: u.GetSiteAdmin()}, nil
}

func (g *github) TokenInfo(ctx context.Context) (TokenInfo, error) {
	// HAND-ROLLED FROM RESPONSE HEADERS (D7): GitHub has no token-introspection JSON
	// endpoint. A classic PAT's granted scopes come back in X-OAuth-Scopes and its
	// expiry in GitHub-Authentication-Token-Expiration on ANY authenticated request;
	// validity is simply whether the request 200s. Make one lightweight call and read
	// the headers off *github.Response.Response.Header.
	_, resp, err := g.client.Users.Get(ctx, "")
	if err != nil {
		return TokenInfo{}, g.wrapErr("token info", err)
	}
	if resp == nil || resp.Response == nil {
		return TokenInfo{}, errors.New("github: token info: response carried no headers")
	}
	h := resp.Header
	if _, present := h[textproto.CanonicalMIMEHeaderKey("X-OAuth-Scopes")]; !present {
		// No scopes header at all → a fine-grained / un-introspectable token. VerifyToken
		// already rejects github_pat_, so this is a defensive fallback: surface the
		// introspection-unsupported sentinel (the caller downgrades to a warning).
		return TokenInfo{}, ErrTokenIntrospectionUnsupported
	}
	info := TokenInfo{
		Scopes: parseGitHubScopes(h.Get("X-OAuth-Scopes")),
		Active: true, // the call 200'd, so the token is usable
	}
	// Expiry: absent/empty means never expires (zero time). GitHub emits it as
	// "2006-01-02 15:04:05 -0700" or "... UTC"; on an unparseable value leave zero
	// rather than error (a missing expiry is not a scope-check failure).
	if exp := strings.TrimSpace(h.Get("GitHub-Authentication-Token-Expiration")); exp != "" {
		for _, layout := range []string{"2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05 MST"} {
			if t, perr := time.Parse(layout, exp); perr == nil {
				info.ExpiresAt = t
				break
			}
		}
	}
	return info, nil
}

// parseGitHubScopes splits the X-OAuth-Scopes header (comma-separated) into a
// trimmed, empty-dropped scope list. A classic PAT with the {repo} scope yields
// []string{"repo"}; a scope-less header yields an empty slice.
func parseGitHubScopes(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
