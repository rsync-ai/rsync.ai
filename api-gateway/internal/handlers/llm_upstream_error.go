package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"
)

// respondLLMUpstreamFailed answers a failed llm-service call with fixed text,
// the upstream status and the request's trace id. It never returns the
// llm-service body: a FastAPI 422 quotes the request it rejected, an older
// llm-service quotes the provider's error, and text shown in the UI can be
// pasted into a chat and sent to a model. The body goes to the log, scrubbed,
// under the same trace id.
func respondLLMUpstreamFailed(c *gin.Context, where string, upstreamStatus int, body []byte, publicMessage string) {
	traceID := c.GetString("trace_id")
	log.WithFields(log.Fields{
		"upstream_status": upstreamStatus,
		"trace_id":        traceID,
	}).Errorf("[%s] llm-service failed: %s", where, llmscrub.ScrubMax(string(body), 500))
	c.JSON(browserStatusForUpstream(upstreamStatus), gin.H{
		"error":           publicMessage,
		"upstream_status": upstreamStatus,
		"trace_id":        traceID,
	})
}
