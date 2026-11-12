package issuetracker

import (
	"reflect"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// TestDriverIsNotCodeHost pins PRD #4173 S1: the Driver method set must not
// satisfy forge.CodeHost, nor even share its code-host methods.
func TestDriverIsNotCodeHost(t *testing.T) {
	driver := reflect.TypeFor[Driver]()
	codeHost := reflect.TypeFor[forge.CodeHost]()
	if driver.Implements(codeHost) {
		t.Fatal("Driver implements forge.CodeHost")
	}
	// VerifyToken and ListProjects legitimately exist on both with different
	// signatures; every other CodeHost method must be absent from Driver.
	shared := map[string]bool{"VerifyToken": true, "ListProjects": true}
	for m := range codeHost.Methods() {
		if shared[m.Name] {
			continue
		}
		if _, ok := driver.MethodByName(m.Name); ok {
			t.Errorf("Driver exposes code-host method %s", m.Name)
		}
	}
}

func TestDriverEmbedsIssueTracker(t *testing.T) {
	if !reflect.TypeFor[Driver]().Implements(reflect.TypeFor[forge.IssueTracker]()) {
		t.Fatal("Driver does not implement forge.IssueTracker")
	}
}
