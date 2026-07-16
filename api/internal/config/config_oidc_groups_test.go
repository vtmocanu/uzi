package config

import (
	"reflect"
	"testing"
)

// oidcEnabledEnv layers a valid OIDC config on top of oidcBaseEnv so the group
// subtests can exercise the enabled path.
func oidcEnabledEnv(t *testing.T) {
	t.Helper()
	oidcBaseEnv(t)
	t.Setenv("UZI_OIDC_ISSUER_URL", "https://idp.example.com/realms/uzi")
	t.Setenv("UZI_OIDC_CLIENT_ID", "uzi-client")
	t.Setenv("UZI_OIDC_CLIENT_SECRET", "s3cr3t")
}

// TestOIDCGroupsDormantByDefault: with OIDC on but no group vars, the claim name
// defaults to "groups" and both group lists are empty (feature dormant).
func TestOIDCGroupsDormantByDefault(t *testing.T) {
	oidcEnabledEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.OIDCGroupsClaim != "groups" {
		t.Errorf("OIDCGroupsClaim = %q, want default %q", cfg.OIDCGroupsClaim, "groups")
	}
	if len(cfg.OIDCAdminGroups) != 0 {
		t.Errorf("OIDCAdminGroups = %v, want empty", cfg.OIDCAdminGroups)
	}
	if len(cfg.OIDCAllowedGroups) != 0 {
		t.Errorf("OIDCAllowedGroups = %v, want empty", cfg.OIDCAllowedGroups)
	}
}

// TestOIDCGroupsUnsetWhenOIDCOff: with OIDC fully unconfigured and no group vars,
// nothing group-derived is populated (fully dormant, Decision 7).
func TestOIDCGroupsUnsetWhenOIDCOff(t *testing.T) {
	oidcBaseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.OIDCGroupsClaim != "" {
		t.Errorf("OIDCGroupsClaim = %q, want empty when OIDC off", cfg.OIDCGroupsClaim)
	}
	if len(cfg.OIDCAdminGroups) != 0 || len(cfg.OIDCAllowedGroups) != 0 {
		t.Errorf("group lists populated with OIDC off: admin=%v allowed=%v", cfg.OIDCAdminGroups, cfg.OIDCAllowedGroups)
	}
}

// TestOIDCGroupsRefuseWhenOIDCOff: a GATING var (admin/allowed groups) set while OIDC
// is unconfigured refuses to start (Decision 7 all-or-nothing posture). The claim-name
// knob does NOT arm the guard — see TestOIDCGroupsClaimNameAloneDormantWhenOIDCOff.
func TestOIDCGroupsRefuseWhenOIDCOff(t *testing.T) {
	cases := map[string]map[string]string{
		"admin groups only":   {"UZI_OIDC_ADMIN_GROUPS": "uzi-admins"},
		"allowed groups only": {"UZI_OIDC_ALLOWED_GROUPS": "uzi-users"},
		"gating + claim name": {"UZI_OIDC_GROUPS_CLAIM": "roles", "UZI_OIDC_ADMIN_GROUPS": "a", "UZI_OIDC_ALLOWED_GROUPS": "b"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			oidcBaseEnv(t)
			for k, v := range env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Errorf("Load() = nil error for gating var set with OIDC off (%v), want refuse-to-start", env)
			}
		})
	}
}

// TestOIDCGroupsClaimNameAloneDormantWhenOIDCOff: UZI_OIDC_GROUPS_CLAIM is a format
// knob that ships as a compose/.env.example default ("groups"); with OIDC off and no
// gating groups it must boot clean and dormant, NOT trip the refuse-to-start guard —
// otherwise the default password-login stack would refuse to boot (regression fix).
func TestOIDCGroupsClaimNameAloneDormantWhenOIDCOff(t *testing.T) {
	for _, claim := range []string{"groups", "roles"} {
		t.Run(claim, func(t *testing.T) {
			oidcBaseEnv(t)
			t.Setenv("UZI_OIDC_GROUPS_CLAIM", claim)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() rejected a bare UZI_OIDC_GROUPS_CLAIM=%q with OIDC off: %v", claim, err)
			}
			// Dormant: OIDC off leaves nothing group-derived populated.
			if cfg.OIDCGroupsClaim != "" {
				t.Errorf("OIDCGroupsClaim = %q, want empty when OIDC off", cfg.OIDCGroupsClaim)
			}
			if len(cfg.OIDCAdminGroups) != 0 || len(cfg.OIDCAllowedGroups) != 0 {
				t.Errorf("group lists populated with OIDC off: admin=%v allowed=%v", cfg.OIDCAdminGroups, cfg.OIDCAllowedGroups)
			}
		})
	}
}

// TestOIDCGroupsEmptyVarsWhenOIDCOffAllowed: group vars present but resolving to
// empty (blank, comma-only) are treated as unset and do NOT block boot with OIDC off.
func TestOIDCGroupsEmptyVarsWhenOIDCOffAllowed(t *testing.T) {
	oidcBaseEnv(t)
	t.Setenv("UZI_OIDC_ADMIN_GROUPS", " , , ")
	t.Setenv("UZI_OIDC_ALLOWED_GROUPS", "")
	if _, err := Load(); err != nil {
		t.Errorf("Load() rejected empty-resolving group vars with OIDC off: %v", err)
	}
}

// TestOIDCGroupsParseTrimDedup: comma-separated lists are trimmed, de-duped,
// empties dropped, first-seen order preserved, and NOT lowercased (case-sensitive).
func TestOIDCGroupsParseTrimDedup(t *testing.T) {
	oidcEnabledEnv(t)
	t.Setenv("UZI_OIDC_GROUPS_CLAIM", "  roles  ")
	t.Setenv("UZI_OIDC_ADMIN_GROUPS", " uzi-Admins , ops ,, uzi-Admins , platform ")
	t.Setenv("UZI_OIDC_ALLOWED_GROUPS", "staff")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.OIDCGroupsClaim != "roles" {
		t.Errorf("OIDCGroupsClaim = %q, want %q (trimmed)", cfg.OIDCGroupsClaim, "roles")
	}
	if want := []string{"uzi-Admins", "ops", "platform"}; !reflect.DeepEqual(cfg.OIDCAdminGroups, want) {
		t.Errorf("OIDCAdminGroups = %v, want %v", cfg.OIDCAdminGroups, want)
	}
	if want := []string{"staff"}; !reflect.DeepEqual(cfg.OIDCAllowedGroups, want) {
		t.Errorf("OIDCAllowedGroups = %v, want %v", cfg.OIDCAllowedGroups, want)
	}
}
