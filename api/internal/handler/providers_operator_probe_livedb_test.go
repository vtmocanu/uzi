package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #4540 M1: the admin Test of an organization key, through the real router against a loopback fake
// gateway. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. Tests that capture the
// audit log swap the global slog default, so none of them runs in parallel.

// testRoute is the admin Test route of a definition.
func testRoute(id string) string { return opPath(id, "test") }

// TestOperatorTestLiveDB: the admin Test dials the definition's gateway with the organization key,
// records evidence for that exact key, and shows it in the admin read.
func TestOperatorTestLiveDB(t *testing.T) {
	s := newOpFx(t)
	key := orgKeyFixture()
	s.mustPutOrgKey(t, s.op.ID, key)
	logs := captureProviderAudit(t)

	rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
	var out struct {
		Probe *apitypes.ProviderProbeDTO `json:"probe"`
	}
	if err := strictUnmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Probe == nil ||
		out.Probe.AuthResult != "ok" || out.Probe.ToolResult != "passed" || out.Probe.ProbedAt.IsZero() {
		t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
	}
	if auths := s.gw.seenAuth(); len(auths) != 2 || auths[0] != "Bearer "+key || auths[1] != "Bearer "+key {
		t.Errorf("gateway Authorization headers = %d, want two carrying the organization key", len(auths))
	}
	got := s.adminGetID(t, s.op.ID)
	if got.OperatorKey == nil || got.OperatorKey.Probe == nil || got.OperatorKey.Probe.AuthResult != "ok" || got.OperatorKey.Probe.ToolResult != "passed" {
		t.Fatalf("admin get after test = %+v", got.OperatorKey)
	}
	if ev := auditEvents(logs, "admin_test"); len(ev) != 1 || auditField(ev[0], "outcome") != "probed_recorded" ||
		auditField(ev[0], "auth_result") != "ok" || auditField(ev[0], "tool_result") != "passed" || auditField(ev[0], "revision") != "1" {
		t.Errorf("admin_test audit = %v", ev)
	}
	for _, leak := range []string{rec.Body.String(), logs.String(), got.Name} {
		if strings.Contains(leak, key) {
			t.Errorf("key material leaked into %q", leak)
		}
	}

	// A gateway that rejects the key is recorded as rejected, never as an error.
	s.gw.setMode("unauthorized")
	rec = s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
	if err := strictUnmarshal(rec.Body.Bytes(), &out); rec.Code != http.StatusOK || err != nil || out.Probe.AuthResult != "rejected" {
		t.Fatalf("rejected key test = %d %s", rec.Code, rec.Body.String())
	}
}

// TestOperatorTestCaptureRefusalsLiveDB: a Test that cannot start makes no request and writes no audit
// line; each refusal has its own status and message.
func TestOperatorTestCaptureRefusalsLiveDB(t *testing.T) {
	s := newOpFx(t)
	logs := captureProviderAudit(t)
	check := func(label string, id string, code int, msg string) {
		t.Helper()
		rec := s.adminDo(t, http.MethodPost, testRoute(id), "")
		if rec.Code != code || !strings.Contains(rec.Body.String(), msg) {
			t.Errorf("%s = %d %s, want %d containing %q", label, rec.Code, rec.Body.String(), code, msg)
		}
	}
	personal := s.f.create(t, s.personalBody("personal-"+uuid.NewString()[:6]))
	perUser := s.createShared(t, "peruser-"+uuid.NewString()[:6], "bearer")
	check("unknown id", uuid.NewString(), http.StatusNotFound, "provider not found")
	check("personal id", personal.ID, http.StatusNotFound, "provider not found")
	check("per-user-key definition", perUser.ID, http.StatusNotFound, "provider not found")
	check("keyless", s.op.ID, http.StatusConflict, "no organization key")
	s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
	if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID, "key", "enabled"), `{"enabled":false}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	check("disabled", s.op.ID, http.StatusConflict, "key is disabled")
	if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID, "key", "enabled"), `{"enabled":true}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := s.adminDo(t, http.MethodPost, opPath(s.op.ID, "retire"), ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	check("retired", s.op.ID, http.StatusConflict, "retired")

	if n := len(s.gw.seenAuth()); n != 0 {
		t.Errorf("a refused Test made %d gateway request(s)", n)
	}
	if ev := auditEvents(logs, "admin_test"); len(ev) != 0 {
		t.Errorf("a refused Test wrote audit lines: %v", ev)
	}
	if n := s.evidenceRows(t, s.op.ID); n != 0 {
		t.Errorf("a refused Test wrote %d evidence row(s)", n)
	}
}

// TestOperatorKeyWritesRequireCSRFAndAdminLiveDB: every new key write and the Test refuse a valid admin
// cookie with a missing or invalid CSRF token (403, no mutation, no audit, no dial) and succeed with a
// valid one; a uza_ token is 401 and a non-admin session is 403.
func TestOperatorKeyWritesRequireCSRFAndAdminLiveDB(t *testing.T) {
	type call struct {
		name, method, suffix, body string
		keyed                      bool // the definition holds a key beforehand
		okCode                     int
	}
	calls := []call{
		{"put key", http.MethodPut, "/key", `{"key":"` + orgKeyFixture() + `"}`, false, http.StatusOK},
		{"enable/disable", http.MethodPatch, "/key/enabled", `{"enabled":false}`, true, http.StatusOK},
		{"delete key", http.MethodDelete, "/key", "", true, http.StatusOK},
		{"test", http.MethodPost, "/test", "", true, http.StatusOK},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			s := newOpFx(t)
			logs := captureProviderAudit(t)
			if c.keyed {
				s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
			}
			path := opPath(s.op.ID) + c.suffix
			_, revBefore, _, hadKey := s.custody(t, s.op.ID)
			auditsBefore := strings.Count(logs.String(), "provider_audit")
			unchanged := func(label string) {
				t.Helper()
				_, rev, dis, ok := s.custody(t, s.op.ID)
				if ok != hadKey || rev != revBefore || dis {
					t.Errorf("%s: custody changed (present %v->%v, key_rev %d->%d, disabled %v)", label, hadKey, ok, revBefore, rev, dis)
				}
				if n := strings.Count(logs.String(), "provider_audit"); n != auditsBefore {
					t.Errorf("%s: wrote %d audit line(s)", label, n-auditsBefore)
				}
				if n := len(s.gw.seenAuth()); n != 0 {
					t.Errorf("%s: dialed the gateway %d time(s)", label, n)
				}
				if n := s.evidenceRows(t, s.op.ID); n != 0 {
					t.Errorf("%s: wrote evidence", label)
				}
			}
			bad := "AAAA.BBBB"
			for label, csrf := range map[string]*string{"missing CSRF": nil, "invalid CSRF": &bad} {
				rec := csrfCookieReq(t, s.f.router, c.method, path, s.admin.jwt, c.body, csrf)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "CSRF validation failed") {
					t.Errorf("%s: %d %s, want 403 CSRF validation failed", label, rec.Code, rec.Body.String())
				}
				unchanged(label)
			}
			// uza_ is 401 before the handler; a user token is refused; a non-admin session is 403.
			if rec := bearerReqBody(s.f.router, c.method, path, s.uza, c.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("uza_ = %d, want 401", rec.Code)
			}
			if rec := bearerReqBody(s.f.router, c.method, path, s.f.uzc, c.body); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Errorf("uzc_ = %d, want 401/403", rec.Code)
			}
			if rec := cookieReq(t, s.f.router, c.method, path, s.b.jwt, c.body); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin access required") {
				t.Errorf("non-admin = %d %s, want 403 admin access required", rec.Code, rec.Body.String())
			}
			unchanged("refused callers")
			// The same request with a valid token succeeds.
			if rec := cookieReq(t, s.f.router, c.method, path, s.admin.jwt, c.body); rec.Code != c.okCode {
				t.Errorf("valid request = %d %s, want %d", rec.Code, rec.Body.String(), c.okCode)
			}
		})
	}
}

// TestOperatorTestAuditOutcomesLiveDB: each outcome of an admitted Test is induced and recorded as
// exactly one admin_test line, with the gateway request count the step order implies, and no key.
func TestOperatorTestAuditOutcomesLiveDB(t *testing.T) {
	boom := errors.New("injected store failure")
	replaceKey := func(t *testing.T, s *opFx) func() {
		return func() { s.mustPutOrgKey(t, s.op.ID, orgKeyFixture()) }
	}
	toggle := func(t *testing.T, s *opFx, enabled string) {
		if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID, "key", "enabled"), `{"enabled":`+enabled+`}`); rec.Code != http.StatusOK {
			t.Errorf("toggle %s = %d", enabled, rec.Code)
		}
	}
	cases := []struct {
		name, outcome string
		code          int
		gateway       int
		evidence      int
		verdicts      bool
		setup         func(t *testing.T, s *opFx, hk *probeHooks)
	}{
		{"key unavailable", "key_unavailable", 409, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.beforeKeyRead = func() error { toggle(t, s, "false"); return nil }
		}},
		{"key read error", "key_read_error", 500, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.beforeKeyRead = func() error { return boom }
		}},
		{"open failed", "open_failed", 500, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			if _, err := s.f.pool.Exec(context.Background(),
				`UPDATE model_provider_operator_keys SET ciphertext = decode(repeat('ab', 48), 'hex') WHERE provider_id = $1`, s.op.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"recheck error", "recheck_error", 500, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.getProvider = func(call int) error {
				if call == 2 {
					return boom
				}
				return nil
			}
		}},
		{"changed before dial", "changed_before_dial", 409, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.afterKeyRead = replaceKey(t, s)
		}},
		{"disable then enable before the recheck", "changed_before_dial", 409, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.afterKeyRead = func() { toggle(t, s, "false"); toggle(t, s, "true") }
		}},
		{"refused not allowed", "refused_not_allowed", 400, 0, 0, false, func(t *testing.T, s *opFx, hk *probeHooks) {
			allowed := s.f.h.cfg.ProviderAllowedBaseURLs
			hk.afterKeyRead = func() { s.f.h.cfg.ProviderAllowedBaseURLs = nil }
			t.Cleanup(func() { s.f.h.cfg.ProviderAllowedBaseURLs = allowed })
		}},
		{"recorded", "probed_recorded", 200, 2, 1, true, func(t *testing.T, s *opFx, hk *probeHooks) {}},
		{"stale", "probed_stale", 409, 2, 0, true, func(t *testing.T, s *opFx, hk *probeHooks) {
			s.f.h.adminProbeAfterDial = replaceKey(t, s)
			t.Cleanup(func() { s.f.h.adminProbeAfterDial = nil })
		}},
		{"store error", "probed_store_error", 500, 2, 0, true, func(t *testing.T, s *opFx, hk *probeHooks) {
			hk.upsert = boom
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newOpFx(t)
			key := orgKeyFixture()
			s.mustPutOrgKey(t, s.op.ID, key)
			hk := wireOrgProbe(s.f.h)
			t.Cleanup(func() { s.f.h.adminProbeWrap = nil })
			c.setup(t, s, hk)
			logs := captureProviderAudit(t)

			rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
			if rec.Code != c.code {
				t.Errorf("status = %d %s, want %d", rec.Code, rec.Body.String(), c.code)
			}
			ev := auditEvents(logs, "admin_test")
			if len(ev) != 1 || auditField(ev[0], "outcome") != c.outcome {
				t.Fatalf("admin_test audit = %v, want exactly one with outcome=%s", ev, c.outcome)
			}
			if got := auditField(ev[0], "revision"); got != "1" {
				t.Errorf("audit revision = %q, want 1", got)
			}
			if (auditField(ev[0], "auth_result") != "") != c.verdicts {
				t.Errorf("audit verdicts present = %v, want %v: %s", auditField(ev[0], "auth_result") != "", c.verdicts, ev[0])
			}
			if n := len(s.gw.seenAuth()); n != c.gateway {
				t.Errorf("gateway requests = %d, want %d", n, c.gateway)
			}
			if n := s.evidenceRows(t, s.op.ID); n != c.evidence {
				t.Errorf("evidence rows = %d, want %d", n, c.evidence)
			}
			if strings.Contains(logs.String(), key) || strings.Contains(rec.Body.String(), key) {
				t.Error("the key reached the audit log or the response")
			}
		})
	}
}

// TestOperatorKeyTestEvidenceFenceLiveDB: a Test records evidence only for the exact tuple it ran
// against; every change that commits before the evidence locks wins, one that starts while they are
// held waits, and a newer probe's evidence is never overwritten by an older one.
func TestOperatorKeyTestEvidenceFenceLiveDB(t *testing.T) {
	// (i) a change committed between the dial and step 7.
	changes := []struct {
		name  string
		apply func(t *testing.T, s *opFx)
	}{
		{"key replace", func(t *testing.T, s *opFx) { s.mustPutOrgKey(t, s.op.ID, orgKeyFixture()) }},
		{"key disable", func(t *testing.T, s *opFx) {
			if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID, "key", "enabled"), `{"enabled":false}`); rec.Code != 200 {
				t.Error(rec.Body.String())
			}
		}},
		{"key disable then enable", func(t *testing.T, s *opFx) {
			for _, v := range []string{"false", "true"} {
				if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID, "key", "enabled"), `{"enabled":`+v+`}`); rec.Code != 200 {
					t.Error(rec.Body.String())
				}
			}
		}},
		{"key delete", func(t *testing.T, s *opFx) {
			if rec := s.adminDo(t, http.MethodDelete, opPath(s.op.ID, "key"), ""); rec.Code != 200 {
				t.Error(rec.Body.String())
			}
		}},
		{"model-map edit", func(t *testing.T, s *opFx) {
			if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID), `{"model_map":{"haiku":{"model":"h-fence"}}}`); rec.Code != 200 {
				t.Error(rec.Body.String())
			}
		}},
		{"retire", func(t *testing.T, s *opFx) {
			if rec := s.adminDo(t, http.MethodPost, opPath(s.op.ID, "retire"), ""); rec.Code != 200 {
				t.Error(rec.Body.String())
			}
		}},
	}
	for _, c := range changes {
		t.Run("committed after the dial: "+c.name, func(t *testing.T) {
			s := newOpFx(t)
			s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
			logs := captureProviderAudit(t)
			s.f.h.adminProbeAfterDial = func() { c.apply(t, s) }
			t.Cleanup(func() { s.f.h.adminProbeAfterDial = nil })
			rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
			if rec.Code != http.StatusConflict {
				t.Errorf("test = %d %s, want 409", rec.Code, rec.Body.String())
			}
			if n := s.evidenceRows(t, s.op.ID); n != 0 {
				t.Errorf("a late result wrote %d evidence row(s)", n)
			}
			if ev := auditEvents(logs, "admin_test"); len(ev) != 1 || auditField(ev[0], "outcome") != "probed_stale" {
				t.Errorf("audit = %v, want one probed_stale", ev)
			}
			if n := len(s.gw.seenAuth()); n != 2 {
				t.Errorf("gateway requests = %d, want the probe's two", n)
			}
		})
	}

	// (ii) a change that starts while step 7 holds its locks waits for it; once both commit the status
	// join hides the evidence.
	for _, c := range []struct {
		name, method, path, body string
	}{
		{"key replace", http.MethodPut, "/key", `{"key":"` + orgKeyFixture() + `"}`},
		{"retire", http.MethodPost, "/retire", ""},
	} {
		t.Run("started while the locks are held: "+c.name, func(t *testing.T) {
			s := newOpFx(t)
			s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
			logs := captureProviderAudit(t)
			hk := wireOrgProbe(s.f.h)
			t.Cleanup(func() { s.f.h.adminProbeWrap = nil })
			var change chan *httptest.ResponseRecorder
			hk.afterKeyLock = func() {
				change = blockedRequest(cookieReqPrepared(t, s.f.router, c.method, opPath(s.op.ID)+c.path, s.admin.jwt, c.body))
				assertBlocked(t, change, c.name)
				var waiting int
				if err := s.f.pool.QueryRow(context.Background(),
					`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%name: LockSharedModelProviderForEdit%'`).Scan(&waiting); err != nil || waiting < 1 {
					t.Errorf("no backend is waiting on the definition lock (count %d, %v)", waiting, err)
				}
			}
			rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("test = %d %s, want 200: the locks were taken first", rec.Code, rec.Body.String())
			}
			// The evidence was written, so the Test is recorded whatever the change did to it afterwards.
			var body map[string]apitypes.ProviderProbeDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["probe"].AuthResult != "ok" || body["probe"].ProbedAt.IsZero() {
				t.Errorf("response = %s (%v), want this probe's own result", rec.Body.String(), err)
			}
			if ev := auditEvents(logs, "admin_test"); len(ev) != 1 || auditField(ev[0], "outcome") != "probed_recorded" {
				t.Errorf("audit = %v, want one probed_recorded", ev)
			}
			if done := awaitDone(t, change, c.name); done.Code != http.StatusOK {
				t.Fatalf("%s = %d %s", c.name, done.Code, done.Body.String())
			}
			if got := s.adminGetID(t, s.op.ID); got.OperatorKey == nil || got.OperatorKey.Probe != nil {
				t.Errorf("after %s the admin read still shows evidence: %+v", c.name, got.OperatorKey)
			}
		})
	}

	t.Run("a newer tuple's evidence is preserved", func(t *testing.T) {
		s := newOpFx(t)
		s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
		logs := captureProviderAudit(t)
		_, router2 := secondHandler(t, s.f.h)
		s.f.h.adminProbeAfterDial = func() {
			// A is paused after its dial, holding h1's admission: the key is replaced (T2) and probe B
			// on a second API instance, with its own limiter, runs to completion at T2.
			s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
			if rec := cookieReq(t, router2, http.MethodPost, testRoute(s.op.ID), s.admin.jwt, ""); rec.Code != http.StatusOK {
				t.Errorf("probe B = %d %s", rec.Code, rec.Body.String())
			}
		}
		t.Cleanup(func() { s.f.h.adminProbeAfterDial = nil })
		rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("probe A = %d %s, want 409", rec.Code, rec.Body.String())
		}
		kid, rev, _, ok := s.custody(t, s.op.ID)
		var ek uuid.UUID
		var er int64
		var auth string
		if err := s.f.pool.QueryRow(context.Background(),
			`SELECT key_id, key_rev, auth_result FROM model_provider_operator_key_test_results WHERE provider_id = $1`, s.op.ID).Scan(&ek, &er, &auth); err != nil {
			t.Fatalf("no evidence row after B: %v", err)
		}
		if !ok || ek != kid || er != rev || auth != "ok" {
			t.Errorf("evidence = key %s rev %d %s, want B's tuple key %s rev %d ok", ek, er, auth, kid, rev)
		}
		outcomes := map[string]int{}
		for _, l := range auditEvents(logs, "admin_test") {
			outcomes[auditField(l, "outcome")]++
		}
		if outcomes["probed_recorded"] != 1 || outcomes["probed_stale"] != 1 || len(outcomes) != 2 {
			t.Errorf("outcomes = %v, want B recorded and A stale", outcomes)
		}
	})

	t.Run("a second request on the same handler is limited and not audited", func(t *testing.T) {
		s := newOpFx(t)
		s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
		logs := captureProviderAudit(t)
		s.f.h.adminProbeAfterDial = func() {
			rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), "")
			if rec.Code != http.StatusTooManyRequests || providerReason(t, rec) != reasonProviderProbeRateLimited || rec.Header().Get("Retry-After") == "" {
				t.Errorf("second request = %d %s retry-after %q, want 429 %s", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"), reasonProviderProbeRateLimited)
			}
		}
		t.Cleanup(func() { s.f.h.adminProbeAfterDial = nil })
		if rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), ""); rec.Code != http.StatusOK {
			t.Fatalf("probe A = %d %s", rec.Code, rec.Body.String())
		}
		if ev := auditEvents(logs, "admin_test"); len(ev) != 1 {
			t.Errorf("admin_test audit lines = %d, want 1 (a refused admission is not audited): %v", len(ev), ev)
		}
		if n := len(s.gw.seenAuth()); n != 2 {
			t.Errorf("gateway requests = %d, want only A's two", n)
		}
	})

	t.Run("a rename between the dial and the evidence still records it", func(t *testing.T) {
		s := newOpFx(t)
		s.mustPutOrgKey(t, s.op.ID, orgKeyFixture())
		logs := captureProviderAudit(t)
		s.f.h.adminProbeAfterDial = func() {
			if rec := s.adminDo(t, http.MethodPatch, opPath(s.op.ID), `{"name":"renamed-`+uuid.NewString()[:6]+`"}`); rec.Code != http.StatusOK {
				t.Errorf("rename = %d", rec.Code)
			}
		}
		t.Cleanup(func() { s.f.h.adminProbeAfterDial = nil })
		if rec := s.adminDo(t, http.MethodPost, testRoute(s.op.ID), ""); rec.Code != http.StatusOK {
			t.Fatalf("test = %d %s", rec.Code, rec.Body.String())
		}
		if ev := auditEvents(logs, "admin_test"); len(ev) != 1 || auditField(ev[0], "outcome") != "probed_recorded" {
			t.Errorf("audit = %v, want probed_recorded", ev)
		}
		if got := s.adminGetID(t, s.op.ID); got.OperatorKey == nil || got.OperatorKey.Probe == nil {
			t.Error("a rename hid the evidence")
		}
	})
}

// countingServer counts the requests it receives and answers with handler.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// createOperatorAt creates an organization definition whose gateway is base (added to the allowlist).
func (s *opFx) createOperatorAt(t *testing.T, base string) apitypes.AdminSharedProviderDTO {
	t.Helper()
	s.f.h.cfg.ProviderAllowedBaseURLs = append(s.f.h.cfg.ProviderAllowedBaseURLs, base)
	body := strings.Replace(s.operatorBody("at-"+uuid.NewString()[:6], "bearer"), s.base, base, 1)
	rec := s.adminDo(t, http.MethodPost, adminProvidersPath, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create at %s = %d %s", base, rec.Code, rec.Body.String())
	}
	d := decodeAdminProvider(t, rec)
	s.cleanupShared(t, d.ID)
	return d
}

// TestOperatorTestFollowsNoRedirectLiveDB: a gateway that redirects gets exactly one request, and the
// Location target, which would receive the key and the body, gets none.
func TestOperatorTestFollowsNoRedirectLiveDB(t *testing.T) {
	s := newOpFx(t)
	target, targetHits := countingServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	gateway, gatewayHits := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
	})
	d := s.createOperatorAt(t, gateway.URL+"/anthropic")
	s.mustPutOrgKey(t, d.ID, orgKeyFixture())
	rec := s.adminDo(t, http.MethodPost, testRoute(d.ID), "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"auth_result":"inconclusive"`) {
		t.Fatalf("test = %d %s, want 200 with an inconclusive verdict", rec.Code, rec.Body.String())
	}
	if g, x := gatewayHits.Load(), targetHits.Load(); g != 1 || x != 0 {
		t.Errorf("gateway requests = %d, redirect target requests = %d, want 1 and 0", g, x)
	}
}

// TestOperatorTestSurvivesAShortWriteTimeoutLiveDB: the listener's WriteTimeout is shorter than two
// probe steps against a slow gateway that is still inside the probe budget; the handler extends the
// write deadline, so the verdict still reaches the client.
func TestOperatorTestSurvivesAShortWriteTimeoutLiveDB(t *testing.T) {
	s := newOpFx(t)
	slow, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/v1/messages") {
			_, _ = w.Write([]byte(probeOKMessage))
		}
	})
	d := s.createOperatorAt(t, slow.URL+"/anthropic")
	s.mustPutOrgKey(t, d.ID, orgKeyFixture())

	srv := httptest.NewUnstartedServer(s.f.router)
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+testRoute(d.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: s.admin.jwt}) //nolint:gosec // G124: test-only client cookie.
	req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, s.admin.jwt))
	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("the response never arrived (WriteTimeout not extended?): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"auth_result"`) {
		t.Fatalf("test = %d %s", resp.StatusCode, body)
	}
	if took := time.Since(start); took < 700*time.Millisecond {
		t.Errorf("the test took %v: the slow gateway was not exercised past the WriteTimeout", took)
	}
}

// opTrace records every statement of a pool with its arguments.
type opTrace struct {
	mu   sync.Mutex
	sqls []string
	args [][]any
}

func (o *opTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sqls = append(o.sqls, data.SQL)
	o.args = append(o.args, append([]any(nil), data.Args...))
	return ctx
}

func (o *opTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TestOperatorKeyPlaintextAbsentLiveDB: through a full life of an organization key (set, Test, replace,
// disable, enable, Test, delete) the plaintext appears in no response, log line, query argument or
// database row, and the custody insert carries ciphertext only.
func TestOperatorKeyPlaintextAbsentLiveDB(t *testing.T) {
	trace := &opTrace{}
	h, router, pool := tracedHandler(t, trace)
	gw := newFakeProviderGateway(t)
	base := gw.srv.URL + "/anthropic"
	h.cfg.ProviderAllowedBaseURLs = []string{base}
	h.cfg.ProviderAllowLoopback = true
	adminID := cliSeedUser(t, pool, true)
	jwt := cliMintJWT(t, pool, adminID)
	logs := captureProviderAudit(t)

	var responses []string
	do := func(method, path, body string, want int) string {
		t.Helper()
		rec := cookieReq(t, router, method, path, jwt, body)
		responses = append(responses, rec.Body.String())
		if rec.Code != want {
			t.Fatalf("%s %s = %d %s, want %d", method, path, rec.Code, rec.Body.String(), want)
		}
		return rec.Body.String()
	}
	create := `{"name":"plain-` + uuid.NewString()[:6] + `","credential_mode":"operator_key","acknowledge_recipient_trust":true,"base_url":"` + base + `",` +
		`"auth_style":"bearer","model_map":{"opus":{"model":"o"},"sonnet":{"model":"s"},"haiku":{"model":"h"}}}`
	var created apitypes.AdminSharedProviderDTO
	{
		rec := cookieReq(t, router, http.MethodPost, adminProvidersPath, jwt, create)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
		}
		created = decodeAdminProvider(t, rec)
		responses = append(responses, rec.Body.String())
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM model_providers WHERE id = $1`, created.ID)
	})

	key := orgKeyFixture()
	do(http.MethodPut, opPath(created.ID, "key"), `{"key":"`+key+`"}`, http.StatusOK)
	do(http.MethodPost, testRoute(created.ID), "", http.StatusOK)
	do(http.MethodPut, opPath(created.ID, "key"), `{"key":"`+key+`x"}`, http.StatusOK) // replace
	do(http.MethodPatch, opPath(created.ID, "key", "enabled"), `{"enabled":false}`, http.StatusOK)
	do(http.MethodPatch, opPath(created.ID, "key", "enabled"), `{"enabled":true}`, http.StatusOK)
	do(http.MethodPost, testRoute(created.ID), "", http.StatusOK)
	do(http.MethodGet, opPath(created.ID), "", http.StatusOK)
	do(http.MethodGet, adminProvidersPath, "", http.StatusOK)

	// The custody row and the evidence, read before the delete.
	var custody, evidence string
	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT row_to_json(k)::text, k.ciphertext FROM model_provider_operator_keys k WHERE provider_id = $1`, created.ID).Scan(&custody, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT row_to_json(e)::text FROM model_provider_operator_key_test_results e WHERE provider_id = $1`, created.ID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	do(http.MethodDelete, opPath(created.ID, "key"), "", http.StatusOK)

	needles := []string{key}
	for label, hay := range map[string]string{"responses": strings.Join(responses, "\n"), "log": logs.String(), "custody row": custody, "evidence row": evidence} {
		for _, n := range needles {
			if strings.Contains(hay, n) {
				t.Errorf("the key appears in the %s", label)
			}
		}
	}
	if bytes.Contains(ciphertext, []byte(key)) {
		t.Error("the stored ciphertext contains the plaintext key")
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	inserts := 0
	for i, sql := range trace.sqls {
		for j, a := range trace.args[i] {
			var rendered []byte
			switch v := a.(type) {
			case []byte:
				rendered = v
			case string:
				rendered = []byte(v)
			default:
				rendered = []byte(fmt.Sprint(v))
			}
			if bytes.Contains(rendered, []byte(key)) {
				t.Errorf("a query argument carries the plaintext key: %.60s arg %d", sql, j)
			}
		}
		for marker, ctIndex := range map[string]int{"-- name: InsertOperatorProviderKey ": 1, "-- name: ReplaceOperatorProviderKey ": 0} {
			if strings.HasPrefix(sql, marker) {
				inserts++
				if ct, ok := trace.args[i][ctIndex].([]byte); !ok || len(ct) <= len(key) {
					t.Errorf("%s carries %T at %d, want a sealed ciphertext", marker, trace.args[i][ctIndex], ctIndex)
				}
			}
		}
	}
	if inserts != 2 {
		t.Errorf("traced %d custody writes, want the insert and the replace", inserts)
	}
}

// tracedHandler is cliLiveDB over a pool with a query tracer installed.
func tracedHandler(t *testing.T, tracer pgx.QueryTracer) (*Handler, http.Handler, *pgxpool.Pool) {
	t.Helper()
	_, _, base := cliLiveDB(t) // migrates and skips without a database
	cfg := base.Config()
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{
		pool: pool, q: q, box: box, vault: vault.NewPostgres(box, pool),
		cfg:      config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour},
		wsvc:     workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
		settings: settings.New(&settingsStore{}, time.Minute),
		hub:      hub.New(),
	}
	h.wsvc.SetVault(h.vault)
	lim := mw.NewLimiter(100000, time.Minute, nil)
	return h, h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim), pool
}
