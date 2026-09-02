package forge

// forgejo_auth.go is the Forgejo token-verification / identity seam: VerifyToken
// (version gate + bot identity) and TokenInfo (hand-rolled token introspection).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

func (f *forgejo) VerifyToken(ctx context.Context) (BotIdentity, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return BotIdentity{}, err
	}
	// Version gate FIRST (D4/D4a). GET /version is public and unauthenticated, so
	// it needs no valid token; refusing an unsupported instance up front means the
	// user hears "wrong version" rather than a confusing later failure. The gate's
	// error is deliberately NOT redacted — it carries only the server's
	// self-reported version string plus uzi's copy, no secret, and CreateConnection
	// surfaces it to the user verbatim.
	raw, _, err := c.ServerVersion()
	if err != nil {
		return BotIdentity{}, f.wrapErr("read server version", err)
	}
	if err := f.checkForgejoVersion(raw); err != nil {
		return BotIdentity{}, err
	}
	u, _, err := c.GetMyUserInfo()
	if err != nil {
		return BotIdentity{}, f.wrapErr("verify token", err)
	}
	// Forgejo always emits is_admin (no omitempty), so false means a real
	// non-admin — the privilege checker treats true as a violation.
	return BotIdentity{ForgeUserID: u.ID, Username: u.UserName, IsAdmin: u.IsAdmin}, nil
}

func (f *forgejo) TokenInfo(ctx context.Context) (TokenInfo, error) {
	c, err := f.newClient(ctx)
	if err != nil {
		return TokenInfo{}, err
	}
	// GET /users/{username}/tokens is keyed by username and gated reqSelfOrAdmin (it
	// admits the bot querying itself, D5), so resolve the bot's own login first.
	u, _, err := c.GetMyUserInfo()
	if err != nil {
		return TokenInfo{}, f.wrapErr("token info: identify bot", err)
	}
	// Hand-rolled, NOT via the SDK: gitea-sdk's ListAccessTokens refuses without
	// BasicAuth ("username not set: only BasicAuth allowed"), a CLIENT-side gate the
	// server does not impose for the GET (D5). The list carries no secret — sha1 is
	// empty except at creation, only token_last_eight is returned — but the error
	// path still routes through the redactor (rawGetLimited does this).
	//
	// token_last_eight is the ONLY per-token fingerprint the list exposes (sha1 is
	// empty post-creation), so on the astronomically rare last-eight COLLISION
	// between two of the bot's own tokens, uzi cannot tell which one authenticated.
	// This is a known, accepted Forgejo-API limit, not an oversight — there is no
	// better disambiguator to invent. We therefore scan every page, require EXACTLY
	// one match, and on 0 or >1 fail SAFE: a generic error that the checker
	// downgrades to a "could not verify scopes" warning. Picking the first match
	// would fail OPEN — an over-scoped ("all") authenticating token could be masked
	// by a correctly-scoped colliding sibling, sliding it past PRD #5's only blocking
	// token check (D6b).
	var last8 string
	if len(f.token) >= 8 {
		last8 = f.token[len(f.token)-8:]
	}
	var matchedScopes []string
	matches := 0
	for page := 1; ; page++ {
		raw, err := f.rawGetLimited(ctx, fmt.Sprintf("/users/%s/tokens?page=%d&limit=%d",
			url.PathEscape(u.UserName), page, forgejoPerPage), maxTraceBytes+1)
		if err != nil {
			return TokenInfo{}, err // already redacted by rawGetLimited
		}
		var tokens []forgejoAccessToken
		if err := json.Unmarshal(raw, &tokens); err != nil {
			return TokenInfo{}, f.wrapErr("parse tokens", err)
		}
		for _, tk := range tokens {
			if tk.TokenLastEight == last8 {
				matches++
				matchedScopes = append([]string(nil), tk.Scopes...)
			}
		}
		if len(tokens) < forgejoPerPage {
			break
		}
		if page >= maxForgePages {
			return TokenInfo{}, f.wrapErr("token info", forgePaginationCapErr("page", maxForgePages))
		}
	}
	if matches == 1 {
		// The unique, listed match authenticated this very request, so it is active.
		// Forgejo PATs report neither an active flag nor an expiry (the API has no such
		// fields — verified live on 16.0.0), so Active is true and ExpiresAt stays zero
		// ("never expires"). Scopes come back normalized and REORDERED (Forgejo
		// re-emits in canonical order, not mint order), which is why the privilege
		// checker compares them as an unordered set (D6b).
		return TokenInfo{Scopes: matchedScopes, Active: true}, nil
	}
	if matches > 1 {
		return TokenInfo{}, fmt.Errorf("forgejo: token info: %d tokens share the authenticating token's last-eight fingerprint; cannot uniquely identify its scopes", matches)
	}
	// 0 matches: the token authenticated but does not appear in its own owner's token
	// list. Surfaced as a generic error, which the privilege checker downgrades to a
	// "could not verify scopes" warning rather than a hard block.
	return TokenInfo{}, fmt.Errorf("forgejo: token info: the authenticating token was not found in its owner's token list")
}

// forgejoAccessToken is the subset of Forgejo's access-token payload uzi reads:
// the scopes, and token_last_eight to identify which listed token is the one
// authenticating the call. Forgejo emits no expiry or active field.
type forgejoAccessToken struct {
	TokenLastEight string   `json:"token_last_eight"`
	Scopes         []string `json:"scopes"`
}
