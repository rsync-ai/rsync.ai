package agentgw

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"api-gateway/internal/services"
)

// The topic guides: how each product area works and what a good draft looks
// like, each rule said once (docs/internal/assistant-token-design.md § 7.2). A
// tool's description is short and names its guide; read_guide serves the
// guide on both doors, and the in-app assistant loads the ones a question
// needs before the model's first step.
//
//go:embed guides/*.md
var guideFiles embed.FS

// GuideTool is the tool that serves a guide.
const GuideTool = "read_guide"

// guideTopics are the guides, in the order read_guide's enum lists them.
var guideTopics = []string{"workflows", "dashboards", "pipelines", "models", "connectors"}

// maxGuideChars caps one guide: a guide the assistant loads is sent on every
// step of the question.
const maxGuideChars = 6000

// guides are the guides' paragraphs by topic. A paragraph is the file's lines
// up to a blank line, joined with spaces, so each stays one string under the
// maxStringRunes every returned string is cut to.
var guides = loadGuides()

func loadGuides() map[string][]string {
	out := map[string][]string{}
	for _, topic := range guideTopics {
		raw, err := guideFiles.ReadFile("guides/" + topic + ".md")
		if err != nil {
			panic(fmt.Sprintf("agentgw: guide %q is not embedded: %v", topic, err))
		}
		var rules []string
		for _, para := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n\n") {
			if para = strings.Join(strings.Fields(para), " "); para != "" {
				rules = append(rules, para)
			}
		}
		out[topic] = rules
	}
	return out
}

type guideOut struct {
	Topic string   `json:"topic"`
	Rules []string `json:"rules"`
}

// guideOf is topic's guide as read_guide returns it.
func guideOf(topic string) guideOut { return guideOut{Topic: topic, Rules: guides[topic]} }

// GuideText is the text read_guide returns for topic, exactly as a model
// reads it, so the assistant can load a guide without a tool call: the same
// sanitize and encoding as callTool. ok is false for an unknown topic.
func GuideText(topic string) (text string, ok bool) {
	if _, ok := guides[topic]; !ok {
		return "", false
	}
	clean, err := sanitize(guideOf(topic))
	if err != nil {
		return "", false
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// checkGuides fails a guide over its cap or with a paragraph sanitize would
// cut, so a bad guide fails at startup, not in an answer.
func checkGuides() error {
	for _, topic := range guideTopics {
		n := 0
		for _, rule := range guides[topic] {
			runes := len([]rune(rule))
			if runes > maxStringRunes {
				return fmt.Errorf("agentgw: guide %q has a paragraph of %d characters, over %d", topic, runes, maxStringRunes)
			}
			n += runes
		}
		if n == 0 || n > maxGuideChars {
			return fmt.Errorf("agentgw: guide %q has %d characters, want 1 to %d", topic, n, maxGuideChars)
		}
	}
	return nil
}

// guideSuffix is how a tool's description names its guide.
func guideSuffix(topic string) string { return "Rules: " + GuideTool + "('" + topic + "')." }

func guideTools() []*Tool {
	return []*Tool{{
		Name:  GuideTool,
		Title: "Read a guide",
		Description: "rsync.ai's rules for one area: how it works and what a good draft looks like. Each tool's " +
			"description names its guide; read it before you draft or answer in that area.",
		Args: []Arg{
			{Name: "topic", Type: "string", Required: true, Enum: guideTopics, Description: "The area."},
		},
		Action:       services.ActionGuideRead,
		ResourceType: "guide",
		Output:       guideOut{},
		run: func(call *toolCall) (any, error) {
			return guideOf(call.str("topic")), nil
		},
	}}
}
