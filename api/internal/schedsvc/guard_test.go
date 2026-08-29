package schedsvc

import (
	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #274 M1 structural guarantee: widening the scheduler's run-creation seam with
// CreateScheduledAutopilotRun must NOT change the poller's seam. *workersvc.Service must
// keep satisfying BOTH interfaces simultaneously — schedsvc.RunCreator (the widened seam)
// and poller.RunStarter (the label-autopilot seam, whose CreateAutopilotRun stays
// byte-identical). If someone "unifies" the two by changing CreateAutopilotRun's
// signature, poller.RunStarter stops being satisfied and this file fails to compile,
// catching the regression before the poller path can drift.
var (
	_ RunCreator        = (*workersvc.Service)(nil)
	_ poller.RunStarter = (*workersvc.Service)(nil)
)
