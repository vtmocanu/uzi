package schedsvc

import (
	"github.com/vtmocanu/uzi/api/internal/poller"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Both automatic consumers must agree with workersvc's harness-selection seams.
// Scheduled creation additionally carries schedule wait/model/override defaults.
var (
	_ RunCreator        = (*workersvc.Service)(nil)
	_ poller.RunStarter = (*workersvc.Service)(nil)
)
