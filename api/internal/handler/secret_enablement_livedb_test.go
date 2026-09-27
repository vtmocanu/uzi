package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func enablementRequest(t *testing.T, h *Handler, user uuid.UUID, kind string, id uuid.UUID, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.PatchSecretEnabled(rec, userReq(http.MethodPatch, "/api/me/secrets/"+kind+"/"+id.String()+"/enabled", body, user, map[string]string{"kind": kind, "id": id.String()}))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

func TestSecretEnablementDependentPage(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	items := []struct{ ID uuid.UUID }{{ids[0]}, {ids[1]}, {ids[2]}}
	page := dependentPage(items, 9, func(item struct{ ID uuid.UUID }) uuid.UUID { return item.ID }, 2)
	if len(page.Items) != 2 || page.Total != 9 || page.NextCursor != ids[1].String() {
		t.Fatalf("first page = %+v", page)
	}
	last := dependentPage(items[2:], 9, func(item struct{ ID uuid.UUID }) uuid.UUID { return item.ID }, 2)
	if len(last.Items) != 1 || last.NextCursor != "" {
		t.Fatalf("last page = %+v", last)
	}
}

func TestSecretEnablementTransitionLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	user := mkSecretUser(t, pool)
	other := mkSecretUser(t, pool)
	a := uuid.New()
	b := uuid.New()
	for i, id := range []uuid.UUID{a, b} {
		_, err := pool.Exec(t.Context(), `INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token',$3,$4,'x','master')`, id, user, []string{"a", "b"}[i], i == 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := enablementRequest(t, h, other, "anthropic_token", a, `{"enabled":false}`); code != 404 {
		t.Fatalf("foreign: %d", code)
	}
	if code, _ := enablementRequest(t, h, user, "anthropic_token", a, `{"enabled":false}`); code != 409 {
		t.Fatalf("missing replacement: %d", code)
	}
	body := `{"enabled":false,"new_default_id":"` + b.String() + `"}`
	if code, _ := enablementRequest(t, h, user, "anthropic_token", a, body); code != 200 {
		t.Fatalf("handoff: %d", code)
	}
	var disabledAt string
	var rev int64
	var def bool
	if err := pool.QueryRow(t.Context(), `SELECT disabled_at::text,enablement_rev,is_default FROM user_secrets WHERE id=$1`, a).Scan(&disabledAt, &rev, &def); err != nil {
		t.Fatal(err)
	}
	if rev != 1 || def {
		t.Fatalf("transition: rev=%d default=%v", rev, def)
	}
	if code, _ := enablementRequest(t, h, user, "anthropic_token", a, body); code != 200 {
		t.Fatalf("repeat: %d", code)
	}
	var again string
	var againRev int64
	if err := pool.QueryRow(t.Context(), `SELECT disabled_at::text,enablement_rev FROM user_secrets WHERE id=$1`, a).Scan(&again, &againRev); err != nil {
		t.Fatal(err)
	}
	if again != disabledAt || againRev != rev {
		t.Fatalf("repeat moved timestamp/revision: %q/%d -> %q/%d", disabledAt, rev, again, againRev)
	}
	if code, _ := enablementRequest(t, h, user, "anthropic_token", b, `{"enabled":false}`); code != 200 {
		t.Fatalf("last enabled: %d", code)
	}
	var defaults int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM user_secrets WHERE user_id=$1 AND is_default`, user).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 0 {
		t.Fatalf("default not cleared: %d", defaults)
	}
	if code, _ := enablementRequest(t, h, user, "anthropic_token", a, `{"enabled":true}`); code != 200 {
		t.Fatalf("re-enable: %d", code)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM user_secrets WHERE id=$1 AND is_default AND disabled_at IS NULL`, a).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 1 {
		t.Fatalf("re-enable did not take empty default slot")
	}
}

func TestConcurrentSecretDisableHandoffLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	user := mkSecretUser(t, pool)
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		_, err := pool.Exec(t.Context(), `INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,'anthropic_token',$3,$4,'x','master')`, id, user, []string{"a", "b", "c"}[i], i == 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids[1:] {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			enablementRequest(t, h, user, "anthropic_token", ids[0], `{"enabled":false,"new_default_id":"`+id.String()+`"}`)
		}(id)
	}
	wg.Wait()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM user_secrets WHERE user_id=$1 AND is_default AND disabled_at IS NULL`, user).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enabled defaults=%d", n)
	}
}
