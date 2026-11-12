package assistant

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Stop ends a question on the server. The browser names each question with a
// request_id and Stop posts POST /assistant/messages/:request_id/cancel: the
// browser's own abort is not enough, because a proxy between it and the
// gateway can keep the request open after the browser lets go.
//
// The replica that runs the question stops it at once. Any other replica marks
// the id stopped in the store, and the running one reads the mark within
// Limits.StopCheck. A stopped question runs no further model step or tool, and
// its answer, if one lands anyway, is not kept.

// errStopped is the cause of a question's context when Stop ended it.
var errStopped = errors.New("assistant: question stopped")

// statusStopped answers a question that ended because nobody is waiting for
// it (nginx's "client closed request").
const statusStopped = 499

func stopKey(userID, requestID string) string {
	return "assistant:stop:" + userID + ":" + requestID
}

// running is the questions this replica is answering, by stop key.
type running struct {
	mu    sync.Mutex
	stops map[string]context.CancelCauseFunc
}

// add registers a question; false when the key is already running.
func (r *running) add(key string, stop context.CancelCauseFunc) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.stops[key]; ok {
		return false
	}
	if r.stops == nil {
		r.stops = map[string]context.CancelCauseFunc{}
	}
	r.stops[key] = stop
	return true
}

func (r *running) remove(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stops, key)
}

// stop ends the question if this replica runs it.
func (r *running) stop(key string) bool {
	r.mu.Lock()
	stop, ok := r.stops[key]
	r.mu.Unlock()
	if ok {
		stop(errStopped)
	}
	return ok
}

// watchStop ends the question when another replica marks it stopped.
func (s *Service) watchStop(ctx context.Context, key string, stop context.CancelCauseFunc) {
	t := time.NewTicker(s.limits.StopCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if stopped, err := s.opts.Store.Stopped(ctx, key); err == nil && stopped {
				stop(errStopped)
				return
			}
		}
	}
}

// Cancel handles POST /api/v1/assistant/messages/:request_id/cancel. It
// answers 204 whether or not the id names a running question, so a caller
// learns nothing about anyone else's; a stop key is the caller's own.
func (s *Service) Cancel(c *gin.Context) {
	id := c.Param("request_id")
	if !conversationIDPattern.MatchString(id) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "request_id must be 1 to 64 letters, digits, _ or -"})
		return
	}
	actor := s.opts.Actor(c)
	if actor.UserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "message": "sign in to use the assistant"})
		return
	}
	key := stopKey(actor.UserID, id)
	if !s.running.stop(key) {
		// Running on another replica, or not arrived yet: leave a mark it reads.
		if err := s.opts.Store.MarkStopped(c.Request.Context(), key, s.limits.Deadline+time.Minute); err != nil {
			log.WithError(err).Warn("assistant: could not mark a question stopped")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "stop_unavailable", "message": "the question could not be stopped; it ends at its time limit"})
			return
		}
	}
	c.Status(http.StatusNoContent)
}
