package main

import "testing"

const sampleStatus = `Name:	codex-supervisor
Umask:	0022
State:	R (running)
Tgid:	7
Pid:	7
PPid:	1
Uid:	10002	10002	10002	10002
Gid:	10002	10002	10002	10002
NoNewPrivs:	1
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	0000000000000000
CapAmb:	0000000000000000
Seccomp:	0
`

func TestParseProcStatusValid(t *testing.T) {
	st, err := parseProcStatus(sampleStatus)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.UID != 10002 {
		t.Errorf("UID = %d, want 10002 (effective)", st.UID)
	}
	if st.NoNewPrivs != 1 {
		t.Errorf("NoNewPrivs = %d, want 1", st.NoNewPrivs)
	}
	for name, got := range map[string]uint64{
		"CapInh": st.CapInh, "CapPrm": st.CapPrm, "CapEff": st.CapEff,
		"CapBnd": st.CapBnd, "CapAmb": st.CapAmb,
	} {
		if got != 0 {
			t.Errorf("%s = %#x, want 0", name, got)
		}
	}
}

func TestParseProcStatusEffectiveUID(t *testing.T) {
	// Effective uid is the SECOND field; a differing real uid must not be read.
	status := `Uid:	0	10002	0	0
NoNewPrivs:	1
CapInh:	0
CapPrm:	0
CapEff:	0
CapBnd:	0
CapAmb:	0
`
	st, err := parseProcStatus(status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.UID != 10002 {
		t.Errorf("UID = %d, want the effective 10002", st.UID)
	}
}

func TestParseProcStatusCapBndResidue(t *testing.T) {
	status := `Uid:	10002	10002	10002	10002
NoNewPrivs:	1
CapInh:	0000000000000000
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	00000000000000c0
CapAmb:	0000000000000000
`
	st, err := parseProcStatus(status)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if st.CapBnd != 0xc0 {
		t.Errorf("CapBnd = %#x, want 0xc0", st.CapBnd)
	}
}

func TestParseProcStatusFailsClosed(t *testing.T) {
	// Missing CapEff must be an error, never a permissive zero default.
	cases := map[string]string{
		"missing CapEff": `Uid:	10002	10002	10002	10002
NoNewPrivs:	1
CapInh:	0
CapPrm:	0
CapBnd:	0
CapAmb:	0
`,
		"missing Uid": `NoNewPrivs:	1
CapInh:	0
CapPrm:	0
CapEff:	0
CapBnd:	0
CapAmb:	0
`,
		"missing NoNewPrivs": `Uid:	10002	10002	10002	10002
CapInh:	0
CapPrm:	0
CapEff:	0
CapBnd:	0
CapAmb:	0
`,
		"empty": ``,
	}
	for name, text := range cases {
		if _, err := parseProcStatus(text); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

func okStatus() procStatus {
	return procStatus{UID: 10002, NoNewPrivs: 1}
}

func TestEvaluateProfilePass(t *testing.T) {
	if ok, field := evaluateProfile(okStatus(), 10002, true, true); !ok {
		t.Errorf("expected pass, failed on %q", field)
	}
	// CapBnd == 0xc0 residue is accepted.
	st := okStatus()
	st.CapBnd = capBndSetuidSetgidResidue
	if ok, field := evaluateProfile(st, 10002, true, true); !ok {
		t.Errorf("expected pass with 0xc0 CapBnd, failed on %q", field)
	}
}

func TestEvaluateProfileFailures(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*procStatus)
		subreaper   bool
		nondumpable bool
		expectUID   int
		wantField   string
	}{
		{"subreaper", func(*procStatus) {}, false, true, 10002, "subreaper"},
		{"nondumpable", func(*procStatus) {}, true, false, 10002, "nondumpable"},
		{"uid", func(*procStatus) {}, true, true, 999, "uid"},
		{"capEff", func(s *procStatus) { s.CapEff = 1 }, true, true, 10002, "capEff"},
		{"capPrm", func(s *procStatus) { s.CapPrm = 1 }, true, true, 10002, "capPrm"},
		{"capInh", func(s *procStatus) { s.CapInh = 1 }, true, true, 10002, "capInh"},
		{"capAmb", func(s *procStatus) { s.CapAmb = 1 }, true, true, 10002, "capAmb"},
		{"capBnd-nonresidue", func(s *procStatus) { s.CapBnd = 0x200 }, true, true, 10002, "capBnd"},
		{"noNewPrivs", func(s *procStatus) { s.NoNewPrivs = 0 }, true, true, 10002, "noNewPrivs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := okStatus()
			tc.mutate(&st)
			ok, field := evaluateProfile(st, tc.expectUID, tc.subreaper, tc.nondumpable)
			if ok {
				t.Fatalf("expected failure on %q, got ok", tc.wantField)
			}
			if field != tc.wantField {
				t.Errorf("field = %q, want %q", field, tc.wantField)
			}
		})
	}
}
