package httpx

import "time"

// ServerReadTimeout and ServerWriteTimeout are the base http.Server ReadTimeout and
// WriteTimeout of both api listeners (cmd/server). A route that legitimately runs longer, such
// as the brokered checkpoint publish, extends its own deadlines per request through
// http.NewResponseController; tests scale these constants to exercise that extension.
const (
	ServerReadTimeout  = 15 * time.Second
	ServerWriteTimeout = 15 * time.Second
)
