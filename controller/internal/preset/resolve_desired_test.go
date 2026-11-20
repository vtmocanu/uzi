package preset

import (
	"errors"
	"testing"
)

func seedResources(s sizeSeed) *Resources {
	return &Resources{
		CPURequestMillicores: s.CPURequestMillicores,
		CPULimitMillicores:   s.CPULimitMillicores,
		MemoryRequestMiB:     s.MemoryRequestMiB,
		MemoryLimitMiB:       s.MemoryLimitMiB,
		DataGiB:              s.DataGiB,
		DinDDataGiB:          s.DinDDataGiB,
	}
}

func newTestResolver(t *testing.T) Resolver {
	t.Helper()
	r, err := NewResolver("harbor.example.com/uzi", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// With nil resources ResolveDesired is exactly the compiled-in Resolve.
func TestResolveDesiredNilIsTheCompiledInPath(t *testing.T) {
	r := newTestResolver(t)
	for _, name := range SizeNames() {
		want, err := r.Resolve("base", name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.ResolveDesired("base", name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Image != want.Image || got.NixSize.String() != want.NixSize.String() {
			t.Errorf("%s: image/nix differ: %+v vs %+v", name, got, want)
		}
		if got.Size.CPURequest.String() != want.Size.CPURequest.String() ||
			got.Size.CPULimit.String() != want.Size.CPULimit.String() ||
			got.Size.MemoryRequest.String() != want.Size.MemoryRequest.String() ||
			got.Size.MemoryLimit.String() != want.Size.MemoryLimit.String() ||
			got.Size.DataSize.String() != want.Size.DataSize.String() ||
			got.Size.DinDDataSize.String() != want.Size.DinDDataSize.String() {
			t.Errorf("%s: nil path differs from Resolve: %+v vs %+v", name, got.Size, want.Size)
		}
	}
}

// A non-builtin size name with no integers is a typed size miss; with integers it resolves.
func TestResolveDesiredUnknownSizeNeedsIntegers(t *testing.T) {
	r := newTestResolver(t)
	_, err := r.ResolveDesired("base", "xs", nil)
	var u *UnknownError
	if !errors.As(err, &u) || u.Field != "size" || u.Value != "xs" {
		t.Fatalf("err = %v, want UnknownError on size xs", err)
	}
	if _, err := r.ResolveDesired("base", "xs", &Resources{250, 1000, 1024, 2048, 5, 20}); err != nil {
		t.Fatalf("custom size with integers: %v", err)
	}
	if _, err := r.ResolveDesired("nope", "xs", &Resources{250, 1000, 1024, 2048, 5, 20}); !IsUnknown(err) {
		t.Fatalf("unknown template must still miss, got %v", err)
	}
}

// The integers of every builtin seed rebuild the compiled-in quantities, value and
// canonical string form (what a rendered pod carries).
func TestResolveDesiredIntegersMatchBuiltinsAndFormat(t *testing.T) {
	r := newTestResolver(t)
	wantStrings := map[string][6]string{
		"s":         {"250m", "1", "2Gi", "4Gi", "5Gi", "20Gi"},
		"m":         {"1", "4", "8Gi", "12Gi", "25Gi", "20Gi"},
		"l":         {"1", "4", "14Gi", "20Gi", "50Gi", "50Gi"},
		"l-highmem": {"1", "4", "24Gi", "32Gi", "50Gi", "50Gi"},
	}
	for _, seed := range readSizeSeeds(t) {
		compiled, err := r.Resolve("base", seed.Name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := r.ResolveDesired("base", seed.Name, seedResources(seed))
		if err != nil {
			t.Fatal(err)
		}
		s := got.Size
		have := [6]string{s.CPURequest.String(), s.CPULimit.String(), s.MemoryRequest.String(), s.MemoryLimit.String(), s.DataSize.String(), s.DinDDataSize.String()}
		if have != wantStrings[seed.Name] {
			t.Errorf("%s: strings = %v, want %v", seed.Name, have, wantStrings[seed.Name])
		}
		c := compiled.Size
		cs := [6]string{c.CPURequest.String(), c.CPULimit.String(), c.MemoryRequest.String(), c.MemoryLimit.String(), c.DataSize.String(), c.DinDDataSize.String()}
		if have != cs {
			t.Errorf("%s: integer path %v differs from compiled-in %v", seed.Name, have, cs)
		}
		if got.Image != compiled.Image || got.NixSize.Cmp(compiled.NixSize) != 0 {
			t.Errorf("%s: image or nix differ", seed.Name)
		}
	}
}

// Non-positive and overflow-prone integers are a typed refusal, never a partial spec,
// and there is no per-field fallback to the compiled-in table.
func TestResolveDesiredRejectsInvalidIntegers(t *testing.T) {
	r := newTestResolver(t)
	ok := Resources{250, 1000, 1024, 2048, 5, 20}
	mutations := map[string]func(*Resources){
		"cpu request zero":   func(x *Resources) { x.CPURequestMillicores = 0 },
		"cpu limit neg":      func(x *Resources) { x.CPULimitMillicores = -1 },
		"cpu over":           func(x *Resources) { x.CPULimitMillicores = 64001 },
		"mem request zero":   func(x *Resources) { x.MemoryRequestMiB = 0 },
		"mem limit over":     func(x *Resources) { x.MemoryLimitMiB = 524289 },
		"mem huge":           func(x *Resources) { x.MemoryLimitMiB = 1 << 60 },
		"data zero":          func(x *Resources) { x.DataGiB = 0 },
		"data over":          func(x *Resources) { x.DataGiB = 2049 },
		"dind negative":      func(x *Resources) { x.DinDDataGiB = -5 },
		"dind huge overflow": func(x *Resources) { x.DinDDataGiB = 1 << 62 },
	}
	for name, mut := range mutations {
		res := ok
		mut(&res)
		spec, err := r.ResolveDesired("base", "m", &res)
		var inv *InvalidResourcesError
		if !errors.As(err, &inv) {
			t.Errorf("%s: err = %v, want InvalidResourcesError", name, err)
		}
		if spec.Image != "" {
			t.Errorf("%s: partial spec returned", name)
		}
	}
	// The bounds themselves are accepted.
	if _, err := r.ResolveDesired("base", "m", &Resources{64000, 64000, 524288, 524288, 2048, 2048}); err != nil {
		t.Errorf("max bounds refused: %v", err)
	}
}
