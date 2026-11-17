// Package providers holds the pure validation rules for user-defined model providers
// (PRD #4320): the exact-match base-URL normalization shared by the operator allowlist parser
// (internal/config) and the request-time destination check (internal/handler), and the bounds on
// a provider's key and model map. It imports nothing from config, store or handler, so every
// layer can use the same rules without an import cycle.
package providers

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxBaseURLBytes bounds a base URL, whether it is an allowlist entry or a user candidate.
const MaxBaseURLBytes = 2048

// IsLoopbackHost reports whether host is a loopback name or IP (localhost, 127.0.0.0/8, ::1).
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// NormalizeBaseURL validates raw as a provider base URL and returns its canonical form:
// lowercase scheme and host, the scheme's default port elided, one trailing slash removed. The
// path is part of the identity (a provider matches only an equal allowlist entry, never a prefix).
//
// It refuses, rather than strips or decodes: anything outside printable ASCII (so no non-ASCII
// host and no whitespace), any percent sign (which covers %2e, %2f and every other encoding),
// a semicolon, a backslash, a query or fragment marker, userinfo, an empty or "." / ".." path
// segment, a missing host and an invalid port. The scheme must be https; http is accepted only
// for a loopback host and only when allowLoopback is set (the dev flag), and a loopback host of
// either scheme is refused without it, so the api cannot be pointed at its own local ports in
// production. The error text names the rule and never echoes the input.
func NormalizeBaseURL(raw string, allowLoopback bool) (string, error) {
	if raw == "" {
		return "", errors.New("base URL must not be empty")
	}
	if len(raw) > MaxBaseURLBytes {
		return "", fmt.Errorf("base URL must be at most %d bytes", MaxBaseURLBytes)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c < 0x21 || c > 0x7E {
			return "", errors.New("base URL must contain only printable ASCII characters")
		}
		switch c {
		case '%':
			return "", errors.New("base URL must not contain percent-encoding")
		case ';':
			return "", errors.New("base URL must not contain a semicolon")
		case '\\':
			return "", errors.New("base URL must not contain a backslash")
		case '?', '#':
			return "", errors.New("base URL must not carry a query or fragment")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("base URL is not a valid URL")
	}
	if u.User != nil || strings.Contains(u.Host, "@") {
		return "", errors.New("base URL must not contain userinfo")
	}
	if u.Opaque != "" || u.Host == "" {
		return "", errors.New("base URL must be an absolute URL with a host")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("base URL has an invalid port")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("base URL must have a host")
	}
	loopback := IsLoopbackHost(host)
	switch {
	case loopback && !allowLoopback:
		return "", errors.New("loopback hosts are not accepted (UZI_PROVIDER_ALLOW_LOOPBACK is not set)")
	case scheme == "https":
	case scheme == "http" && loopback:
	default:
		return "", errors.New("base URL must use https")
	}
	port := u.Port()
	if port != "" {
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return "", errors.New("base URL has an invalid port")
		}
		port = strconv.Itoa(n)
		if (scheme == "https" && n == 443) || (scheme == "http" && n == 80) {
			port = ""
		}
	}
	path := u.Path
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return "", errors.New("base URL path must start with a slash")
		}
		trimmed := strings.TrimSuffix(path, "/")
		if trimmed != "" {
			for _, seg := range strings.Split(trimmed[1:], "/") {
				switch seg {
				case "":
					return "", errors.New("base URL path must not contain empty segments")
				case ".", "..":
					return "", errors.New("base URL path must not contain dot segments")
				}
			}
		}
		path = trimmed
	}
	hostPart := host
	if strings.Contains(host, ":") {
		hostPart = "[" + host + "]"
	}
	if port != "" {
		hostPart += ":" + port
	}
	return scheme + "://" + hostPart + path, nil
}

// MatchAllowed reports whether candidate normalizes to exactly one of the allowlist entries
// and returns that entry verbatim (the form to store). list holds entries already normalized at
// boot by NormalizeBaseURL; a candidate that does not validate, or matches none, is not allowed.
// An empty list allows nothing: the feature is off.
func MatchAllowed(list []string, candidate string, allowLoopback bool) (string, bool) {
	if len(list) == 0 {
		return "", false
	}
	norm, err := NormalizeBaseURL(candidate, allowLoopback)
	if err != nil {
		return "", false
	}
	for _, e := range list {
		if e == norm {
			return e, true
		}
	}
	return "", false
}
