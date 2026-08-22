package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// A valid repo UUID is passed through resolveProjectSyncRepo verbatim (no ListRepos
// round-trip), which keeps these tests focused on the status/resync rendering.
const psRepoID = "11111111-1111-1111-1111-111111111111"

// status on a linked repo renders every field: project number, ownership, health,
// last sync, item count, and the unmatched-columns list.
func TestProjectSyncStatusLinked(t *testing.T) {
	synced := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fc := &uzicli.FakeClient{ProjectSyncStatusResult: uzicli.ProjectSyncStatus{
		ProjectNumber:    7,
		OwnedByUzi:       true,
		LastSyncedAt:     &synced,
		ItemCount:        42,
		UnmatchedColumns: []string{"Blocked", "Icebox"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastProjectSyncStatusRepoID != psRepoID {
		t.Fatalf("status queried repo %q, want %q", fc.LastProjectSyncStatusRepoID, psRepoID)
	}
	for _, want := range []string{"LINKED", "#7", "OWNED_BY_UZI", "true", "42", "Blocked", "Icebox"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// A last_error surfaces as HEALTH=error plus the message.
func TestProjectSyncStatusUnhealthy(t *testing.T) {
	msg := "forge rate limited"
	fc := &uzicli.FakeClient{ProjectSyncStatusResult: uzicli.ProjectSyncStatus{
		ProjectNumber: 3,
		LastError:     &msg,
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "error") || !strings.Contains(out, msg) {
		t.Errorf("unhealthy status must show error + message:\n%s", out)
	}
}

// A 404 (foreign/unknown repo, or a repo with no link row) is the not-linked case:
// normal output, exit 0 — NOT an error.
func TestProjectSyncStatusNotLinked(t *testing.T) {
	fc := &uzicli.FakeClient{GetProjectSyncStatusErr: uzicli.Exitf(uzicli.ExitNotFound, "project sync not enabled for this repo")}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("not-linked exit = %d, want 0 (not an error)", code)
	}
	if !strings.Contains(out, "not linked") {
		t.Errorf("a 404 must print a 'not linked' line, got:\n%s", out)
	}
}

// --json emits linked:false for the not-linked case, so an agent can branch on it.
func TestProjectSyncStatusNotLinkedJSON(t *testing.T) {
	fc := &uzicli.FakeClient{GetProjectSyncStatusErr: uzicli.Exitf(uzicli.ExitNotFound, "not found")}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID, "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"linked": false`) {
		t.Errorf("--json not-linked must carry linked:false:\n%s", out)
	}
}

// --json on a linked repo passes the fields through.
func TestProjectSyncStatusLinkedJSON(t *testing.T) {
	fc := &uzicli.FakeClient{ProjectSyncStatusResult: uzicli.ProjectSyncStatus{
		ProjectNumber: 9, OwnedByUzi: true, ItemCount: 5, UnmatchedColumns: []string{},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID, "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"linked": true`) || !strings.Contains(out, `"project_number": 9`) || !strings.Contains(out, `"item_count": 5`) {
		t.Errorf("--json linked output missing fields:\n%s", out)
	}
}

// A non-404 error (e.g. a 5xx) is NOT softened: it propagates as its exit code.
func TestProjectSyncStatusServerError(t *testing.T) {
	fc := &uzicli.FakeClient{GetProjectSyncStatusErr: uzicli.Exitf(uzicli.ExitUnreachable, "server error (500)")}
	_, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", psRepoID)
	if code != uzicli.ExitUnreachable {
		t.Fatalf("exit = %d, want ExitUnreachable (%d) — a 5xx must not read as not-linked", code, uzicli.ExitUnreachable)
	}
}

// resync calls the client with the resolved id and prints a confirmation.
func TestProjectSyncResync(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "resync", psRepoID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastResyncProjectSyncRepoID != psRepoID {
		t.Fatalf("resync called with repo %q, want %q", fc.LastResyncProjectSyncRepoID, psRepoID)
	}
	if !strings.Contains(out, "resync started") {
		t.Errorf("resync output = %q, want a 'resync started' confirmation", out)
	}
}

// resync --json emits a machine-readable confirmation.
func TestProjectSyncResyncJSON(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, _, code := runCLI(t, fakeEnv(fc), "project-sync", "resync", psRepoID, "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"resync": "started"`) {
		t.Errorf("resync --json = %q, want the started envelope", out)
	}
}

// resync surfaces a 404 (unknown repo, or one with no link row) as exit 4.
func TestProjectSyncResyncNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{ResyncProjectSyncErr: uzicli.Exitf(uzicli.ExitNotFound, "this repo has no linked project to resync")}
	_, _, code := runCLI(t, fakeEnv(fc), "project-sync", "resync", psRepoID)
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want ExitNotFound (%d)", code, uzicli.ExitNotFound)
	}
}

// A path-with-namespace <repo> resolves through ListRepos to exactly one repo id,
// which is then what the status read is issued against.
func TestProjectSyncResolvesPathWithNamespace(t *testing.T) {
	fc := &uzicli.FakeClient{
		Repos: []apitypes.RepoDTO{
			{ID: psRepoID, PathWithNamespace: "org/repo"},
			{ID: "22222222-2222-2222-2222-222222222222", PathWithNamespace: "org/other"},
		},
	}
	_, _, code := runCLI(t, fakeEnv(fc), "project-sync", "status", "org/repo")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastProjectSyncStatusRepoID != psRepoID {
		t.Fatalf("path resolved to repo %q, want %q", fc.LastProjectSyncStatusRepoID, psRepoID)
	}
}

// An unmatched path-with-namespace is a usage error naming `uzi repo list`.
func TestProjectSyncRepoNoMatchIsUsageError(t *testing.T) {
	fc := &uzicli.FakeClient{Repos: []apitypes.RepoDTO{{ID: psRepoID, PathWithNamespace: "org/repo"}}}
	_, errb, code := runCLI(t, fakeEnv(fc), "project-sync", "status", "org/nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage (%d)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errb, "repo list") {
		t.Errorf("no-match error should point at 'uzi repo list':\n%s", errb)
	}
}
