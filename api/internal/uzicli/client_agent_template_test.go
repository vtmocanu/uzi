package uzicli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serveOnce(t *testing.T, status int, contentType, body string) (*httptest.Server, *string) {
	t.Helper()
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = r.Method + " " + r.URL.Path + " " + string(b)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &sent
}

func TestListMyAgentTemplatesKeepsEnvelope(t *testing.T) {
	env := `{"templates":[{"id":"a","name":"n","scope":"user","model":"m","codex_model":null,"customized":true,"editable":true}],"x":1}`
	srv, sent := serveOnce(t, 200, "application/json", env)
	res, err := newTestClient(srv).ListMyAgentTemplates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *sent != "GET /api/me/agent-templates " || string(res.Raw) != env || len(res.Templates) != 1 ||
		res.Templates[0].Model == nil || *res.Templates[0].Model != "m" || res.Templates[0].CodexModel != nil {
		t.Fatalf("sent=%q res=%+v", *sent, res)
	}
}

func TestListMyAgentTemplatesMalformed(t *testing.T) {
	for _, body := range []string{`{"templates":`, `{}`, `{"templates":null}`} {
		srv, _ := serveOnce(t, 200, "application/json", body)
		_, err := newTestClient(srv).ListMyAgentTemplates(context.Background())
		if ee := exitOf(t, err); ee.Code != ExitGeneric {
			t.Fatalf("%s: code %d", body, ee.Code)
		}
	}
}

func TestListMyAgentTemplatesOlderServer(t *testing.T) {
	for _, tc := range []struct {
		status int
		ct     string
		body   string
	}{{404, "text/plain; charset=utf-8", "404 page not found\n"}, {405, "", ""}} {
		srv, _ := serveOnce(t, tc.status, tc.ct, tc.body)
		_, err := newTestClient(srv).ListMyAgentTemplates(context.Background())
		if ee := exitOf(t, err); ee.Code != ExitGeneric || ee.Error() != errAgentTemplatesUnsupported {
			t.Fatalf("%d: %d %q", tc.status, ee.Code, ee.Error())
		}
	}
	srv, _ := serveOnce(t, 404, "application/json", `{"error":"gone"}`)
	_, err := newTestClient(srv).ListMyAgentTemplates(context.Background())
	if ee := exitOf(t, err); ee.Code != ExitNotFound {
		t.Fatalf("JSON 404: %d", ee.Code)
	}
}

func TestPutAgentTemplateModelPins(t *testing.T) {
	env := `{"template":{"id":"a/b","name":"n","scope":"user","model":null,"codex_model":"c","customized":true,"editable":true}}`
	srv, sent := serveOnce(t, 200, "application/json", env)
	c := "c"
	res, err := newTestClient(srv).PutAgentTemplateModelPins(context.Background(), "a/b", map[string]*string{"codex_model": &c, "model": nil})
	if err != nil {
		t.Fatal(err)
	}
	if *sent != `PUT /api/me/agent-templates/a/b/model-pins {"codex_model":"c","model":null}` && *sent != `PUT /api/me/agent-templates/a%2Fb/model-pins {"codex_model":"c","model":null}` {
		t.Fatalf("sent = %q", *sent)
	}
	if string(res.Raw) != env || res.Template.CodexModel == nil || *res.Template.CodexModel != "c" {
		t.Fatalf("res = %+v", res)
	}
}

func TestPutAgentTemplateModelPinsErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		ct     string
		body   string
		code   int
		msg    string
		reason string
	}{
		{"not editable", 403, "application/json", `{"error":"hostile \u001b[2J","reason":"template_not_editable"}`, ExitAuth,
			"builtin and global templates are edited in the web UI by an admin", ReasonTemplateNotEditable},
		{"commit unknown", 500, "application/json", `{"error":"rolled back","reason":"template_commit_unknown"}`, ExitUnreachable,
			"the save may or may not have been applied; check with `uzi agent-template show`", ReasonTemplateCommitUnknown},
		{"readback", 500, "application/json", `{"error":"x","reason":"template_saved_readback_failed"}`, ExitUnreachable,
			"template pins saved; reading them back failed", ReasonTemplateSavedReadbackFailed},
		{"json 404", 404, "application/json", `{"error":"template not found"}`, ExitNotFound, "template not found", ""},
		{"text 404", 404, "text/plain", "404 page not found\n", ExitNotFound, "not found", ""},
		{"empty 404", 404, "", "", ExitNotFound, "not found", ""},
		{"empty 405", 405, "", "", ExitGeneric, errAgentTemplatesUnsupported, ""},
		{"bad id 400", 400, "application/json", `{"error":"invalid template id"}`, ExitUsage, "invalid template id", ""},
		{"plain 403 keeps scope hint", 403, "application/json", `{"error":"forbidden"}`, ExitAuth, "forbidden: your token lacks the required scope (admin views need an admin-scoped token)", ""},
		{"malformed 200", 200, "application/json", `{}`, ExitGeneric, "malformed response from uzi (/api/me/agent-templates/a/model-pins)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serveOnce(t, tc.status, tc.ct, tc.body)
			_, err := newTestClient(srv).PutAgentTemplateModelPins(context.Background(), "a", map[string]*string{"model": nil})
			ee := exitOf(t, err)
			if ee.Code != tc.code || ee.Error() != tc.msg || ee.Reason != tc.reason {
				t.Fatalf("code=%d msg=%q reason=%q", ee.Code, ee.Error(), ee.Reason)
			}
		})
	}
}
