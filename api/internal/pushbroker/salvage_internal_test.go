package pushbroker

import (
	"crypto/sha1" //nolint:gosec // git pack trailers ARE SHA-1; this verifies the pinned empty-pack trailer, not a security primitive.
	"encoding/binary"
	"testing"
)

// TestEmptyPackTrailer pins emptyPack's layout (PRD #1867 M1): "PACK", version 2, zero
// objects, then the SHA-1 of those 12 header bytes. A wrong trailer makes every real
// receive-pack refuse the salvage create.
func TestEmptyPackTrailer(t *testing.T) {
	if len(emptyPack) != 32 {
		t.Fatalf("len(emptyPack) = %d, want 32", len(emptyPack))
	}
	hdr := emptyPack[:12]
	if string(hdr[:4]) != "PACK" {
		t.Fatalf("signature = %q, want PACK", hdr[:4])
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	if n := binary.BigEndian.Uint32(hdr[8:12]); n != 0 {
		t.Fatalf("object count = %d, want 0", n)
	}
	sum := sha1.Sum(hdr) //nolint:gosec // G401: the git pack trailer IS SHA-1 by format.
	if string(emptyPack[12:]) != string(sum[:]) {
		t.Fatalf("trailer = %x, want %x", emptyPack[12:], sum)
	}
}

func TestPromoteResultString(t *testing.T) {
	cases := map[PromoteResult]string{
		PromoteFailed:                "failed",
		PromoteDone:                  "done",
		PromoteSalvagedBranchPending: "salvaged_branch_pending",
		PromoteUnavailable:           "unavailable",
		PromoteRefused:               "refused",
		PromoteResult(99):            "PromoteResult(99)",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("PromoteResult(%d).String() = %q, want %q", int(r), got, want)
		}
	}
	var zero PromoteResult
	if zero != PromoteFailed {
		t.Fatalf("zero PromoteResult = %v, want PromoteFailed", zero)
	}
}
