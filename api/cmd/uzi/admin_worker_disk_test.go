package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestAdminWorkersDiskCleanup(t *testing.T) {
	fc := &uzicli.FakeClient{AdminWorkers: []apitypes.AdminWorkerDTO{
		{WorkerDTO: apitypes.WorkerDTO{ID: "w1", Name: "pressured"}, OwnerEmail: "owner@example.com", DiskPressureVolumes: []string{"nix", "data", "dind"}, CleanupPending: true},
		{WorkerDTO: apitypes.WorkerDTO{ID: "w2", Name: "waiting"}, CleanupPending: true},
		{WorkerDTO: apitypes.WorkerDTO{ID: "w3", Name: "clear"}},
		{WorkerDTO: apitypes.WorkerDTO{ID: "w4", Name: "hostile\x1b[31m\nname"}, OwnerEmail: "owner\x1b[31m@example.com", DiskPressureVolumes: []string{"dind\x1b[31m"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "workers")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 5 {
		t.Fatalf("forged rows: %q", out)
	}
	if !strings.Contains(lines[0], "DISK") || !strings.Contains(lines[0], "CLEANUP") {
		t.Fatalf("headers: %s", out)
	}
	if !strings.Contains(lines[1], "nix,data,dind") || !strings.Contains(lines[1], "cleanup pending") {
		t.Fatalf("pressure row: %s", lines[1])
	}
	if !strings.Contains(lines[2], "cleanup pending") {
		t.Fatalf("operation row: %s", lines[2])
	}
	if strings.Contains(lines[3], "cleanup pending") {
		t.Fatalf("clear row: %s", lines[3])
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("ANSI injection: %q", out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "admin", "workers", "--json")
	if code != uzicli.ExitOK || !strings.Contains(out, `"disk_pressure_volumes"`) || !strings.Contains(out, `"cleanup_pending": true`) {
		t.Fatalf("json: code=%d %s", code, out)
	}
}
