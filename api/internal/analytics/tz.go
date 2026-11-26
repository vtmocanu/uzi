package analytics

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidTZ is returned by ValidateTZ for any rejected timezone name.
var ErrInvalidTZ = errors.New("analytics: invalid timezone")

// ValidateTZ accepts name only when it is non-empty, not "Local", present in
// dbSet (the database's pg_timezone_names) AND loadable by loader. Both sides
// must agree so the Go calendar and the SQL session never diverge. Defaulting
// an omitted tz to UTC is the handler's job, not this function's.
func ValidateTZ(name string, dbSet map[string]struct{}, loader func(string) (*time.Location, error)) (*time.Location, error) {
	if name == "" || name == "Local" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidTZ, name)
	}
	if _, ok := dbSet[name]; !ok {
		return nil, fmt.Errorf("%w: %q not known to the database", ErrInvalidTZ, name)
	}
	loc, err := loader(name)
	if err != nil || loc == nil {
		return nil, fmt.Errorf("%w: %q not loadable", ErrInvalidTZ, name)
	}
	return loc, nil
}
