package assistant

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"api-gateway/internal/agentgw"
)

// Each rule the model reads has one home (docs/internal/assistant-token-design.md
// § 7.1): the core rules in step.yaml, a topic guide, a tool's description or
// an argument's. A rule said in two homes is sent twice whenever both are
// loaded, and the two copies drift apart when one is edited.

const stepPrompt = "../../../llm-service/prompts/assistant/step.yaml"

// minHomeWords is the shortest sentence counted as a rule said again; a
// shorter one, such as "Rules: read_guide('workflows').", is a phrase every
// tool may share.
const minHomeWords = 8

var (
	// A sentence ends at . ! or ? before a space, at a list item and at a
	// blank line.
	sentenceEnd = regexp.MustCompile(`[.!?]\s+|\n\s*-\s+|\n\s*\n`)
	homeWord    = regexp.MustCompile(`[a-z0-9_']+`)
	jinjaTag    = regexp.MustCompile(`\{%.*?%\}|\{\{.*?\}\}|\{#.*?#\}`)
)

func TestEachRuleHasOneHome(t *testing.T) {
	homes := ruleHomes(t)
	// Armed: every kind of home is read, so the test is not passing on less.
	kinds := map[string]int{}
	for name := range homes {
		kinds[strings.SplitN(name, " ", 2)[0]]++
	}
	if kinds["core"] != 1 || kinds["guide"] < 5 || kinds["tool"] < 30 || kinds["argument"] < 40 {
		t.Fatalf("homes read: %v; the test would pass on too little", kinds)
	}
	for _, r := range ruleRepeats(homes) {
		t.Error(r)
	}
}

// The check finds a sentence copied whole or inside a longer one, whatever
// its case or punctuation, and leaves a short phrase alone.
func TestRuleRepeatsFindsASentenceSaidTwice(t *testing.T) {
	homes := ruleHomes(t)
	guide := homes["guide workflows"]
	sentence := "A workflow never joins two connections or runs a second query"
	if !strings.Contains(guide, sentence) {
		t.Fatalf("control: the workflows guide no longer says %q", sentence)
	}
	for name, desc := range map[string]string{
		"copied whole":      sentence + ".",
		"inside a sentence": "Drafts a workflow; " + strings.ToLower(sentence) + ", ever!",
		"after a list item": "Lists workflows.\n- " + sentence,
	} {
		homes["tool list_workflows"] = desc
		if got := ruleRepeats(homes); len(got) != 1 || !strings.Contains(got[0], "guide workflows") || !strings.Contains(got[0], "tool list_workflows") {
			t.Errorf("%s: repeats = %q", name, got)
		}
	}
	homes["tool list_workflows"] = "A workflow never joins two connections."
	if got := ruleRepeats(homes); len(got) != 0 {
		t.Errorf("a phrase shorter than %d words counted as a rule: %q", minHomeWords, got)
	}
}

// ruleRepeats lists each sentence of minHomeWords or more words that one home
// says and another home also says.
func ruleRepeats(homes map[string]string) []string {
	words := func(s string) string {
		return " " + strings.Join(homeWord.FindAllString(strings.ToLower(s), -1), " ") + " "
	}
	all := make(map[string]string, len(homes))
	for name, text := range homes {
		all[name] = words(text)
	}
	var out []string
	seen := map[string]bool{}
	for name, text := range homes {
		for _, sentence := range sentenceEnd.Split(text, -1) {
			w := words(sentence)
			if len(strings.Fields(w)) < minHomeWords {
				continue
			}
			for other, otherWords := range all {
				if other == name || !strings.Contains(otherWords, w) {
					continue
				}
				// A sentence copied whole is found from both homes: say it once.
				pair := []string{name, other}
				sort.Strings(pair)
				if key := pair[0] + "|" + pair[1] + "|" + w; !seen[key] {
					seen[key] = true
					out = append(out, "the "+pair[0]+" and the "+pair[1]+" both say: "+strings.TrimLeft(strings.TrimSpace(sentence), "- "))
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// ruleHomes is every text the model reads its rules from, by home: the core
// rules, each guide, each tool's description and each argument's. An argument
// on several tools, such as model_id, is one home: its descriptions together.
func ruleHomes(t *testing.T) map[string]string {
	t.Helper()
	homes := map[string]string{"core rules": coreRules(t)}
	specs := realToolSpecs(t)
	tools := []Tool{sampleRowsSpec, generateSQLSpec}
	for _, s := range specs {
		tools = append(tools, Tool{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema})
	}
	var args func(schema map[string]any)
	args = func(schema map[string]any) {
		props, _ := schema["properties"].(map[string]any)
		for name, v := range props {
			p, _ := v.(map[string]any)
			if d, _ := p["description"].(string); d != "" {
				homes["argument "+name] += d + "\n\n"
			}
			if items, ok := p["items"].(map[string]any); ok {
				args(items)
			}
		}
	}
	for _, tool := range tools {
		homes["tool "+tool.Name] = tool.Description
		args(tool.InputSchema)
	}
	for _, topic := range guideTopics(t, specs) {
		text, ok := agentgw.GuideText(topic)
		var g struct{ Rules []string }
		if !ok || json.Unmarshal([]byte(text), &g) != nil || len(g.Rules) == 0 {
			t.Fatalf("guide %q: %q", topic, text)
		}
		homes["guide "+topic] = strings.Join(g.Rules, "\n\n")
	}
	return homes
}

// coreRules is step.yaml's system prompt with its template tags taken out.
func coreRules(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(stepPrompt)
	if err != nil {
		t.Fatalf("read %s: %v", stepPrompt, err)
	}
	var lines []string
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "system: |") {
			in = true
			continue
		}
		if in && line != "" && !strings.HasPrefix(line, "  ") {
			break
		}
		if in {
			lines = append(lines, line)
		}
	}
	if len(lines) < 20 {
		t.Fatalf("%s has no system block of rules; update coreRules to where they are", stepPrompt)
	}
	return jinjaTag.ReplaceAllString(strings.Join(lines, "\n"), " ")
}

// guideTopics are the topics read_guide takes, from its schema.
func guideTopics(t *testing.T, specs []agentgw.ToolSpec) []string {
	t.Helper()
	for _, s := range specs {
		if s.Name != agentgw.GuideTool {
			continue
		}
		props, _ := s.InputSchema["properties"].(map[string]any)
		topic, _ := props["topic"].(map[string]any)
		raw, _ := json.Marshal(topic["enum"])
		var topics []string
		if json.Unmarshal(raw, &topics) == nil && len(topics) > 0 {
			return topics
		}
	}
	t.Fatalf("%s has no topic enum", agentgw.GuideTool)
	return nil
}
