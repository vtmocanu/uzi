package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// An isolated database keeps builtin reconciliation and lock probes hermetic.
type templatePinsLive struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	h     *Handler
	admin store.User
}

func newTemplatePinsLive(t *testing.T) templatePinsLive {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; lead runs the LiveDB lane")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	adminPool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "template_pins_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	adminPool.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		p, err := store.OpenPool(cleanup, dsn)
		if err != nil {
			t.Error(err)
			return
		}
		defer p.Close()
		if _, err := p.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if err := store.Migrate(ctx, u.String()); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	f := templatePinsLive{ctx: ctx, pool: pool, h: &Handler{pool: pool, q: q}, admin: store.User{ID: uuid.New(), IsAdmin: true, IsActive: true}}
	f.exec(t, "INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'x',true)", f.admin.ID, f.admin.ID.String()+"@example.test")
	return f
}

func (f templatePinsLive) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

// Authenticated context is supplied through the same middleware seam as the
// existing handler LiveDB tests; production HTTP handlers and transactions run.
func (f templatePinsLive) call(ctx context.Context, actor *store.User, method string, id uuid.UUID, body any, reset bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	return f.callReader(ctx, actor, method, id, strings.NewReader(string(raw)), reset)
}

func (f templatePinsLive) callReader(ctx context.Context, actor *store.User, method string, id uuid.UUID, body io.Reader, reset bool) *httptest.ResponseRecorder {
	path := "/agent-templates"
	if id != uuid.Nil {
		path += "/" + id.String()
	}
	if reset {
		path += "/reset"
	}
	req := httptest.NewRequest(method, path, body).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if actor != nil {
		req = req.WithContext(mw.ContextWithUser(req.Context(), *actor))
	}
	router := chi.NewRouter()
	router.Post("/agent-templates", f.h.CreateAgentTemplate)
	router.Get("/agent-templates/{id}", f.h.GetAgentTemplate)
	router.Put("/agent-templates/{id}", f.h.UpdateAgentTemplate)
	router.Post("/agent-templates/{id}/reset", f.h.ResetAgentTemplate)
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	return out
}

func templatePinsResponse(t *testing.T, out *httptest.ResponseRecorder, status int) agentTemplateDTO {
	t.Helper()
	if out.Code != status {
		t.Fatalf("HTTP %d want %d: %s", out.Code, status, out.Body.String())
	}
	var response struct {
		Template agentTemplateDTO `json:"template"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Template.ID == "" {
		t.Fatalf("missing template: %s", out.Body.String())
	}
	return response.Template
}

func templatePinsCheckDTO(t *testing.T, dto agentTemplateDTO, model, codex string) {
	t.Helper()
	check := func(got *string, want string) bool {
		if want == "" {
			return got == nil
		}
		return got != nil && *got == want
	}
	if !check(dto.Model, model) || !check(dto.CodexModel, codex) {
		t.Fatalf("DTO pins model=%v codex=%v want=%q/%q", dto.Model, dto.CodexModel, model, codex)
	}
}

func (f templatePinsLive) checkRow(t *testing.T, id uuid.UUID, model, codex string, customized bool, origin string) {
	t.Helper()
	var m, c, o *string
	var custom bool
	if err := f.pool.QueryRow(f.ctx, "SELECT model,codex_model,customized,origin FROM agent_templates WHERE id=$1", id).Scan(&m, &c, &custom, &o); err != nil {
		t.Fatal(err)
	}
	templatePinsCheckDTO(t, agentTemplateDTO{Model: m, CodexModel: c}, model, codex)
	if custom != customized || (origin == "" && o != nil) || (origin != "" && (o == nil || *o != origin)) {
		t.Fatalf("row customized=%v origin=%v want=%v/%q", custom, o, customized, origin)
	}
}

func TestAgentTemplatePinsRoundTripLiveDB(t *testing.T) {
	f := newTemplatePinsLive(t)
	body := map[string]any{"name": "pins-" + uuid.NewString(), "description": "pin roundtrip", "prompt_body": "fixture body\n", "model": "opus", "codex_model": " \tgpt-6-luna\v", "scope": "global"}
	dto := templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPost, uuid.Nil, body, false), http.StatusCreated)
	id := uuid.MustParse(dto.ID)
	templatePinsCheckDTO(t, dto, "opus", "gpt-6-luna")
	f.checkRow(t, id, "opus", "gpt-6-luna", false, "")
	get := func() {
		templatePinsCheckDTO(t, templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodGet, id, nil, false), 200), "opus", "gpt-6-luna")
	}
	get()
	delete(body, "codex_model")
	dto = templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPut, id, body, false), 200)
	templatePinsCheckDTO(t, dto, "opus", "gpt-6-luna")
	f.checkRow(t, id, "opus", "gpt-6-luna", true, "")
	get()
	for _, clear := range []any{nil, " \t\n\v"} {
		body["codex_model"] = "gpt-6-sol"
		templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPut, id, body, false), 200)
		body["model"] = clear
		body["codex_model"] = clear
		dto = templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPut, id, body, false), 200)
		templatePinsCheckDTO(t, dto, "", "")
		f.checkRow(t, id, "", "", true, "")
		body["model"] = "opus"
	}
	// Invalid pins must reject the entire request, including an otherwise valid
	// change of body or of the opposite lane.
	for _, bad := range []struct{ field, value string }{
		{"model", "gpt-6-luna"}, {"codex_model", "opus"}, {"codex_model", "custom-model"},
		{"codex_model", "gpt-6-solv"}, {"model", "bad\nmodel"},
	} {
		before, err := f.h.q.GetAgentTemplate(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		body["model"] = "sonnet"
		body["codex_model"] = "gpt-6-sol"
		body["prompt_body"] = "must not persist"
		body[bad.field] = bad.value
		out := f.call(f.ctx, &f.admin, http.MethodPut, id, body, false)
		if out.Code != 400 || !strings.Contains(out.Body.String(), bad.field+":") {
			t.Fatalf("invalid %s=%q: HTTP %d %s", bad.field, bad.value, out.Code, out.Body.String())
		}
		after, err := f.h.q.GetAgentTemplate(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("invalid %s caused a partial write", bad.field)
		}
		// Create must validate both lanes too, without leaving a row/allocation.
		body["name"] = "invalid-" + uuid.NewString()
		out = f.call(f.ctx, &f.admin, http.MethodPost, uuid.Nil, body, false)
		if out.Code != 400 || !strings.Contains(out.Body.String(), bad.field+":") {
			t.Fatalf("invalid create: HTTP %d %s", out.Code, out.Body.String())
		}
		var count int
		if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM agent_templates WHERE name=$1", body["name"]).Scan(&count); err != nil || count != 0 {
			t.Fatalf("invalid create left %d rows: %v", count, err)
		}
	}
	nonadmin := store.User{ID: uuid.New(), IsActive: true}
	for _, actor := range []*store.User{nil, &nonadmin} {
		want := 401
		if actor != nil {
			want = 403
		}
		out := f.call(f.ctx, actor, http.MethodPut, id, body, false)
		if out.Code != want || strings.Contains(out.Body.String(), "model:") {
			t.Fatalf("authorization before validation: HTTP %d want %d: %s", out.Code, want, out.Body.String())
		}
	}
}

func templatePinsBuiltinBody(t *testing.T) (agenttmpl.Definition, map[string]any) {
	t.Helper()
	def, ok := agenttmpl.BuiltinByName("coder")
	if !ok {
		t.Fatal("missing shipped coder")
	}
	if def.CodexModel != "" {
		t.Fatal("fixture requires Claude-only shipped coder")
	}
	return def, map[string]any{"description": def.Description, "prompt_body": def.PromptBody, "model": def.Model, "tools": def.Tools}
}

func TestAgentTemplatePinsBuiltinLifecycleLiveDB(t *testing.T) {
	f := newTemplatePinsLive(t)
	if _, err := store.ReconcileBuiltinTemplates(f.ctx, f.h.q); err != nil {
		t.Fatal(err)
	}
	def, body := templatePinsBuiltinBody(t)
	row, err := f.h.q.GetSharedAgentTemplateByName(f.ctx, def.Name)
	if err != nil {
		t.Fatal(err)
	}
	f.checkRow(t, row.ID, def.Model, "", false, "embedded")
	body["codex_model"] = "gpt-6-luna"
	dto := templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPut, row.ID, body, false), 200)
	templatePinsCheckDTO(t, dto, def.Model, "gpt-6-luna")
	if !dto.DiffersFromBuiltin {
		t.Fatal("Codex pin must differ from builtin")
	}
	f.checkRow(t, row.ID, def.Model, "gpt-6-luna", true, "admin")
	delete(body, "codex_model")
	templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPut, row.ID, body, false), 200)
	f.checkRow(t, row.ID, def.Model, "gpt-6-luna", true, "admin")
	before, err := f.h.q.GetAgentTemplate(f.ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileBuiltinTemplates(f.ctx, f.h.q); err != nil {
		t.Fatal(err)
	}
	after, err := f.h.q.GetAgentTemplate(f.ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reconcile refreshed Codex-customized builtin")
	}
	// Simulate a later shipped body through the same public refresh query used
	// by boot. A nonzero row count would mean customization no longer protects it.
	n, err := f.h.q.RefreshPristineBuiltin(f.ctx, store.RefreshPristineBuiltinParams{
		Name: def.Name, Description: "new shipped description", PromptBody: "new shipped body\n",
		Model: row.Model, Tools: row.Tools,
	})
	if err != nil || n != 0 {
		t.Fatalf("customized pristine refresh wrote %d rows: %v", n, err)
	}
	f.checkRow(t, row.ID, def.Model, "gpt-6-luna", true, "admin")
	dto = templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPost, row.ID, nil, true), 200)
	templatePinsCheckDTO(t, dto, def.Model, "")
	if dto.DiffersFromBuiltin {
		t.Fatal("Reset still differs from builtin")
	}
	f.checkRow(t, row.ID, def.Model, "", false, "embedded")
	if dto.PromptBody != def.PromptBody || dto.Description != def.Description {
		t.Fatal("Reset did not restore shipped body")
	}
	n, err = f.h.q.RefreshPristineBuiltin(f.ctx, store.RefreshPristineBuiltinParams{
		Name: def.Name, Description: "new shipped description", PromptBody: "new shipped body\n",
		Model: row.Model, Tools: row.Tools,
	})
	if err != nil || n != 1 {
		t.Fatalf("Reset did not restore pristine refresh: rows=%d: %v", n, err)
	}
}

// The fixture pool is isolated and all requests in this test are synchronous,
// so an acquired connection during Read belongs to the handler under test.
type templatePinsBodyReader struct {
	t     *testing.T
	pool  *pgxpool.Pool
	body  io.Reader
	reads int
}

func (r *templatePinsBodyReader) Read(p []byte) (int, error) {
	r.t.Helper()
	r.reads++
	if acquired := r.pool.Stat().AcquiredConns(); acquired != 0 {
		r.t.Errorf("UpdateAgentTemplate held %d database connections during body Read; want 0", acquired)
	}
	return r.body.Read(p)
}

func TestAgentTemplatePinsBodyReadBeforeTransactionLiveDB(t *testing.T) {
	f := newTemplatePinsLive(t)
	owner := store.User{ID: uuid.New(), IsActive: true}
	other := store.User{ID: uuid.New(), IsActive: true}
	for _, user := range []store.User{owner, other} {
		f.exec(t, "INSERT INTO users (id,email,password_hash,is_admin) VALUES ($1,$2,'x',false)", user.ID, user.ID.String()+"@example.test")
	}
	body := map[string]any{
		"name": "body-read-" + uuid.NewString(), "scope": "user",
		"description": "before", "prompt_body": "fixture body\n",
		"model": "opus", "codex_model": "gpt-6-luna",
	}
	dto := templatePinsResponse(t, f.call(f.ctx, &owner, http.MethodPost, uuid.Nil, body, false), http.StatusCreated)
	id := uuid.MustParse(dto.ID)
	// Establish customized=true through the real update path before checking
	// that an update omitting the Codex pin retains both pieces of state.
	templatePinsResponse(t, f.call(f.ctx, &owner, http.MethodPut, id, body, false), http.StatusOK)
	f.checkRow(t, id, "opus", "gpt-6-luna", true, "")
	delete(body, "codex_model")
	body["description"] = "after"
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	reader := &templatePinsBodyReader{t: t, pool: f.pool, body: strings.NewReader(string(raw))}
	dto = templatePinsResponse(t, f.callReader(f.ctx, &owner, http.MethodPut, id, reader, false), http.StatusOK)
	if reader.reads == 0 {
		t.Fatal("PUT did not exercise the body reader")
	}
	if dto.Description != "after" {
		t.Fatalf("description=%q want after", dto.Description)
	}
	templatePinsCheckDTO(t, dto, "opus", "gpt-6-luna")
	f.checkRow(t, id, "opus", "gpt-6-luna", true, "")

	body["name"] = "body-read-global-" + uuid.NewString()
	body["scope"] = "global"
	global := templatePinsResponse(t, f.call(f.ctx, &f.admin, http.MethodPost, uuid.Nil, body, false), http.StatusCreated)
	globalID := uuid.MustParse(global.ID)
	missingID := uuid.New()
	for _, tc := range []struct {
		name   string
		actor  *store.User
		id     uuid.UUID
		status int
	}{
		{"authorized", &owner, id, http.StatusBadRequest},
		{"forbidden-global", &owner, globalID, http.StatusForbidden},
		{"forbidden-admin-private", &f.admin, id, http.StatusForbidden},
		{"hidden-private", &other, id, http.StatusNotFound},
		{"missing", &owner, missingID, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := make([]store.AgentTemplate, 0, 2)
			for _, existingID := range []uuid.UUID{id, globalID} {
				row, err := f.h.q.GetAgentTemplate(f.ctx, existingID)
				if err != nil {
					t.Fatal(err)
				}
				before = append(before, row)
			}
			malformed := &templatePinsBodyReader{
				t: t, pool: f.pool,
				body: strings.NewReader(`{"description":"must not persist","codex_model":"gpt-6-sol","prompt_body":`),
			}
			out := f.callReader(f.ctx, tc.actor, http.MethodPut, tc.id, malformed, false)
			if malformed.reads == 0 {
				t.Fatal("malformed PUT did not exercise the body reader")
			}
			if out.Code != tc.status {
				t.Fatalf("malformed PUT: HTTP %d want %d: %s", out.Code, tc.status, out.Body.String())
			}
			if tc.status != http.StatusBadRequest && strings.Contains(out.Body.String(), "invalid request body") {
				t.Fatalf("decode error revealed before authorization: %s", out.Body.String())
			}
			for _, row := range before {
				after, err := f.h.q.GetAgentTemplate(f.ctx, row.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(row, after) {
					t.Fatal("malformed PUT caused a partial row update")
				}
			}
		})
	}
}

func TestAgentTemplatePinsConcurrentOmittedPutLiveDB(t *testing.T) {
	f := newTemplatePinsLive(t)
	if _, err := store.ReconcileBuiltinTemplates(f.ctx, f.h.q); err != nil {
		t.Fatal(err)
	}
	_, body := templatePinsBuiltinBody(t)
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted-%v", deleted), func(t *testing.T) {
			row, err := f.h.q.GetSharedAgentTemplateByName(f.ctx, "coder")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
			defer cancel()
			writer, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Rollback(context.Background()) }()
			var writerPID int32
			if err := writer.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
				t.Fatal(err)
			}
			if err := writer.QueryRow(ctx, "SELECT id FROM agent_templates WHERE id=$1 FOR UPDATE", row.ID).Scan(&row.ID); err != nil {
				t.Fatal(err)
			}
			if deleted {
				if _, err := writer.Exec(ctx, "DELETE FROM agent_templates WHERE id=$1", row.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := writer.Exec(ctx, "UPDATE agent_templates SET codex_model='gpt-6-luna' WHERE id=$1", row.ID); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- f.call(ctx, &f.admin, http.MethodPut, row.ID, body, false) }()
			joined := false
			defer func() {
				if !joined {
					_ = writer.Rollback(context.Background())
					cancel()
					<-done
				}
			}()
			// Poll the lock graph, not elapsed time: this backend is blocked by our
			// exact writer, on the handler's named SELECT FOR UPDATE.
			for {
				var waiting bool
				err := f.pool.QueryRow(ctx, `SELECT EXISTS (
      SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
      AND wait_event_type='Lock' AND $1::int=ANY(pg_blocking_pids(pid))
      AND query LIKE '%GetAgentTemplateForUpdate%')`, writerPID).Scan(&waiting)
				if err != nil {
					t.Fatalf("observe handler lock barrier: %v", err)
				}
				if waiting {
					break
				}
				select {
				case out := <-done:
					joined = true
					t.Fatalf("PUT returned before row lock release: HTTP %d %s", out.Code, out.Body.String())
				case <-ctx.Done():
					t.Fatal("PUT never reached row lock barrier")
				default:
				}
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var out *httptest.ResponseRecorder
			select {
			case out = <-done:
				joined = true
			case <-ctx.Done():
				t.Fatal("PUT did not finish after writer commit")
			}
			if deleted {
				if out.Code != 404 {
					t.Fatalf("deleted-before-lock: HTTP %d %s", out.Code, out.Body.String())
				}
				var count int
				if err := f.pool.QueryRow(f.ctx, "SELECT count(*) FROM agent_templates WHERE id=$1", row.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("deleted row resurrected: count=%d err=%v", count, err)
				}
			} else {
				dto := templatePinsResponse(t, out, 200)
				def, _ := agenttmpl.BuiltinByName("coder")
				templatePinsCheckDTO(t, dto, def.Model, "gpt-6-luna")
				f.checkRow(t, row.ID, def.Model, "gpt-6-luna", true, "admin")
			}
		})
	}
}
