package releasecheck

import (
	"os/exec"
	"strings"
	"testing"
)

func TestNoCLIDeps(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-buildvcs=false", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps -buildvcs=false .: %v\n%s", err, out)
	}
	const cli = "github.com/vtmocanu/uzi/api/internal/uzicli"
	for _, dependency := range strings.Fields(string(out)) {
		if dependency == cli || strings.HasPrefix(dependency, cli+"/") {
			t.Errorf("releasecheck must not depend on CLI package %s", dependency)
		}
	}
}
