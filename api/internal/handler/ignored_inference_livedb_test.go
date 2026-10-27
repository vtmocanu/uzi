package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRunToDTOIgnoredInference(t *testing.T) {
	for _, caps := range [][]string{nil, {"jvm"}} {
		dto := runToDTO(store.Run{IgnoredInferredCapabilities: caps}, "normal", 0, 0, 0, dtoTestNow, 0)
		if dto.IgnoredInferredCapabilities == nil || len(dto.IgnoredInferredCapabilities) != len(caps) {
			t.Fatalf("captured policy not exposed as an array: %v", dto.IgnoredInferredCapabilities)
		}
		if len(caps) > 0 && dto.IgnoredInferredCapabilities[0] != "jvm" {
			t.Fatal(dto.IgnoredInferredCapabilities)
		}
	}
}

func TestPatchRepoIgnoredInferenceLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner, stranger, admin := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false), cliSeedUser(t, pool, true)
	repo := cliSeedOwnedRepo(t, pool, owner)
	call := func(user uuid.UUID, body string, csrf bool) *httptest.ResponseRecorder {
		jwt := cliMintJWT(t, pool, user)
		req := httptest.NewRequest(http.MethodPatch, "/api/repos/"+repo.String(), strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // Test session cookie.
		if csrf {
			req.Header.Set("X-CSRF-Token", cliCSRFHeader(t, jwt))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(owner, `{"ignored_inferred_capabilities":["jvm"]}`, false); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", rec.Code)
	}
	if rec := call(stranger, `{"ignored_inferred_capabilities":["docker"]}`, true); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign policy write: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(owner, `{"required_capabilities":["jvm"]}`, true); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	rec := call(owner, `{"ignored_inferred_capabilities":["jvm","unknown","jvm"]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("policy write: %d %s", rec.Code, rec.Body.String())
	}
	dto := decodeRepoDTO(t, rec)
	if !reflect.DeepEqual(dto.IgnoredInferredCapabilities, []string{"jvm"}) || !reflect.DeepEqual(dto.RequiredCapabilities, []string{"jvm"}) {
		t.Fatalf("policy/hint=%+v", dto)
	}
	var response struct {
		Warning string `json:"warning"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Warning, "remain required") || !strings.Contains(response.Warning, "jvm") {
		t.Fatalf("missing overlap note: %s", rec.Body.String())
	}
	if rec := call(owner, `{"ignored_inferred_capabilities":["jvm"],"required_capabilities":["jvm"]}`, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "remain required") {
		t.Fatalf("overlap must be accepted with a note: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(owner, `{"ignored_inferred_capabilities":[],"repo_devbox_opt_in":true}`, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed settings group: %d", rec.Code)
	}
	rec = call(admin, `{"ignored_inferred_capabilities":["docker"]}`, true)
	if rec.Code != http.StatusOK || !reflect.DeepEqual(decodeRepoDTO(t, rec).IgnoredInferredCapabilities, []string{"docker"}) {
		t.Fatalf("admin write: %d %s", rec.Code, rec.Body.String())
	}
	rec = call(owner, `{"ignored_inferred_capabilities":[]}`, true)
	if rec.Code != http.StatusOK || len(decodeRepoDTO(t, rec).IgnoredInferredCapabilities) != 0 {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	var stored []string
	if err := pool.QueryRow(t.Context(), "SELECT ignored_inferred_capabilities FROM repos WHERE id=$1", repo).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("policy not cleared: %v", stored)
	}
}
