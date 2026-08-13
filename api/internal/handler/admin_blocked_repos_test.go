package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/privcheck"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// guardrailBlockedForRepo is the pure server-side badge-STATE rule (PRD #66 M9, D8):
// the stored findings run through the SINGLE shared DowngradeOverridden, then Blocks().
// These cases pin the contract the web reads instead of re-deriving the waivable set —
// especially the key correctness case that an override NEVER waives
// protection_unreadable (D3/R8), so it needs no DB.
func TestGuardrailBlockedForRepo(t *testing.T) {
	const id = "repo-1"
	block := privcheck.Finding{Code: privcheck.CodeWriteRoleCanPush, Severity: privcheck.SeverityBlock, Message: "the write role can push to main"}
	unreadable := privcheck.Finding{Code: privcheck.CodeProtectionUnreadable, Severity: privcheck.SeverityBlock, Message: "protection could not be read"}
	warn := privcheck.Finding{Code: privcheck.CodeBotRoleBelowWrite, Severity: privcheck.SeverityWarn, Message: "bot role below write"}

	repWith := func(findings ...privcheck.Finding) *privcheck.Report {
		return &privcheck.Report{Repos: []privcheck.RepoReport{{RepoID: id, Findings: findings}}}
	}

	cases := []struct {
		name       string
		report     *privcheck.Report
		overridden bool
		want       bool
	}{
		{"waivable block, no override -> blocked", repWith(block), false, true},
		{"waivable block, overridden -> not blocked", repWith(block), true, false},
		// THE KEY CASE: an override never waives protection_unreadable, so an overridden
		// repo whose only finding is unreadable protection is STILL blocked (D3/R8).
		{"unreadable only, overridden -> STILL blocked", repWith(unreadable), true, true},
		{"unreadable only, no override -> blocked", repWith(unreadable), false, true},
		{"warn only -> not blocked", repWith(warn), false, false},
		{"nil report -> unknown, not blocked", nil, false, false},
		{"repo absent from report -> not blocked", &privcheck.Report{Repos: nil}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := guardrailBlockedForRepo(c.report, id, c.overridden); got != c.want {
				t.Fatalf("guardrailBlockedForRepo = %v, want %v", got, c.want)
			}
		})
	}
}

// stampReport writes a stored privilege_report + status onto a connection (the admin
// list reads the STORED blob, unlike M3's live scan). A "" status leaves
// privilege_status NULL — the never-checked / INTERVAL=0 case (R1).
func (f enableGuardFixture) stampReport(ctx context.Context, t *testing.T, connID uuid.UUID, status string, rep privcheck.Report) {
	t.Helper()
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var st any
	if status != "" {
		st = status
	}
	mustExecT(ctx, t, f.pool,
		`UPDATE forge_connections SET privilege_report = $2, privilege_status = $3, privilege_checked_at = now() WHERE id = $1`,
		connID, b, st)
}

// addConn inserts a forge connection for owner (admin flag ignored — the owner is
// created by the caller), returning its id.
func (f enableGuardFixture) addConn(ctx context.Context, t *testing.T, ownerID uuid.UUID, baseURL string) uuid.UUID {
	t.Helper()
	sealed, err := f.h.box.Seal([]byte("glpat-dummy-token"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	connID := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', $3, 'uzi-bot', 999, $4)`,
		connID, ownerID, baseURL, sealed)
	return connID
}

// addRepoRow inserts a repo on connID with an optional override reason (empty = none),
// returning its id. No forge interaction — the admin list reads the stored report.
func (f enableGuardFixture) addRepoRow(ctx context.Context, t *testing.T, connID uuid.UUID, projectID int64, path, overrideReason string, overrideBy uuid.UUID) uuid.UUID {
	t.Helper()
	repoID := uuid.New()
	mustExecT(ctx, t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.example/x', 'main', true)`,
		repoID, connID, projectID, path)
	if overrideReason != "" {
		mustExecT(ctx, t, f.pool,
			`UPDATE repos SET guardrail_override_reason = $2, guardrail_override_by = $3, guardrail_override_at = now() WHERE id = $1`,
			repoID, overrideReason, overrideBy)
	}
	return repoID
}

func repoReport(repoID uuid.UUID, path string, findings ...privcheck.Finding) privcheck.RepoReport {
	return privcheck.RepoReport{RepoID: repoID.String(), Path: path, Findings: findings}
}

// The admin cross-user blocked-repos list returns every user's blocked OR overridden
// repos, resolves the override actor's email, and flags checks_unknown when any
// connection was never checked (R1). Members are refused via RequireAdminRO.
func TestAdminListBlockedReposLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newEnableGuardFixture(ctx, t)
	admin := f.mkAdmin(ctx, t)

	block := privcheck.Finding{Code: privcheck.CodeWriteRoleCanPush, Severity: privcheck.SeverityBlock, Message: "the write role can push to main"}
	warn := privcheck.Finding{Code: privcheck.CodeBotRoleBelowWrite, Severity: privcheck.SeverityWarn, Message: "bot role below write"}

	// Owner 1 (the fixture owner, a member): a blocked repo, an overridden-clean repo,
	// and a clean repo that must NOT appear.
	repoBlocked := f.addRepoRow(ctx, t, f.connID, 9001, "g/blocked", "", uuid.Nil)
	repoOverridden := f.addRepoRow(ctx, t, f.connID, 9002, "g/overridden", "forge fix scheduled", admin.ID)
	repoClean := f.addRepoRow(ctx, t, f.connID, 9003, "g/clean", "", uuid.Nil)
	f.stampReport(ctx, t, f.connID, "violations", privcheck.Report{
		CheckedAt: time.Now().UTC(),
		Status:    privcheck.StatusViolations,
		Repos: []privcheck.RepoReport{
			repoReport(repoBlocked, "g/blocked", block),
			repoReport(repoOverridden, "g/overridden", block), // waivable → downgraded by the override
			repoReport(repoClean, "g/clean", warn),
		},
	})

	// Owner 2 (a different user): a blocked repo — proves the list is cross-user.
	owner2 := store.User{ID: uuid.New(), Email: fmt.Sprintf("owner2-%s@e2e", uuid.NewString()[:8])}
	mustExecT(ctx, t, f.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner2.ID, owner2.Email)
	conn2 := f.addConn(ctx, t, owner2.ID, "https://forge2.example")
	repoOther := f.addRepoRow(ctx, t, conn2, 9101, "h/other", "", uuid.Nil)
	f.stampReport(ctx, t, conn2, "violations", privcheck.Report{
		CheckedAt: time.Now().UTC(),
		Status:    privcheck.StatusViolations,
		Repos:     []privcheck.RepoReport{repoReport(repoOther, "h/other", block)},
	})

	// A never-checked connection (privilege_status NULL): it must flip checks_unknown so
	// the UI says "unknown", not "none" (R1). Its repo carries an OVERRIDE, so it is kept
	// in the action set even though its report is nil (never swept) — the case that must
	// still serialize block_messages as [] (never null), asserted below.
	conn3 := f.addConn(ctx, t, f.owner.ID, "https://forge3.example")
	repoUnknownOverridden := f.addRepoRow(ctx, t, conn3, 9201, "g/unknown-overridden", "allowed before any sweep", admin.ID)

	// Admin GET.
	r := httptest.NewRequest(http.MethodGet, "/admin/blocked-repos", nil)
	r = r.WithContext(mw.ContextWithUser(r.Context(), admin))
	w := httptest.NewRecorder()
	f.h.AdminListBlockedRepos(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp apitypes.AdminBlockedReposDTO
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}

	byID := map[string]apitypes.BlockedRepoDTO{}
	for _, d := range resp.Repos {
		byID[d.ID] = d
	}

	// Clean repo omitted.
	if _, ok := byID[repoClean.String()]; ok {
		t.Errorf("a clean, non-overridden repo must NOT appear in the blocked list")
	}
	// The never-checked repo is omitted from the rows...
	if _, ok := byID[repoOther.String()]; !ok {
		t.Errorf("owner2's blocked repo must appear (cross-user list)")
	}
	// ...but its connection flips the unknown flag.
	if !resp.ChecksUnknown {
		t.Errorf("checks_unknown must be true when a connection was never checked (R1)")
	}

	// Blocked repo: Blocked=true, block message present, no override.
	if d, ok := byID[repoBlocked.String()]; !ok {
		t.Errorf("blocked repo must appear")
	} else {
		if !d.Blocked {
			t.Errorf("blocked repo Blocked = false, want true")
		}
		if len(d.BlockMessages) == 0 {
			t.Errorf("blocked repo must carry block messages")
		}
		if d.Override != nil {
			t.Errorf("blocked-not-overridden repo must have null override")
		}
		if d.OwnerEmail != f.owner.Email {
			t.Errorf("owner email = %q, want %q", d.OwnerEmail, f.owner.Email)
		}
	}

	// Overridden-clean repo: appears, Blocked=false, override metadata with the actor
	// email resolved.
	if d, ok := byID[repoOverridden.String()]; !ok {
		t.Errorf("overridden repo must appear (admin's action set)")
	} else {
		if d.Blocked {
			t.Errorf("overridden waivable repo Blocked = true, want false (override downgraded it)")
		}
		if d.Override == nil {
			t.Fatalf("overridden repo must carry override metadata")
		}
		if d.Override.By != admin.Email {
			t.Errorf("override.by = %q, want the resolved actor email %q", d.Override.By, admin.Email)
		}
		if d.Override.Reason != "forge fix scheduled" {
			t.Errorf("override.reason = %q", d.Override.Reason)
		}
	}

	// Cross-user repo carries the other owner's email.
	if d := byID[repoOther.String()]; d.OwnerEmail != owner2.Email {
		t.Errorf("cross-user repo owner email = %q, want %q", d.OwnerEmail, owner2.Email)
	}

	// Overridden-but-never-swept repo: kept (overridden), Blocked=false (no report), and
	// block_messages MUST be [] — never null. A null here (msgs left nil because rep was
	// nil) would contradict the BlockedRepoDTO "Never null" contract and the non-nullable
	// TS string[]. Decoding [] yields a non-nil empty slice; null decodes to nil.
	if d, ok := byID[repoUnknownOverridden.String()]; !ok {
		t.Errorf("an overridden repo must appear even when its connection was never swept")
	} else {
		if d.Blocked {
			t.Errorf("never-swept overridden repo Blocked = true, want false (no report)")
		}
		if d.BlockMessages == nil {
			t.Errorf("block_messages must never be null; want [] for an overridden never-swept repo")
		}
		if len(d.BlockMessages) != 0 {
			t.Errorf("never-swept overridden repo block_messages = %v, want empty", d.BlockMessages)
		}
	}

	// A member is refused by the RequireAdminRO gate the read group mounts this under.
	rm := httptest.NewRequest(http.MethodGet, "/admin/blocked-repos", nil)
	rm = rm.WithContext(mw.ContextWithUser(rm.Context(), f.owner)) // fixture owner is a member
	wm := httptest.NewRecorder()
	mw.RequireAdminRO(http.HandlerFunc(f.h.AdminListBlockedRepos)).ServeHTTP(wm, rm)
	if wm.Code != http.StatusForbidden {
		t.Errorf("member GET status = %d, want 403", wm.Code)
	}
}
