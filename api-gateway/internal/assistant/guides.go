package assistant

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"

	"api-gateway/internal/agentgw"
)

// The loop loads the topic guides a question needs before the model's first
// step, so each area's rules are sent only to the questions about it
// (docs/internal/assistant-token-design.md § 7.2). The choice is rules, with no
// model call: the page, then the question's words, then the previous
// question's guides, at most maxGuides. A guide it missed the model reads with
// read_guide, and a planning tool refuses to run until its guide is in the
// conversation.

const maxGuides = 3

// pageGuides is the guide each page the panel can be opened on loads.
var pageGuides = map[string]string{
	"dashboard":  "dashboards",
	"pipeline":   "pipelines",
	"run":        "pipelines",
	"connection": "connectors",
}

// guideWords are the words in a question that load each guide, matched at a
// word's start on the lowercased question, in read_guide's topic order.
var guideWords = []struct {
	topic string
	words *regexp.Regexp
}{
	{"workflows", regexp.MustCompile(`\b(workflow|every (day|week|month|hour)|email|alert|slack|notif|digest)`)},
	{"dashboards", regexp.MustCompile(`\b(chart|tile|dashboard|scorecard|graphs?\b|plot)`)},
	{"pipelines", regexp.MustCompile(`\b(pipeline|copy|sync|stream|replicat|cdc)`)},
	{"models", regexp.MustCompile(`\b(model|silver|gold|data check|transform)`)},
	{"connectors", regexp.MustCompile(`\b(connector|openapi|swagger|apis?\b)`)},
}

// chooseGuides is the guides the loop loads for q, in load order.
func chooseGuides(q question) []string {
	var out []string
	add := func(topic string) {
		if topic != "" && len(out) < maxGuides && !contains(out, topic) {
			out = append(out, topic)
		}
	}
	add(pageGuides[q.pageKind])
	text := strings.ToLower(q.text)
	for _, g := range guideWords {
		if g.words.MatchString(text) {
			add(g.topic)
		}
	}
	if n := len(q.history); n > 0 {
		for _, topic := range q.history[n-1].Guides {
			add(topic)
		}
	}
	return out
}

// guidesLoaded tracks the guides in this question's conversation and the
// characters they take, which the eval reports apart from tool results.
type guidesLoaded struct {
	topics []string
	chars  int
}

func (g *guidesLoaded) has(topic string) bool { return contains(g.topics, topic) }

func (g *guidesLoaded) add(topic, text string) {
	if !g.has(topic) {
		g.topics = append(g.topics, topic)
	}
	g.chars += utf8.RuneCountInString(text)
}

// preload is the read_guide calls and results the loop puts before the
// model's first step, as if the model had made them: one assistant turn and
// a result for each guide. The ids are fixed, so the same guides make the
// same messages.
func (g *guidesLoaded) preload(topics []string, usedIDs map[string]bool) []Message {
	turn := Message{Role: "assistant"}
	var results []Message
	for _, topic := range topics {
		text, ok := agentgw.GuideText(topic)
		if !ok {
			continue
		}
		id := "guide_" + topic
		usedIDs[id] = true
		turn.ToolCalls = append(turn.ToolCalls, ToolCall{ID: id, Name: agentgw.GuideTool, Arguments: `{"topic":"` + topic + `"}`})
		results = append(results, Message{Role: "tool", ToolCallID: id, Content: text})
		g.add(topic, text)
	}
	if len(results) == 0 {
		return nil
	}
	return append([]Message{turn}, results...)
}

// guideArg is the topic a read_guide call's arguments name, "" when none.
func guideArg(args string) string {
	var in struct {
		Topic string `json:"topic"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return ""
	}
	return in.Topic
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
