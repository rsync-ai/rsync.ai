package assistant

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// Admin > Settings > AI models: a platform admin sets, tests and removes the
// key each picker entry answers with. A panel key overrides the environment
// key for its entry; Remove falls back to the environment. No route returns a
// key, and no audit row carries one. There is no endpoint field: a key goes
// to its own provider (llm-service pins the base URL), and a model id is a
// name, never a URL.

// modelIDPattern matches llm-service's valid_model_id.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)

const adminTestPrompt = "Reply with the word OK."

type adminEntry struct {
	Entry        string     `json:"entry"`
	Label        string     `json:"label"`
	PanelKeySet  bool       `json:"panel_key_set"`
	Model        string     `json:"model"`         // the panel key's model; "" is the default
	DefaultModel string     `json:"default_model"` // what a panel key answers with when Model is ""
	UpdatedBy    string     `json:"updated_by,omitempty"`
	UpdatedAt    *time.Time `json:"updated_at,omitempty"`
	EnvKeySet    bool       `json:"env_key_set"`
	EnvModel     string     `json:"env_model,omitempty"`
	Source       string     `json:"source"` // panel | env | none: where a question's key comes from
	// SampleRows is whether an admin approved sending sample rows to this
	// entry's provider (samples.go).
	SampleRows bool `json:"sample_rows"`
}

func (s *Service) adminView(ctx context.Context) (gin.H, error) {
	rows, err := s.opts.Keys.List(ctx)
	if err != nil {
		return nil, err
	}
	panel := map[string]KeyRow{}
	for _, r := range rows {
		panel[r.Entry] = r
	}
	envKnown := true
	prov, err := s.opts.LLM.Providers(ctx)
	if err != nil {
		envKnown = false
		prov = &Providers{}
	}
	approved, samplesKnown := s.sampleApprovals(ctx)
	entries := make([]adminEntry, 0, len(Entries))
	for _, e := range Entries {
		v := adminEntry{Entry: e, Label: labels[e], DefaultModel: prov.PanelDefaults[e], Source: "none", SampleRows: approved[e]}
		if env, ok := prov.Entries[e]; ok {
			v.EnvKeySet, v.EnvModel, v.Source = true, env.Model, "env"
		}
		if r, ok := panel[e]; ok {
			at := r.UpdatedAt
			v.PanelKeySet, v.Model, v.UpdatedBy, v.UpdatedAt, v.Source = true, r.Model, r.UpdatedBy, &at, "panel"
		}
		entries = append(entries, v)
	}
	// sample_rows_known is false when sample rows are not wired or their
	// approvals could not be read: the panel then shows no switch.
	return gin.H{"entries": entries, "env_known": envKnown, "sample_rows_known": samplesKnown}, nil
}

// AdminList handles GET /api/v1/admin/assistant/models.
func (s *Service) AdminList(c *gin.Context) {
	view, err := s.adminView(c.Request.Context())
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

func adminEntryParam(c *gin.Context) (string, bool) {
	entry := c.Param("entry")
	if !knownEntry(entry) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "unknown model; one of claude, chatgpt, gemini"})
		return "", false
	}
	return entry, true
}

func adminID(c *gin.Context) string {
	if id := c.GetString("admin_user_id"); id != "" {
		return id
	}
	return c.GetString("user_id")
}

// validKey is a pasted API key: 8 to 512 printable ASCII characters, no spaces.
func validKey(key string) bool {
	if len(key) < 8 || len(key) > 512 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// AdminSet handles PUT /api/v1/admin/assistant/models/:entry with
// {"api_key"?: string, "model"?: string}. With api_key the key is saved (or
// replaced); without it only the saved key's model changes. model "" means
// the default.
func (s *Service) AdminSet(c *gin.Context) {
	entry, ok := adminEntryParam(c)
	if !ok {
		return
	}
	var req struct {
		APIKey *string `json:"api_key"`
		Model  string  `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "the body must be a JSON object"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model != "" && !modelIDPattern.MatchString(model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "model must be a model id such as claude-sonnet-5-5, not a URL"})
		return
	}
	ctx := c.Request.Context()
	if req.APIKey != nil {
		key := strings.TrimSpace(*req.APIKey)
		if !validKey(key) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "api_key must be 8 to 512 printable characters with no spaces"})
			return
		}
		if err := s.opts.Keys.SetKey(ctx, entry, key, model, adminID(c)); err != nil {
			log.WithError(err).Error("assistant: save model key")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "failed to save the key"})
			return
		}
	} else {
		found, err := s.opts.Keys.SetModel(ctx, entry, model, adminID(c))
		if err != nil {
			log.WithError(err).Error("assistant: save model id")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "failed to save the model"})
			return
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad_request", "message": "save a key for " + labels[entry] + " before choosing its model"})
			return
		}
	}
	// Never the key: only whether it changed.
	s.opts.Audit(c, "admin_set_assistant_model_key", "assistant_model", entry, map[string]interface{}{
		"key_changed": req.APIKey != nil,
		"model":       model,
	})
	s.AdminList(c)
}

// AdminRemove handles DELETE /api/v1/admin/assistant/models/:entry: the panel
// key goes, and the entry falls back to the environment key, if any.
func (s *Service) AdminRemove(c *gin.Context) {
	entry, ok := adminEntryParam(c)
	if !ok {
		return
	}
	removed, err := s.opts.Keys.Remove(c.Request.Context(), entry)
	if err != nil {
		log.WithError(err).Error("assistant: remove model key")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": "failed to remove the key"})
		return
	}
	if !removed {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "no key is saved for " + labels[entry]})
		return
	}
	s.opts.Audit(c, "admin_remove_assistant_model_key", "assistant_model", entry, map[string]interface{}{})
	s.AdminList(c)
}

// AdminTest handles POST /api/v1/admin/assistant/models/:entry/test: one
// short model call with the key a question would use (panel, else
// environment), no tools. A key the provider refuses is a normal outcome:
// 200 with ok false and the reason.
func (s *Service) AdminTest(c *gin.Context) {
	entry, ok := adminEntryParam(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	key, model, _, err := s.opts.Keys.Get(ctx, entry)
	if err != nil {
		writeError(c, err)
		return
	}
	resp, err := s.opts.LLM.Step(ctx, StepRequest{
		Entry: entry, APIKey: secret(key), Model: model, UserID: adminID(c),
		Messages: []Message{{Role: "user", Content: adminTestPrompt}}, MaxTokens: 16,
	})
	details := map[string]interface{}{}
	var se *StepError
	switch {
	case err == nil:
		details["outcome"], details["source"], details["model"] = "ok", resp.Source, resp.Model
		s.opts.Audit(c, "admin_test_assistant_model", "assistant_model", entry, details)
		c.JSON(http.StatusOK, gin.H{"ok": true, "entry": entry, "label": resp.Label, "model": resp.Model, "source": resp.Source})
	case errors.As(err, &se):
		details["outcome"] = se.Code
		s.opts.Audit(c, "admin_test_assistant_model", "assistant_model", entry, details)
		c.JSON(http.StatusOK, gin.H{"ok": false, "entry": entry, "error": se.Code, "message": se.Reason})
	default:
		writeError(c, err)
	}
}
