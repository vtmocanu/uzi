// Package egressprofile holds the pure rules for egress profiles (PRD #1906 Decision 4):
// admin-managed, named site lists that a later milestone binds a no-internet research run
// to. Nothing here touches the database or the network; the handler and, later, the
// fetcher both call these functions so a host is normalized the same way when a list is
// written and when a URL is checked against it.
//
// An entry is either an exact host ("docs.example.com") or a wildcard over a base domain
// ("*.example.com"). Normalization (NormalizeEntry, NormalizeHost) lowercases, converts to
// the IDNA A-label form, and strips one trailing dot. Everything that is not a DNS name is
// refused: IP literals, ports, schemes, paths, userinfo, a bare "*", a "*" anywhere but as
// the whole leftmost label, and empty labels.
//
// Wildcard semantics: "*.example.com" matches every PROPER subdomain of example.com
// ("a.example.com", "a.b.example.com") and does NOT match "example.com" itself. An admin
// who wants both lists both. This keeps a wildcard from silently widening to the apex,
// which is often a different site (a marketing host, a redirector) from the docs subtree.
//
// A wildcard that could cover a public suffix is refused: the base must not be a suffix
// on the Public Suffix List (golang.org/x/net/publicsuffix, which embeds both the ICANN
// and the private sections), so "*.com", "*.co.uk", "*.github.io" and "*.cloudfront.net"
// are refused, and no PSL rule may lie below the base, so "*.kawasaki.jp" (the ICANN rule
// "*.kawasaki.jp") and "*.run.app" (the private rule "a.run.app") are refused too. The
// second set is psl_ancestors_gen.go, generated from the same pinned x/net table. A short
// built-in list (sharedParents) adds parents with no PSL rule below them whose subdomains
// still belong to many customers ("*.googleusercontent.com").
//
// Hosts where many unrelated publishers serve content under one host name (code hosting,
// path-style object storage, docs and package hosting, forums) are on a built-in list
// (see multipublisher.go). Such an entry is accepted only with an explicit per-entry
// override, and the admin API returns a warning for every overridden entry.
package egressprofile

// psl_ancestors_gen.go is derived from the pinned golang.org/x/net Public Suffix List
// table; internal/genpsl's test fails when it is stale.
//go:generate go run ./internal/genpsl

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// Limits. They are small on purpose: a site list is read by a human reviewing what a
// research run may reach.
const (
	// MaxEntries caps the host entries in one profile.
	MaxEntries = 200
	// MaxEntryLen caps one entry at the DNS name limit, 253 octets, measured on the
	// normalized A-label form (a wildcard's "*." included). The raw input gets only a loose
	// 4x byte bound, because a Unicode name is longer in UTF-8 than its A-label form.
	MaxEntryLen = 253
	// MaxNameLen caps a profile name.
	MaxNameLen = 64
	// MaxDescriptionLen caps a profile description, in characters (runes).
	MaxDescriptionLen = 500
)

// Problem codes. They are stable strings a client may branch on; the message beside a
// code is for humans.
const (
	CodeEmpty                = "empty"
	CodeTooLong              = "too_long"
	CodeWhitespace           = "whitespace"
	CodeScheme               = "scheme"
	CodeUserinfo             = "userinfo"
	CodePath                 = "path"
	CodePort                 = "port"
	CodeIPAddress            = "ip_address"
	CodeBareWildcard         = "bare_wildcard"
	CodeWildcardPosition     = "wildcard_position"
	CodeEmptyLabel           = "empty_label"
	CodeInvalidHost          = "invalid_host"
	CodeSingleLabel          = "single_label"
	CodeNumericTLD           = "numeric_tld"
	CodePublicSuffixHost     = "public_suffix_host"
	CodePublicSuffixWildcard = "public_suffix_wildcard"
	CodeSharedParentWildcard = "shared_parent_wildcard"
	CodeMultiPublisher       = "multi_publisher_needs_override"
	CodeOverrideNotInHosts   = "override_not_in_hosts"
	CodeTooManyEntries       = "too_many_entries"
	CodeNoEntries            = "no_entries"
	CodeInvalidName          = "invalid_name"
	CodeUnsafeText           = "unsafe_text"
	CodeDescriptionTooLong   = "description_too_long"
)

// EntryError is a refused host entry: a stable Code and a human Message.
type EntryError struct {
	Code    string
	Message string
}

func (e *EntryError) Error() string { return e.Message }

func entryErr(code, format string, args ...any) *EntryError {
	return &EntryError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// idnaProfile is the IDNA mapping used for every host: UTS #46 lookup mapping (which
// lowercases and folds full-width forms), the bidi rule, label validation, DNS length
// checks, and STD3 (letters, digits, hyphen) so "_" and other punctuation are refused.
var idnaProfile = idna.New(
	idna.MapForLookup(),
	idna.BidiRule(),
	idna.ValidateLabels(true),
	idna.VerifyDNSLength(true),
	idna.StrictDomainName(true),
	idna.Transitional(false),
)

// ldhLabel is the post-IDNA shape of one label: 1-63 lowercase letters, digits and
// hyphens, not starting or ending with a hyphen. The IDNA profile already enforces this;
// the regexp is a second, independent check on what is actually stored.
var ldhLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// trailingDots are the full stops UTS #46 maps to ".", so a pasted "example.com" + U+3002 loses its
// trailing dot like "example.com." does.
var trailingDots = []string{".", "\u3002", "\uff0e", "\uff61"}

// NormalizeEntry normalizes one host entry for storage: an exact host or "*.base". It
// returns the canonical form, or an *EntryError naming why the entry is refused. It does
// not apply the multi-publisher rule, which needs the per-entry override (see Validate).
func NormalizeEntry(raw string) (string, error) {
	s, err := preflight(raw)
	if err != nil {
		return "", err
	}
	wildcard := false
	if s == "*" {
		return "", entryErr(CodeBareWildcard, "a bare \"*\" matches every host and is not allowed; use \"*.example.com\"")
	}
	if strings.HasPrefix(s, "*.") {
		wildcard = true
		s = s[2:]
	}
	if strings.Contains(s, "*") {
		return "", entryErr(CodeWildcardPosition, "\"*\" is allowed only as the whole leftmost label, as in \"*.example.com\"")
	}
	host, err := normalizeName(s, wildcard)
	if err != nil {
		return "", err
	}
	if !wildcard {
		if suffix, icann := publicsuffix.PublicSuffix(host); icann && suffix == host {
			return "", entryErr(CodePublicSuffixHost, "%q is a top-level or registry domain, not a site", host)
		}
		return host, nil
	}
	if suffix, _ := publicsuffix.PublicSuffix(host); suffix == host {
		return "", entryErr(CodePublicSuffixWildcard,
			"\"*.%s\" is refused: %s is a public suffix, so the wildcard would cover sites run by unrelated owners", host, host)
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(host); err != nil {
		return "", entryErr(CodePublicSuffixWildcard,
			"\"*.%s\" is refused: %s is not a registrable domain", host, host)
	}
	if parent, ok := underSharedParent(host); ok {
		return "", entryErr(CodeSharedParentWildcard,
			"\"*.%s\" is refused: the subdomains of %s belong to many different customers; list the exact hosts instead", host, parent)
	}
	// A public suffix BELOW base: "*.kawasaki.jp" covers the ICANN wildcard rule
	// "*.kawasaki.jp", "*.run.app" the private rule "a.run.app". PublicSuffix(base) is
	// not base in either case, so the check above cannot see it.
	if _, ok := pslRuleAncestors[host]; ok {
		return "", entryErr(CodePublicSuffixWildcard,
			"\"*.%s\" is refused: public suffixes lie under %s, so the wildcard would cover sites run by unrelated owners; list the exact hosts instead", host, host)
	}
	if len(host)+2 > MaxEntryLen {
		return "", entryErr(CodeTooLong, "an entry must be at most %d characters", MaxEntryLen)
	}
	return "*." + host, nil
}

// NormalizeHost normalizes a host name the way NormalizeEntry normalizes an exact entry,
// for match time. A wildcard is not a host, so "*" is refused here.
func NormalizeHost(raw string) (string, error) {
	s, err := preflight(raw)
	if err != nil {
		return "", err
	}
	if strings.Contains(s, "*") {
		return "", entryErr(CodeWildcardPosition, "a host name cannot contain \"*\"")
	}
	return normalizeName(s, false)
}

// Match reports whether host is covered by entries. Both sides are normalized first, so a
// host differing only in case, Unicode form or a trailing dot matches the same entries.
// An exact entry matches only that host; "*.base" matches every proper subdomain of base
// and not base itself. An entry that no longer normalizes is skipped (fail closed), and a
// host that does not normalize matches nothing.
//
// Invariant for callers: Match answers for NormalizeHost(host), not for the raw string.
// Normalization folds case and Unicode forms and drops ignorable code points (a soft
// hyphen in "docs\u00ad.example.com" disappears), so the raw string and the name Match
// approved can differ. A fetcher must therefore take NormalizeHost's output and use that
// exact name for the DNS lookup, the connection, TLS SNI and certificate verification,
// never the raw host it was given.
//
// Match knows nothing about overrides: for a stored profile, pass
// EffectiveEntries(hosts, overrides), never the stored hosts, so a multi-publisher entry
// without an override matches nothing.
func Match(host string, entries []string) bool {
	h, err := NormalizeHost(host)
	if err != nil {
		return false
	}
	for _, raw := range entries {
		e, err := NormalizeEntry(raw)
		if err != nil {
			continue
		}
		if base, ok := strings.CutPrefix(e, "*."); ok {
			if strings.HasSuffix(h, "."+base) {
				return true
			}
			continue
		}
		if h == e {
			return true
		}
	}
	return false
}

// preflight trims surrounding spaces, bounds the length, and gives the common URL-shaped
// mistakes their own message before the IDNA mapping turns them into a generic refusal.
func preflight(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", entryErr(CodeEmpty, "an entry must not be empty")
	}
	// A loose byte bound on the raw input (a Unicode name is longer in UTF-8 than in
	// characters); the real 253-character limit applies to the normalized form.
	if len(s) > 4*MaxEntryLen {
		return "", entryErr(CodeTooLong, "an entry must be at most %d characters", MaxEntryLen)
	}
	if !utf8.ValidString(s) {
		return "", entryErr(CodeInvalidHost, "an entry must be valid UTF-8")
	}
	switch {
	case strings.Contains(s, "://"):
		return "", entryErr(CodeScheme, "enter a host name, not a URL: remove the scheme (\"https://\")")
	case strings.Contains(s, "@"):
		return "", entryErr(CodeUserinfo, "a host name cannot contain \"@\" (user information)")
	case strings.ContainsAny(s, "/?#\\"):
		return "", entryErr(CodePath, "enter a host name only: remove the path, query or fragment")
	case strings.ContainsFunc(s, isSpaceOrControl):
		return "", entryErr(CodeWhitespace, "a host name cannot contain spaces or control characters")
	}
	// An IP literal before the port check, so "::1" and "[::1]" read as an address.
	if isIPLiteral(s) {
		return "", entryErr(CodeIPAddress, "IP addresses are not allowed; list host names")
	}
	if strings.Contains(s, ":") {
		return "", entryErr(CodePort, "a host entry cannot carry a port; the fetcher uses HTTPS on 443")
	}
	for _, d := range trailingDots {
		if strings.HasSuffix(s, d) {
			s = strings.TrimSuffix(s, d)
			break
		}
	}
	return s, nil
}

// normalizeName maps a name (a host, or a wildcard's base) to its lowercase A-label form
// and checks its shape: no empty label, LDH labels, at least two labels, and a top-level
// label that is not numeric (a dotted all-numeric name reads as an IPv4 address to URL
// parsers). A wildcard base may be a single label: the public suffix check that follows
// refuses it with the more useful reason ("*.com" is a public suffix wildcard).
func normalizeName(s string, wildcardBase bool) (string, error) {
	if s == "" || strings.HasPrefix(s, ".") || strings.Contains(s, "..") || strings.HasSuffix(s, ".") {
		return "", entryErr(CodeEmptyLabel, "a host name cannot have an empty label (a leading dot, \"..\" or two trailing dots)")
	}
	if len(s) > MaxEntryLen && isASCII(s) {
		return "", entryErr(CodeTooLong, "a host name must be at most %d characters", MaxEntryLen)
	}
	a, err := idnaProfile.ToASCII(s)
	if err != nil {
		return "", entryErr(CodeInvalidHost, "%q is not a valid host name", s)
	}
	// The mapping can fold a full-width digit form into an address, so check again.
	if isIPLiteral(a) {
		return "", entryErr(CodeIPAddress, "IP addresses are not allowed; list host names")
	}
	if len(a) > MaxEntryLen {
		return "", entryErr(CodeTooLong, "a host name must be at most %d characters", MaxEntryLen)
	}
	labels := strings.Split(a, ".")
	for _, l := range labels {
		if l == "" {
			return "", entryErr(CodeEmptyLabel, "a host name cannot have an empty label")
		}
		if !ldhLabel.MatchString(l) {
			return "", entryErr(CodeInvalidHost, "%q is not a valid host name", s)
		}
	}
	if len(labels) < 2 && !wildcardBase {
		return "", entryErr(CodeSingleLabel, "%q is a single-label name; list a fully qualified host", a)
	}
	if tld := labels[len(labels)-1]; isNumericLabel(tld) {
		return "", entryErr(CodeNumericTLD, "%q ends in a numeric label, which URL parsers read as an IP address", a)
	}
	return a, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func isSpaceOrControl(r rune) bool {
	return r == ' ' || r == '\u00a0' || r == '\u3000' || r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
}

// isIPLiteral reports an IPv4 or IPv6 literal, bracketed or not, with or without a port.
func isIPLiteral(s string) bool {
	if _, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil && ap.IsValid() {
		return true
	}
	return false
}

// isNumericLabel reports an all-digit label or a hex-number label ("0x7f"), both of which
// the WHATWG URL parser treats as part of an IPv4 address.
func isNumericLabel(l string) bool {
	if l == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(l, "0x"); ok {
		for _, r := range rest {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
		return true
	}
	for _, r := range l {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// nameRe is the profile-name shape: a lowercase slug, 1-64 characters, starting with a
// letter or digit. A slug keeps the name usable as a path segment and a CLI argument.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidateName checks a profile name: 1-64 characters of lowercase letters, digits and
// hyphens, starting with a letter or digit, and terminal-safe.
func ValidateName(name string) error {
	if err := termsafe.Validate("name", name); err != nil {
		return &EntryError{Code: CodeUnsafeText, Message: err.Error()}
	}
	if !nameRe.MatchString(name) {
		return entryErr(CodeInvalidName, "name must be 1-%d characters of lowercase letters, digits and hyphens, starting with a letter or digit", MaxNameLen)
	}
	return nil
}

// ValidateDescription checks a profile description: optional, at most 500 characters,
// and terminal-safe (no control or invisible formatting characters, no edge whitespace).
func ValidateDescription(desc string) error {
	if err := termsafe.Validate("description", desc); err != nil {
		return &EntryError{Code: CodeUnsafeText, Message: err.Error()}
	}
	if utf8.RuneCountInString(desc) > MaxDescriptionLen {
		return entryErr(CodeDescriptionTooLong, "description must be at most %d characters", MaxDescriptionLen)
	}
	return nil
}
