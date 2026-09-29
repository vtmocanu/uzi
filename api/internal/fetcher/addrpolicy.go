package fetcher

import (
	"fmt"
	"net/netip"
	"strings"
)

// blockedPrefixes are the ranges no fetch may reach, whatever the site list says: every
// non-public IPv4 and IPv6 range, plus the IPv6 ranges that embed or tunnel to an IPv4
// address (NAT64, 6to4, Teredo, IPv4-compatible), which could otherwise carry a private
// IPv4 address past an IPv4-only check. Addresses are unmapped (::ffff:a.b.c.d ->
// a.b.c.d) before they are checked against this list.
var blockedPrefixes = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network", includes the unspecified address
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // CGNAT (RFC 6598)
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, includes the 169.254.169.254 metadata address
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation (TEST-NET-1)
	"192.88.99.0/24",  // 6to4 relay anycast
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation (TEST-NET-2)
	"203.0.113.0/24",  // documentation (TEST-NET-3)
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, includes the broadcast address
	// IPv6
	"::/96",          // unspecified, and IPv4-compatible (deprecated) addresses
	"::1/128",        // loopback
	"::ffff:0:0/96",  // IPv4-mapped (unmapped before the check; kept as a backstop)
	"64:ff9b::/96",   // NAT64 well-known prefix
	"64:ff9b:1::/48", // NAT64 local-use prefix
	"100::/64",       // discard-only
	"2001::/32",      // Teredo
	"2001:2::/48",    // benchmarking
	"2001:10::/28",   // ORCHID (deprecated)
	"2001:20::/28",   // ORCHIDv2
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4
	"3fff::/20",      // documentation
	"5f00::/16",      // SRv6 SIDs
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"fec0::/10",      // site-local (deprecated)
	"ff00::/8",       // multicast
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// AddressPolicy decides whether an address may be dialed. The zero value is the
// production policy with no extra ranges; NewAddressPolicy adds the cluster's pod,
// service and node ranges.
// There is no loopback or test exception: tests reach their httptest servers through the
// Options.testDial seam, after this policy has approved the (public) address.
type AddressPolicy struct {
	extra []netip.Prefix
}

// NewAddressPolicy returns the production policy plus extra refused ranges (the cluster's
// pod, service and node CIDRs), parsed from CIDR strings.
func NewAddressPolicy(extraCIDRs []string) (AddressPolicy, error) {
	var p AddressPolicy
	for _, raw := range extraCIDRs {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(s)
		if err != nil {
			return AddressPolicy{}, fmt.Errorf("invalid CIDR %q: %w", s, err)
		}
		pfx = pfx.Masked()
		if pfx.Addr().Is4In6() {
			// An IPv4-mapped prefix would never match an unmapped address; store it as IPv4.
			// A masked prefix whose address is still IPv4-mapped is at least /96.
			pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96)
		}
		p.extra = append(p.extra, pfx)
	}
	return p, nil
}

// Allowed reports whether a may be dialed. An IPv4-mapped IPv6 address is judged as the
// IPv4 address it carries; an address with a zone is refused.
func (p AddressPolicy) Allowed(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, pfx := range blockedPrefixes {
		if pfx.Contains(a) {
			return false
		}
	}
	for _, pfx := range p.extra {
		if pfx.Contains(a) {
			return false
		}
	}
	return true
}
