package main

import (
	"fmt"
	"strconv"
	"strings"
)

// procStatus is the subset of /proc/self/status the pre-fork profile check needs.
// All capability sets are the decoded 64-bit hex masks; UID is the EFFECTIVE uid
// (the second field of the Uid line).
type procStatus struct {
	UID        int
	CapInh     uint64
	CapPrm     uint64
	CapEff     uint64
	CapBnd     uint64
	CapAmb     uint64
	NoNewPrivs int
}

// parseProcStatus decodes the fields of /proc/self/status that the profile
// decision consumes. It is pure so it can be unit-tested without root/fork.
//
// It FAILS CLOSED: a missing Uid, NoNewPrivs, or any of the five capability
// lines is an error, so an absent field can never be silently read as a
// permissive zero.
func parseProcStatus(text string) (procStatus, error) {
	var st procStatus
	var (
		sawUID, sawNNP                         bool
		sawInh, sawPrm, sawEff, sawBnd, sawAmb bool
	)
	for _, line := range strings.Split(text, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Uid":
			fields := strings.Fields(val)
			if len(fields) < 2 {
				return st, fmt.Errorf("malformed Uid line")
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return st, fmt.Errorf("malformed effective uid")
			}
			st.UID, sawUID = n, true
		case "CapInh":
			v, err := parseCapHex(val)
			if err != nil {
				return st, err
			}
			st.CapInh, sawInh = v, true
		case "CapPrm":
			v, err := parseCapHex(val)
			if err != nil {
				return st, err
			}
			st.CapPrm, sawPrm = v, true
		case "CapEff":
			v, err := parseCapHex(val)
			if err != nil {
				return st, err
			}
			st.CapEff, sawEff = v, true
		case "CapBnd":
			v, err := parseCapHex(val)
			if err != nil {
				return st, err
			}
			st.CapBnd, sawBnd = v, true
		case "CapAmb":
			v, err := parseCapHex(val)
			if err != nil {
				return st, err
			}
			st.CapAmb, sawAmb = v, true
		case "NoNewPrivs":
			fields := strings.Fields(val)
			if len(fields) < 1 {
				return st, fmt.Errorf("malformed NoNewPrivs line")
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				return st, fmt.Errorf("malformed NoNewPrivs value")
			}
			st.NoNewPrivs, sawNNP = n, true
		}
	}
	if !(sawUID && sawNNP && sawInh && sawPrm && sawEff && sawBnd && sawAmb) {
		return st, fmt.Errorf("incomplete /proc status")
	}
	return st, nil
}

func parseCapHex(val string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed capability mask")
	}
	return v, nil
}

// capBndSetuidSetgidResidue is the documented inert SETUID|SETGID (bits 7|6)
// bounding-set residue of the real setpriv-to-runner path. It grants no live
// authority (the effective/permitted/inheritable/ambient sets are all zero and
// NoNewPrivs blocks re-acquiring), so it is accepted alongside a fully-empty
// bounding set (docker --cap-drop ALL).
const capBndSetuidSetgidResidue = 0xc0

// evaluateProfile is the pure fail-before-fork decision. Given the parsed status,
// the --expect-uid value, and whether subreaper+dumpable were established and
// CONFIRMED, it returns ok, or the first field that failed (for the
// "profile:<field>" abnormal reason). Every check is mandatory; no flag disables
// any of them.
func evaluateProfile(st procStatus, expectUID int, subreaper, dumpable bool) (ok bool, field string) {
	if !subreaper {
		return false, "subreaper"
	}
	if !dumpable {
		return false, "dumpable"
	}
	if st.UID != expectUID {
		return false, "uid"
	}
	if st.CapEff != 0 {
		return false, "capEff"
	}
	if st.CapPrm != 0 {
		return false, "capPrm"
	}
	if st.CapInh != 0 {
		return false, "capInh"
	}
	if st.CapAmb != 0 {
		return false, "capAmb"
	}
	if st.CapBnd != 0 && st.CapBnd != capBndSetuidSetgidResidue {
		return false, "capBnd"
	}
	if st.NoNewPrivs != 1 {
		return false, "noNewPrivs"
	}
	return true, ""
}
