package issuetracker

import (
	"errors"
	"testing"
)

func TestNewUnknownKind(t *testing.T) {
	d, err := New(Kind("nope"), Options{})
	if d != nil {
		t.Fatalf("driver = %v, want nil", d)
	}
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("err = %v, want ErrUnsupportedKind", err)
	}
}

// Every constructor key must be a supported kind; the converse holds once a
// driver registers (m2 registers YouTrack), so it is checked only then.
func TestConstructorsMatchSupportedKinds(t *testing.T) {
	supported := map[Kind]bool{}
	for _, k := range SupportedKinds() {
		supported[k] = true
	}
	for k := range constructors {
		if !supported[k] {
			t.Errorf("constructor for %q is not in SupportedKinds", k)
		}
	}
	if len(constructors) > 0 {
		for k := range supported {
			if constructors[k] == nil {
				t.Errorf("SupportedKinds entry %q has no constructor", k)
			}
		}
	}
}
