package handlers

import (
	"context"
	"errors"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// statusClientClosed answers a request whose asker went away before the answer
// was ready (nginx's "client closed request"). Nobody reads it.
const statusClientClosed = 499

// requesterGone reports whether ctx, a request's own context or one derived
// from it, was cancelled: the browser dropped the request (the Explorer's Stop
// aborts its fetch), or the in-app assistant's Stop cancelled the question
// that agentgw runs these handlers on. That is the user's own action, not a
// failure of the work it interrupted. A deadline is not this: a timeout stays
// a failure.
func requesterGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}

// answerRequesterGone logs at info and answers 499 when the request's asker
// went away, and reports whether it did; the caller then returns. Call it
// first in the failure branch of work done on the request's context, so a
// Stop is never logged as an error or counted as a 5xx.
func answerRequesterGone(c *gin.Context, where string, err error) bool {
	if !requesterGone(c.Request.Context()) {
		return false
	}
	log.WithError(err).Infof("[%s] stopped: the request was cancelled by its asker", where)
	c.JSON(statusClientClosed, gin.H{"error": "cancelled"})
	return true
}
