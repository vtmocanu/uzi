package egressprofile

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// The multi-publisher rule (PRD #1906 Decision 4). The fetcher controls access per host,
// so a host where many unrelated publishers serve content under the SAME host name
// (distinguished only by the URL path) cannot be made safe by a site list: allowing
// github.com allows every repository on it, not just the vendor's. Such hosts are listed
// here and an entry that reaches one is accepted only with an explicit per-entry
// override.
//
// The rule is about publishers sharing a host NAME, not an address. A host where each
// publisher gets its own subdomain ("vendor.readthedocs.io", "bucket.s3.amazonaws.com")
// is one publisher per host, so those subdomains are NOT flagged: the Public Suffix List
// already refuses a wildcard across them, and an exact subdomain is a single site. The
// platform hosts that serve many publishers by path ("readthedocs.io", "s3.amazonaws.com")
// are flagged.
//
// Three shapes:
//   - multiPublisherHosts: flagged as that exact host only. A sibling subdomain that is
//     not listed is not flagged, whatever it serves: "docs.github.com" (GitHub's own
//     documentation) is clean, but so would be a new GitHub host that serves every
//     account's content until it is added here. Listing a host is a claim about that
//     host, not about the rest of its domain.
//   - multiPublisherDomains: flagged as the domain AND every subdomain, because the
//     subdomains are shared too ("old.reddit.com", "raw.githubusercontent.com").
//   - an exact host that is itself a Public Suffix List entry in the PRIVATE section
//     ("github.io", "s3.amazonaws.com"): the platform hands the names under it to its
//     customers, and its apex is the platform's own endpoint, which may serve those
//     customers by path. Which apexes do cannot be read off the list, so every one is
//     flagged and needs the override. (An ICANN-section suffix is not a site at all and is
//     refused outright, and a wildcard over any suffix is refused.)
//
// A wildcard is flagged when it could reach a flagged host: "*.github.com" covers
// "gist.github.com", and "*.medium.com" is under a flagged domain.
//
// The list is deliberately short and incomplete: a vendor's own community forum cannot be
// listed in advance. It catches the common, well-known cases so an admin sees the warning
// where it matters most. docs/egress-profiles.md carries the same list for operators.
var multiPublisherHosts = map[string]string{
	// Code hosting: every account's repositories, gists and raw files share these hosts.
	"github.com":          "code hosting",
	"api.github.com":      "code hosting",
	"gist.github.com":     "code hosting",
	"codeload.github.com": "code hosting",
	"raw.github.com":      "code hosting",
	"gitlab.com":          "code hosting",
	"codeberg.org":        "code hosting",
	"gitee.com":           "code hosting",
	// Object storage addressed by path: every bucket shares the endpoint.
	"s3.amazonaws.com":          "object storage",
	"storage.googleapis.com":    "object storage",
	"storage.cloud.google.com":  "object storage",
	"dl.dropboxusercontent.com": "file sharing",
	// Shared documents and files.
	"docs.google.com":  "shared documents",
	"drive.google.com": "file sharing",
	"sites.google.com": "site hosting",
	// Documentation and package hosting served by path.
	"readthedocs.io":  "documentation hosting",
	"readthedocs.org": "documentation hosting",
	"gitbook.io":      "documentation hosting",
	"docs.rs":         "documentation hosting",
	"pkg.go.dev":      "documentation hosting",
	"pypi.org":        "package hosting",
	"test.pypi.org":   "package hosting",
	"www.npmjs.com":   "package hosting",
	"hub.docker.com":  "image hosting",
	"ghcr.io":         "image hosting",
	// Package registries and CDNs that serve every package's files by path.
	"unpkg.com":              "package CDN",
	"cdnjs.cloudflare.com":   "package CDN",
	"registry.npmjs.org":     "package hosting",
	"files.pythonhosted.org": "package hosting",
	"proxy.golang.org":       "package hosting",
	"repo.maven.apache.org":  "package hosting",
}

var multiPublisherDomains = map[string]string{
	// Every subdomain serves content from every account or repository
	// (raw., gist., objects., objects-origin., media.githubusercontent.com; cdn., fastly.
	// jsdelivr.net; cdn-lfs.huggingface.co).
	"githubusercontent.com": "code hosting",
	"jsdelivr.net":          "package CDN",
	"huggingface.co":        "model and dataset hosting",
	// Forums and user-generated publishing: the subdomains are shared too.
	"stackoverflow.com": "forum",
	"stackexchange.com": "forum",
	"reddit.com":        "forum",
	"quora.com":         "forum",
	"medium.com":        "blog hosting",
	"substack.com":      "blog hosting",
	"wordpress.com":     "blog hosting",
	"blogspot.com":      "blog hosting",
	"sourceforge.net":   "project hosting",
	"dropbox.com":       "file sharing",
	"npmjs.com":         "package hosting",
	// Package registries whose every subdomain serves every package (crates.io and
	// static.crates.io; rubygems.org and index.rubygems.org; repo1.maven.org and
	// central.maven.org), and code hosting whose API host is shared too
	// (api.bitbucket.org).
	"crates.io":     "package hosting",
	"rubygems.org":  "package hosting",
	"maven.org":     "package hosting",
	"bitbucket.org": "code hosting",
	// Web archives: every archived site shares web.archive.org, and archive.org serves
	// every uploader's items by path.
	"archive.org": "web archive",
}

// sharedParents are parents whose subdomains belong to many customers. A wildcard at or
// under one is refused outright, with no override: list the exact hosts instead. Most of
// them also have Public Suffix List rules below them (amazonaws.com, windows.net,
// fastly.net), which the generated pslRuleAncestors set refuses on its own; azure.com,
// googleusercontent.com and sharepoint.com have none in the pinned list, so this list is
// what refuses them. It is checked first so these keep their more specific code.
var sharedParents = []string{
	"amazonaws.com",
	"azure.com",
	"windows.net",
	"googleusercontent.com",
	"fastly.net",
	"sharepoint.com",
}

// underSharedParent reports the shared parent that host equals or falls under.
func underSharedParent(host string) (string, bool) {
	for _, p := range sharedParents {
		if host == p || strings.HasSuffix(host, "."+p) {
			return p, true
		}
	}
	return "", false
}

// isS3PathStyleRegional reports a regional path-style S3 endpoint such as
// "s3.us-east-1.amazonaws.com" or "s3-us-west-2.amazonaws.com" (and the dualstack form,
// and the China regions under amazonaws.com.cn), which serve every bucket in the region
// by path like s3.amazonaws.com does.
func isS3PathStyleRegional(host string) bool {
	rest, ok := strings.CutSuffix(host, ".amazonaws.com")
	if !ok {
		rest, ok = strings.CutSuffix(host, ".amazonaws.com.cn")
	}
	if !ok || strings.Contains(rest, "..") {
		return false
	}
	labels := strings.Split(rest, ".")
	switch {
	case len(labels) == 1:
		return strings.HasPrefix(labels[0], "s3-") && len(labels[0]) > 3
	case len(labels) == 2:
		return labels[0] == "s3"
	case len(labels) == 3:
		return labels[0] == "s3" && labels[1] == "dualstack"
	}
	return false
}

// MultiPublisher reports whether a NORMALIZED entry reaches a known multi-publisher host,
// and a short human reason ("github.com is code hosting ...") when it does.
func MultiPublisher(entry string) (string, bool) {
	if base, ok := strings.CutPrefix(entry, "*."); ok {
		// Under a flagged domain, or covering a flagged host or domain.
		if d, kind, ok := underFlaggedDomain(base); ok {
			return fmt.Sprintf("%s is under %s (%s, many publishers)", entry, d, kind), true
		}
		for _, h := range sortedKeys(multiPublisherHosts) {
			if strings.HasSuffix(h, "."+base) {
				return fmt.Sprintf("%s covers %s (%s, many publishers)", entry, h, multiPublisherHosts[h]), true
			}
		}
		for _, d := range sortedKeys(multiPublisherDomains) {
			if strings.HasSuffix(d, "."+base) {
				return fmt.Sprintf("%s covers %s (%s, many publishers)", entry, d, multiPublisherDomains[d]), true
			}
		}
		return "", false
	}
	if kind, ok := multiPublisherHosts[entry]; ok {
		return fmt.Sprintf("%s is %s: many publishers share this host", entry, kind), true
	}
	if d, kind, ok := underFlaggedDomain(entry); ok {
		if d == entry {
			return fmt.Sprintf("%s is %s: many publishers share this host", entry, kind), true
		}
		return fmt.Sprintf("%s is under %s (%s, many publishers)", entry, d, kind), true
	}
	if isS3PathStyleRegional(entry) {
		return fmt.Sprintf("%s is object storage: every bucket in the region shares this host", entry), true
	}
	if suffix, icann := publicsuffix.PublicSuffix(entry); suffix == entry && !icann {
		return fmt.Sprintf("%s is a public suffix: the platform gives the names under it to its customers, and its own host may serve them by path", entry), true
	}
	return "", false
}

func underFlaggedDomain(host string) (domain, kind string, ok bool) {
	for _, d := range sortedKeys(multiPublisherDomains) {
		if host == d || strings.HasSuffix(host, "."+d) {
			return d, multiPublisherDomains[d], true
		}
	}
	return "", "", false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Problem is one reason a profile write is refused. Field names the input
// ("name", "description", "hosts[3]", "multi_publisher_override[0]"); Entry echoes the
// offending entry as the caller sent it, when there is one.
type Problem struct {
	Field   string `json:"field"`
	Entry   string `json:"entry,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Warning is a stored entry the admin should know about: a multi-publisher host admitted
// by an explicit override, or (on read) an entry EffectiveEntries drops because the
// current rules no longer accept it or now flag it as multi-publisher without an override.
type Warning struct {
	Entry   string `json:"entry"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Warning codes.
const (
	// WarningCodeMultiPublisherOverride marks a multi-publisher entry admitted by an override.
	WarningCodeMultiPublisherOverride = "multi_publisher_override"
	// WarningCodeMultiPublisherNeedsOverride marks a stored entry the CURRENT rules flag as
	// multi-publisher that carries no override (the list grew after the profile was
	// written); EffectiveEntries drops it. The same code refuses such an entry on a write.
	WarningCodeMultiPublisherNeedsOverride = CodeMultiPublisher
	// WarningCodeStaleEntry marks a stored entry the current rules refuse; EffectiveEntries
	// and Match skip it.
	WarningCodeStaleEntry = "stale_entry"
)

// Input is a profile write as the caller sent it.
type Input struct {
	Name                   string
	Description            string
	Hosts                  []string
	MultiPublisherOverride []string
}

// Validated is a profile write that passed: normalized, de-duplicated host entries in the
// caller's order, the overrides that apply (only flagged entries; an override naming an
// entry that is not multi-publisher is dropped), and a warning per overridden entry.
type Validated struct {
	Name                   string
	Description            string
	Hosts                  []string
	MultiPublisherOverride []string
	Warnings               []Warning
}

// Validate checks a whole profile write and returns every problem at once, so an admin
// fixes a list in one pass. The name is checked only when checkName is set (an update
// takes the name from the path and never changes it).
func Validate(in Input, checkName bool) (Validated, []Problem) {
	var probs []Problem
	out := Validated{Name: in.Name, Description: in.Description}
	if checkName {
		if err := ValidateName(in.Name); err != nil {
			probs = append(probs, fieldProblem("name", err))
		}
	}
	if err := ValidateDescription(in.Description); err != nil {
		probs = append(probs, fieldProblem("description", err))
	}
	switch {
	case len(in.Hosts) == 0:
		probs = append(probs, Problem{Field: "hosts", Code: CodeNoEntries, Message: "a profile needs at least one host entry"})
	case len(in.Hosts) > MaxEntries:
		probs = append(probs, Problem{Field: "hosts", Code: CodeTooManyEntries,
			Message: fmt.Sprintf("a profile may list at most %d host entries (got %d)", MaxEntries, len(in.Hosts))})
		return out, probs
	}
	// The overrides name host entries, so there can never legitimately be more of them than
	// entries; bound them before normalizing any, like hosts.
	if len(in.MultiPublisherOverride) > MaxEntries {
		probs = append(probs, Problem{Field: "multi_publisher_override", Code: CodeTooManyEntries,
			Message: fmt.Sprintf("a profile may list at most %d overrides (got %d)", MaxEntries, len(in.MultiPublisherOverride))})
		return out, probs
	}

	// Overrides are normalized like entries; one that does not normalize, or names an
	// entry not in hosts, is a problem (a stale override would otherwise sit silently).
	override := map[string]bool{}
	for i, raw := range in.MultiPublisherOverride {
		n, err := NormalizeEntry(raw)
		if err != nil {
			probs = append(probs, entryProblem(fmt.Sprintf("multi_publisher_override[%d]", i), raw, err))
			continue
		}
		override[n] = true
	}

	seen := map[string]bool{}
	for i, raw := range in.Hosts {
		field := fmt.Sprintf("hosts[%d]", i)
		n, err := NormalizeEntry(raw)
		if err != nil {
			probs = append(probs, entryProblem(field, raw, err))
			continue
		}
		if seen[n] {
			continue // a duplicate after normalization is folded, not refused
		}
		seen[n] = true
		if reason, flagged := MultiPublisher(n); flagged {
			if !override[n] {
				probs = append(probs, Problem{Field: field, Entry: echo(raw), Code: CodeMultiPublisher,
					Message: reason + "; allowing it admits every publisher on it. Add it to multi_publisher_override to accept that"})
				continue
			}
			out.MultiPublisherOverride = append(out.MultiPublisherOverride, n)
			out.Warnings = append(out.Warnings, Warning{Entry: n, Code: WarningCodeMultiPublisherOverride,
				Message: reason + "; admitted by an explicit override, so every publisher on it is reachable"})
		}
		out.Hosts = append(out.Hosts, n)
	}
	for i, raw := range in.MultiPublisherOverride {
		if n, err := NormalizeEntry(raw); err == nil && !seen[n] {
			probs = append(probs, Problem{Field: fmt.Sprintf("multi_publisher_override[%d]", i), Entry: echo(raw),
				Code: CodeOverrideNotInHosts, Message: fmt.Sprintf("override %q does not name one of the profile's host entries", n)})
		}
	}
	return out, probs
}

// WarningsFor recomputes the warnings for a stored profile, so a read shows the same
// override warnings the write did, plus a warning for every stored entry that
// EffectiveEntries drops: a stale-entry warning for an entry the current rules no longer
// accept (a newer Public Suffix List can turn an accepted wildcard into a refused one), and
// a needs-override warning for an entry the current multi-publisher list flags that has no
// override (the list grew after the profile was written). Without them an admin would see
// the entry listed while it silently allows nothing.
func WarningsFor(hosts, overrides []string) []Warning {
	out := []Warning{}
	override := normalizedSet(overrides)
	for _, h := range hosts {
		n, err := NormalizeEntry(h)
		if err != nil {
			out = append(out, Warning{Entry: h, Code: WarningCodeStaleEntry,
				Message: fmt.Sprintf("%s is no longer accepted (%v), so it matches nothing; edit the profile to fix or remove it", echo(h), err)})
			continue
		}
		reason, flagged := MultiPublisher(n)
		switch {
		case !flagged:
		case override[n]:
			out = append(out, Warning{Entry: h, Code: WarningCodeMultiPublisherOverride,
				Message: reason + "; admitted by an explicit override, so every publisher on it is reachable"})
		default:
			out = append(out, Warning{Entry: h, Code: WarningCodeMultiPublisherNeedsOverride,
				Message: reason + "; it has no multi_publisher_override, so it matches nothing. Add the override to accept every publisher on it, or remove it"})
		}
	}
	return out
}

// EffectiveEntries is the set of a stored profile's entries that may allow anything, in
// stored order and normalized: an entry is dropped when it no longer normalizes, or when
// the CURRENT multi-publisher rules flag it and overrides does not name it (fail closed:
// a host added to the built-in list after a profile was written stops matching until an
// admin adds the override). WarningsFor reports every dropped entry.
//
// This is the only way from a stored profile to a match set. The fetcher and the
// claim-time profile snapshot must take EffectiveEntries(hosts, overrides) and pass that
// to Match; Match on the raw stored hosts would still admit an unoverridden
// multi-publisher entry.
func EffectiveEntries(hosts, overrides []string) []string {
	override := normalizedSet(overrides)
	out := []string{}
	seen := map[string]bool{}
	for _, h := range hosts {
		n, err := NormalizeEntry(h)
		if err != nil || seen[n] {
			continue
		}
		if _, flagged := MultiPublisher(n); flagged && !override[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// normalizedSet is the normalized form of each entry that still normalizes.
func normalizedSet(entries []string) map[string]bool {
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if n, err := NormalizeEntry(e); err == nil {
			set[n] = true
		}
	}
	return set
}

func entryProblem(field, raw string, err error) Problem {
	p := fieldProblem(field, err)
	p.Entry = echo(raw)
	return p
}

func fieldProblem(field string, err error) Problem {
	p := Problem{Field: field, Code: CodeInvalidHost, Message: err.Error()}
	var ee *EntryError
	if errors.As(err, &ee) {
		p.Code = ee.Code
	}
	return p
}

// echo is the caller's raw entry as it is repeated back in a Problem: control and
// invisible formatting characters stripped (termsafe.CellText) and the length bounded, so
// a refused entry cannot carry a terminal escape or a bidi override into a UI that shows
// the error.
func echo(raw string) string {
	s := termsafe.CellText(raw)
	const maxEcho = MaxEntryLen + 8
	if utf8.RuneCountInString(s) > maxEcho {
		r := []rune(s)
		s = string(r[:maxEcho]) + "…"
	}
	return s
}
