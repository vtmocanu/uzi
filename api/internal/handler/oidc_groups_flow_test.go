package handler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #55 M3 — the exhaustive group gate + admin-sync callback matrix, driven through
// the REAL OIDCLogin -> OIDCCallback flow against a live Postgres + signing fake IdP
// (skipped unless UZI_TEST_DATABASE_URL is set, like the rest of oidc_flow_integration_test.go).
// The pure matcher semantics live in oidc_groups_test.go (no DB).

// oidcGroupsCfg is oidcLiveCfg with group mapping configured. GroupsClaim mirrors the
// signingIDP default ("groups") so the provider parses what the IdP emits.
func oidcGroupsCfg(admin, allowed []string) config.Config {
	c := oidcLiveCfg()
	c.OIDCGroupsClaim = "groups"
	c.OIDCAdminGroups = admin
	c.OIDCAllowedGroups = allowed
	return c
}

// seedOIDCUser inserts a subject-matched OIDC user (bound to f.issuer) with a chosen
// is_admin, so the subject-match resolve path (and its admin sync) can be exercised.
func seedOIDCUser(t *testing.T, pool *pgxpool.Pool, f *signingIDP, email, sub string, isAdmin bool) store.User {
	t.Helper()
	u, err := store.New(pool).CreateUserOIDC(context.Background(), store.CreateUserOIDCParams{
		Email: email, IsAdmin: isAdmin, OidcIssuer: oidcTxt(f.issuer), OidcSubject: oidcTxt(sub),
	})
	if err != nil {
		t.Fatalf("seed oidc user: %v", err)
	}
	return u
}

func reloadUser(t *testing.T, pool *pgxpool.Pool, u store.User) store.User {
	t.Helper()
	got, err := store.New(pool).GetUserByID(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return got
}

// captureOIDCLogs redirects the default slog logger to a buffer for the duration of
// the test, so the group grant/demote/fail-safe log assertions can inspect output.
func captureOIDCLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func assertNoUser(t *testing.T, pool *pgxpool.Pool, email string) {
	t.Helper()
	if _, err := store.New(pool).GetUserByEmail(context.Background(), email); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("a rejected login must not create/keep a user for %q (err=%v)", email, err)
	}
}

// --- allowlist gate: existing user, member vs non-member -------------------

func TestOIDCGateExistingUserLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	t.Run("member passes", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		sub := "sub-gate-mem-" + oidcUniq(t)
		email := "gate-mem-" + oidcUniq(t) + "@example.com"
		u := seedOIDCUser(t, pool, f, email, sub, false)
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{"staff", "eng"}

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		if got := reloadUser(t, pool, u); !got.LastLogin.Valid {
			t.Error("a successful gated login should record last_login")
		}
	})

	t.Run("non-member rejected, row untouched", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		sub := "sub-gate-non-" + oidcUniq(t)
		email := "gate-non-" + oidcUniq(t) + "@example.com"
		u := seedOIDCUser(t, pool, f, email, sub, false)
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{"contractors"}

		rec := callbackFor(t, h, f, true)
		assertErrorRedirect(t, rec, "oidc_forbidden")
		assertNoAuthCookie(t, rec)
		// Gate rejects BEFORE any DB read/write: last_login must stay NULL.
		if got := reloadUser(t, pool, u); got.LastLogin.Valid {
			t.Error("a gate-rejected login touched the user row (last_login set); the gate must run before any DB write")
		}
	})
}

// --- allowlist gate: JIT member / non-member / absent-claim ----------------

func TestOIDCGateJITLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	t.Run("member JIT-provisions", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		f.sub = "sub-jit-mem-" + oidcUniq(t)
		f.email = "jit-mem-" + oidcUniq(t) + "@example.com"
		f.emailVerified = true
		f.groupsSet, f.groups = true, []any{"staff"}

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		if _, err := store.New(pool).GetUserByEmail(context.Background(), f.email); err != nil {
			t.Errorf("an allowed-group member must JIT-provision: %v", err)
		}
	})

	t.Run("non-member refused, no user", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		f.sub = "sub-jit-non-" + oidcUniq(t)
		f.email = "jit-non-" + oidcUniq(t) + "@example.com"
		f.emailVerified = true
		f.groupsSet, f.groups = true, []any{"contractors"}

		rec := callbackFor(t, h, f, true)
		assertErrorRedirect(t, rec, "oidc_forbidden")
		assertNoAuthCookie(t, rec)
		assertNoUser(t, pool, f.email)
	})

	t.Run("gated + absent claim refuses new user (fail-safe), warns", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		f.sub = "sub-jit-abs-" + oidcUniq(t)
		f.email = "jit-abs-" + oidcUniq(t) + "@example.com"
		f.emailVerified = true
		f.groupsSet = false // claim absent

		logs := captureOIDCLogs(t)
		rec := callbackFor(t, h, f, true)
		assertErrorRedirect(t, rec, "oidc_forbidden")
		assertNoAuthCookie(t, rec)
		assertNoUser(t, pool, f.email)
		if !strings.Contains(logs.String(), "groups claim absent/unparseable") {
			t.Error("expected the absent-claim fail-safe warn to be logged")
		}
	})
}

// --- admin sync: grant / demote --------------------------------------------

func TestOIDCAdminGrantLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, nil))
	sub := "sub-grant-" + oidcUniq(t)
	email := "grant-" + oidcUniq(t) + "@example.com"
	u := seedOIDCUser(t, pool, f, email, sub, false) // starts non-admin
	f.sub, f.email, f.emailVerified = sub, email, true
	// The claimed set includes a group that is NOT configured; it must never appear in
	// the logs (only configured group names may be logged — decision 4/log-PII).
	f.groupsSet, f.groups = true, []any{"uzi-admins", "claimed-only-secret"}

	logs := captureOIDCLogs(t)
	assertLoggedInRedirect(t, callbackFor(t, h, f, true))
	if got := reloadUser(t, pool, u); !got.IsAdmin {
		t.Error("membership in an admin group must grant is_admin")
	}
	out := logs.String()
	if !strings.Contains(out, "grant") || !strings.Contains(out, "uzi-admins") {
		t.Errorf("grant log missing direction/configured group name: %s", out)
	}
	if strings.Contains(out, "claimed-only-secret") {
		t.Error("the user's CLAIMED (non-configured) group leaked into the logs; only configured names may be logged")
	}
}

func TestOIDCAdminDemoteLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, nil))
	sub := "sub-demote-" + oidcUniq(t)
	email := "demote-" + oidcUniq(t) + "@example.com"
	u := seedOIDCUser(t, pool, f, email, sub, true) // starts admin
	f.sub, f.email, f.emailVerified = sub, email, true
	f.groupsSet, f.groups = true, []any{"eng"} // present, not in admin group

	logs := captureOIDCLogs(t)
	assertLoggedInRedirect(t, callbackFor(t, h, f, true))
	if got := reloadUser(t, pool, u); got.IsAdmin {
		t.Error("loss of admin-group membership must demote is_admin (authoritative sync)")
	}
	if !strings.Contains(logs.String(), "demote") {
		t.Error("expected a demote direction in the sync log")
	}
}

// --- seed-admin exemption (demotion-only) ----------------------------------

func TestOIDCSeedAdminExemptionLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	t.Run("seed admin NOT demoted", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		email := "seed-" + oidcUniq(t) + "@example.com"
		cfg := oidcGroupsCfg([]string{"uzi-admins"}, nil)
		cfg.SeedEmail = email // break-glass identity
		h := oidcLiveHandlerWith(t, pool, f, cfg)
		sub := "sub-seedkeep-" + oidcUniq(t)
		u := seedOIDCUser(t, pool, f, email, sub, true) // admin
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{"eng"} // would demote a normal user

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		if got := reloadUser(t, pool, u); !got.IsAdmin {
			t.Error("the seed admin must be exempt from group demotion (break-glass)")
		}
	})

	t.Run("seed admin still promotable", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		email := "seedpromo-" + oidcUniq(t) + "@example.com"
		cfg := oidcGroupsCfg([]string{"uzi-admins"}, nil)
		cfg.SeedEmail = email
		h := oidcLiveHandlerWith(t, pool, f, cfg)
		sub := "sub-seedpromo-" + oidcUniq(t)
		u := seedOIDCUser(t, pool, f, email, sub, false) // starts non-admin
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{"uzi-admins"}

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		if got := reloadUser(t, pool, u); !got.IsAdmin {
			t.Error("the exemption is demotion-only; a seed admin in the admin group must still be promoted")
		}
	})
}

// TestOIDCEmptySeedEmailNoBlanketExemptionLiveDB: with SeedEmail unset (""), the
// exemption guard must NOT fire for a user whose email compares — a normal admin
// outside the group is demoted like anyone else.
func TestOIDCEmptySeedEmailNoBlanketExemptionLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	cfg := oidcGroupsCfg([]string{"uzi-admins"}, nil)
	cfg.SeedEmail = "" // seeding disabled
	h := oidcLiveHandlerWith(t, pool, f, cfg)
	sub := "sub-noseed-" + oidcUniq(t)
	email := "noseed-" + oidcUniq(t) + "@example.com"
	u := seedOIDCUser(t, pool, f, email, sub, true) // admin
	f.sub, f.email, f.emailVerified = sub, email, true
	f.groupsSet, f.groups = true, []any{"eng"} // not in admin group

	assertLoggedInRedirect(t, callbackFor(t, h, f, true))
	if got := reloadUser(t, pool, u); got.IsAdmin {
		t.Error("empty SeedEmail must not become a blanket demotion exemption; this admin should be demoted")
	}
}

// --- fail-safe: absent / malformed claim -----------------------------------

func TestOIDCFailSafeExistingUserLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	// Both admin AND allowed gating configured; the existing admin must keep their
	// role AND pass the gate when the claim is unusable.
	run := func(t *testing.T, setClaim bool, claim any) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, []string{"staff"}))
		sub := "sub-failsafe-" + oidcUniq(t)
		email := "failsafe-" + oidcUniq(t) + "@example.com"
		u := seedOIDCUser(t, pool, f, email, sub, true) // admin
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = setClaim, claim

		logs := captureOIDCLogs(t)
		assertLoggedInRedirect(t, callbackFor(t, h, f, true)) // passes the gate (fail-safe)
		if got := reloadUser(t, pool, u); !got.IsAdmin {
			t.Error("an unusable claim must NOT demote an existing admin (fail-safe)")
		}
		out := logs.String()
		if !strings.Contains(out, "groups claim absent/unparseable") {
			t.Error("expected the per-login absent/unparseable warn")
		}
		if !strings.Contains(out, "groups_claim=groups") {
			t.Error("the warn should log the configured claim NAME")
		}
	}

	t.Run("claim absent", func(t *testing.T) { run(t, false, nil) })
	t.Run("claim malformed string", func(t *testing.T) { run(t, true, "uzi-admins") })
}

// --- present-but-empty [] is authoritative (demotes / gates) ---------------

func TestOIDCPresentEmptyArrayLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	t.Run("empty array demotes", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, nil))
		sub := "sub-empty-demote-" + oidcUniq(t)
		email := "empty-demote-" + oidcUniq(t) + "@example.com"
		u := seedOIDCUser(t, pool, f, email, sub, true)
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{} // present, empty = authoritative no membership

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		if got := reloadUser(t, pool, u); got.IsAdmin {
			t.Error("a present-but-empty groups array is authoritative and must demote")
		}
	})

	t.Run("empty array gates", func(t *testing.T) {
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
		sub := "sub-empty-gate-" + oidcUniq(t)
		email := "empty-gate-" + oidcUniq(t) + "@example.com"
		u := seedOIDCUser(t, pool, f, email, sub, false)
		f.sub, f.email, f.emailVerified = sub, email, true
		f.groupsSet, f.groups = true, []any{}

		rec := callbackFor(t, h, f, true)
		assertErrorRedirect(t, rec, "oidc_forbidden")
		assertNoAuthCookie(t, rec)
		if got := reloadUser(t, pool, u); got.LastLogin.Valid {
			t.Error("a present-but-empty array must fail the gate before any DB write")
		}
	})
}

// TestOIDCEmptyStringGroupGatesNothingLiveDB: a claim like ["a", ""] (from ["a", null])
// must not satisfy a gate it does not really intersect — the end-to-end complement to
// the pure matcher cases in oidc_groups_test.go.
func TestOIDCEmptyStringGroupGatesNothingLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg(nil, []string{"staff"}))
	f.sub = "sub-emptystr-" + oidcUniq(t)
	f.email = "emptystr-" + oidcUniq(t) + "@example.com"
	f.emailVerified = true
	f.groupsSet, f.groups = true, []any{"a", ""} // neither "a" nor "" is in {"staff"}

	rec := callbackFor(t, h, f, true)
	assertErrorRedirect(t, rec, "oidc_forbidden")
	assertNoAuthCookie(t, rec)
	assertNoUser(t, pool, f.email)
}

// --- dormant when unset ----------------------------------------------------

// TestOIDCGroupsDormantWhenUnsetLiveDB: with neither group var set, the provider may
// still parse a groups claim, but the handler's gate + admin sync are fully inert —
// an admin-group claim does NOT flip a stored role.
func TestOIDCGroupsDormantWhenUnsetLiveDB(t *testing.T) {
	pool := oidcLivePool(t)
	f := newSigningIDP(t)
	defer f.srv.Close()
	h := oidcLiveHandler(t, pool, f) // default oidcLiveCfg: no group vars
	sub := "sub-dormant-" + oidcUniq(t)
	email := "dormant-" + oidcUniq(t) + "@example.com"
	u := seedOIDCUser(t, pool, f, email, sub, false)
	f.sub, f.email, f.emailVerified = sub, email, true
	f.groupsSet, f.groups = true, []any{"uzi-admins"} // IdP sends groups; handler must ignore

	assertLoggedInRedirect(t, callbackFor(t, h, f, true))
	if got := reloadUser(t, pool, u); got.IsAdmin {
		t.Error("group mapping is unset; an admin-group claim must NOT grant is_admin")
	}
}

// --- first-user admin gating when AdminGroups configured -------------------

func TestOIDCFirstUserWithAdminGroupsLiveDB(t *testing.T) {
	pool := oidcLivePool(t)

	t.Run("first user NOT admin when not in admin group", func(t *testing.T) {
		resetUsers(t, pool)
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, nil))
		f.sub = "sub-first-non-" + oidcUniq(t)
		f.email = "first-non-" + oidcUniq(t) + "@example.com"
		f.emailVerified = true
		f.groupsSet, f.groups = true, []any{"eng"} // not in the admin group

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		got, err := store.New(pool).GetUserByEmail(context.Background(), f.email)
		if err != nil {
			t.Fatalf("first user not found: %v", err)
		}
		if got.IsAdmin {
			t.Error("with AdminGroups configured the count==0 first-admin rule is disabled; a non-member first user must NOT be admin")
		}
	})

	t.Run("first user IS admin when in admin group", func(t *testing.T) {
		resetUsers(t, pool)
		f := newSigningIDP(t)
		defer f.srv.Close()
		h := oidcLiveHandlerWith(t, pool, f, oidcGroupsCfg([]string{"uzi-admins"}, nil))
		f.sub = "sub-first-adm-" + oidcUniq(t)
		f.email = "first-adm-" + oidcUniq(t) + "@example.com"
		f.emailVerified = true
		f.groupsSet, f.groups = true, []any{"uzi-admins"}

		assertLoggedInRedirect(t, callbackFor(t, h, f, true))
		got, err := store.New(pool).GetUserByEmail(context.Background(), f.email)
		if err != nil {
			t.Fatalf("first user not found: %v", err)
		}
		if !got.IsAdmin {
			t.Error("a first user in the admin group must be admin at creation")
		}
	})
}
