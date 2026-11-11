package privcheck

import (
	"maps"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

func appTestPermissions() map[string]string {
	return map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read", "actions": "read", "checks": "read", "statuses": "read"}
}

func TestGitHubAppTokenPrivilegeEvaluationUsesPermissionsAndRenewableExpiry(t *testing.T) {
	clock := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	base := forge.TokenInfo{GitHubApp: true, AppPermissions: appTestPermissions(), Scopes: []string{"contents:write"}, Active: true, ExpiresAt: clock.Add(time.Hour)}
	report := evaluateToken(forge.TypeGitHub, base, false, clock, 14*24*time.Hour, "")
	if len(report.Violations) != 0 || len(report.Warnings) != 0 || report.ExpiresAt == nil || !report.ExpiresAt.Equal(base.ExpiresAt) {
		t.Fatalf("renewable App token incorrectly treated as classic PAT: %#v", report)
	}
	for name, mutate := range map[string]func(*forge.TokenInfo){
		"missing-permission": func(i *forge.TokenInfo) { delete(i.AppPermissions, "issues") },
		"read-only-content":  func(i *forge.TokenInfo) { i.AppPermissions["contents"] = "read" },
		"extra-admin-write":  func(i *forge.TokenInfo) { i.AppPermissions["administration"] = "write" },
		"inactive":           func(i *forge.TokenInfo) { i.Active = false },
		"expired":            func(i *forge.TokenInfo) { i.ExpiresAt = clock },
	} {
		t.Run(name, func(t *testing.T) {
			info := base
			info.AppPermissions = maps.Clone(base.AppPermissions)
			mutate(&info)
			r := evaluateToken(forge.TypeGitHub, info, false, clock, 14*24*time.Hour, "")
			if len(r.Violations) == 0 {
				t.Fatal("unsafe App credential passed privilege validation")
			}
		})
	}
	if r := evaluateToken(forge.TypeGitLab, base, false, clock, 14*24*time.Hour, ""); len(r.Violations) == 0 {
		t.Fatal("App permissions accepted for non-GitHub forge")
	}
	if r := evaluateToken(forge.TypeGitHub, base, true, clock, 14*24*time.Hour, ""); len(r.Violations) == 0 {
		t.Fatal("App marker bypassed existing admin prohibition")
	}
}

func TestGitHubClassicPATStillRequiresScopesAndWarnsBeforeExpiry(t *testing.T) {
	clock := time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	info := forge.TokenInfo{Scopes: []string{"repo"}, Active: true, ExpiresAt: clock.Add(time.Hour)}
	r := evaluateToken(forge.TypeGitHub, info, false, clock, 14*24*time.Hour, "")
	if len(r.Violations) != 0 || len(r.Warnings) != 1 {
		t.Fatalf("classic PAT expiry behavior changed: %#v", r)
	}
	info.Scopes = []string{"repo", "workflow"}
	if r := evaluateToken(forge.TypeGitHub, info, false, clock, 14*24*time.Hour, ""); len(r.Violations) == 0 {
		t.Fatal("classic PAT over-privilege check was weakened")
	}
	info.Scopes = nil
	info.AppPermissions = appTestPermissions()
	if r := evaluateToken(forge.TypeGitHub, info, false, clock, 14*24*time.Hour, ""); len(r.Violations) == 0 {
		t.Fatal("permission map without App credential marker bypassed PAT scope checks")
	}
}
