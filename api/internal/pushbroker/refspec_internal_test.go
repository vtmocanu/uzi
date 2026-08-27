package pushbroker

import (
	"strings"
	"testing"
)

// TestPushRefSpecNotForced pins the never-forced invariant at the WIRE level: the
// checkpoint push refspec must carry no leading '+'. The strict-descendant check in
// Publish is only a local guard — a '+' reintroduced here would let a forced push
// overwrite origin's checkpoint while every ancestry-based test stayed green, so this
// asserts the literal refspec directly.
func TestPushRefSpecNotForced(t *testing.T) {
	rs := pushRefSpec("refs/uzi-checkpoints/agent/issue-7")
	if strings.HasPrefix(rs.String(), "+") {
		t.Fatalf("push refspec %q is FORCED (leading +); checkpoint push must be non-forced", rs.String())
	}
	if rs.IsForceUpdate() {
		t.Fatalf("push refspec %q reports force-update; checkpoint push must be non-forced", rs.String())
	}
}
