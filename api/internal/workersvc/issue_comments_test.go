package workersvc

import "time"

func commentTS(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }
