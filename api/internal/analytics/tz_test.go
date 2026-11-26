package analytics

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata" // embedded zone database: a fallback used only when the host has no zoneinfo
)

func TestValidateTZ(t *testing.T) {
	set := map[string]struct{}{
		"Europe/Bucharest": {},
		"Only/InDB":        {},
		"Local":            {},
		"":                 {},
	}
	loader := func(name string) (*time.Location, error) {
		if name == "Only/InDB" {
			return nil, errors.New("go cannot load it")
		}
		return time.LoadLocation(name)
	}

	loc, err := ValidateTZ("Europe/Bucharest", set, loader)
	if err != nil || loc.String() != "Europe/Bucharest" {
		t.Fatalf("valid name: loc=%v err=%v", loc, err)
	}
	// In the DB set but rejected by the Go loader.
	if _, err := ValidateTZ("Only/InDB", set, loader); !errors.Is(err, ErrInvalidTZ) {
		t.Errorf("db-only name: got %v", err)
	}
	// Accepted by the Go loader but absent from the DB set.
	if _, err := ValidateTZ("America/New_York", set, loader); !errors.Is(err, ErrInvalidTZ) {
		t.Errorf("loader-only name: got %v", err)
	}
	for _, bad := range []string{"", "Local"} {
		if _, err := ValidateTZ(bad, set, loader); !errors.Is(err, ErrInvalidTZ) {
			t.Errorf("%q: got %v", bad, err)
		}
	}
}
