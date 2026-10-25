package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Record writes, including a write that would leave an existing value unchanged.
type roleSettingsDB struct {
	*fakeSettingsDB
	writes int
}

func (d *roleSettingsDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE ") {
		d.writes++
	}
	return d.fakeSettingsDB.QueryRow(ctx, sql, args...)
}
func (d *roleSettingsDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.writes++
	return d.fakeSettingsDB.Exec(ctx, sql, args...)
}

func TestPutMySettingsClaudeRejectsCodexRoles(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/codex-models/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Role []string }
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Role) == 0 {
		t.Fatal("empty role fixture")
	}
	for _, model := range fixture.Role {
		for _, field := range []string{"default_claude_model", "default_model"} {
			t.Run(field+"/"+model, func(t *testing.T) {
				db := &roleSettingsDB{fakeSettingsDB: &fakeSettingsDB{
					claudeModel: pgtype.Text{String: "sonnet", Valid: true},
					codexModel:  pgtype.Text{String: "custom-codex", Valid: true},
				}}
				body, err := json.Marshal(map[string]any{field: " \t" + model + " ", "default_harness": "claude", "default_codex_model": "changed-custom", "default_effort": "high"})
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				h := &Handler{q: store.New(db)}
				h.PutMySettings(rec, authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", bytes.NewReader(body))))
				message := field + ": model is a Codex model id"
				if field == "default_model" {
					message = "default_model is not valid for the target harness lane: " + message
				}
				want, err := json.Marshal(map[string]string{"error": message})
				if err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusBadRequest || rec.Body.String() != string(want)+"\n" {
					t.Fatalf("response = %d %q, want 400 %q", rec.Code, rec.Body.String(), string(want)+"\n")
				}
				if db.writes != 0 || db.claudeModel.String != "sonnet" || db.codexModel.String != "custom-codex" || db.defaultHarness.Valid || db.effort.Valid {
					t.Fatalf("rejection wrote settings: %+v; writes=%d", db.fakeSettingsDB, db.writes)
				}
			})
		}
	}
}

func TestPutMySettingsClaudeRoleControls(t *testing.T) {
	for _, field := range []string{"default_claude_model", "default_model"} {
		for _, value := range []any{"claude-opus-custom", "gpt-custom-non-role", nil, "", "   "} {
			raw, err := json.Marshal(map[string]any{field: value})
			if err != nil {
				t.Fatal(err)
			}
			t.Run(string(raw), func(t *testing.T) {
				db := &fakeSettingsDB{claudeModel: pgtype.Text{String: "sonnet", Valid: true}, codexModel: pgtype.Text{String: "retained-custom", Valid: true}}
				rec := putUserSettings(t, db, string(raw))
				if rec.Code != http.StatusOK {
					t.Fatalf("response = %d %s", rec.Code, rec.Body.String())
				}
				want, _ := value.(string)
				want = strings.TrimSpace(want)
				if db.claudeModel.Valid != (want != "") || db.claudeModel.String != want || db.codexModel.String != "retained-custom" {
					t.Fatalf("lanes = Claude %+v, Codex %+v", db.claudeModel, db.codexModel)
				}
			})
		}
	}
	db := &fakeSettingsDB{claudeModel: pgtype.Text{String: "retained-claude", Valid: true}}
	rec := putUserSettings(t, db, `{"default_model":"custom-codex","default_harness":"codex"}`)
	if rec.Code != http.StatusOK || db.codexModel.String != "custom-codex" || db.claudeModel.String != "retained-claude" {
		t.Fatalf("Codex legacy control: %d %s; lanes=%+v %+v", rec.Code, rec.Body.String(), db.claudeModel, db.codexModel)
	}
}
