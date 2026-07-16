package workersize

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for _, name := range Names {
		if !Valid(name) {
			t.Errorf("Valid(%q) = false, want true (it is in Names)", name)
		}
	}
	// Untrusted junk a user might POST, plus the near-misses that matter: the
	// upper-case spellings the UI displays (the wire value is lowercase), and
	// anything that would be free text reaching a pod spec.
	for _, bad := range []string{
		"", " ", "S", "M", "L", "s ", " s", "xl", "small", "../s", "s/../m", "unknown",
	} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true, want false", bad)
		}
	}
}

// The wire golden (hostedsvc/testdata/controller_poll_wire.json) pins the size
// spelling as lowercase, and the controller resolves the name it receives against
// its own preset table. An upper-case or padded entry here would be accepted at
// provision, stored, sent — and never resolve on the far side, leaving the worker
// pending until its token expired. Cheap to assert, silent and slow to debug.
func TestNamesAreWireSafe(t *testing.T) {
	for _, n := range Names {
		if n == "" {
			t.Error("Names contains an empty entry")
			continue
		}
		if n != strings.ToLower(n) || n != strings.TrimSpace(n) {
			t.Errorf("Names entry %q must be lowercase and unpadded (it goes on the wire as DesiredWorker.Size)", n)
		}
	}
}

func TestNamesHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Names {
		if seen[n] {
			t.Errorf("Names contains %q twice", n)
		}
		seen[n] = true
	}
}
