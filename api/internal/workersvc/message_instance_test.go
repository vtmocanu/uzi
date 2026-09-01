package workersvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// instanceBroadcaster records the full PublishMessage argument list, unlike the
// seq-only fakeBroadcaster in service_test.go — the point here is what the LIVE
// frame carries, which is the only thing that lets the browser lane a message
// without a REST re-read (PRD #99 Decision 6).
type instanceBroadcaster struct {
	instances []string
	labels    []string
}

func (b *instanceBroadcaster) PublishMessage(_ uuid.UUID, _ int32, _, _, agentInstance, agentLabel string, _ []byte, _ time.Time) {
	b.instances = append(b.instances, agentInstance)
	b.labels = append(b.labels, agentLabel)
}
func (b *instanceBroadcaster) PublishState(uuid.UUID, string)                {}
func (b *instanceBroadcaster) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *instanceBroadcaster) PublishInput(uuid.UUID)                        {}

// TestAppendMessagesCarriesInstanceAndLabel proves the two PRD #99 fields survive
// the worker→store hop with the empty-string→SQL-NULL contract intact: a lead
// frame (neither field) must insert pgtype.Text{Valid:false}, NOT a valid empty
// string, because every downstream consumer's role-name fallback keys off Valid.
func TestAppendMessagesCarriesInstanceAndLabel(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"t":"delegating"}`)},
		{Seq: 2, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_A", AgentLabel: "web gate UX", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 3, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_B", AgentLabel: "API wiring", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 4, Kind: "text", Agent: "reviewer", AgentInstance: "toolu_C", Payload: json.RawMessage(`{"t":"no label"}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.insertedMessages) != 4 {
		t.Fatalf("expected 4 inserts, got %d", len(fs.insertedMessages))
	}
	lead := fs.insertedMessages[0]
	if lead.AgentInstance.Valid || lead.AgentLabel.Valid {
		t.Fatalf("a lead frame must insert NULL for both columns, got instance=%+v label=%+v",
			lead.AgentInstance, lead.AgentLabel)
	}
	a, b := fs.insertedMessages[1], fs.insertedMessages[2]
	if a.AgentInstance.String != "toolu_A" || !a.AgentInstance.Valid ||
		b.AgentInstance.String != "toolu_B" || !b.AgentInstance.Valid {
		t.Fatalf("parallel same-role instances must persist distinctly, got %+v / %+v",
			a.AgentInstance, b.AgentInstance)
	}
	if a.AgentLabel.String != "web gate UX" || b.AgentLabel.String != "API wiring" {
		t.Fatalf("labels must persist verbatim, got %q / %q", a.AgentLabel.String, b.AgentLabel.String)
	}
	if noLabel := fs.insertedMessages[3]; !noLabel.AgentInstance.Valid || noLabel.AgentLabel.Valid {
		t.Fatalf("an instance frame with no task_description must persist instance-valid/label-NULL, got %+v / %+v",
			noLabel.AgentInstance, noLabel.AgentLabel)
	}
}

// TestIncomingMessageDecodesWorkerJSONKeys pins the worker→API JSON key names.
// This is the ONE seam in the PRD #99 wire with no other guard: both ends are
// mutation-tested in isolation, but nothing else asserts that IncomingMessage's
// struct tags match what agent/src/protocol.ts's OutgoingMessage actually sends.
//
// A tag drift here is the worst failure mode in the whole design: both columns go
// NULL forever, and the pane degrades to a role-fallback lane — which is
// INDISTINGUISHABLE from "the SDK didn't provide it", the documented healthy
// degradation. It would not look like a bug. The e2e leg that would otherwise
// catch it is deferred until PRD #97 lands, so until then this test is the only
// thing standing between a renamed tag and silent permanent data loss.
//
// The literal below is the exact shape the batcher emits (batcher.ts builds
// `{seq, kind, agent, agent_instance, agent_label, payload}`); keep them in sync
// BY HAND — that is the point of the test.
func TestIncomingMessageDecodesWorkerJSONKeys(t *testing.T) {
	const workerPayload = `{"seq":1,"kind":"tool_use","agent":"coder",` +
		`"agent_instance":"toolu_A","agent_label":"web gate UX","payload":{"name":"Edit"}}`

	var m IncomingMessage
	if err := json.Unmarshal([]byte(workerPayload), &m); err != nil {
		t.Fatalf("a real worker payload must decode into IncomingMessage: %v", err)
	}
	if m.AgentInstance != "toolu_A" {
		t.Fatalf(`the "agent_instance" JSON key must land on AgentInstance, got %q`, m.AgentInstance)
	}
	if m.AgentLabel != "web gate UX" {
		t.Fatalf(`the "agent_label" JSON key must land on AgentLabel, got %q`, m.AgentLabel)
	}
	// The pre-existing fields are asserted too: this test is the tag contract for
	// the whole struct, not just the two new members.
	if m.Seq != 1 || m.Kind != "tool_use" || m.Agent != "coder" {
		t.Fatalf("base fields mis-decoded: %+v", m)
	}

	// A lead frame omits both keys entirely (the batcher only sets them when the
	// SDK frame carried them). They must decode to "", which pgText maps to NULL.
	var lead IncomingMessage
	if err := json.Unmarshal([]byte(`{"seq":2,"kind":"text","agent":"lead","payload":{}}`), &lead); err != nil {
		t.Fatalf("a lead payload must decode: %v", err)
	}
	if lead.AgentInstance != "" || lead.AgentLabel != "" {
		t.Fatalf("omitted keys must decode to the empty string, got %q / %q",
			lead.AgentInstance, lead.AgentLabel)
	}
}

// TestAppendMessagesCapsAttributionFields proves the server-side bound on the
// UNTRUSTED worker insert path. The only other ceiling is httpx.DecodeJSON's
// 1 MiB LimitReader, which is PER BATCH, so without this one message could carry
// a ~1 MiB label — stored once per FRAME of that invocation (Decision 1 repeats
// the label on every frame). Truncate, never reject: a rejected batch is a lost
// message. Web-side truncation would not cover this — it does nothing for storage
// and nothing for the `uzi` CLI, which prints to a terminal.
func TestAppendMessagesCapsAttributionFields(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())
	b := &instanceBroadcaster{}
	svc.SetBroadcaster(b)

	long := strings.Repeat("x", 5000)
	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "coder", AgentInstance: long, AgentLabel: long, Payload: json.RawMessage(`{}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("an over-long label must be TRUNCATED, not rejected: %v", err)
	}
	got := fs.insertedMessages[0]
	if n := utf8.RuneCountInString(got.AgentLabel.String); n != maxAgentLabelRunes {
		t.Fatalf("agent_label capped to %d runes, want %d", n, maxAgentLabelRunes)
	}
	if n := utf8.RuneCountInString(got.AgentInstance.String); n != maxAgentInstanceRunes {
		t.Fatalf("agent_instance capped to %d runes, want %d", n, maxAgentInstanceRunes)
	}
	// The cap must reach the LIVE frame too, not just the stored row — otherwise
	// the browser renders a label the database does not hold.
	if n := utf8.RuneCountInString(b.labels[0]); n != maxAgentLabelRunes {
		t.Fatalf("the broadcast label is %d runes, want the same %d cap as the insert", n, maxAgentLabelRunes)
	}
}

// TestAppendMessagesCapCutsOnRuneBoundary is the half a byte-slice would get
// wrong. Byte-slicing multibyte text splits a rune into invalid UTF-8, which
// Postgres then REJECTS on insert — turning a cosmetic cap into a 500 and a lost
// batch. (handler/cli_auth_flow.go:148 documents the same trap.)
func TestAppendMessagesCapCutsOnRuneBoundary(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	// 3-byte runes: a byte cut lands mid-sequence for most offsets.
	label := strings.Repeat("日", 500)
	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "coder", AgentLabel: label, Payload: json.RawMessage(`{}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	got := fs.insertedMessages[0].AgentLabel.String
	if !utf8.ValidString(got) {
		t.Fatalf("the truncated label must stay valid UTF-8, got %q", got)
	}
	if n := utf8.RuneCountInString(got); n != maxAgentLabelRunes {
		t.Fatalf("multibyte label capped to %d runes, want %d", n, maxAgentLabelRunes)
	}
}

// TestAppendMessagesLeavesShortAttributionAlone guards the other direction: the
// cap must be inert for realistic values, or every lane title silently changes.
func TestAppendMessagesLeavesShortAttributionAlone(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "coder", AgentInstance: "toolu_A", AgentLabel: "web gate UX", Payload: json.RawMessage(`{}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	got := fs.insertedMessages[0]
	if got.AgentLabel.String != "web gate UX" || got.AgentInstance.String != "toolu_A" {
		t.Fatalf("a normal-length label must pass through verbatim, got %q / %q",
			got.AgentInstance.String, got.AgentLabel.String)
	}
}

// TestAppendMessagesBroadcastsInstanceAndLabel proves the LIVE frame carries both
// fields. Persisting them is not enough: useRunStream builds its RunMessage from
// the WS frame with no REST re-read, so a live subagent message lands in the
// wrong lane if the broadcast drops the id.
func TestAppendMessagesBroadcastsInstanceAndLabel(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgconv.UUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())
	b := &instanceBroadcaster{}
	svc.SetBroadcaster(b)

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"t":"hi"}`)},
		{Seq: 2, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_A", AgentLabel: "web gate UX", Payload: json.RawMessage(`{"tool":"Edit"}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(b.instances) != 2 {
		t.Fatalf("expected 2 broadcasts, got %d", len(b.instances))
	}
	if b.instances[0] != "" || b.labels[0] != "" {
		t.Fatalf("lead frame should broadcast empty instance/label, got %q / %q", b.instances[0], b.labels[0])
	}
	if b.instances[1] != "toolu_A" || b.labels[1] != "web gate UX" {
		t.Fatalf("subagent frame should broadcast its instance + label, got %q / %q", b.instances[1], b.labels[1])
	}
}
