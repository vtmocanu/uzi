package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These service calls must persist the same body as the corresponding NUL-free
// call. A raw NUL sent to a Postgres text parameter fails with SQLSTATE 22021.
func TestOwnerTextNULNonChatLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	type observation struct {
		result SubmitInputResult
		count  int
		body   *string
		before int32
		after  int32
	}
	for _, kind := range []string{"follow_up", "approve_plan", "revise_plan"} {
		t.Run(kind, func(t *testing.T) {
			submit := func(body string) observation {
				t.Helper()
				var run uuid.UUID
				if kind == "follow_up" {
					run = f.newRun("issue")
				} else {
					run = f.gatedRun()
				}
				var got observation
				if err := f.pool.QueryRow(f.ctx, `SELECT revise_count FROM runs WHERE id = $1`, run).Scan(&got.before); err != nil {
					t.Fatal(err)
				}
				var err error
				got.result, err = f.svc.SubmitInputWithOptions(f.ctx, f.userID, run, kind, body, nil, SubmitInputOptions{})
				if err != nil {
					t.Fatalf("SubmitInputWithOptions(%s, %q): %v", kind, body, err)
				}
				if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, run).Scan(&got.count); err != nil {
					t.Fatal(err)
				}
				if got.count != 1 {
					t.Fatalf("%s %q created %d rows, want one", kind, body, got.count)
				}
				if err := f.pool.QueryRow(f.ctx, `SELECT body FROM run_user_inputs WHERE run_id = $1`, run).Scan(&got.body); err != nil {
					t.Fatal(err)
				}
				if err := f.pool.QueryRow(f.ctx, `SELECT revise_count FROM runs WHERE id = $1`, run).Scan(&got.after); err != nil {
					t.Fatal(err)
				}
				wantAfter := got.before
				if kind == "revise_plan" {
					wantAfter++
				}
				if got.after != wantAfter {
					t.Fatalf("%s %q revise_count %d -> %d, want %d", kind, body, got.before, got.after, wantAfter)
				}
				if got.result.ServerSide || got.result.ID == 0 || got.result.CreatedAt.IsZero() {
					t.Fatalf("%s %q response = %+v, want enqueued row", kind, body, got.result)
				}
				return got
			}
			assertSame := func(name string, control, withNUL string) {
				t.Helper()
				want := submit(control)
				got := submit(withNUL)
				if got.result.ServerSide != want.result.ServerSide ||
					(got.result.ID != 0) != (want.result.ID != 0) ||
					got.result.CreatedAt.IsZero() != want.result.CreatedAt.IsZero() ||
					len(got.result.ExcludedGuardRoles) != len(want.result.ExcludedGuardRoles) ||
					got.count != want.count || got.before != want.before || got.after != want.after {
					t.Fatalf("%s: NUL response/state %+v differs from control %+v", name, got, want)
				}
				if (got.body == nil) != (want.body == nil) ||
					(got.body != nil && *got.body != *want.body) {
					t.Fatalf("%s: NUL body %v differs from control %v", name, got.body, want.body)
				}
				if control == "" && got.body != nil {
					t.Fatalf("%s: empty body must be SQL NULL, got %q", name, *got.body)
				}
				if control != "" && (got.body == nil || *got.body != control) {
					t.Fatalf("%s: want exact persisted body %q, got %v", name, control, got.body)
				}
			}
			assertSame("embedded", "beforeafter", "before\x00after")
			assertSame("all NUL", "", "\x00\x00")
			assertSame("NUL and whitespace", " \t\n ", "\x00 \t\x00\n \x00")
		})
	}
}

func TestOwnerTextNULCreateChatRunLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	check := func(input, want, wantTitle string) {
		t.Helper()
		run, err := f.svc.CreateChatRun(f.ctx, f.userID, input)
		if err != nil {
			t.Fatalf("CreateChatRun(%q): %v", input, err)
		}
		var description, issueTitle, title, body string
		var count int
		if err := f.pool.QueryRow(f.ctx, `SELECT issue_description, issue_title, title FROM runs WHERE id = $1`, run.ID).
			Scan(&description, &issueTitle, &title); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*), min(body) FROM run_user_inputs WHERE run_id = $1 AND kind = 'follow_up'`, run.ID).
			Scan(&count, &body); err != nil {
			t.Fatal(err)
		}
		if description != want || body != want || count != 1 {
			t.Fatalf("chat fields description=%q body=%q count=%d, want %q and one seed", description, body, count, want)
		}
		if issueTitle != wantTitle || title != wantTitle {
			t.Fatalf("chat titles issue=%q title=%q, want %q", issueTitle, title, wantTitle)
		}
	}
	check(" \x00hello\x00 world\x00 ", "hello world", "hello world")
	check("hello world", "hello world", "hello world")
	check(strings.Repeat("a", MaxChatMessageBytes)+"\x00", strings.Repeat("a", MaxChatMessageBytes), strings.Repeat("a", 80)+"…")
	var runsBefore, inputsBefore int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND kind = 'chat'`, f.userID).Scan(&runsBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id IN (SELECT id FROM runs WHERE user_id = $1 AND kind = 'chat')`, f.userID).Scan(&inputsBefore); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\x00\x00", "\x00 \t\x00"} {
		if _, err := f.svc.CreateChatRun(f.ctx, f.userID, input); !errors.Is(err, ErrEmptyChatMessage) {
			t.Fatalf("CreateChatRun(%q) error = %v, want empty message", input, err)
		}
	}
	if _, err := f.svc.CreateChatRun(f.ctx, f.userID, strings.Repeat("a", MaxChatMessageBytes+1)+"\x00"); !errors.Is(err, ErrChatMessageTooLarge) {
		t.Fatalf("over-limit stripped chat error = %v, want size limit", err)
	}
	var runsAfter int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND kind = 'chat'`, f.userID).Scan(&runsAfter); err != nil {
		t.Fatal(err)
	}
	if runsAfter != runsBefore {
		t.Fatalf("rejected chat creation changed run count from %d to %d", runsBefore, runsAfter)
	}
	var inputsAfter int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id IN (SELECT id FROM runs WHERE user_id = $1 AND kind = 'chat')`, f.userID).Scan(&inputsAfter); err != nil {
		t.Fatal(err)
	}
	if inputsAfter != inputsBefore {
		t.Fatalf("rejected chat creation changed input count from %d to %d", inputsBefore, inputsAfter)
	}
}

func TestOwnerTextNULSubmitChatMessageLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("chat")
	check := func(input, want string) {
		t.Helper()
		var before int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, run).Scan(&before); err != nil {
			t.Fatal(err)
		}
		res, err := f.svc.SubmitChatMessage(f.ctx, f.userID, run, input)
		if err != nil {
			t.Fatalf("SubmitChatMessage(%q): %v", input, err)
		}
		if res.ServerSide {
			t.Fatalf("SubmitChatMessage(%q) was server-side", input)
		}
		var body, kind string
		var after int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, run).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(f.ctx, `SELECT kind, body FROM run_user_inputs WHERE run_id = $1 ORDER BY id DESC LIMIT 1`, run).Scan(&kind, &body); err != nil {
			t.Fatal(err)
		}
		if after != before+1 {
			t.Fatalf("successful chat submission changed input count from %d to %d, want +1", before, after)
		}
		if kind != "follow_up" || body != want {
			t.Fatalf("persisted chat row kind=%q body=%q, want follow_up %q", kind, body, want)
		}
	}
	check(" \x00hello\x00 world\x00 ", "hello world")
	check("hello world", "hello world")
	check(strings.Repeat("a", MaxChatMessageBytes)+"\x00", strings.Repeat("a", MaxChatMessageBytes))
	var before int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, run).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\x00\x00", "\x00 \t\x00"} {
		if _, err := f.svc.SubmitChatMessage(f.ctx, f.userID, run, input); !errors.Is(err, ErrEmptyChatMessage) {
			t.Fatalf("SubmitChatMessage(%q) error = %v, want empty message", input, err)
		}
	}
	if _, err := f.svc.SubmitChatMessage(f.ctx, f.userID, run, strings.Repeat("a", MaxChatMessageBytes+1)+"\x00"); !errors.Is(err, ErrChatMessageTooLarge) {
		t.Fatalf("over-limit stripped chat error = %v, want size limit", err)
	}
	var after int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1`, run).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("invalid chat messages created %d rows", after-before)
	}
}
