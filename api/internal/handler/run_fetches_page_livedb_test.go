package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/fetchctl"
)

// TestRunFetchesPaginationLiveDB: GET /api/runs/{id}/fetches is keyset-paginated. The
// default page is fetchctl.FetchesPageSize with a next_cursor when more rows follow; a
// smaller ?limit= walks the log in order across created_at ties (rows written in one
// statement share now()), with no row repeated or skipped and no cursor on the last page;
// a malformed limit, a malformed cursor and another run's row id are 400.
func TestRunFetchesPaginationLiveDB(t *testing.T) {
	e := newFCEnv(t, nil, true)
	run, _ := e.boundRun("running", 1, 1)
	other, _ := e.boundRun("running", 1, 1)
	owner := cliMintToken(t, e.pool, e.userID, clitoken.ScopeUser)
	total := fetchctl.FetchesPageSize + 1
	// One INSERT: every row gets the same created_at, so the order rests on the id tie-break.
	e.exec(`INSERT INTO run_fetches (run_id, reservation_id, url, verdict, started_at, finished_at)
	        SELECT $1, gen_random_uuid(), 'https://docs.example.com/' || g, 'allowed', now(), now()
	        FROM generate_series(1, $2::int) g`, run, total)
	e.exec(`INSERT INTO run_fetches (run_id, reservation_id, url, verdict, started_at, finished_at)
	        VALUES ($1, gen_random_uuid(), 'https://docs.example.com/other', 'allowed', now(), now())`, other)

	page := func(query string) (int, apitypes.RunFetchesDTO) {
		t.Helper()
		rec := bearerReq(e.router, http.MethodGet, "/api/runs/"+run.String()+"/fetches"+query, owner)
		var dto apitypes.RunFetchesDTO
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return rec.Code, dto
	}

	// Default page: the page size, and a cursor naming its last row.
	code, first := page("")
	if code != http.StatusOK || len(first.Fetches) != fetchctl.FetchesPageSize || first.NextCursor != first.Fetches[len(first.Fetches)-1].ID {
		t.Fatalf("default page = %d, %d rows, cursor %q; want %d rows and the last row's id", code, len(first.Fetches), first.NextCursor, fetchctl.FetchesPageSize)
	}
	code, rest := page("?after=" + first.NextCursor)
	if code != http.StatusOK || len(rest.Fetches) != 1 || rest.NextCursor != "" {
		t.Fatalf("second default page = %d, %d rows, cursor %q; want 1 row and no cursor", code, len(rest.Fetches), rest.NextCursor)
	}
	// An over-large limit is clamped to the page size, not honoured.
	if code, big := page("?limit=100000"); code != http.StatusOK || len(big.Fetches) != fetchctl.FetchesPageSize {
		t.Fatalf("limit=100000 = %d, %d rows; want %d (clamped)", code, len(big.Fetches), fetchctl.FetchesPageSize)
	}

	// A small page walks the whole log: same rows, same order, nothing repeated or skipped.
	want := append(append([]apitypes.RunFetchDTO{}, first.Fetches...), rest.Fetches...)
	var walked []apitypes.RunFetchDTO
	after := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("pagination did not terminate")
		}
		q := "?limit=7"
		if after != "" {
			q += "&after=" + after
		}
		code, p := page(q)
		if code != http.StatusOK || len(p.Fetches) == 0 || len(p.Fetches) > 7 {
			t.Fatalf("page %d = %d with %d rows", pages, code, len(p.Fetches))
		}
		walked = append(walked, p.Fetches...)
		if p.NextCursor == "" {
			break
		}
		after = p.NextCursor
	}
	if len(walked) != len(want) {
		t.Fatalf("walked %d rows, want %d", len(walked), len(want))
	}
	for i := range want {
		if walked[i].ID != want[i].ID {
			t.Fatalf("row %d = %s, want %s (order differs between page sizes)", i, walked[i].ID, want[i].ID)
		}
	}

	// Another run's row is not a cursor here, and bad values are 400.
	var foreign string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM run_fetches WHERE run_id = $1`, other).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"?after=" + foreign, "?after=not-a-uuid", "?limit=0", "?limit=-1", "?limit=x"} {
		if code, _ := page(q); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", q, code)
		}
	}
}
