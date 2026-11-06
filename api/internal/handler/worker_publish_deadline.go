package handler

import (
	"errors"
	"net"
	"os"
	"time"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The /publish route (WorkerRunPublish) extends its own deadlines past the 15 s the base listeners
// impose, through http.NewResponseController (the worker_held_publication.go precedent):
//
//	upload    60 s  read deadline: the pack body read
//	service  workersvc.PublishServiceBound  PublishAttested from entry to return, including the
//	                detached retention work that can outlive its context
//	grace     15 s  writing the answer
//
// The write deadline is their sum, so the answer to a maximal upload followed by a maximal service
// still fits. publishContextBudget is the one context wrapped around the service call (it is the
// part of the service bound the context can cut short; publishServiceBound adds the tail that
// runs past it). The vars are named so a test can scale them, keeping the ratio.
var (
	publishUploadBudget  = 60 * time.Second
	publishContextBudget = workersvc.PublishContextBudget
	publishServiceBound  = workersvc.PublishServiceBound
	publishResponseGrace = 15 * time.Second
)

// isUploadTimeout reports a body read that ended on a connection read deadline.
func isUploadTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}
