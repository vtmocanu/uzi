package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3264 M3 template authorization. Every case drives the real h.Routes() router through a
// Bearer (uzc_ or uza_) so the mount, the auth middleware and the handler are proven together.
// Skipped unless UZI_TEST_DATABASE_URL is set (./e2e/run-store-it.sh).

const (
	pinsToolsJSON  = `["Read","Grep"]`
	pinsDesc       = "pin test description"
	pinsPromptBody = "pin test prompt body\n"
)

// pinsSeedTemplate inserts a template row and returns its id. scope is builtin|global|user; owner
// is only used for user scope.
func pinsSeedTemplate(t *testing.T, pool *pgxpool.Pool, scope string, owner uuid.UUID, model, codex *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	name := "pins-" + id.String()
	switch scope {
	case "user":
		cliMustExec(t, pool, `INSERT INTO agent_templates (id, name, description, model, codex_model, tools, prompt_body, scope, user_id)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,'user',$8)`, id, name, pinsDesc, model, codex, pinsToolsJSON, pinsPromptBody, owner)
	case "global":
		cliMustExec(t, pool, `INSERT INTO agent_templates (id, name, description, model, codex_model, tools, prompt_body, scope)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,'global')`, id, name, pinsDesc, model, codex, pinsToolsJSON, pinsPromptBody)
	case "builtin":
		cliMustExec(t, pool, `INSERT INTO agent_templates (id, name, description, model, codex_model, tools, prompt_body, scope, is_builtin, origin)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,'builtin',true,'embedded')`, id, name, pinsDesc, model, codex, pinsToolsJSON, pinsPromptBody)
	default:
		t.Fatalf("unknown scope %q", scope)
	}
	return id
}

type pinsRow struct {
	model, codex         *string
	desc, tools, prompt  string
	customized           bool
	updatedBy            *uuid.UUID
	updatedAt, createdAt string
	name, scope, origin_ string
}

func pinsReadRow(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) pinsRow {
	t.Helper()
	var r pinsRow
	var origin *string
	if err := pool.QueryRow(context.Background(),
		`SELECT model, codex_model, description, tools::text, prompt_body, customized, updated_by, updated_at::text, created_at::text, name, scope, origin
		 FROM agent_templates WHERE id=$1`, id).
		Scan(&r.model, &r.codex, &r.desc, &r.tools, &r.prompt, &r.customized, &r.updatedBy, &r.updatedAt, &r.createdAt, &r.name, &r.scope, &origin); err != nil {
		t.Fatalf("read template row: %v", err)
	}
	if origin != nil {
		r.origin_ = *origin
	}
	return r
}

func pinStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// pinsAssertContentUnchanged checks the non-pin columns still hold the seeded values.
func pinsAssertContentUnchanged(t *testing.T, r pinsRow) {
	t.Helper()
	if r.desc != pinsDesc || r.prompt != pinsPromptBody || strings.ReplaceAll(r.tools, " ", "") != pinsToolsJSON {
		t.Fatalf("description/tools/prompt changed: %q %q %q", r.desc, r.tools, r.prompt)
	}
}

func pinsPut(router http.Handler, token string, id uuid.UUID, body string) *httptest.ResponseRecorder {
	return bearerReqBody(router, http.MethodPut, "/api/me/agent-templates/"+id.String()+"/model-pins", token, body)
}

func pinsErrBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("error body %q is not a string map: %v", rec.Body.String(), err)
	}
	return m
}

func pinsOKTemplate(t *testing.T, rec *httptest.ResponseRecorder) cliTemplateDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Template cliTemplateDTO `json:"template"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	return body.Template
}

func pinsList(t *testing.T, router http.Handler, token string) map[string]cliTemplateDTO {
	t.Helper()
	rec := bearerReq(router, http.MethodGet, "/api/me/agent-templates", token)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Templates []cliTemplateDTO `json:"templates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	out := map[string]cliTemplateDTO{}
	for _, tpl := range body.Templates {
		out[tpl.ID] = tpl
	}
	return out
}

func TestAgentTemplateModelPinsSetResetLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	id := pinsSeedTemplate(t, pool, "user", user, strp("sonnet"), strp("gpt-6-sol"))

	// Set each lane alone; the omitted lane keeps its value.
	dto := pinsOKTemplate(t, pinsPut(router, uzc, id, `{"model":"opus"}`))
	if pinStr(dto.Model) != "opus" || pinStr(dto.CodexModel) != "gpt-6-sol" || !dto.Editable || dto.Scope != "user" || dto.ID != id.String() {
		t.Fatalf("after model set: %+v", dto)
	}
	r := pinsReadRow(t, pool, id)
	if pinStr(r.model) != "opus" || pinStr(r.codex) != "gpt-6-sol" {
		t.Fatalf("stored pins %s/%s", pinStr(r.model), pinStr(r.codex))
	}

	dto = pinsOKTemplate(t, pinsPut(router, uzc, id, `{"codex_model":"gpt-6-luna"}`))
	if pinStr(dto.Model) != "opus" || pinStr(dto.CodexModel) != "gpt-6-luna" {
		t.Fatalf("after codex set: %+v", dto)
	}

	// A custom Claude id is accepted where validateTemplatePin allows it.
	dto = pinsOKTemplate(t, pinsPut(router, uzc, id, `{"model":"claude-custom-9"}`))
	if pinStr(dto.Model) != "claude-custom-9" {
		t.Fatalf("custom claude id: %+v", dto)
	}

	// Both lanes in one request.
	dto = pinsOKTemplate(t, pinsPut(router, uzc, id, `{"model":"haiku","codex_model":"gpt-6-sol"}`))
	if pinStr(dto.Model) != "haiku" || pinStr(dto.CodexModel) != "gpt-6-sol" {
		t.Fatalf("both lanes: %+v", dto)
	}

	// Reset each lane with null; the other lane is preserved and the list shows null (inherit).
	dto = pinsOKTemplate(t, pinsPut(router, uzc, id, `{"model":null}`))
	if dto.Model != nil || pinStr(dto.CodexModel) != "gpt-6-sol" {
		t.Fatalf("after model reset: %+v", dto)
	}
	dto = pinsOKTemplate(t, pinsPut(router, uzc, id, `{"codex_model":null}`))
	if dto.Model != nil || dto.CodexModel != nil {
		t.Fatalf("after codex reset: %+v", dto)
	}
	r = pinsReadRow(t, pool, id)
	if r.model != nil || r.codex != nil {
		t.Fatalf("reset must store NULL, got %s/%s", pinStr(r.model), pinStr(r.codex))
	}
	listed := pinsList(t, router, uzc)[id.String()]
	if listed.ID == "" || listed.Model != nil || listed.CodexModel != nil || !listed.Editable {
		t.Fatalf("list after reset: %+v", listed)
	}
	// A cleared pin serializes as an explicit null, not an omitted key.
	if raw := bearerReq(router, http.MethodGet, "/api/me/agent-templates", uzc).Body.String(); !strings.Contains(raw, `"model":null`) || !strings.Contains(raw, `"codex_model":null`) {
		t.Fatalf("list must carry explicit nulls: %s", raw)
	}
	pinsAssertContentUnchanged(t, r)
	if r.updatedBy == nil || *r.updatedBy != user {
		t.Fatalf("updated_by = %v, want caller %s", r.updatedBy, user)
	}
	if r.customized {
		t.Fatalf("a pin write must leave customized untouched (false)")
	}
}

func TestAgentTemplateModelPinsRejectionsWriteNothingLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	id := pinsSeedTemplate(t, pool, "user", user, strp("sonnet"), strp("gpt-6-sol"))
	before := pinsReadRow(t, pool, id)

	cases := []struct {
		name, body, field string
	}{
		{"codex id in model lane", `{"model":"gpt-6-luna"}`, "model"},
		{"claude alias in codex lane", `{"codex_model":"opus"}`, "codex_model"},
		{"unsupported codex id", `{"codex_model":"custom-model"}`, "codex_model"},
		{"control character", `{"model":"bad\nmodel"}`, "model"},
		{"model wrong type", `{"model":5}`, "model"},
		{"codex wrong type", `{"codex_model":true}`, "codex_model"},
		{"valid lane beside invalid lane", `{"model":"opus","codex_model":"opus"}`, "codex_model"},
		{"prompt_body", `{"prompt_body":"x"}`, "prompt_body"},
		{"scope", `{"scope":"global"}`, "scope"},
		{"user_id", `{"user_id":"` + uuid.NewString() + `"}`, "user_id"},
		{"effort", `{"effort":"max"}`, "effort"},
		{"description", `{"description":"x"}`, "description"},
		{"tools", `{"tools":["Read"]}`, "tools"},
		{"valid pin beside unsupported field", `{"model":"opus","prompt_body":"x"}`, "prompt_body"},
	}
	for _, tc := range cases {
		rec := pinsPut(router, uzc, id, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if got := pinsErrBody(t, rec); got["field"] != tc.field || got["error"] == "" {
			t.Errorf("%s: body %v, want field %q", tc.name, got, tc.field)
		}
	}
	// Several unknown keys: the sorted first one is named.
	rec := pinsPut(router, uzc, id, `{"zeta":1,"alpha":2,"model":"opus"}`)
	if rec.Code != http.StatusBadRequest || pinsErrBody(t, rec)["field"] != "alpha" {
		t.Errorf("sorted first unknown key: %d %s", rec.Code, rec.Body.String())
	}
	// Whole-body defects with no field.
	for name, body := range map[string]string{
		"empty object":  `{}`,
		"trailing json": `{"model":"opus"} {"codex_model":null}`,
		"trailing junk": `{"model":"opus"} x`,
		"null body":     `null`,
		"array body":    `[]`,
		"not json":      `nope`,
	} {
		if rec := pinsPut(router, uzc, id, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}

	after := pinsReadRow(t, pool, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a rejected request wrote something:\nbefore %+v\nafter  %+v", before, after)
	}
	pinsAssertContentUnchanged(t, after)
}

func TestAgentTemplateModelPinsAuthorizationLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	ownerTok := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	adminUzc := cliMintToken(t, pool, admin, clitoken.ScopeUser)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	builtin := pinsSeedTemplate(t, pool, "builtin", uuid.Nil, strp("sonnet"), nil)
	global := pinsSeedTemplate(t, pool, "global", uuid.Nil, strp("sonnet"), nil)
	others := pinsSeedTemplate(t, pool, "user", other, strp("sonnet"), nil)

	// builtin and global: 403 template_not_editable for everyone, admin tokens included, even with
	// an invalid body (authorization precedes validation).
	for _, tok := range []struct{ name, tok string }{{"owner uzc", ownerTok}, {"admin uzc", adminUzc}, {"admin uza", adminUza}} {
		for _, row := range []struct {
			name string
			id   uuid.UUID
		}{{"builtin", builtin}, {"global", global}} {
			for _, body := range []string{`{"model":"opus"}`, `{"prompt_body":"x"}`, `nope`} {
				rec := pinsPut(router, tok.tok, row.id, body)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s %s %s: status %d, want 403: %s", tok.name, row.name, body, rec.Code, rec.Body.String())
					continue
				}
				got := pinsErrBody(t, rec)
				if got["reason"] != "template_not_editable" || got["error"] != "builtin and global templates are edited in the web UI by an admin" {
					t.Errorf("%s %s: body %v", tok.name, row.name, got)
				}
			}
		}
	}

	// Another user's template and an unknown id: 404 template not found, for admins too.
	for _, tok := range []struct{ name, tok string }{{"owner uzc", ownerTok}, {"admin uzc", adminUzc}, {"admin uza", adminUza}} {
		for _, body := range []string{`{"model":"opus"}`, `nope`} {
			for name, id := range map[string]uuid.UUID{"other user's": others, "unknown": uuid.New()} {
				rec := pinsPut(router, tok.tok, id, body)
				if rec.Code != http.StatusNotFound || pinsErrBody(t, rec)["error"] != "template not found" {
					t.Errorf("%s %s %s: %d %s, want 404 template not found", tok.name, name, body, rec.Code, rec.Body.String())
				}
			}
		}
	}
	// A malformed id is refused like every other template route.
	if rec := bearerReqBody(router, http.MethodPut, "/api/me/agent-templates/not-a-uuid/model-pins", ownerTok, `{"model":"opus"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed id: %d %s, want 400", rec.Code, rec.Body.String())
	}

	// Nothing was written to any of them.
	for name, id := range map[string]uuid.UUID{"builtin": builtin, "global": global, "other user's": others} {
		r := pinsReadRow(t, pool, id)
		if pinStr(r.model) != "sonnet" || r.codex != nil || r.updatedBy != nil {
			t.Errorf("%s row was written: model=%s codex=%s updated_by=%v", name, pinStr(r.model), pinStr(r.codex), r.updatedBy)
		}
		pinsAssertContentUnchanged(t, r)
	}

	// A uza_ token may edit its own user template: there is no admin branch, so it is the owner
	// path and nothing else.
	adminOwn := pinsSeedTemplate(t, pool, "user", admin, nil, nil)
	if dto := pinsOKTemplate(t, pinsPut(router, adminUza, adminOwn, `{"model":"opus"}`)); pinStr(dto.Model) != "opus" || !dto.Editable {
		t.Fatalf("admin own template: %+v", dto)
	}
}

func TestAgentTemplateListScopeAndEditableLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	adminUza := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)
	ownerTok := cliMintToken(t, pool, owner, clitoken.ScopeUser)

	builtin := pinsSeedTemplate(t, pool, "builtin", uuid.Nil, strp("sonnet"), nil)
	global := pinsSeedTemplate(t, pool, "global", uuid.Nil, nil, strp("gpt-6-sol"))
	mine := pinsSeedTemplate(t, pool, "user", owner, strp("opus"), nil)
	others := pinsSeedTemplate(t, pool, "user", other, strp("opus"), nil)
	adminOwn := pinsSeedTemplate(t, pool, "user", admin, nil, nil)
	cliMustExec(t, pool, `UPDATE agent_templates SET customized = true WHERE id = $1`, mine)

	check := func(name string, got map[string]cliTemplateDTO, visible, hidden []uuid.UUID, editable map[uuid.UUID]bool) {
		t.Helper()
		for _, id := range visible {
			d, ok := got[id.String()]
			if !ok {
				t.Errorf("%s: %s missing from the list", name, id)
				continue
			}
			if d.Editable != editable[id] {
				t.Errorf("%s: %s editable=%v, want %v", name, id, d.Editable, editable[id])
			}
		}
		for _, id := range hidden {
			if _, ok := got[id.String()]; ok {
				t.Errorf("%s: %s must not be listed", name, id)
			}
		}
		for id, d := range got {
			if d.Scope == "user" && !d.Editable {
				t.Errorf("%s: user-scope row %s listed but not the caller's", name, id)
			}
			if (d.Scope == "builtin" || d.Scope == "global") && d.Editable {
				t.Errorf("%s: %s row %s must not be editable", name, d.Scope, id)
			}
		}
	}

	got := pinsList(t, router, ownerTok)
	check("owner", got, []uuid.UUID{builtin, global, mine}, []uuid.UUID{others, adminOwn},
		map[uuid.UUID]bool{builtin: false, global: false, mine: true})
	if d := got[mine.String()]; pinStr(d.Model) != "opus" || d.CodexModel != nil || !d.Customized || d.Name == "" || d.Scope != "user" {
		t.Errorf("own row view: %+v", d)
	}
	if d := got[global.String()]; d.Model != nil || pinStr(d.CodexModel) != "gpt-6-sol" || d.Customized {
		t.Errorf("global row view: %+v", d)
	}

	// An admin_ro token reaches handlers with IsAdmin=true; the list must still hide other users'
	// private templates and mark only the admin's own as editable.
	got = pinsList(t, router, adminUza)
	check("admin uza", got, []uuid.UUID{builtin, global, adminOwn}, []uuid.UUID{mine, others},
		map[uuid.UUID]bool{builtin: false, global: false, adminOwn: true})

	// The view is restricted: no prompt body, description or tools on the wire.
	raw := bearerReq(router, http.MethodGet, "/api/me/agent-templates", ownerTok).Body.Bytes()
	var generic struct {
		Templates []map[string]any `json:"templates"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	want := []string{"codex_model", "customized", "editable", "id", "model", "name", "scope"}
	for _, m := range generic.Templates {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("list keys = %v, want %v", keys, want)
		}
	}
	if strings.Contains(string(raw), pinsPromptBody[:12]) || strings.Contains(string(raw), pinsDesc) {
		t.Fatalf("list leaks template content")
	}
}

func TestAgentTemplateModelPinsFailureReasonsLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)
	id := pinsSeedTemplate(t, pool, "user", user, strp("sonnet"), nil)

	// Commit error: 500 template_commit_unknown whether the write landed or not.
	for _, apply := range []bool{false, true} {
		cliMustExec(t, pool, `UPDATE agent_templates SET model = 'sonnet' WHERE id = $1`, id)
		h.templatePinsBeginTx = func(ctx context.Context) (pgx.Tx, error) {
			tx, err := pool.Begin(ctx)
			return commitFailTx{Tx: tx, apply: apply}, err
		}
		rec := pinsPut(router, uzc, id, `{"model":"opus"}`)
		h.templatePinsBeginTx = nil
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("apply=%v: status %d, want 500: %s", apply, rec.Code, rec.Body.String())
		}
		got := pinsErrBody(t, rec)
		if got["reason"] != "template_commit_unknown" ||
			got["error"] != "the save may or may not have been applied; check with `uzi agent-template show`" {
			t.Fatalf("apply=%v: body %v", apply, got)
		}
		want := "sonnet"
		if apply {
			want = "opus"
		}
		if v := pinStr(pinsReadRow(t, pool, id).model); v != want {
			t.Fatalf("apply=%v: stored model %q, want %q", apply, v, want)
		}
	}

	// Readback failure: 500 template_saved_readback_failed with the write durable.
	cliMustExec(t, pool, `UPDATE agent_templates SET model = 'sonnet' WHERE id = $1`, id)
	h.templatePinsReadback = func(context.Context, uuid.UUID) (store.AgentTemplate, error) {
		return store.AgentTemplate{}, errors.New("readback boom")
	}
	rec := pinsPut(router, uzc, id, `{"model":"haiku"}`)
	h.templatePinsReadback = nil
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("readback failure: status %d: %s", rec.Code, rec.Body.String())
	}
	got := pinsErrBody(t, rec)
	if got["reason"] != "template_saved_readback_failed" || got["error"] != "template pins saved; reading them back failed" {
		t.Fatalf("readback failure body %v", got)
	}
	if v := pinStr(pinsReadRow(t, pool, id).model); v != "haiku" {
		t.Fatalf("a failed readback must leave the write durable: model %q", v)
	}

	// No pool and no seam: 500, nothing written.
	cliMustExec(t, pool, `UPDATE agent_templates SET model = 'sonnet' WHERE id = $1`, id)
	savedPool := h.pool
	h.pool = nil
	rec = pinsPut(router, uzc, id, `{"model":"opus"}`)
	h.pool = savedPool
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("no pool: status %d: %s", rec.Code, rec.Body.String())
	}
	if v := pinStr(pinsReadRow(t, pool, id).model); v != "sonnet" {
		t.Fatalf("no pool: stored model %q", v)
	}
}

// An unmatched route over a valid Bearer is a bare 404 with a non-JSON body, and a wrong method on
// an existing path is a bare 405 with a non-JSON body: the CLI tells "older server without this
// route" from a handler's own JSON 404 ("template not found") by exactly that.
func TestUnmatchedRouteIsNonJSONLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	user := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, user, clitoken.ScopeUser)

	isJSON := func(b []byte) bool { return json.Valid(b) && len(strings.TrimSpace(string(b))) > 0 }

	rec := bearerReq(router, http.MethodGet, "/api/me/does-not-exist", uzc)
	if rec.Code != http.StatusNotFound || isJSON(rec.Body.Bytes()) {
		t.Fatalf("unmatched GET: %d %q, want 404 with a non-JSON body", rec.Code, rec.Body.String())
	}
	rec = bearerReq(router, http.MethodPut, "/api/me/agent-templates/"+uuid.NewString()+"/model-pinz", uzc)
	if rec.Code != http.StatusNotFound || isJSON(rec.Body.Bytes()) {
		t.Fatalf("unmatched PUT: %d %q, want 404 with a non-JSON body", rec.Code, rec.Body.String())
	}
	// A handler's own 404 is JSON, which is what distinguishes it.
	rec = pinsPut(router, uzc, uuid.New(), `{"model":"opus"}`)
	if rec.Code != http.StatusNotFound || !isJSON(rec.Body.Bytes()) {
		t.Fatalf("handler 404: %d %q, want 404 with a JSON body", rec.Code, rec.Body.String())
	}
	// Wrong method on an existing path.
	rec = bearerReq(router, http.MethodPost, "/api/me/agent-templates", uzc)
	if rec.Code != http.StatusMethodNotAllowed || isJSON(rec.Body.Bytes()) {
		t.Fatalf("wrong method: %d %q, want 405 with a non-JSON body", rec.Code, rec.Body.String())
	}
}
