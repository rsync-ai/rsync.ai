package main

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// modelStatementMax bounds one model statement sent to /agent/explorer-query with
// model_run: the api-gateway's whole model run is capped at 30 m (modelRunTimeout), so
// no single statement of it can need longer.
const modelStatementMax = 30 * time.Minute

// modelStatementMargin is how much longer the orchestrator waits than the connector is
// told to: the warehouse's own timeout should fire first, so the error the user sees
// names the statement's timeout rather than a dropped call.
const modelStatementMargin = 30 * time.Second

// modelStatementContext is the context one model statement runs under. A model build
// runs for minutes, past the server's WriteTimeout (120 s), which would drop the
// response the gateway waits for; so the write deadline is lifted, for this request
// only, to past the statement's own deadline. ReadTimeout needs nothing: net/http
// clears the read deadline once the body is read. It returns the statement budget in
// whole seconds, for the connector's timeout_seconds.
func modelStatementContext(c *gin.Context, timeoutSeconds int) (context.Context, context.CancelFunc, int) {
	budget := time.Duration(timeoutSeconds) * time.Second
	if budget <= 0 || budget > modelStatementMax {
		budget = modelStatementMax
	}
	wait := budget + modelStatementMargin
	rc := http.NewResponseController(c.Writer)
	if err := rc.SetWriteDeadline(time.Now().Add(wait + time.Minute)); err != nil {
		log.Warnf("model statement: could not lift the write deadline, a long statement's response may be lost at the server's WriteTimeout: %v", err)
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), wait)
	return ctx, cancel, int(budget / time.Second)
}
