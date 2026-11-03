package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestCrossCheckHistoryLimitsLiveDB(t *testing.T) {
	f := newHistoryLiveFixture(t)
	base := time.Now().UTC().Truncate(time.Microsecond)
	title, metadata := strings.Repeat("<", 512), strings.Repeat("<", 128)
	parents := make([]string, 0, 26)
	runningChildren, completedChildren := map[string]string{}, map[string]string{}
	// These are schema-valid legacy nonautomatic rounds, not a larger automatic budget.
	// Distinct latest timestamps give a deterministic parent order without UUID ties.
	for i := 0; i < 26; i++ {
		parent := f.parent("completed", title)
		running := f.child(parent, "running")
		completed := f.child(parent, "completed")
		created := base.Add(time.Duration(i) * time.Hour)
		for round := 1; round <= 21; round++ {
			if round == 21 {
				// A retained legacy gap makes the valid boundary cursor an even
				// encoded length, so percent encoding can reach exactly 256 bytes.
				f.plan(parent, running, 100, "pending", "", created.Add(100*time.Second))
			} else {
				f.plan(parent, nil, round, "revise", "revise", created.Add(time.Duration(round)*time.Second))
			}
		}
		f.code(parent, completed, "completed", "", "[]", created.Add(time.Minute))
		cliMustExec(t, f.pool, "UPDATE cross_checks SET checker_model=$2,checker_effort=$2 WHERE lead_run_id=$1", parent, metadata)
		parents = append(parents, parent.String())
		runningChildren[parent.String()] = running.String()
		completedChildren[parent.String()] = completed.String()
	}

	trace := &historyQueryTrace{}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = trace
	traced, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	f.h.pool = traced
	readCount := func() int {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		return trace.queries
	}
	list := func(query string, cli bool) apitypes.CrossCheckHistoryDTO {
		t.Helper()
		before := readCount()
		rec := f.call("/api/cross-checks?"+query, cli)
		if rec.Code != http.StatusOK {
			t.Fatalf("history request cli=%t: status=%d body bytes=%d", cli, rec.Code, rec.Body.Len())
		}
		if reads := readCount() - before; reads != 2 {
			t.Fatalf("history request cli=%t: named history SELECTs=%d want 2", cli, reads)
		}
		raw := rec.Body.Bytes()
		if len(raw) > crossCheckHistoryEnvelopeLimit || len(raw) == 0 || raw[len(raw)-1] != '\n' {
			t.Fatalf("history envelope bytes=%d, want newline and at most %d", len(raw), crossCheckHistoryEnvelopeLimit)
		}
		if !bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte("<")) {
			t.Fatal("history response did not exercise JSON HTML escaping")
		}
		var dto apitypes.CrossCheckHistoryDTO
		if err := json.Unmarshal(raw, &dto); err != nil {
			t.Fatal(err)
		}
		return dto
	}
	assertWindow := func(g apitypes.CrossCheckHistoryGroupDTO, index, first, last int, code bool) {
		t.Helper()
		wantParent := parents[len(parents)-1-index]
		if g.ParentID != wantParent {
			t.Fatalf("parent index %d: got %s want %s", index, g.ParentID, wantParent)
		}
		if g.ParentTitle != title || !g.Active || g.TotalAttempts != 22 || g.MatchingAttempts != 22 || g.ActiveAttempts != 1 || g.RefreshNeeded {
			t.Fatalf("parent index %d: incorrect title, lifecycle, or retained totals", index)
		}
		wantLen := last - first + 1
		if code {
			wantLen++
		}
		if len(g.Attempts) != wantLen {
			t.Fatalf("parent index %d: attempts=%d want %d", index, len(g.Attempts), wantLen)
		}
		for j, a := range g.Attempts {
			stage, round := "plan", first+j
			if code && j == wantLen-1 {
				stage, round = "code", 1
			}
			if stage == "plan" && round == 21 {
				round = 100
			}
			if a.Stage != stage || int(a.Round) != round {
				t.Fatalf("parent index %d attempt index %d: got %s/%d want %s/%d", index, j, a.Stage, a.Round, stage, round)
			}
			if a.Model == nil || *a.Model != metadata || a.Effort == nil || *a.Effort != metadata {
				t.Fatalf("parent index %d attempt index %d: bounded model/effort missing", index, j)
			}
			if stage == "plan" && round == 100 {
				if a.ChildID == nil || *a.ChildID != runningChildren[wantParent] || a.ChildStatus == nil || *a.ChildStatus != "running" || a.Execution != "running" || a.Result != "pending" {
					t.Fatalf("parent index %d: newest Plan lost bound running child", index)
				}
			}
			if stage == "code" {
				if a.ChildID == nil || *a.ChildID != completedChildren[wantParent] || a.ChildStatus == nil || *a.ChildStatus != "completed" || a.Result != "completed" || a.Execution != "finished" {
					t.Fatalf("parent index %d: retained Code lost completed child", index)
				}
			}
		}
	}
	// Both existing session and CLI identities exercise each maximum workload.
	for _, cli := range []bool{false, true} {
		maximum := list("limit=25&attempt_limit=20", cli)
		if len(maximum.Groups) != 25 || maximum.NextCursor == "" {
			t.Fatalf("maximum page cli=%t: groups=%d next cursor present=%t", cli, len(maximum.Groups), maximum.NextCursor != "")
		}
		total := 0
		for i, g := range maximum.Groups {
			assertWindow(g, i, 3, 21, true)
			if g.OlderCursor == "" {
				t.Fatalf("maximum parent index %d: missing older cursor", i)
			}
			total += len(g.Attempts)
		}
		if total != 500 {
			t.Fatalf("maximum page attempts=%d want 500", total)
		}

		initial := list("limit=8&attempt_limit=2", cli)
		if len(initial.Groups) != 8 || initial.NextCursor == "" {
			t.Fatalf("initial vector page: groups=%d next cursor present=%t", len(initial.Groups), initial.NextCursor != "")
		}
		parts := []string{"attempt_limit=20"}
		encodedBytes := 0
		cursors := map[string]bool{}
		for i, g := range initial.Groups {
			assertWindow(g, i, 21, 21, true)
			if g.OlderCursor == "" || cursors[g.OlderCursor] {
				t.Fatalf("initial parent index %d: missing or duplicate older cursor", i)
			}
			cursors[g.OlderCursor] = true
			token := g.OlderCursor
			extra := 256 - len(token)
			if extra < 0 || extra%2 != 0 || extra/2 > len(token) {
				t.Fatal("fixture cannot reach exact raw cursor byte boundary")
			}
			var raw strings.Builder
			for j := 0; j < len(token); j++ {
				if j < extra/2 {
					fmt.Fprintf(&raw, "%%%02X", token[j])
				} else {
					raw.WriteByte(token[j])
				}
			}
			if raw.Len() != 256 {
				t.Fatalf("raw cursor bytes=%d want 256", raw.Len())
			}
			encodedBytes += raw.Len()
			parts = append(parts, "attempt_cursor="+raw.String())
		}
		if len(cursors) != 8 {
			t.Fatalf("distinct older cursors=%d want 8", len(cursors))
		}
		// Only the continuation vector is sent: no fresh group cursor or page.
		query := strings.Join(parts, "&")
		if encodedBytes != 2048 || len("/api/cross-checks?"+query) > 4096 {
			t.Fatalf("maximum vector bytes=%d target bytes=%d", encodedBytes, len("/api/cross-checks?"+query))
		}
		older := list(query, cli)
		if len(older.Groups) != 8 || older.NextCursor != "" {
			t.Fatalf("continuation vector: groups=%d next cursor present=%t", len(older.Groups), older.NextCursor != "")
		}
		total = 0
		for i, g := range older.Groups {
			assertWindow(g, i, 1, 20, false)
			if g.OlderCursor != "" {
				t.Fatalf("older parent index %d: exhausted window retained a cursor", i)
			}
			total += len(g.Attempts)
		}
		if total != 160 {
			t.Fatalf("continuation attempts=%d want 160", total)
		}
	}
}
