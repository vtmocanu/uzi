package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A nil forge service deliberately makes accidental transport/client construction
// visible as a test panic. All of these cases must stop before external work.
func TestGitHubAppConnectionRouteRejectsBeforeTransport(t *testing.T) {
	appMaterial := `{"auth_type":"github_app","app_id":"1","installation_id":2,"private_key":"app-private-key-canary","repositories":["aifactory"]}`
	for _, tc := range []struct {
		name, kind, token, extra string
		authorized               bool
		want                     int
		message                  string
	}{
		{"unauthenticated", "github", appMaterial, "", false, http.StatusUnauthorized, "authentication required"},
		{"gitlab", "gitlab", appMaterial, "", true, http.StatusBadRequest, "GitHub App connection is required"},
		{"forgejo", "forgejo", appMaterial, "", true, http.StatusBadRequest, "GitHub App connection is required"},
		{"missing_forge", "", appMaterial, "", true, http.StatusBadRequest, "GitHub App connection is required"},
		{"personal_token", "github", "personal-token-canary", "", true, http.StatusBadRequest, "GitHub App connection is required"},
		{"empty_token", "github", "", "", true, http.StatusBadRequest, "GitHub App connection is required"},
		{"unknown_schema_field", "github", appMaterial, `,"private_key":"body-private-key-canary"`, true, http.StatusBadRequest, "invalid request body"},
		{"app_still_requires_allowed_url", "github", appMaterial, "", true, http.StatusBadRequest, "base URL is not in the allowed forge list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := json.Marshal(tc.token)
			if err != nil {
				t.Fatal(err)
			}
			kind, err := json.Marshal(tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			body := `{"forge_type":` + string(kind) + `,"base_url":"https://github.com","token":` + string(token) + tc.extra + `}`
			req := httptest.NewRequest(http.MethodPost, "/api/forge/connections/github-app", strings.NewReader(body))
			if tc.authorized {
				req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
			}
			rr := httptest.NewRecorder()
			(&Handler{}).CreateConnection(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d, want=%d", rr.Code, tc.want)
			}
			if !strings.Contains(rr.Body.String(), tc.message) {
				t.Fatalf("unexpected fixed error: %s", rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "private-key-canary") || strings.Contains(rr.Body.String(), tc.token) && tc.token != "" {
				t.Fatal("credential material escaped in response")
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("App credential response can be cached")
			}
		})
	}
}

func TestLegacyForgeConnectionRouteRejectsAppEnvelopeBeforeTransport(t *testing.T) {
	material := `{"auth_type":"github_app","private_key":"legacy-private-key-canary"}`
	for _, kind := range []string{"github", "gitlab", "forgejo", ""} {
		t.Run(kind, func(t *testing.T) {
			body, err := json.Marshal(createConnectionRequest{ForgeType: kind, BaseURL: "https://github.com", Token: material})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/forge/connections/", strings.NewReader(string(body)))
			req = req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()}))
			rr := httptest.NewRecorder()
			(&Handler{}).CreateConnection(rr, req)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "use GitHub App connection endpoint") {
				t.Fatalf("legacy route did not reject App envelope before transport: %d %s", rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "legacy-private-key-canary") {
				t.Fatal("legacy route echoed private key material")
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("legacy App rejection can be cached")
			}
		})
	}
}
