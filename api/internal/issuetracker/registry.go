package issuetracker

import "fmt"

// Constructor builds a driver from options. A build error never carries the
// token, a part of it or any other remote content, because callers show it to
// the user.
type Constructor func(Options) (Driver, error)

// constructors maps each Kind to its driver constructor. A driver adds one
// entry here; there is no init-time registration.
var constructors = map[Kind]Constructor{KindYouTrack: newYouTrack}

// New constructs the driver for kind. An unknown kind wraps ErrUnsupportedKind.
// Like every Constructor, it never puts the token in an error.
func New(kind Kind, opts Options) (Driver, error) {
	ctor, ok := constructors[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %q (supported: %s)", ErrUnsupportedKind, string(kind), SupportedKindsList())
	}
	return ctor(opts)
}
