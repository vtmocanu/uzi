package runkind

import (
	"sort"
	"testing"
)

// TestDeliverySupportingPartitionAll pins issue #4684's kind sets: Delivery and
// Supporting are disjoint, duplicate-free and together equal All() exactly.
func TestDeliverySupportingPartitionAll(t *testing.T) {
	seen := map[string]string{}
	for name, set := range map[string][]string{"Delivery": Delivery(), "Supporting": Supporting()} {
		if len(set) == 0 {
			t.Fatalf("%s() is empty", name)
		}
		inSet := map[string]bool{}
		for _, k := range set {
			if inSet[k] {
				t.Errorf("%s() lists %q twice", name, k)
			}
			inSet[k] = true
			if other, dup := seen[k]; dup {
				t.Errorf("%q is in both %s() and %s()", k, other, name)
			}
			seen[k] = name
		}
	}
	var union, all []string
	for k := range seen {
		union = append(union, k)
	}
	all = append(all, All()...)
	sort.Strings(union)
	sort.Strings(all)
	if len(union) != len(all) {
		t.Fatalf("Delivery()+Supporting() = %v, want exactly All() = %v", union, all)
	}
	for i := range all {
		if union[i] != all[i] {
			t.Fatalf("Delivery()+Supporting() = %v, want exactly All() = %v", union, all)
		}
	}
}

// TestDeliverySupportingContents pins the membership itself, so moving a kind
// between the sets fails here and not only in a downstream ratio.
func TestDeliverySupportingContents(t *testing.T) {
	wantDelivery := []string{Issue, MRRework, CIFix, Prompt, SelfImprove, Task, Job}
	wantSupporting := []string{Judge, CrossCheck, Chat}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if got := Delivery(); !eq(got, wantDelivery) {
		t.Errorf("Delivery() = %v, want %v", got, wantDelivery)
	}
	if got := Supporting(); !eq(got, wantSupporting) {
		t.Errorf("Supporting() = %v, want %v", got, wantSupporting)
	}
	// Each call returns a fresh slice: mutating one result must not leak.
	d := Delivery()
	d[0] = "mutated"
	if Delivery()[0] != Issue {
		t.Error("Delivery() shares its backing array between calls")
	}
	s := Supporting()
	s[0] = "mutated"
	if Supporting()[0] != Judge {
		t.Error("Supporting() shares its backing array between calls")
	}
}
