package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// crossCheckPinOrderTracer synchronizes only the first UPSERT on each connection.
// Waiting after it would block the sorted transactions on the same first row.
// The request deadline bounds the barrier and DB work; neither save is retried.
type crossCheckPinOrderTracer struct {
	mu     sync.Mutex
	orders map[*pgx.Conn][]string
	ready  chan struct{}
}

func (tr *crossCheckPinOrderTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(data.SQL, "-- name: PatchUserCrossCheckPin :exec\n") {
		return ctx
	}
	tr.mu.Lock()
	first := len(tr.orders[conn]) == 0
	tr.orders[conn] = append(tr.orders[conn], data.Args[1].(string)+"/"+data.Args[2].(string))
	if first && len(tr.orders) == 2 {
		close(tr.ready)
	}
	tr.mu.Unlock()
	if first {
		select {
		case <-tr.ready:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*crossCheckPinOrderTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestCrossCheckPinOppositeOrderSavesLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		name := "new cells"
		if existing {
			name = "existing cells"
		}
		t.Run(name, func(t *testing.T) {
			tracer := &crossCheckPinOrderTracer{orders: make(map[*pgx.Conn][]string), ready: make(chan struct{})}
			config, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			config.MaxConns = 2
			config.ConnConfig.Tracer = tracer
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			q := store.New(pool)
			h := &Handler{pool: pool, q: q, wsvc: workersvc.New(q, nil, workersvc.Params{})}
			user := mkSecretUser(t, pool)
			if existing {
				_, err = pool.Exec(ctx, `INSERT INTO user_cross_check_pins (user_id,stage,harness,model,effort)
					VALUES ($1,'plan','claude','sonnet','low'),($1,'plan','codex','gpt-6-sol','low')`, user)
				if err != nil {
					t.Fatal(err)
				}
			}

			var wg sync.WaitGroup
			results := make(chan *httptest.ResponseRecorder, 2)
			for _, body := range []string{
				`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"plan","harness":"codex","model":null}]}`,
				`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"xhigh"},{"stage":"plan","harness":"claude","effort":"high"}]}`,
			} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req := userReq(http.MethodPut, "/api/me/settings", body, user, nil)
					reqCtx, stop := context.WithTimeout(req.Context(), 10*time.Second)
					defer stop()
					rec := httptest.NewRecorder()
					h.PutMySettings(rec, req.WithContext(reqCtx))
					results <- rec
				}()
			}
			wg.Wait()
			close(results)
			// Assert the actual SQL sequence even if an unsorted mutation happens
			// to avoid a deadlock through scheduling.
			tracer.mu.Lock()
			if len(tracer.orders) != 2 {
				t.Errorf("UPSERT connections: got %d, want 2", len(tracer.orders))
			}
			for conn, order := range tracer.orders {
				if !slices.Equal(order, []string{"plan/claude", "plan/codex"}) {
					t.Errorf("connection %p UPSERT order: got %v, want [plan/claude plan/codex]", conn, order)
				}
			}
			tracer.mu.Unlock()
			for rec := range results {
				if rec.Code != http.StatusOK {
					t.Errorf("concurrent save: %d %s", rec.Code, rec.Body.String())
				}
			}
			pins, err := q.ListUserCrossCheckPins(ctx, user)
			if err != nil {
				t.Fatal(err)
			}
			if len(pins) != 2 || pins[0].Model.String != "haiku" || !pins[0].Model.Valid ||
				pins[0].Effort.String != "high" || !pins[0].Effort.Valid ||
				pins[1].Model.Valid || pins[1].Effort.String != "xhigh" || !pins[1].Effort.Valid {
				t.Fatalf("concurrent saves lost omitted fields or explicit null: %+v", pins)
			}
		})
	}
}

func TestCrossCheckPinSettingsLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
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
	request := func(body string, status int) []crossCheckPinDTO {
		t.Helper()
		rec := httptest.NewRecorder()
		req := userReq(http.MethodPut, "/api/me/settings", body, user, nil)
		h.PutMySettings(rec, req)
		if rec.Code != status {
			t.Fatalf("PUT %s: %d %s", body, rec.Code, rec.Body.String())
		}
		if status != 200 {
			return nil
		}
		var response struct {
			Settings struct {
				Pins []crossCheckPinDTO `json:"cross_check_pins"`
			} `json:"settings"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		metadataCells(t, rec)
		if len(response.Settings.Pins) != 2 {
			t.Fatalf("cells: %+v", response)
		}
		return response.Settings.Pins
	}
	initial := request("{}", 200)
	if initial[0].Harness != "claude" || initial[0].Active || !initial[1].Active || initial[1].ResolvedModel == nil ||
		*initial[1].ResolvedModel != "gpt-6.1-sol" || initial[1].ResolvedEffort != "medium" || initial[1].Model != nil {
		t.Fatalf("defaults/dormancy: %+v", initial)
	}
	exec("UPDATE users SET default_codex_model='gpt-6-sol',default_codex_effort='high' WHERE id=$1", user)
	got := request(`{"cross_check_pins":[{"stage":"plan","harness":"codex","model":"gpt-6-astra","effort":"xhigh"},{"stage":"plan","harness":"claude","model":"sonnet"}]}`, 200)
	if *got[1].Model != "gpt-6-astra" || *got[1].ResolvedModel != "gpt-6-astra" || got[1].ResolvedEffort != "xhigh" ||
		got[1].ModelSource != "pin" || got[1].EffortSource != "pin" || got[0].Active || *got[0].Model != "sonnet" {
		t.Fatalf("pins: %+v", got)
	}
	got = request(`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"max"}]}`, 200)
	if got[1].Effort == nil || *got[1].Effort != "max" || got[1].ResolvedEffort != "max" {
		t.Fatal("Codex max pin not accepted")
	}
	request(`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"xhigh"}]}`, 200)
	for _, body := range []string{`{"cross_check_pins":[]}`, `{"cross_check_pins":[{"stage":"plan","harness":"codex"}]}`, "{}"} {
		got = request(body, 200)
		if *got[1].Model != "gpt-6-astra" || *got[1].Effort != "xhigh" {
			t.Fatal("omission cleared a pin")
		}
	}
	got = request(`{"cross_check_pins":[{"stage":"plan","harness":"codex","model":null}]}`, 200)
	if got[1].Model != nil || *got[1].ResolvedModel != "gpt-6-sol" || *got[1].Effort != "xhigh" ||
		got[1].ModelSource != "worker default" || got[1].EffortSource != "pin" {
		t.Fatal("independent reset failed")
	}
	exec("UPDATE users SET default_codex_model='gpt-6.1-sol',default_codex_effort='low' WHERE id=$1", user)
	got = request(`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"  "}]}`, 200)
	if *got[1].ResolvedModel != "gpt-6.1-sol" || got[1].Effort != nil || got[1].ResolvedEffort != "low" {
		t.Fatal("Default did not follow updated lane")
	}
	for _, body := range []string{
		`{"cross_check_pins":[{"stage":"plan","harness":"claude","worker_default_model":null}]}`,
		`{"cross_check_pins":null}`, `{"cross_check_pins":{}}`,
		`{"cross_check_pins":[{"stage":"code","harness":"codex"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"other"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"codex"},{"stage":"plan","harness":"codex"}]}`,
		`{"default_effort":"low","cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"plan","harness":"codex","model":"sonnet"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"gpt-6-sol"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"invalid"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"codex","model":"a\u202eb"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":5}]}`,
		`{"default_effort":"bad","cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"}]}`,
	} {
		request(body, 400)
		got = request("{}", 200)
		settings, err := q.GetUserSettings(ctx, user)
		if err != nil || settings.DefaultEffort.Valid || *got[0].Model != "sonnet" || got[1].Model != nil || got[1].Effort != nil {
			t.Fatal("bad request wrote fields")
		}
	}
	// A second-cell store failure must roll back the first cell.
	exec("ALTER TABLE user_cross_check_pins ADD CONSTRAINT test_pin_write_failure CHECK (model IS DISTINCT FROM 'reject-db-write')")
	request(`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":"haiku"},{"stage":"plan","harness":"codex","model":"reject-db-write"}]}`, 500)
	exec("ALTER TABLE user_cross_check_pins DROP CONSTRAINT test_pin_write_failure")
	got = request("{}", 200)
	if *got[0].Model != "sonnet" {
		t.Fatal("partial transaction committed")
	}

	// Concurrent model and effort patches must retain both columns.
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, body := range []string{
		`{"cross_check_pins":[{"stage":"plan","harness":"codex","model":"gpt-6-astra"}]}`,
		`{"cross_check_pins":[{"stage":"plan","harness":"codex","effort":"xhigh"}]}`,
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.PutMySettings(rec, userReq(http.MethodPut, "/api/me/settings", body, user, nil))
			results <- rec.Code
		}()
	}
	wg.Wait()
	close(results)
	for status := range results {
		if status != 200 {
			t.Fatalf("concurrent save: %d", status)
		}
	}
	got = request("{}", 200)
	if *got[1].Model != "gpt-6-astra" || *got[1].Effort != "xhigh" {
		t.Fatal("concurrent field patch lost a column")
	}

	request(`{"cross_check_pins":[{"stage":"plan","harness":"claude","model":null}]}`, 200)
	exec("UPDATE users SET default_claude_model=NULL WHERE id=$1", user)
	// Own mixed-case delivered leads sort in the same order as ListClaimAgentTemplates.
	exec("INSERT INTO agent_template_allocations (template_id,user_id,enabled) SELECT id,$1,false FROM agent_templates WHERE lower(name) IN ('lead','orchestrator') ON CONFLICT (template_id, (COALESCE(user_id, '00000000-0000-0000-0000-000000000000'))) DO UPDATE SET enabled=false", user)
	lead, second := uuid.New(), uuid.New()
	exec("INSERT INTO agent_templates (id,name,description,prompt_body,scope,user_id,model) VALUES ($1,'LEAD','d','b','user',$2,NULL),($3,'Orchestrator','d','b','user',$2,'opus')", lead, user, second)
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM agent_templates WHERE id IN ($1,$2)", lead, second)
	}()
	exec("INSERT INTO agent_template_allocations (template_id,user_id,enabled) VALUES ($1,$2,true),($3,$2,true)", lead, user, second)
	got = request("{}", 200)
	if got[0].ResolvedModel != nil {
		t.Fatal("null first lead fell through to second lead/opus")
	}
	exec("UPDATE agent_templates SET model='haiku' WHERE id=$1", lead)
	got = request("{}", 200)
	if got[0].ResolvedModel == nil || *got[0].ResolvedModel != "haiku" {
		t.Fatal("first delivered lead model not resolved")
	}
	exec("UPDATE agent_template_allocations SET enabled=false WHERE template_id IN ($1,$2) AND user_id=$3", lead, second, user)
	got = request("{}", 200)
	if got[0].ResolvedModel != nil {
		t.Fatal("undelivered templates used as defaults")
	}
}
