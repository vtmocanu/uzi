package workersvc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandlerNeverReferencesCollectCodexAccountUsage is the belt-and-braces token-boundary
// guard for PRD #1209 M2: the raw access token never leaves package workersvc BECAUSE
// CollectCodexAccountUsage returns a token-free CodexUsageReading and is reached only from
// the in-process poller. The token stays in-package by construction, so this only documents
// the boundary — but it documents it the same way FreezeCodexBinding's "no public route
// reaches it" does: by proving the handler package (every HTTP route) names neither the
// method nor the reading type, so no route can ever surface one.
//
// It scans handler SOURCE (a grep over ../handler/*.go), which is exactly what makes it
// catch a future accidental wiring that would still compile.
func TestHandlerNeverReferencesCollectCodexAccountUsage(t *testing.T) {
	const handlerDir = "../handler"
	entries, err := os.ReadDir(handlerDir)
	if err != nil {
		t.Fatalf("read handler dir: %v", err)
	}

	// The symbols a route must never name: the in-package collector entrypoint and its
	// token-adjacent return/failure types. The M1 poke seam (CodexUsagePoker.Poke) IS allowed
	// in handler — it carries only a user id, never a token — so it is deliberately not listed.
	forbidden := []string{
		"CollectCodexAccountUsage",
		"CodexUsageReading",
		"CodexUsageFailure",
		"newCodexPollPrincipal",
	}

	var found []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(handlerDir, e.Name())
		data, rerr := os.ReadFile(path) //nolint:gosec // G304: a test grepping sibling-package source files under a fixed relative dir, not user input
		if rerr != nil {
			t.Fatalf("read %s: %v", path, rerr)
		}
		src := string(data)
		for _, sym := range forbidden {
			if strings.Contains(src, sym) {
				found = append(found, e.Name()+" references "+sym)
			}
		}
	}
	if len(found) > 0 {
		t.Fatalf("handler package must not reference workersvc's in-package Codex usage collector "+
			"(the raw access token must never leave workersvc): %s", strings.Join(found, "; "))
	}
}
