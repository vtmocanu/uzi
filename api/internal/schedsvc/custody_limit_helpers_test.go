package schedsvc

import (
	"context"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// defaultCustodyLimitReader is the workersvc custody limit reader for a scheduler test whose
// service claims: the settings default, parsed rather than restated.
type defaultCustodyLimitReader struct{}

var _ workersvc.CustodyHoldLimitReader = defaultCustodyLimitReader{}

func (defaultCustodyLimitReader) CustodyHoldLimit(context.Context) (int32, error) {
	n, err := strconv.ParseInt(settings.DefaultCustodyHoldLimit, 10, 32)
	return int32(n), err
}
