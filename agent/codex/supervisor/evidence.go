package main

import (
	"encoding/json"
	"fmt"
	"io"
)

// drainResult states and the fixed authority/reason vocabulary the wire protocol
// pins. "drained" is the ONLY success; a deadline or an ECHILD contradicted by
// remaining children is "unconfirmed", never "drained".
const (
	stateDrained             = "drained"
	stateUnconfirmed         = "unconfirmed"
	authorityECHILD          = "ECHILD+__WALL"
	reasonDeadline           = "deadline"
	reasonEchildContradicted = "echild-contradicted"
)

// drainResult is the outcome of one drain/dispose attempt, shared by the dispose
// evidence event and the cleanup object embedded in an abnormal event.
type drainResult struct {
	State     string
	Authority string
	Reason    string
	Killed    []int
	Reaped    []int
	Children  []int
}

// fields renders the drain result to the exact wire shape. killed/reaped are
// always present (as [] when empty, never null); authority appears only for a
// drained result; reason and children appear only for an unconfirmed one.
func (d drainResult) fields() map[string]any {
	m := map[string]any{
		"state":  d.State,
		"killed": intSlice(d.Killed),
		"reaped": intSlice(d.Reaped),
	}
	if d.Authority != "" {
		m["authority"] = d.Authority
	}
	if d.State == stateUnconfirmed {
		m["reason"] = d.Reason
		m["children"] = intSlice(d.Children)
	}
	return m
}

// startedEvidence is the fixed-shape "started" event proving the established,
// verified posture at the moment the child was launched.
type startedEvidence struct {
	Event          string `json:"event"`
	SupervisorPid  int    `json:"supervisorPid"`
	ChildPid       int    `json:"childPid"`
	Subreaper      bool   `json:"subreaper"`
	Nondumpable    bool   `json:"nondumpable"`
	UID            int    `json:"uid"`
	LiveCapsZero   bool   `json:"liveCapsZero"`
	CapBoundingSet string `json:"capBoundingSet"`
	NoNewPrivs     bool   `json:"noNewPrivs"`
}

// procRow is one sanitized process record: pid/ppid/pgid and comm ONLY. comm is
// read from /proc/<pid>/comm (<=15 chars) and is NEVER argv/cmdline.
type procRow struct {
	Pid  int    `json:"pid"`
	PPid int    `json:"ppid"`
	Pgid int    `json:"pgid"`
	Comm string `json:"comm"`
}

// snapshotEvidence is the fixed-shape "snapshot" event.
type snapshotEvidence struct {
	Event     string    `json:"event"`
	ID        int       `json:"id"`
	Processes []procRow `json:"processes"`
}

// childExitEvidence is emitted exactly once when the supervised primary child
// exits. The status is normalized to the conventional shell code (128+signal
// for a signal death), so callers never need raw WaitStatus details.
type childExitEvidence struct {
	Event string `json:"event"`
	Code  int    `json:"code"`
}

// disposeEvidence builds the "dispose" event for a completed drain attempt.
func disposeEvidence(id int, d drainResult) map[string]any {
	m := d.fields()
	m["event"] = opDispose
	m["id"] = id
	return m
}

// abnormalEvidence builds an "abnormal" event. cleanup is nil for a pre-fork
// failure (profile / bad argv), where there is nothing to drain; otherwise it
// carries the best-effort drain result, kept SEPARATE from the primary failure.
func abnormalEvidence(reason string, cleanup *drainResult) map[string]any {
	m := map[string]any{"event": "abnormal", "reason": reason}
	if cleanup != nil {
		m["cleanup"] = cleanup.fields()
	}
	return m
}

// evidence is the bounded writer for fd 4: it caps the total line count and the
// per-line byte length so no path can turn evidence into an unbounded channel.
type evidence struct {
	w     io.Writer
	count int
}

// writeJSON marshals v to one NDJSON line, enforcing the count and size bounds.
// Beyond maxEvidenceLines it silently drops (the channel is spent); an
// over-length line is an error the caller may surface.
func (e *evidence) writeJSON(v any) error {
	if e.count >= maxEvidenceLines {
		return nil
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > maxEvidenceBytes {
		return fmt.Errorf("evidence line exceeds %d bytes", maxEvidenceBytes)
	}
	e.count++
	_, err = e.w.Write(append(payload, '\n'))
	return err
}

// intSlice normalizes a nil slice to an empty one so JSON renders [] not null.
func intSlice(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}
