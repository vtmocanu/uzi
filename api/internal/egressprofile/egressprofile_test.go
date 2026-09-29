package egressprofile

import (
	"errors"
	"strings"
	"testing"
)

// codeOf returns the EntryError code of err, or "" for nil / another error type.
func codeOf(err error) string {
	var ee *EntryError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ""
}

// TestNormalizeEntryAccepts pins the canonical form: lowercase, IDNA A-label, one
// trailing dot stripped, surrounding spaces trimmed, wildcard kept as "*.".
func TestNormalizeEntryAccepts(t *testing.T) {
	cases := map[string]string{
		"docs.example.com":        "docs.example.com",
		"Docs.Example.COM":        "docs.example.com",
		"docs.example.com.":       "docs.example.com",
		"  docs.example.com  ":    "docs.example.com",
		"*.example.com":           "*.example.com",
		"*.Example.com.":          "*.example.com",
		"bücher.de":               "xn--bcher-kva.de",
		"BÜCHER.de":               "xn--bcher-kva.de",
		"xn--bcher-kva.de":        "xn--bcher-kva.de",
		"*.münchen.de":            "*.xn--mnchen-3ya.de",
		"example。com":             "example.com", // ideographic full stop maps to "."
		"example.com。":            "example.com", // ... also as the trailing dot
		"a-b.example.co.uk":       "a-b.example.co.uk",
		"*.example.co.uk":         "*.example.co.uk",
		"vendor.github.io":        "vendor.github.io",        // an exact host under a private suffix is one site
		"bucket.s3.amazonaws.com": "bucket.s3.amazonaws.com", // one bucket is one publisher
		"s3.amazonaws.com":        "s3.amazonaws.com",        // a private-section suffix as an exact host is allowed (then flagged multi-publisher)
		"123.example.com":         "123.example.com",         // numeric labels are fine below the TLD
	}
	for in, want := range cases {
		got, err := NormalizeEntry(in)
		if err != nil {
			t.Errorf("NormalizeEntry(%q) = error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("NormalizeEntry(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeEntryRefuses pins every refusal class with its stable code.
func TestNormalizeEntryRefuses(t *testing.T) {
	cases := []struct {
		in   string
		code string
	}{
		{"", CodeEmpty},
		{"   ", CodeEmpty},
		{"https://docs.example.com", CodeScheme},
		{"http://docs.example.com/", CodeScheme},
		{"user@docs.example.com", CodeUserinfo},
		{"user:pw@docs.example.com", CodeUserinfo},
		{"docs.example.com/path", CodePath},
		{"docs.example.com?q=1", CodePath},
		{"docs.example.com#frag", CodePath},
		{"docs.example.com:443", CodePort},
		{"docs.example.com:8080", CodePort},
		{"docs example.com", CodeWhitespace},
		{"docs.example.com\n", CodeEmpty + "_trimmed"}, // TrimSpace removes the newline: accepted, see below
		{"docs\texample.com", CodeWhitespace},
		{"docs\x1b[31m.example.com", CodeWhitespace},
		{"127.0.0.1", CodeIPAddress},
		{"10.0.0.1", CodeIPAddress},
		{"::1", CodeIPAddress},
		{"[::1]", CodeIPAddress},
		{"[2001:db8::1]:443", CodeIPAddress},
		{"::ffff:127.0.0.1", CodeIPAddress},
		{"127.0.0.1:443", CodeIPAddress},
		{"１２７.0.0.1", CodeIPAddress}, // full-width digits fold to 127.0.0.1
		{"*", CodeBareWildcard},
		{"*.*.example.com", CodeWildcardPosition},
		{"docs.*.example.com", CodeWildcardPosition},
		{"*docs.example.com", CodeWildcardPosition},
		{"docs*.example.com", CodeWildcardPosition},
		{"example.*", CodeWildcardPosition},
		{"*.", CodeBareWildcard}, // the trailing dot is stripped first, leaving "*"
		{".example.com", CodeEmptyLabel},
		{"docs..example.com", CodeEmptyLabel},
		{"example.com..", CodeEmptyLabel},
		{"exa_mple.com", CodeInvalidHost},
		{"-bad.example.com", CodeInvalidHost},
		{"bad-.example.com", CodeInvalidHost},
		{"xn--zz.com", CodeInvalidHost},
		{"localhost", CodeSingleLabel},
		{"intranet.", CodeSingleLabel},
		{"1.2.3.999", CodeNumericTLD},
		{"foo.0x7f", CodeNumericTLD},
		{"com", CodeSingleLabel},
		{"co.uk", CodePublicSuffixHost},
		{strings.Repeat("a.", 127) + "com", CodeTooLong},
		{strings.Repeat("a", 64) + ".com", CodeInvalidHost}, // a label over 63 octets
	}
	for _, c := range cases {
		if c.code == CodeEmpty+"_trimmed" {
			if _, err := NormalizeEntry(c.in); err != nil {
				t.Errorf("NormalizeEntry(%q) = %v, want accepted (surrounding whitespace is trimmed)", c.in, err)
			}
			continue
		}
		got, err := NormalizeEntry(c.in)
		if err == nil {
			t.Errorf("NormalizeEntry(%q) = %q, want refusal %s", c.in, got, c.code)
			continue
		}
		if codeOf(err) != c.code {
			t.Errorf("NormalizeEntry(%q) code = %q (%v), want %q", c.in, codeOf(err), err, c.code)
		}
	}
}

// TestNormalizeEntryPublicSuffixWildcards pins the PSL rule across both sections: a
// wildcard is refused when its base is a public suffix, when any PSL rule lies BELOW its
// base (psl_ancestors_gen.go), and when the base is at or under a shared parent.
func TestNormalizeEntryPublicSuffixWildcards(t *testing.T) {
	cases := []struct {
		in   string
		code string
	}{
		{"*.com", CodePublicSuffixWildcard},              // ICANN
		{"*.co.uk", CodePublicSuffixWildcard},            // ICANN, two labels
		{"*.github.io", CodePublicSuffixWildcard},        // private section
		{"*.cloudfront.net", CodePublicSuffixWildcard},   // private section
		{"*.s3.amazonaws.com", CodePublicSuffixWildcard}, // private section
		{"*.readthedocs.io", CodePublicSuffixWildcard},   // private section
		{"*.blob.core.windows.net", CodePublicSuffixWildcard},
		{"*.amazonaws.com", CodeSharedParentWildcard},
		{"*.elb.amazonaws.com", CodeSharedParentWildcard},
		{"*.core.windows.net", CodeSharedParentWildcard},
		{"*.googleusercontent.com", CodeSharedParentWildcard},
		{"*.sharepoint.com", CodeSharedParentWildcard},
		{"*.azure.com", CodeSharedParentWildcard},
		// A rule BELOW the base: PublicSuffix(base) is not base, so only the generated
		// ancestor set refuses these. ICANN wildcard rules first ("*.kawasaki.jp",
		// "*.sch.uk", "*.nom.br" are rules, so every child is a public suffix) ...
		{"*.kawasaki.jp", CodePublicSuffixWildcard},
		{"*.sch.uk", CodePublicSuffixWildcard},
		{"*.nom.br", CodePublicSuffixWildcard},
		// ... then private-section rules below the base.
		{"*.stg.dev", CodePublicSuffixWildcard},
		{"*.lcl.dev", CodePublicSuffixWildcard},
		{"*.platformsh.site", CodePublicSuffixWildcard},
		{"*.run.app", CodePublicSuffixWildcard},                // rule a.run.app
		{"*.railway.app", CodePublicSuffixWildcard},            // rule up.railway.app
		{"*.digitaloceanspaces.com", CodePublicSuffixWildcard}, // rule nyc3.digitaloceanspaces.com, ...
		{"*.salesforce.com", CodePublicSuffixWildcard},         // rules several labels below
		{"*.jp", CodePublicSuffixWildcard},
	}
	for _, c := range cases {
		got, err := NormalizeEntry(c.in)
		if err == nil {
			t.Errorf("NormalizeEntry(%q) = %q, want refusal %s", c.in, got, c.code)
			continue
		}
		if codeOf(err) != c.code {
			t.Errorf("NormalizeEntry(%q) code = %q (%v), want %q", c.in, codeOf(err), err, c.code)
		}
	}
	// A wildcard over a registrable domain is fine, including one under a private suffix.
	for _, ok := range []string{
		"*.example.com", "*.vendor.github.io", "*.docs.example.co.uk", "*.vendor.co.uk",
		"*.docs.vendor.com", "*.vendor.kawasaki.jp.example.com",
		"*.city.kawasaki.jp", // "!city.kawasaki.jp" is an exception: a registrable domain with no rule below it
	} {
		if _, err := NormalizeEntry(ok); err != nil {
			t.Errorf("NormalizeEntry(%q) = %v, want accepted", ok, err)
		}
	}
}

// TestNormalizeHostRefusesWildcard: a match-time host is never a pattern.
func TestNormalizeHostRefusesWildcard(t *testing.T) {
	if _, err := NormalizeHost("*.example.com"); err == nil {
		t.Fatal("NormalizeHost(*.example.com) accepted a wildcard")
	}
	if got, err := NormalizeHost("Docs.Example.com."); err != nil || got != "docs.example.com" {
		t.Fatalf("NormalizeHost = %q, %v; want docs.example.com", got, err)
	}
}

// TestMatch pins the match-time semantics: both sides normalized, exact entries match only
// themselves, and "*.base" matches proper subdomains of base but NOT base itself.
func TestMatch(t *testing.T) {
	entries := []string{"docs.example.com", "*.vendor.test.com", "xn--bcher-kva.de"}
	yes := []string{
		"docs.example.com", "DOCS.example.com", "docs.example.com.",
		"a.vendor.test.com", "a.b.vendor.test.com", "A.Vendor.Test.Com.",
		"bücher.de", "BÜCHER.DE",
	}
	no := []string{
		"example.com", "www.docs.example.com", "docs.example.com.evil.com",
		"vendor.test.com",     // the wildcard does not cover its base
		"evilvendor.test.com", // suffix match must be on a label boundary
		"a.vendor.test.com:443", "https://docs.example.com", "", "*.vendor.test.com",
		"docs.example.com/x", "127.0.0.1",
	}
	for _, h := range yes {
		if !Match(h, entries) {
			t.Errorf("Match(%q) = false, want true", h)
		}
	}
	for _, h := range no {
		if Match(h, entries) {
			t.Errorf("Match(%q) = true, want false", h)
		}
	}
	// Entries are normalized at match time too, so a stored entry in a non-canonical form
	// still matches, and a stored entry that no longer normalizes is skipped (fail closed).
	if !Match("docs.example.com", []string{"DOCS.EXAMPLE.COM."}) {
		t.Error("a non-canonical stored entry did not match")
	}
	if Match("anything.com", []string{"*.com", "*"}) {
		t.Error("an invalid stored entry matched")
	}
	// A wildcard whose base has a public suffix below it is not an entry, so a stored one
	// (written under an older list) matches nothing: every child of kawasaki.jp is a
	// public suffix, and foo.kawasaki.jp belongs to someone unrelated to evil.foo's owner.
	for _, h := range []string{"evil.foo.kawasaki.jp", "x.a.run.app", "b.nyc3.digitaloceanspaces.com"} {
		if Match(h, []string{"*.kawasaki.jp", "*.run.app", "*.digitaloceanspaces.com"}) {
			t.Errorf("Match(%q) = true through a wildcard that covers a public suffix", h)
		}
	}
}

// TestMultiPublisher pins the built-in list's two shapes and the wildcard coverage rule.
func TestMultiPublisher(t *testing.T) {
	flagged := []string{
		"github.com", "gist.github.com", "raw.githubusercontent.com", "gitlab.com", "bitbucket.org",
		"s3.amazonaws.com", "s3.us-east-1.amazonaws.com", "s3-us-west-2.amazonaws.com",
		"s3.dualstack.eu-west-1.amazonaws.com",
		"storage.googleapis.com", "docs.google.com", "drive.google.com",
		"readthedocs.io", "gitbook.io", "pypi.org",
		"stackoverflow.com", "meta.stackoverflow.com", "reddit.com", "old.reddit.com",
		"medium.com", "someone.medium.com", "sourceforge.net", "dropbox.com", "www.dropbox.com",
		"*.github.com",            // covers gist.github.com
		"*.google.com",            // covers docs.google.com
		"*.reddit.com",            // under a flagged domain
		"*.blog.medium.com",       // under a flagged domain
		"*.githubusercontent.com", // (a PSL suffix, refused earlier; MultiPublisher alone still flags it)
		// Hosts that serve arbitrary publishers' content by path (review of PRD #1906 M1).
		"api.github.com", "media.githubusercontent.com", "objects-origin.githubusercontent.com",
		"objects.githubusercontent.com", "gist.githubusercontent.com",
		"cdn.jsdelivr.net", "fastly.jsdelivr.net", "unpkg.com", "registry.npmjs.org",
		"files.pythonhosted.org", "proxy.golang.org", "static.crates.io",
		"huggingface.co", "cdn-lfs.huggingface.co",
		"*.huggingface.co", "*.jsdelivr.net", "*.npmjs.org", "*.pythonhosted.org", "*.golang.org", "*.crates.io",
		// China-region path-style S3.
		"s3.cn-north-1.amazonaws.com.cn", "s3.dualstack.cn-northwest-1.amazonaws.com.cn",
		// An exact host that is a private-section public suffix: the platform's own apex.
		"github.io", "gitlab.io", "githubusercontent.com", "cloudfront.net", "blogspot.com",
		"vendor.platformsh.site", // "*.platformsh.site" is a rule, so this is a suffix itself
	}
	for _, e := range flagged {
		if reason, ok := MultiPublisher(e); !ok || reason == "" {
			t.Errorf("MultiPublisher(%q) = (%q, %v), want flagged with a reason", e, reason, ok)
		}
	}
	clean := []string{
		"docs.github.com",         // GitHub's own docs: an exact-only entry does not flag siblings
		"vendor.readthedocs.io",   // one project per subdomain
		"bucket.s3.amazonaws.com", // virtual-hosted bucket: one publisher
		"docs.example.com", "*.example.com",
		"s3.amazonaws.com.evil.com", "xs3.amazonaws.com", "notreddit.com",
		"*.docs.github.com",
		"vendor.github.io", "vendor.gitlab.io", // one site under a private suffix
		"xs3.cn-north-1.amazonaws.com.cn",
		"example.org", "docs.golang.org.example.com",
	}
	for _, e := range clean {
		if reason, ok := MultiPublisher(e); ok {
			t.Errorf("MultiPublisher(%q) flagged (%q), want clean", e, reason)
		}
	}
}

// TestValidateNameAndDescription pins the name slug, the length caps, and that both fields
// go through termsafe (control and invisible formatting characters refused).
func TestValidateNameAndDescription(t *testing.T) {
	for _, ok := range []string{"a", "vendor-docs", "v2", strings.Repeat("a", 64)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-lead", "Upper", "has space", "under_score", "dot.name", strings.Repeat("a", 65),
		"bad\x1b[2Jname", "zero\u200bwidth", "bidi\u202ename", " lead", "é",
	} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want a refusal", bad)
		}
	}
	for _, ok := range []string{"", "Official docs for Vendor X", strings.Repeat("é", 500)} {
		if err := ValidateDescription(ok); err != nil {
			t.Errorf("ValidateDescription(len %d) = %v, want nil", len(ok), err)
		}
	}
	if err := ValidateDescription(strings.Repeat("é", 501)); codeOf(err) != CodeDescriptionTooLong {
		t.Errorf("ValidateDescription(501 runes) code = %q, want %q", codeOf(err), CodeDescriptionTooLong)
	}
	for _, bad := range []string{"line\nbreak", "esc\x1b[31m", "bidi\u202eoverride", "trailing "} {
		if err := ValidateDescription(bad); codeOf(err) != CodeUnsafeText {
			t.Errorf("ValidateDescription(%q) code = %q, want %q", bad, codeOf(err), CodeUnsafeText)
		}
	}
}

// TestValidateProfile pins the whole-write rules: every problem reported at once, the
// multi-publisher override, dedupe after normalization, and the entry-count cap.
func TestValidateProfile(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		v, probs := Validate(Input{
			Name:  "vendor-docs",
			Hosts: []string{"Docs.Vendor.com", "docs.vendor.com.", "*.cdn.vendor.com"},
		}, true)
		if len(probs) != 0 {
			t.Fatalf("problems = %+v, want none", probs)
		}
		if strings.Join(v.Hosts, ",") != "docs.vendor.com,*.cdn.vendor.com" {
			t.Fatalf("hosts = %v, want normalized and de-duplicated in order", v.Hosts)
		}
		if len(v.Warnings) != 0 || len(v.MultiPublisherOverride) != 0 {
			t.Fatalf("warnings/overrides = %v/%v, want none", v.Warnings, v.MultiPublisherOverride)
		}
	})

	t.Run("multi-publisher needs the override", func(t *testing.T) {
		_, probs := Validate(Input{Name: "x", Hosts: []string{"github.com"}}, true)
		if len(probs) != 1 || probs[0].Code != CodeMultiPublisher || probs[0].Field != "hosts[0]" {
			t.Fatalf("problems = %+v, want one multi_publisher_needs_override on hosts[0]", probs)
		}
		v, probs := Validate(Input{Name: "x", Hosts: []string{"docs.vendor.com", "GitHub.com"},
			MultiPublisherOverride: []string{"github.com."}}, true)
		if len(probs) != 0 {
			t.Fatalf("problems with override = %+v, want none", probs)
		}
		if strings.Join(v.MultiPublisherOverride, ",") != "github.com" {
			t.Fatalf("stored overrides = %v, want [github.com]", v.MultiPublisherOverride)
		}
		if len(v.Warnings) != 1 || v.Warnings[0].Entry != "github.com" || v.Warnings[0].Code != WarningCodeMultiPublisherOverride {
			t.Fatalf("warnings = %+v, want one override warning for github.com", v.Warnings)
		}
		if got := WarningsFor(v.Hosts, v.MultiPublisherOverride); len(got) != 1 || got[0] != v.Warnings[0] {
			t.Fatalf("WarningsFor = %+v, want the write's warning %+v", got, v.Warnings)
		}
	})

	t.Run("an override on a clean entry is dropped, on a missing entry refused", func(t *testing.T) {
		v, probs := Validate(Input{Name: "x", Hosts: []string{"docs.vendor.com"},
			MultiPublisherOverride: []string{"docs.vendor.com"}}, true)
		if len(probs) != 0 || len(v.MultiPublisherOverride) != 0 || len(v.Warnings) != 0 {
			t.Fatalf("got probs %+v overrides %v warnings %v, want the needless override dropped", probs, v.MultiPublisherOverride, v.Warnings)
		}
		_, probs = Validate(Input{Name: "x", Hosts: []string{"docs.vendor.com"},
			MultiPublisherOverride: []string{"github.com", "https://x"}}, true)
		codes := map[string]string{}
		for _, p := range probs {
			codes[p.Field] = p.Code
		}
		if codes["multi_publisher_override[0]"] != CodeOverrideNotInHosts || codes["multi_publisher_override[1]"] != CodeScheme {
			t.Fatalf("problems = %+v, want override_not_in_hosts and scheme", probs)
		}
	})

	t.Run("every problem at once", func(t *testing.T) {
		_, probs := Validate(Input{
			Name:        "Bad Name",
			Description: "evil\x1b[31m",
			Hosts:       []string{"ok.example.com", "*.com", "10.0.0.1", "github.com"},
		}, true)
		want := map[string]string{
			"name":        CodeInvalidName,
			"description": CodeUnsafeText,
			"hosts[1]":    CodePublicSuffixWildcard,
			"hosts[2]":    CodeIPAddress,
			"hosts[3]":    CodeMultiPublisher,
		}
		got := map[string]string{}
		for _, p := range probs {
			got[p.Field] = p.Code
		}
		for f, c := range want {
			if got[f] != c {
				t.Errorf("problem %s = %q, want %q (all: %+v)", f, got[f], c, probs)
			}
		}
		if len(probs) != len(want) {
			t.Errorf("got %d problems, want %d: %+v", len(probs), len(want), probs)
		}
	})

	t.Run("name not checked on update", func(t *testing.T) {
		if _, probs := Validate(Input{Name: "", Hosts: []string{"a.example.com"}}, false); len(probs) != 0 {
			t.Fatalf("problems = %+v, want none when checkName=false", probs)
		}
	})

	t.Run("entry count", func(t *testing.T) {
		if _, probs := Validate(Input{Name: "x"}, true); len(probs) != 1 || probs[0].Code != CodeNoEntries {
			t.Fatalf("no hosts: problems = %+v, want no_entries", probs)
		}
		hosts := make([]string, MaxEntries)
		for i := range hosts {
			hosts[i] = "h" + strings.Repeat("x", i%5) + string(rune('a'+i%26)) + ".example.com"
		}
		if _, probs := Validate(Input{Name: "x", Hosts: hosts}, true); len(probs) != 0 {
			t.Fatalf("%d hosts: problems = %+v, want none", MaxEntries, probs)
		}
		hosts = append(hosts, "one-more.example.com")
		if _, probs := Validate(Input{Name: "x", Hosts: hosts}, true); len(probs) != 1 || probs[0].Code != CodeTooManyEntries {
			t.Fatalf("%d hosts: problems = %+v, want too_many_entries", len(hosts), probs)
		}
	})

	t.Run("override count is capped before any is normalized", func(t *testing.T) {
		over := make([]string, MaxEntries+1)
		for i := range over {
			over[i] = "github.com"
		}
		_, probs := Validate(Input{Name: "x", Hosts: []string{"github.com"}, MultiPublisherOverride: over}, true)
		if len(probs) != 1 || probs[0].Code != CodeTooManyEntries || probs[0].Field != "multi_publisher_override" {
			t.Fatalf("%d overrides: problems = %+v, want one too_many_entries on multi_publisher_override", len(over), probs)
		}
		if _, probs := Validate(Input{Name: "x", Hosts: []string{"github.com"}, MultiPublisherOverride: over[:MaxEntries]}, true); len(probs) != 0 {
			t.Fatalf("%d overrides: problems = %+v, want none", MaxEntries, probs)
		}
	})

	t.Run("a refused entry is echoed terminal-safe", func(t *testing.T) {
		_, probs := Validate(Input{Name: "x", Hosts: []string{"bad\u202e\x1b[2Jhost/x"}}, true)
		if len(probs) != 1 {
			t.Fatalf("problems = %+v, want one", probs)
		}
		if strings.ContainsAny(probs[0].Entry, "\x1b\u202e") {
			t.Fatalf("echoed entry %q still carries a control or bidi character", probs[0].Entry)
		}
	})
}

// TestWarningsForStaleEntry: a stored entry the current rules refuse (a list written
// before a Public Suffix List update, or before a rule tightened) is skipped by Match, so
// a read must say so instead of listing it as if it allowed something.
func TestWarningsForStaleEntry(t *testing.T) {
	hosts := []string{"docs.vendor.com", "*.kawasaki.jp", "github.com"}
	got := WarningsFor(hosts, []string{"github.com"})
	byEntry := map[string]string{}
	for _, w := range got {
		byEntry[w.Entry] = w.Code
		if w.Message == "" {
			t.Errorf("warning for %q has no message", w.Entry)
		}
	}
	want := map[string]string{"*.kawasaki.jp": WarningCodeStaleEntry, "github.com": WarningCodeMultiPublisherOverride}
	if len(got) != len(want) {
		t.Fatalf("warnings = %+v, want %v", got, want)
	}
	for e, c := range want {
		if byEntry[e] != c {
			t.Errorf("warning for %q = %q, want %q (all: %+v)", e, byEntry[e], c, got)
		}
	}
	if Match("evil.foo.kawasaki.jp", hosts) {
		t.Error("the stale entry matched")
	}
	if w := WarningsFor([]string{"docs.vendor.com"}, nil); len(w) != 0 {
		t.Errorf("clean profile warnings = %+v, want none", w)
	}
}
