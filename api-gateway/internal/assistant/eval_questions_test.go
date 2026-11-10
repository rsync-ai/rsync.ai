package assistant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/services"
)

// scripts/assistant_eval.py grades each answer by the tools it called. A
// question naming a tool that was renamed could never pass, and a write tool
// missing from write_tools would let a question that changed rsync.ai pass,
// so the questions file is checked against the gateway's own tool list.
func TestEvalQuestionsNameToolsTheAssistantOffers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "assistant_eval_questions.json"))
	if err != nil {
		t.Fatalf("reading scripts/assistant_eval_questions.json: %v", err)
	}
	var doc struct {
		WriteTools []string `json:"write_tools"`
		Questions  []struct {
			ID          string            `json:"id"`
			MustCall    []json.RawMessage `json:"must_call"`
			MustNotCall []string          `json:"must_not_call"`
			Draft       string            `json:"draft"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing scripts/assistant_eval_questions.json: %v", err)
	}

	offered, writes := gatewayTools(t)
	checkWriteTools(t, "scripts/assistant_eval_questions.json", doc.WriteTools, writes)

	named := 0
	check := func(id, tool string) {
		named++
		if !offered[tool] {
			t.Errorf("%s names %q, which Ask rsync.ai does not offer", id, tool)
		}
	}
	for _, q := range doc.Questions {
		for _, item := range q.MustCall {
			var one string
			var anyOf []string
			if json.Unmarshal(item, &one) == nil {
				check(q.ID, one)
			} else if json.Unmarshal(item, &anyOf) == nil && len(anyOf) > 0 {
				for _, tool := range anyOf {
					check(q.ID, tool)
				}
			} else {
				t.Errorf("%s must_call item %s is neither a tool nor a list of tools", q.ID, item)
			}
		}
		for _, tool := range q.MustNotCall {
			check(q.ID, tool)
		}
		if q.Draft != "" {
			check(q.ID, q.Draft)
		}
	}
	if len(doc.Questions) != 20 || named < 40 {
		t.Fatalf("%d questions naming %d tools; the eval asks 20", len(doc.Questions), named)
	}
}

// scripts/assistant_accuracy_eval.py grades any answer that called a write tool
// wrong, whatever it says: a write tool missing from its write_tools would let an
// answer that changed rsync.ai grade right.
func TestAccuracyEvalWriteToolsAreTheGatewaysWriteTools(t *testing.T) {
	doc := accuracyQuestions(t)
	_, writes := gatewayTools(t)
	checkWriteTools(t, "scripts/assistant_accuracy_questions.json", doc.WriteTools, writes)
}

// The accuracy eval grades an answer that opens with one of fallback_answers as
// Ask failing (an error), not as a wrong answer. A reworded finalAnswer missing
// from the list would grade every empty answer wrong, so each text finalAnswer
// writes must open with a listed lead, and each lead must still be written.
func TestAccuracyEvalFallbackAnswersAreFinalAnswersLeads(t *testing.T) {
	doc := accuracyQuestions(t)
	written := map[string]bool{}
	for _, finish := range []string{"stop", "length", "content_filter"} {
		for _, failed := range []failedCalls{nil, {"get_model: timeout"}} {
			got := finalAnswer("", finish, failed)
			lead := ""
			for _, f := range doc.FallbackAnswers {
				if strings.HasPrefix(got, f) {
					lead = f
				}
			}
			if lead == "" {
				t.Errorf("finalAnswer(\"\", %q) = %q opens with none of fallback_answers %q", finish, got, doc.FallbackAnswers)
			}
			written[lead] = true
		}
	}
	for _, f := range doc.FallbackAnswers {
		if !written[f] {
			t.Errorf("fallback_answers lists %q, which finalAnswer never writes", f)
		}
	}
	// An answer the model wrote is never a fallback.
	if got := finalAnswer("You have 3 pipelines.", "stop", nil); got != "You have 3 pipelines." {
		t.Fatalf("finalAnswer kept the model's words as %q", got)
	}
}

func accuracyQuestions(t *testing.T) (doc struct {
	WriteTools      []string `json:"write_tools"`
	FallbackAnswers []string `json:"fallback_answers"`
}) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "assistant_accuracy_questions.json"))
	if err != nil {
		t.Fatalf("reading scripts/assistant_accuracy_questions.json: %v", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing scripts/assistant_accuracy_questions.json: %v", err)
	}
	return doc
}

// gatewayTools is every tool Ask rsync.ai offers, and the write tools among them
// that act at once (a plan_ tool stores a draft the person reviews).
func gatewayTools(t *testing.T) (map[string]bool, []string) {
	t.Helper()
	gw, err := agentgw.New(agentgw.Options{
		Handlers: agentgw.DefaultHandlers(),
		Actor:    func(*gin.Context) services.Actor { return services.Actor{} },
		Audit:    func(*gin.Context, string, string, string, string, int) {},
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{SampleRowsTool: true, GenerateSQLTool: true}
	var writes []string
	for _, tool := range gw.Tools() {
		offered[tool.Name] = true
		if tool.Write && !strings.HasPrefix(tool.Name, "plan_") {
			writes = append(writes, tool.Name)
		}
	}
	if len(writes) == 0 {
		t.Fatal("the gateway has no write tool; this test would pass on nothing")
	}
	sort.Strings(writes)
	return offered, writes
}

func checkWriteTools(t *testing.T, file string, listed, writes []string) {
	t.Helper()
	got := append([]string(nil), listed...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, writes) {
		t.Fatalf("%s write_tools = %v, the gateway's write tools are %v", file, got, writes)
	}
}
