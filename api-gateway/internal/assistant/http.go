package assistant

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

var conversationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var pageKinds = map[string]bool{"pipeline": true, "connection": true, "run": true, "dashboard": true}

const maxQuestionRunes = 4000

type pageRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// askRequest is POST /api/v1/assistant/messages. Any other field, such as a
// history the browser made up, is ignored: the conversation is the server's.
type askRequest struct {
	Question       string   `json:"question"`
	ConversationID string   `json:"conversation_id"`
	Page           *pageRef `json:"page"`
	Model          string   `json:"model"`
	// RequestID names this question for Stop (POST .../:request_id/cancel).
	RequestID string `json:"request_id"`
}

// Messages handles POST /api/v1/assistant/messages: one question, answered.
func (s *Service) Messages(c *gin.Context) {
	var req askRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "the body must be a JSON object"})
		return
	}
	text := strings.TrimSpace(req.Question)
	if text == "" || utf8.RuneCountInString(text) > maxQuestionRunes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "question must be 1 to 4000 characters"})
		return
	}
	entry := strings.TrimSpace(req.Model)
	if entry != "" && !knownEntry(entry) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "model must be one of claude, chatgpt, gemini"})
		return
	}
	var pageKind, pageID string
	if req.Page != nil && (req.Page.Kind != "" || req.Page.ID != "") {
		if _, err := uuid.Parse(req.Page.ID); err != nil || !pageKinds[req.Page.Kind] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "page must be {kind: pipeline|connection|run|dashboard, id: <uuid>}"})
			return
		}
		pageKind, pageID = req.Page.Kind, req.Page.ID
	}
	convID := req.ConversationID
	if !conversationIDPattern.MatchString(convID) {
		convID = uuid.NewString()
	}
	if req.RequestID != "" && !conversationIDPattern.MatchString(req.RequestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "request_id must be 1 to 64 letters, digits, _ or -"})
		return
	}

	actor := s.opts.Actor(c)
	if actor.UserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "message": "sign in to use the assistant"})
		return
	}
	if !s.opts.QuotaOK(c.Request.Context(), actor.UserID) {
		c.JSON(http.StatusPaymentRequired, gin.H{
			"error":   "quota_exceeded",
			"message": "Monthly LLM spend quota exceeded. Contact admin to raise cost_quota_usd_cents.",
		})
		return
	}

	base, stop := context.WithCancelCause(c.Request.Context())
	defer stop(nil)
	ctx, cancel := context.WithTimeout(base, s.limits.Deadline)
	defer cancel()
	// The tools read through c's request, so they share the deadline and Stop.
	c.Request = c.Request.WithContext(ctx)
	if req.RequestID != "" {
		key := stopKey(actor.UserID, req.RequestID)
		if !s.running.add(key, stop) {
			c.JSON(http.StatusConflict, gin.H{"error": "request_in_use", "message": "a question with this request_id is already running"})
			return
		}
		defer s.running.remove(key)
		// The cancel can overtake the question, or land on another replica.
		if stopped, err := s.opts.Store.Stopped(ctx, key); err == nil && stopped {
			stop(errStopped)
		}
		go s.watchStop(ctx, key, stop)
	}

	if entry == "" {
		available, err := s.available(ctx)
		if err != nil {
			writeError(c, err)
			return
		}
		if len(available) == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "model_not_configured", "message": "no model is set up; an admin can add a key in Admin > Settings > AI models"})
			return
		}
		entry = available[0].Entry
	}

	key := conversationKey(actor.UserID, actor.WorkspaceID, convID)
	history, err := s.opts.Store.Load(ctx, key)
	if err != nil {
		log.WithError(err).Warn("assistant: conversation load failed; answering without history")
		history = nil
	}
	out, err := s.ask(c, ctx, question{
		actor: actor, text: text, entry: entry, pageKind: pageKind, pageID: pageID, history: history,
	})
	if errors.Is(context.Cause(ctx), errStopped) {
		// Stopped: whatever came back, nobody is waiting for it and it is not kept.
		c.JSON(statusStopped, gin.H{"error": "stopped", "message": "the question was stopped"})
		return
	}
	if err != nil {
		writeError(c, err)
		return
	}
	out.ConversationID = convID
	if err := s.remember(ctx, key, history, text, out.Answer, out.samplesEntry(), out.guides); err != nil {
		log.WithError(err).Warn("assistant: conversation save failed")
	}
	c.JSON(http.StatusOK, out)
}

// modelEntry is one model the picker offers. Never a key.
type modelEntry struct {
	Entry string `json:"entry"`
	Label string `json:"label"`
	Model string `json:"model"`
}

// Models handles GET /api/v1/assistant/models: the models with a key, the
// first being the default.
func (s *Service) Models(c *gin.Context) {
	available, err := s.available(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	def := ""
	if len(available) > 0 {
		def = available[0].Entry
	}
	c.JSON(http.StatusOK, gin.H{"entries": available, "default": def})
}

// available lists the entries with a panel key or an environment key.
func (s *Service) available(ctx context.Context) ([]modelEntry, error) {
	rows, err := s.opts.Keys.List(ctx)
	if err != nil {
		return nil, err
	}
	panel := map[string]KeyRow{}
	for _, r := range rows {
		panel[r.Entry] = r
	}
	prov, err := s.opts.LLM.Providers(ctx)
	if err != nil {
		log.WithError(err).Warn("assistant: llm-service providers unavailable; listing panel keys only")
		prov = &Providers{}
	}
	out := []modelEntry{}
	for _, e := range Entries {
		if r, ok := panel[e]; ok {
			model := r.Model
			if model == "" {
				model = prov.PanelDefaults[e]
			}
			out = append(out, modelEntry{Entry: e, Label: labels[e], Model: model})
		} else if env, ok := prov.Entries[e]; ok {
			label := env.Label
			if label == "" {
				label = labels[e]
			}
			out = append(out, modelEntry{Entry: e, Label: label, Model: env.Model})
		}
	}
	return out, nil
}

func writeError(c *gin.Context, err error) {
	var se *StepError
	switch {
	case errors.As(err, &se):
		c.JSON(se.Status, gin.H{"error": se.Code, "message": se.Reason})
	case errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "timeout", "message": "the assistant took too long to answer"})
	case errors.Is(err, context.Canceled):
		// The browser went away mid-question: not a server failure.
		log.WithError(err).Info("assistant: the asker went away mid-question")
		c.JSON(statusStopped, gin.H{"error": "cancelled", "message": "the question was cancelled"})
	case errors.Is(err, ErrUndecryptable):
		c.JSON(http.StatusInternalServerError, gin.H{"error": "model_key_unreadable", "message": err.Error()})
	default:
		log.WithError(err).Error("assistant: request failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "the assistant could not answer"})
	}
}
