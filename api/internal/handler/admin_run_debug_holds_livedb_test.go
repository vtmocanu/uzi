package handler

// Issue #4198 M2 live-DB proofs for GET /api/recovery/holds?user=|?run=: the parameter
// validation, the own-scope paths, and the regression that ?user= is honoured for a live
// admin's admin_ro token (before this change it was silently ignored and the caller's own
// holds came back). The access matrix itself is TestAdminRunDebugReadMatrixLiveDB. No
// t.Parallel (the audit capture swaps a package variable).

import (
	"net/http"
	"strings"
	"testing"
)

// TestAdminRunDebugHoldsByUserLiveDB: flag on, admin_ro token, GET /api/recovery/holds?user=
// <other> returns the other user's holds, not the admin's own.
func TestAdminRunDebugHoldsByUserLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	rec := bearerReq(f.router, http.MethodGet, "/api/recovery/holds?user="+f.owner.String(), f.adminRO)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin_ro GET holds?user=owner = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	dto := adrdDecodeHolds(t, rec.Body.String())
	ids := adrdHoldIDs(dto)
	if !ids[f.hold.String()] || !ids[f.hold2.String()] || dto.OwnerID != f.owner.String() {
		t.Fatalf("holds = %v owner_id=%q, want the owner's holds %s %s for owner %s", ids, dto.OwnerID, f.hold, f.hold2, f.owner)
	}
	// The unparameterized call still returns the caller's own (here empty) list, unchanged.
	rec = bearerReq(f.router, http.MethodGet, "/api/recovery/holds", f.adminRO)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin_ro GET holds = %d, want 200", rec.Code)
	}
	own := adrdDecodeHolds(t, rec.Body.String())
	if len(own.Holds) != 0 || own.OwnerID != "" {
		t.Errorf("own holds = %+v, want empty with no owner_id", own)
	}
}

func TestAdminRunDebugHoldsParamValidationLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	audit := adrdCaptureAudit(t)
	both := "/api/recovery/holds?user=" + f.owner.String() + "&run=" + f.run.String()
	cases := []struct {
		name, path string
		want       int
	}{
		{"both user and run", both, http.StatusBadRequest},
		{"malformed user", "/api/recovery/holds?user=not-a-uuid", http.StatusBadRequest},
		{"malformed run", "/api/recovery/holds?run=not-a-uuid", http.StatusBadRequest},
		{"bad state still 400", "/api/recovery/holds?state=all&user=" + f.owner.String(), http.StatusBadRequest},
	}
	for _, c := range cases {
		for who, token := range map[string]string{"admin_ro": f.adminRO, "non-admin": f.otherUzc} {
			if rec := bearerReq(f.router, http.MethodGet, c.path, token); rec.Code != c.want {
				t.Errorf("%s: %s GET %s = %d, want %d\nbody: %s", c.name, who, c.path, rec.Code, c.want, rec.Body.String())
			}
		}
	}
	if recs := audit.take(t); len(recs) != 0 {
		t.Errorf("400s wrote %d audit records, want 0: %v", len(recs), recs)
	}
}

// TestAdminRunDebugHoldsOwnScopeLiveDB: a caller naming their own user or own run is
// served without the predicate and without an audit record, and ?state=open composes.
func TestAdminRunDebugHoldsOwnScopeLiveDB(t *testing.T) {
	f := adrdNewFixture(t)
	f.h.cfg.AdminRunDebug = false // own scope must not depend on the flag
	audit := adrdCaptureAudit(t)

	rec := bearerReq(f.router, http.MethodGet, "/api/recovery/holds?user="+f.owner.String()+"&state=open", f.ownerUzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET ?user=self = %d\nbody: %s", rec.Code, rec.Body.String())
	}
	dto := adrdDecodeHolds(t, rec.Body.String())
	if len(dto.Holds) != 2 || dto.OwnerID != f.owner.String() {
		t.Errorf("own ?user= = %+v, want both holds with owner_id %s", dto, f.owner)
	}

	rec = bearerReq(f.router, http.MethodGet, "/api/recovery/holds?run="+f.run2.String(), f.ownerUzc)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GET ?run=own = %d\nbody: %s", rec.Code, rec.Body.String())
	}
	dto = adrdDecodeHolds(t, rec.Body.String())
	ids := adrdHoldIDs(dto)
	if len(ids) != 1 || !ids[f.hold2.String()] {
		t.Errorf("own ?run= holds = %v, want only %s", ids, f.hold2)
	}
	if dto.Holds == nil {
		t.Errorf("holds is nil, want a non-nil slice")
	}

	// A run of the owner with no holds filters to an empty, non-null list on the wire.
	rec = bearerReq(f.router, http.MethodGet, "/api/recovery/holds?run="+f.judged.String(), f.ownerUzc)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"holds":[]`) {
		t.Errorf("own run without holds = %d %s, want 200 with \"holds\":[]", rec.Code, rec.Body.String())
	}
	if recs := audit.take(t); len(recs) != 0 {
		t.Errorf("own-scope reads wrote %d audit records, want 0: %v", len(recs), recs)
	}
}
