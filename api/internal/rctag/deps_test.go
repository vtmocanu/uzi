package rctag

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"testing"
)

func TestProductionDeps(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-buildvcs=false", "-json", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps -buildvcs=false -json .: %v\n%s", err, stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	count := 0
	for {
		var pkg struct {
			ImportPath string
			Standard   bool
		}
		err := decoder.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode production dependency closure: %v", err)
		}
		count++
		if pkg.ImportPath == "" {
			t.Fatal("dependency has no import path")
		}
		if !pkg.Standard && pkg.ImportPath != "github.com/vtmocanu/uzi/api/internal/rctag" &&
			pkg.ImportPath != "golang.org/x/mod/semver" {
			t.Errorf("rctag production dependency is not allowed: %s", pkg.ImportPath)
		}
	}
	if count == 0 {
		t.Fatal("empty production dependency closure")
	}
}
