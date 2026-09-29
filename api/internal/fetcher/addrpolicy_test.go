package fetcher

import (
	"net/netip"
	"testing"
)

// requiredBlocked is the PRD #1906 M2 floor: every range here must be covered by an
// entry of blockedPrefixes itself, independently of the stdlib predicates (IsPrivate,
// IsGlobalUnicast) that Allowed also applies, so removing a list entry is caught even
// where the stdlib would still refuse the address.
var requiredBlocked = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4",
	"240.0.0.0/4",
	"::/96", "::1/128", "100::/64", "fc00::/7", "fe80::/10", "fec0::/10", "64:ff9b::/96",
	"64:ff9b:1::/48", "2002::/16", "2001::/32", "ff00::/8",
}

func TestBlockedPrefixesCoverRequiredRanges(t *testing.T) {
	for _, s := range requiredBlocked {
		want := netip.MustParsePrefix(s)
		covered := false
		for _, p := range blockedPrefixes {
			if p.Bits() <= want.Bits() && p.Contains(want.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s is not covered by blockedPrefixes", s)
		}
	}
}

// Every required range, probed at its first and last address, is refused by Allowed.
func TestAddressPolicyRefusesRequiredRanges(t *testing.T) {
	var p AddressPolicy
	for _, s := range requiredBlocked {
		pfx := netip.MustParsePrefix(s)
		first := pfx.Addr()
		last := lastAddr(pfx)
		for _, a := range []netip.Addr{first, last} {
			if p.Allowed(a) {
				t.Errorf("%s (in %s) allowed", a, s)
			}
			if a.Is4() {
				mapped := netip.AddrFrom16(a.As16())
				if p.Allowed(mapped) {
					t.Errorf("IPv4-mapped %s (in %s) allowed", mapped, s)
				}
			}
		}
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	bits := p.Bits()
	for i := range b {
		for j := 0; j < 8; j++ {
			if i*8+j >= bits {
				b[i] |= 0x80 >> j
			}
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// Only global unicast is admitted: the stdlib predicates refuse what no list names.
func TestAddressPolicyOnlyGlobalUnicast(t *testing.T) {
	var p AddressPolicy
	for _, s := range []string{
		"0.0.0.0", "::", "255.255.255.255", "ff05::1", "fe80::1%eth0",
	} {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatal(err)
		}
		if p.Allowed(a) {
			t.Errorf("%s allowed", s)
		}
	}
	if p.Allowed(netip.Addr{}) {
		t.Error("the zero address allowed")
	}
	for _, s := range []string{publicV4, publicV4b, publicV6, "8.8.8.8", "::ffff:8.8.8.8", "2a00:1450:4001::1"} {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("public %s refused", s)
		}
	}
}

func TestAddressPolicyExtraCIDRs(t *testing.T) {
	p, err := NewAddressPolicy([]string{" 93.184.0.0/16", "", "::ffff:8.8.8.0/120", "2606:2800::/32", "::ffff:0:0/95"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{publicV4, "8.8.8.8", "::ffff:8.8.8.9", publicV6} {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed despite an extra range", s)
		}
	}
	if !p.Allowed(netip.MustParseAddr("8.8.4.4")) {
		t.Error("8.8.4.4 refused")
	}
	for _, bad := range []string{"10.0.0.0/33", "nope"} {
		if _, err := NewAddressPolicy([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The stdlib layer of Allowed (only global unicast, never private) holds on its own: with
// the prefix list emptied, the addresses it alone covers are still refused. Not parallel:
// it swaps a package variable.
func TestAddressPolicyStdlibLayerAlone(t *testing.T) {
	saved := blockedPrefixes
	blockedPrefixes = nil
	t.Cleanup(func() { blockedPrefixes = saved })
	var p AddressPolicy
	for _, s := range []string{
		"0.0.0.0", "127.0.0.1", "169.254.169.254", "10.1.2.3", "172.16.0.1", "192.168.0.1",
		"224.0.0.1", "255.255.255.255", "::", "::1", "fe80::1", "fd00::1", "ff02::1", "::ffff:10.0.0.1",
	} {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed without the prefix list", s)
		}
	}
}

// IPv6 is admitted only inside 2000::/3, the global unicast allocation: every other IPv6
// range is refused whatever it embeds. ::ffff:0:a.b.c.d (SIIT, ::ffff:0:0:0/96) carries
// an IPv4 address that Unmap does not extract, so a blocklist alone let
// ::ffff:0:a9fe:a9fe (169.254.169.254) through.
func TestAddressPolicyIPv6OnlyInGlobalUnicastAllocation(t *testing.T) {
	var p AddressPolicy
	for _, s := range []string{
		"::ffff:0:a9fe:a9fe", "::ffff:0:a00:1", "::ffff:0:7f00:1", "4000::1", "::1:0:0:1",
		"1000::1", "8000::1", "e000::1", "1fff:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
		"4000::", "c000::1",
	} {
		if p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{publicV6, "2a00:1450:4001::1", "2606:4700::1111", "2c0f:fb50::1", "3ffe:ffff::1"} {
		if !p.Allowed(netip.MustParseAddr(s)) {
			t.Errorf("public %s refused", s)
		}
	}
}
