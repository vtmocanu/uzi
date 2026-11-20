package builtin

import "testing"

func TestValidBuiltin(t *testing.T) {
	for _, n := range []string{"s", "m", "l", "l-highmem"} {
		if !ValidBuiltin(n) {
			t.Errorf("ValidBuiltin(%q) = false, want true", n)
		}
	}
	for _, bad := range []string{"", " ", "S", "L-HIGHMEM", "l_highmem", "xs", " s", "s ", "../s", "unknown"} {
		if ValidBuiltin(bad) {
			t.Errorf("ValidBuiltin(%q) = true, want false", bad)
		}
	}
}

func TestWellFormed(t *testing.T) {
	for _, n := range []string{"s", "xs", "l-highmem", "a0-b", "abcdefghijklmnop"} {
		if !WellFormed(n) {
			t.Errorf("WellFormed(%q) = false, want true", n)
		}
	}
	for _, bad := range []string{"", "S", "1s", "-s", "s_", "s ", "abcdefghijklmnopq", "s/../m"} {
		if WellFormed(bad) {
			t.Errorf("WellFormed(%q) = true, want false", bad)
		}
	}
}

func TestSizesAreWellFormedAndWithinCaps(t *testing.T) {
	for i, s := range Sizes() {
		if !WellFormed(s.Name) {
			t.Errorf("builtin %q is not a well-formed name", s.Name)
		}
		if int(s.DisplayOrder) != i {
			t.Errorf("builtin %q DisplayOrder = %d, want %d", s.Name, s.DisplayOrder, i)
		}
		if s.CPURequestMillicores > s.CPULimitMillicores || s.MemoryRequestMiB > s.MemoryLimitMiB {
			t.Errorf("builtin %q has a request above its limit", s.Name)
		}
		for _, v := range []struct {
			got, max int64
		}{
			{s.CPURequestMillicores, MaxMillicores}, {s.CPULimitMillicores, MaxMillicores},
			{s.MemoryRequestMiB, MaxMemoryMiB}, {s.MemoryLimitMiB, MaxMemoryMiB},
			{s.DataGiB, MaxDiskGiB}, {s.DinDDataGiB, MaxDiskGiB},
		} {
			if v.got <= 0 || v.got > v.max {
				t.Errorf("builtin %q has a value %d outside (0, %d]", s.Name, v.got, v.max)
			}
		}
	}
}

func TestSizesReturnsAFreshCopy(t *testing.T) {
	a := Sizes()
	a[0].Name = "mutated"
	if Sizes()[0].Name != "s" {
		t.Fatal("Sizes shares state between calls")
	}
}
