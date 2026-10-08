package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Inspect raw members before decoding: a pointer alone cannot distinguish missing from null.
func metadataCells(t *testing.T, rec *httptest.ResponseRecorder) []crossCheckPinDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		Settings struct {
			Pins []map[string]json.RawMessage `json:"cross_check_pins"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Settings.Pins) != 2 {
		t.Fatalf("expected both cells: %s", rec.Body.String())
	}
	for i, harness := range []string{"claude", "codex"} {
		if string(raw.Settings.Pins[i]["harness"]) != "\""+harness+"\"" {
			t.Fatalf("cell %d: %v", i, raw.Settings.Pins[i])
		}
		if _, ok := raw.Settings.Pins[i]["worker_default_model"]; !ok {
			t.Fatalf("missing metadata in %s", harness)
		}
	}
	var decoded struct {
		Settings struct {
			Pins []crossCheckPinDTO `json:"cross_check_pins"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Settings.Pins
}

func assertMetadataModel(t *testing.T, got *string, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("model = %q, want null", *got)
		}
	} else if got == nil || *got != want {
		t.Fatalf("model = %v, want %q", got, want)
	}
}

func TestCrossCheckMetadataSettings(t *testing.T) {
	for _, saved := range []bool{false, true} {
		db := &fakeSettingsDB{}
		if saved {
			db.claudeModel = pgtype.Text{String: "haiku", Valid: true}
			db.codexModel = pgtype.Text{String: "gpt-6-sol", Valid: true}
		}
		h := &Handler{q: store.New(db)}
		for _, method := range []string{http.MethodGet, http.MethodPut} {
			rec := httptest.NewRecorder()
			req := userReq(method, "/api/me/settings", "{}", uuid.New(), nil)
			if method == http.MethodGet {
				h.GetMySettings(rec, req)
			} else {
				h.PutMySettings(rec, req)
			}
			cells := metadataCells(t, rec)
			claude, codex := "", "gpt-6.1-sol"
			if saved {
				claude, codex = "haiku", "gpt-6-sol"
			}
			assertMetadataModel(t, cells[0].WorkerDefaultModel, claude)
			assertMetadataModel(t, cells[1].WorkerDefaultModel, codex)
		}
	}
}

func TestCrossCheckMetadataWriteRejected(t *testing.T) {
	h := &Handler{}
	for _, value := range []string{"null", "\"haiku\""} {
		rec := httptest.NewRecorder()
		h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings",
			`{"cross_check_pins":[{"stage":"plan","harness":"claude","worker_default_model":`+value+`}]}`, uuid.New(), nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("derived input: %d %s", rec.Code, rec.Body.String())
		}
	}
}

type metadataTemplate struct {
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	Model     *string `json:"model"`
	Allocated bool    `json:"allocated"`
	Foreign   bool    `json:"foreign"`
}
type metadataCase struct {
	Name      string             `json:"name"`
	Claude    *string            `json:"default_claude_model"`
	Codex     *string            `json:"default_codex_model"`
	Templates []metadataTemplate `json:"templates"`
	Put       json.RawMessage    `json:"put"`
	Get       []crossCheckPinDTO `json:"get_cross_check_pins"`
	Saved     []crossCheckPinDTO `json:"put_cross_check_pins"`
}

// The corpus uses LEAD before ORCHESTRATOR under both supported locales.
// Locale discrimination has its own test; mocks need only match this delivered order.
func TestCrossCheckMetadataCorpusLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, wsvc: workersvc.New(q, nil, workersvc.Params{})}
	str := func(s string) *string { return &s }
	pin := json.RawMessage(`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"sonnet"},{"stage":"plan","harness":"codex","model":"gpt-6-astra"}]}`)
	cases := []metadataCase{
		{Name: "product-and-sdk", Put: pin},
		{Name: "saved-lanes", Claude: str("haiku"), Codex: str("gpt-6-sol"), Put: pin},
		{Name: "pin-masks-template", Templates: []metadataTemplate{{"LEAD", "user", str("haiku"), true, false}}, Put: pin},
		{Name: "model-less-first", Templates: []metadataTemplate{{"LEAD", "user", nil, true, false}, {"ORCHESTRATOR", "user", str("opus"), true, false}}, Put: pin},
		{Name: "unallocated", Templates: []metadataTemplate{{"LEAD", "user", str("haiku"), false, false}}, Put: pin},
		{Name: "foreign", Templates: []metadataTemplate{{"LEAD", "user", str("haiku"), true, true}}, Put: pin},
		{Name: "shared-name-precedence", Templates: []metadataTemplate{{"LEAD", "global", str("opus"), true, false}, {"LEAD", "user", str("haiku"), true, false}}, Put: pin},
		{Name: "shared-name-unallocated", Templates: []metadataTemplate{{"LEAD", "global", str("opus"), false, false}, {"LEAD", "user", str("haiku"), true, false}}, Put: pin},
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := range cases {
		c := &cases[i]
		t.Run(c.Name, func(t *testing.T) {
			user, foreign := mkSecretUser(t, pool), mkSecretUser(t, pool)
			// Suppress existing candidates for this user without modifying their templates.
			exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) SELECT id,$1,false FROM agent_templates WHERE lower(name) IN ('lead','orchestrator') ON CONFLICT DO NOTHING`, user)
			exec("UPDATE users SET default_claude_model=$2,default_codex_model=$3 WHERE id=$1", user, c.Claude, c.Codex)
			for _, tmpl := range c.Templates {
				id, owner := uuid.New(), user
				if tmpl.Foreign {
					owner = foreign
				}
				var ownerValue any
				if tmpl.Scope == "user" {
					ownerValue = owner
				}
				exec("INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model) VALUES($1,$2,'d','b',$3,$4,$5)", id, tmpl.Name, tmpl.Scope, ownerValue, tmpl.Model)
				t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM agent_templates WHERE id=$1", id) })
				if tmpl.Allocated {
					// Allocate even the foreign candidate to this viewer so visibility alone excludes it.
					exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true)", id, user)
				}
			}
			rec := httptest.NewRecorder()
			h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", user, nil))
			c.Get = metadataCells(t, rec)
			want := ""
			switch c.Name {
			case "saved-lanes", "pin-masks-template":
				want = "haiku"
			case "shared-name-precedence":
				want = "opus"
			}
			assertMetadataModel(t, c.Get[0].WorkerDefaultModel, want)
			codex := "gpt-6.1-sol"
			if c.Codex != nil {
				codex = *c.Codex
			}
			assertMetadataModel(t, c.Get[1].WorkerDefaultModel, codex)
			rec = httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", string(c.Put), user, nil))
			c.Saved = metadataCells(t, rec)
			assertMetadataModel(t, c.Saved[0].WorkerDefaultModel, want)
			assertMetadataModel(t, c.Saved[1].WorkerDefaultModel, codex)
			assertMetadataModel(t, c.Saved[0].ResolvedModel, "sonnet")
			assertMetadataModel(t, c.Saved[1].ResolvedModel, "gpt-6-astra")
		})
	}
	got := mustMarshalIndent(t, cases)
	name := "cross_check_metadata.behavior.json"
	want, err := os.ReadFile(filepath.Join(contractFixtureDir, name))
	if err != nil || string(want) != string(got)+"\n" {
		t.Errorf("fixture %s is stale -- re-record it from this exact output (recorded, not authored):\n%s", name, got)
	}
}

func TestCrossCheckMetadataNativeLocaleDiscriminatorLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	h := &Handler{pool: pool, q: q, wsvc: workersvc.New(q, nil, workersvc.Params{})}
	user := mkSecretUser(t, pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	var collate, ctype, column string
	if err := pool.QueryRow(ctx, "SELECT datcollate,datctype FROM pg_database WHERE datname=current_database()").Scan(&collate, &ctype); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT c.collname FROM pg_attribute a JOIN pg_collation c ON c.oid=a.attcollation WHERE a.attrelid='agent_templates'::regclass AND a.attname='name'`).Scan(&column); err != nil {
		t.Fatal(err)
	}
	t.Logf("database LC_COLLATE=%s LC_CTYPE=%s agent_templates.name collation=%s", collate, ctype, column)
	exec(`INSERT INTO agent_template_allocations(template_id,user_id,enabled) SELECT id,$1,false FROM agent_templates WHERE lower(name) IN ('lead','orchestrator') ON CONFLICT DO NOTHING`, user)
	lead, orchestrator := uuid.New(), uuid.New()
	exec(`INSERT INTO agent_templates(id,name,description,prompt_body,scope,user_id,model) VALUES($1,'lead','d','b','user',$2,'haiku'),($3,'Orchestrator','d','b','global',NULL,'opus')`, lead, user, orchestrator)
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM agent_templates WHERE id IN ($1,$2)", lead, orchestrator)
	}()
	exec("INSERT INTO agent_template_allocations(template_id,user_id,enabled) VALUES($1,$2,true),($3,$2,true)", lead, user, orchestrator)
	exec("UPDATE users SET default_claude_model=NULL WHERE id=$1", user)
	rec := httptest.NewRecorder()
	h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", `{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"sonnet"}]}`, user, nil))
	metadataCells(t, rec)
	var firstID uuid.UUID
	for _, phase := range []string{"concrete-first", "null-first"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "null-first" {
				exec("UPDATE agent_templates SET model=NULL WHERE id=$1", firstID)
			}
			delivered, err := q.ListClaimAgentTemplates(ctx, pgtype.UUID{Bytes: user, Valid: true})
			if err != nil {
				t.Fatal(err)
			}
			var first *store.AgentTemplate
			for i := range delivered {
				if name := strings.ToLower(delivered[i].Name); name == "lead" || name == "orchestrator" {
					first = &delivered[i]
					break
				}
			}
			if first == nil {
				t.Fatal("no native delivered lead")
			}
			firstID = first.ID
			t.Logf("first native delivered template=%s model=%+v", first.Name, first.Model)
			rows, err := q.ListUserCrossCheckPins(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].TemplateModel != first.Model {
				t.Fatalf("settings SQL disagrees with claim delivery: %+v vs %+v", rows, first)
			}
			rec := httptest.NewRecorder()
			h.GetMySettings(rec, userReq(http.MethodGet, "/api/me/settings", "", user, nil))
			cells := metadataCells(t, rec)
			want := ""
			if first.Model.Valid {
				want = first.Model.String
			}
			assertMetadataModel(t, cells[0].WorkerDefaultModel, want)
			assertMetadataModel(t, cells[0].ResolvedModel, "sonnet")
			assertMetadataModel(t, cells[1].WorkerDefaultModel, "gpt-6.1-sol")
			rec = httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", "{}", user, nil))
			cells = metadataCells(t, rec)
			assertMetadataModel(t, cells[0].WorkerDefaultModel, want)
			assertMetadataModel(t, cells[0].ResolvedModel, "sonnet")
			assertMetadataModel(t, cells[1].WorkerDefaultModel, "gpt-6.1-sol")
		})
	}
}
