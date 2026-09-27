package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// diskParkRun is a recovery_wait run parked because its worker's data volume is full or nearly full (PRD
// #1809 M5). Forge counters are set on purpose: this cause must never borrow the forge wording
// or its "N of MAX" count; its own count is DiskParkCount.
func diskParkRun(id string) apitypes.RunDTO {
	cause := dataVolumeFullCause
	retry := time.Now().Add(10 * time.Minute)
	return apitypes.RunDTO{
		ID: id, Kind: "issue", Status: statusRecoveryWait, IssueTitle: "parked", Health: "ok",
		RecoveryWaitCause: &cause, RecoveryRetryNotBefore: &retry, ForgeParkCount: 2, ForgeParkMax: 5,
		DiskParkCount: 2,
	}
}

// TestDiskParkLine pins the shared sentence: the retry clause carries local HH:MM, the park
// count reads "counted disk parks: N", the retry time is dropped without a stamp, and any other run
// (including a vault park) gets "". Reddening mutation: drop the count clause, or key
// isDiskFullPark on the cause alone.
func TestDiskParkLine(t *testing.T) {
	r := diskParkRun("r1")
	want := "waiting for disk space: the worker's data volume is full or nearly full; uzi frees space on the worker and the run resumes at its next retry (" +
		r.RecoveryRetryNotBefore.Local().Format("15:04") + "); counted disk parks: 2"
	if got := diskParkLine(r); got != want {
		t.Errorf("diskParkLine = %q, want %q", got, want)
	}
	if strings.Contains(diskParkLine(r), "of 5") {
		t.Errorf("diskParkLine borrowed the forge cap: %q", diskParkLine(r))
	}
	r.RecoveryRetryNotBefore = nil
	if got := diskParkLine(r); !strings.HasSuffix(got, "resumes at its next retry; counted disk parks: 2") {
		t.Errorf("diskParkLine(no stamp) = %q, want the retry clause without a time", got)
	}
	running := diskParkRun("r1")
	running.Status = "running"
	for name, other := range map[string]apitypes.RunDTO{
		"untyped park": {Status: statusRecoveryWait},
		"vault park":   vaultParkRun("r2"),
		"running":      running,
	} {
		if got := diskParkLine(other); got != "" {
			t.Errorf("diskParkLine(%s) = %q, want \"\"", name, got)
		}
	}
}

// TestFitDiskParkLine pins the shedding order: full, then "waiting for disk space: resumes at
// its next retry (HH:MM); counted disk parks: N", then the floor "waiting for disk space · retry
// HH:MM", which is never cut. Reddening mutation: return diskParkLine unshed.
func TestFitDiskParkLine(t *testing.T) {
	r := diskParkRun("r1")
	hhmm := r.RecoveryRetryNotBefore.Local().Format("15:04")
	full := diskParkLine(r)
	short := "waiting for disk space: resumes at its next retry (" + hhmm + "); counted disk parks: 2"
	floor := "waiting for disk space · retry " + hhmm
	for _, tc := range []struct {
		width int
		want  string
	}{
		{visualWidth(full), full},
		{visualWidth(full) - 1, short},
		{visualWidth(short), short},
		{visualWidth(short) - 1, floor},
		{10, floor},
	} {
		if got := fitDiskParkLine(r, tc.width); got != tc.want {
			t.Errorf("fitDiskParkLine(width %d) = %q, want %q", tc.width, got, tc.want)
		}
	}
	r.RecoveryRetryNotBefore = nil
	if got := fitDiskParkLine(r, 30); got != "waiting for disk space" {
		t.Errorf("fitDiskParkLine(no stamp, 30) = %q, want the bare lead", got)
	}
	if got := fitDiskParkLine(vaultParkRun("r2"), 200); got != "" {
		t.Errorf("fitDiskParkLine(vault park) = %q, want \"\"", got)
	}
}

// TestSteerStateDataVolumeFull: a steer row on a disk park names the disk space it waits for.
// Reddening mutation: drop the dataVolumeFullCause arm in steerState.
func TestSteerStateDataVolumeFull(t *testing.T) {
	consumed := time.Now()
	if got, want := steerState(kindFollowUp, nil, nil, statusRecoveryWait, dataVolumeFullCause), "queued (run waiting for disk space)"; got != want {
		t.Errorf("steerState(unconsumed, data_volume_full) = %q, want %q", got, want)
	}
	if got, want := steerState(kindFollowUp, &consumed, nil, statusRecoveryWait, dataVolumeFullCause), "delivered (run waiting for disk space)"; got != want {
		t.Errorf("steerState(consumed, data_volume_full) = %q, want %q", got, want)
	}
}

// TestStateGlyphWordDataVolumeFull pins the TUI token "~ disk wait" in the wait family, fitting
// the board's status-word cell. Reddening mutation: drop the dataVolumeFullCause arm in
// stateGlyphWord (it falls back to "recovery wait").
func TestStateGlyphWordDataVolumeFull(t *testing.T) {
	glyph, word := stateGlyphWord(statusRecoveryWait, "", false, false, "", dataVolumeFullCause)
	if glyph != "~" || word != "disk wait" {
		t.Errorf("data_volume_full token = (%q, %q), want (~, disk wait)", glyph, word)
	}
	if n := len([]rune(word)); n >= boardStatusWordWidth {
		t.Errorf("data_volume_full word %q is %d runes, does not fit boardStatusWordWidth %d", word, n, boardStatusWordWidth)
	}
	if got := runStateWord(apitypes.RunListItemDTO{RunDTO: diskParkRun("r1")}); got != "disk wait" {
		t.Errorf("runStateWord(data_volume_full) = %q, want %q", got, "disk wait")
	}
	p := newPalette(true)
	tok := p.runStateToken(diskParkRun("r1"), false)
	if tok.glyph != "~" || tok.word != "disk wait" || tok.color != p.wait {
		t.Errorf("runStateToken(data_volume_full) = (%q, %q, %v), want (~, disk wait, wait colour)", tok.glyph, tok.word, tok.color)
	}
}

// TestRenderRunDetailDiskRow: `uzi run get` prints the DISK row for a disk park and no such row
// for any other recovery park. Reddening mutation: drop the DISK row in renderRunDetail.
func TestRenderRunDetailDiskRow(t *testing.T) {
	parked := diskParkRun("r1")
	out := renderDetailString(t, parked)
	if line := lineWith(t, out, "DISK"); !strings.Contains(line, diskParkLine(parked)) {
		t.Errorf("DISK row = %q, want it to carry %q", line, diskParkLine(parked))
	}
	if out := renderDetailString(t, apitypes.RunDTO{ID: "r2", Kind: "issue", Status: statusRecoveryWait}); strings.Contains(out, "DISK") {
		t.Errorf("an untyped recovery park gained a DISK row:\n%s", out)
	}
}

// TestRenderRunDetailFailOrigin: `uzi run get` prints FAIL_ORIGIN for a failed run that carries
// one, with the plain disk explanation and park count for data_volume_full, the raw enum for
// any other origin, and no row when the origin is null. Reddening mutation: drop the
// FAIL_ORIGIN row, or the data_volume_full arm in failOriginCell.
func TestRenderRunDetailFailOrigin(t *testing.T) {
	origin := dataVolumeFullCause
	failed := apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "failed", FailOrigin: &origin, DiskParkCount: 3}
	line := lineWith(t, renderDetailString(t, failed), "FAIL_ORIGIN")
	if want := "data_volume_full (the worker's data volume stayed full after 3 counted disk parks)"; !strings.Contains(line, want) {
		t.Errorf("FAIL_ORIGIN row = %q, want it to carry %q", line, want)
	}
	other := "forge_unreachable"
	failed.FailOrigin = &other
	if line := lineWith(t, renderDetailString(t, failed), "FAIL_ORIGIN"); !strings.Contains(line, "forge_unreachable") || strings.Contains(line, "disk") {
		t.Errorf("FAIL_ORIGIN row for forge_unreachable = %q", line)
	}
	failed.FailOrigin = nil
	if out := renderDetailString(t, failed); strings.Contains(out, "FAIL_ORIGIN") {
		t.Errorf("a run with no fail_origin gained a FAIL_ORIGIN row:\n%s", out)
	}
}

func renderDetailString(t *testing.T, r apitypes.RunDTO) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestRunTablesDiskStatusCell: `uzi run list` and `uzi admin runs` suffix a disk park's STATUS
// cell with "(waiting for disk space)". Reddening mutation: drop the isDiskFullPark arm in
// runStatusCell.
func TestRunTablesDiskStatusCell(t *testing.T) {
	rows := []apitypes.RunListItemDTO{
		{RunDTO: diskParkRun("disk-1")},
		{RunDTO: apitypes.RunDTO{ID: "plain-2", Kind: "issue", Status: "running", IssueTitle: "plain"}},
	}
	for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
		fc := &uzicli.FakeClient{Runs: rows, AdminRuns: rows}
		out, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("%v: exit = %d, stderr:\n%s", args, code, stderr)
		}
		if line := lineWith(t, out, "disk-1"); !strings.Contains(line, "recovery_wait (waiting for disk space)") {
			t.Errorf("%v: data_volume_full row lacks the disk suffix: %q", args, line)
		}
		if line := lineWith(t, out, "plain-2"); strings.Contains(line, "disk") {
			t.Errorf("%v: an unparked row gained a disk suffix: %q", args, line)
		}
	}
}

// TestRunLogsFollowDiskParkNotice: `run logs --follow` rides out a disk park and prints its
// one-shot stderr notice with the retry time and park count, not the transient or forge
// wording. Reddening mutation: drop the diskParkLine branch in run_get.go.
func TestRunLogsFollowDiskParkNotice(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	defer func() { logsPollInterval = old }()

	parked := diskParkRun("r1")
	pf := &codexParkFake{
		FakeClient: &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
			"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
		}},
		seq: []apitypes.RunDTO{
			{ID: "r1", Status: "running"},
			parked, parked, parked,
			{ID: "r1", Status: "completed"},
		},
	}
	var out, errBuf bytes.Buffer
	env := fakeEnv(pf)
	env.Stdout, env.Stderr = &out, &errBuf
	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()
	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow hung on a data_volume_full park")
	}
	stderr := errBuf.String()
	want := "run r1 " + diskParkLine(parked) + "; still following\n"
	if n := strings.Count(stderr, want); n != 1 {
		t.Errorf("data_volume_full notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "transient interruption") || strings.Contains(stderr, "waiting for the forge") {
		t.Errorf("a data_volume_full park got the transient-recovery or forge wording:\n%s", stderr)
	}
}

// TestDiskParkBoardSecondLine: the selected disk park row draws the disk park line as its
// second line, shed so the retry HH:MM survives at 60 and 80 columns, and the frame stays in
// bounds. Reddening mutations: drop the fitDiskParkLine branch in boardSecondLine, or the
// diskParkLine term in boardShowSecondLine (the frame overflows by one row).
func TestDiskParkBoardSecondLine(t *testing.T) {
	for _, width := range []int{60, 80, 200} {
		runs := []apitypes.RunListItemDTO{{RunDTO: diskParkRun("00000000-disk")}}
		for i := 0; i < 40; i++ {
			id := "1" + strings.Repeat("0", 6) + string(rune('a'+i%26)) + "-run"
			runs = append(runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: id, Kind: "issue", Status: "running", IssueTitle: "busy", Health: "ok"}})
		}
		m := tuiTestModel(t, &uzicli.FakeClient{}, "")
		m.width, m.height = width, 20
		m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
		if !m.boardShowSecondLine(m.board.runs[0]) {
			t.Errorf("width %d: boardShowSecondLine(disk park) = false", width)
		}
		out := stripANSI(m.View().Content)
		line := lineWith(t, out, "▸ waiting for disk space")
		assertDiskRetryShown(t, "board", width, runs[0].RunDTO, line)
		rows := strings.Split(out, "\n")
		for i, row := range rows {
			if w := visualWidth(row); w > width {
				t.Errorf("width %d: row %d is %d columns wide: %q", width, i, w, row)
			}
		}
		if len(rows) > m.height {
			t.Errorf("width %d: board frame is %d rows, want at most %d", width, len(rows), m.height)
		}
	}
}

// TestDiskParkDetailFitsTerminal: the TUI run detail draws the disk park line, keeps the retry
// HH:MM at every width, and the frame is exactly m.height rows. Reddening mutations: drop the
// fitDiskParkLine branch in renderDetail, or the diskParkLine term in transcriptViewport (the
// frame comes to m.height+1 rows).
func TestDiskParkDetailFitsTerminal(t *testing.T) {
	now := time.Now()
	for _, width := range []int{60, 80, 200} {
		run := diskParkRun("77777777-disk")
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m = applyDetail(m, run, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", now)})
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
		out := stripANSI(m.View().Content)
		assertDiskRetryShown(t, "detail", width, run, lineWith(t, out, "waiting for disk space"))
		rows := strings.Split(out, "\n")
		for i, row := range rows {
			if w := visualWidth(row); w > width {
				t.Errorf("width %d: row %d is %d columns wide: %q", width, i, w, row)
			}
		}
		if len(rows) != m.height {
			t.Errorf("width %d: detail frame is %d rows, want exactly %d\n%s", width, len(rows), m.height, out)
		}
	}
}

func assertDiskRetryShown(t *testing.T, surface string, width int, r apitypes.RunDTO, row string) {
	t.Helper()
	if hhmm := r.RecoveryRetryNotBefore.Local().Format("15:04"); !strings.Contains(row, hhmm) {
		t.Errorf("%s width %d: disk park row lacks the retry time %s: %q", surface, width, hhmm, row)
	}
	if width >= 200 {
		if full := diskParkLine(r); !strings.Contains(row, full) {
			t.Errorf("%s width %d: disk park row lacks the full sentence %q: %q", surface, width, full, row)
		}
	}
}
