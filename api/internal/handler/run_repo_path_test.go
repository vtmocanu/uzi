package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type runRepoPathDB struct {
	store.DBTX
	path         string
	err          error
	args         []any
	cooldownArgs []any
}

func (db *runRepoPathDB) Query(_ context.Context, query string, args ...any) (pgx.Rows, error) {
	if strings.Contains(query, "-- name: ListOwnerRunForgeOriginCooldowns") {
		db.cooldownArgs = args
		return nil, errors.New("optional cooldown projection unavailable")
	}
	return nil, errors.New("unexpected query: " + query)
}

func (db *runRepoPathDB) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "-- name: GetRepoForUser") {
		db.args = args
		return runRepoPathRow{path: db.path, err: db.err}
	}
	return runRepoPathRow{err: pgx.ErrNoRows}
}

type runRepoPathRow struct {
	path string
	err  error
}

func (r runRepoPathRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	// GetRepoForUser's fourth projected column is path_with_namespace.
	*dest[3].(*string) = r.path
	return nil
}

func TestGetRunRepoPath(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	repoID, runID := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name   string
		viewer store.User
		repo   bool
		err    error
		want   string
		status int
	}{
		{"owner", owner, true, nil, "team/project", http.StatusOK},
		{"admin", store.User{ID: uuid.New(), IsAdmin: true}, true, nil, "team/project", http.StatusOK},
		{"foreign", store.User{ID: uuid.New()}, true, nil, "", http.StatusNotFound},
		{"deleted", owner, true, pgx.ErrNoRows, "", http.StatusOK},
		{"unreadable", owner, true, errors.New("offline"), "", http.StatusOK},
		{"repoless", owner, false, nil, "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{ID: runID, UserID: owner.ID, Status: "completed"}
			if tc.repo {
				run.RepoID = pgtype.UUID{Bytes: repoID, Valid: true}
			}
			h := newRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
			db := &runRepoPathDB{path: "team/project", err: tc.err}
			h.q = store.New(db)
			rec := httptest.NewRecorder()
			h.GetRun(rec, runReq(tc.viewer, runID))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.status == http.StatusOK {
				var got struct {
					Run apitypes.RunDTO `json:"run"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Run.RepoPath != tc.want {
					t.Fatalf("repo_path=%q want %q", got.Run.RepoPath, tc.want)
				}
				var body struct {
					Run map[string]json.RawMessage `json:"run"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if _, present := body.Run["forge_origin_cooldown"]; present {
					t.Fatal("optional cooldown field must be absent when its projection fails")
				}
				if !reflect.DeepEqual(db.cooldownArgs, []any{tc.viewer.ID, []uuid.UUID{runID}}) {
					t.Fatalf("cooldown lookup must use viewer and run: %v", db.cooldownArgs)
				}
			}
			if tc.repo && tc.status == http.StatusOK {
				if !reflect.DeepEqual(db.args, []any{repoID, owner.ID}) {
					t.Fatalf("lookup must use run's repo and owner: %v", db.args)
				}
			} else if db.args != nil {
				t.Fatalf("unexpected repo lookup: %v", db.args)
			}
		})
	}
}
