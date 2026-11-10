package assistant

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"api-gateway/internal/agentgw"
)

// Topic guides on the panel (docs/internal/assistant-token-design.md § 7.2):
// which load, where they go, and the planning tool that refuses without one.

func guideText(t *testing.T, topic string) string {
	t.Helper()
	text, ok := agentgw.GuideText(topic)
	if !ok || text == "" {
		t.Fatalf("no %s guide", topic)
	}
	return text
}

func TestGuidesAreChosenByPageThenWordsThenThePreviousQuestion(t *testing.T) {
	last := func(guides ...string) []Turn { return []Turn{{Question: "q", Answer: "a", Guides: guides}} }
	cases := []struct {
		name    string
		q       question
		want    []string
		comment string
	}{
		{"dashboard page", question{pageKind: "dashboard", text: "make it wider"}, []string{"dashboards"}, ""},
		{"pipeline page", question{pageKind: "pipeline", text: "why is it slow?"}, []string{"pipelines"}, ""},
		{"run page", question{pageKind: "run", text: "why did it fail?"}, []string{"pipelines"}, ""},
		{"connection page", question{pageKind: "connection", text: "what tables are there?"}, []string{"connectors"}, ""},
		{"no page, no words", question{text: "how many rows were read?"}, nil, ""},
		{"workflow words", question{text: "Email me every day when sales drop"}, []string{"workflows"}, ""},
		{"a weekly Slack alert", question{text: "post to slack every week"}, []string{"workflows"}, ""},
		{"dashboard words", question{text: "Plot revenue as a bar chart"}, []string{"dashboards"}, ""},
		{"graphs, not graphql", question{text: "show graphs"}, []string{"dashboards"}, ""},
		{"graphql", question{text: "is there a graphql source?"}, nil, "graph must end the word"},
		{"pipeline words", question{text: "set up CDC from postgres"}, []string{"pipelines"}, ""},
		{"rsync is not sync", question{text: "what can rsync.ai do?"}, nil, "\\bsync must not match rsync"},
		{"model words", question{text: "build the silver tables"}, []string{"models"}, ""},
		{"data checks", question{text: "add a data check"}, []string{"models"}, ""},
		{"connector words", question{text: "connect the Stripe APIs"}, []string{"connectors"}, ""},
		{"rapid is not api", question{text: "a rapid answer please"}, nil, "api must start a word"},
		{"two topics, in topic order", question{text: "chart the gold model"}, []string{"dashboards", "models"}, ""},
		{"the previous question's", question{text: "make it daily", history: last("workflows")}, []string{"workflows"}, ""},
		{"only the last question's", question{text: "and then?", history: append(last("models"), Turn{Question: "q2", Answer: "a2"})}, nil, ""},
		{"page before words", question{pageKind: "pipeline", text: "a workflow with a chart"}, []string{"pipelines", "workflows", "dashboards"}, ""},
		{"no repeats", question{pageKind: "dashboard", text: "another dashboard tile", history: last("dashboards")}, []string{"dashboards"}, ""},
		{"at most three: page, words, then history", question{
			pageKind: "dashboard",
			text:     "a pipeline, a model and a workflow",
			history:  last("connectors"),
		}, []string{"dashboards", "workflows", "pipelines"}, ""},
		{"history fills what is left", question{pageKind: "run", text: "why?", history: last("models", "connectors", "workflows")}, []string{"pipelines", "models", "connectors"}, ""},
	}
	for _, c := range cases {
		if got := chooseGuides(c.q); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: guides = %v, want %v %s", c.name, got, c.want, c.comment)
		}
	}
	for _, g := range guideWords {
		if _, ok := agentgw.GuideText(g.topic); !ok {
			t.Errorf("words load %q, which is not a guide", g.topic)
		}
	}
	for page, topic := range pageGuides {
		if _, ok := agentgw.GuideText(topic); !ok {
			t.Errorf("page %q loads %q, which is not a guide", page, topic)
		}
	}
}

func TestAGuideReadTwiceIsOneTopicAndTwiceTheCharacters(t *testing.T) {
	var g guidesLoaded
	g.add("models", "abc")
	g.add("models", "abc")
	if !reflect.DeepEqual(g.topics, []string{"models"}) || g.chars != 6 {
		t.Fatalf("topics %v, chars %d: the text is in the conversation twice, the topic once", g.topics, g.chars)
	}
}

func TestChosenGuidesComeBeforeTheFirstStep(t *testing.T) {
	h := newHarness(t, Limits{}, say("Here is the change."), say("Done."))
	h.tools.extra = []string{agentgw.GuideTool}
	page := map[string]string{"kind": "dashboard", "id": "0b5e8d3c-55a8-4c4b-9d55-6d1f2b6e2a10"}
	w, out := h.ask(t, "u1", map[string]any{"question": "add a workflow", "page": page, "conversation_id": "conv-g"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	first := h.llm.reqs[0].Messages
	if roles(first) != "user,assistant,tool,tool" {
		t.Fatalf("first step roles = %s", roles(first))
	}
	call := first[1]
	if call.Content != "" || len(call.ToolCalls) != 2 {
		t.Fatalf("the loaded guides are one turn of read_guide calls: %+v", call)
	}
	chars := 0
	for i, topic := range []string{"dashboards", "workflows"} {
		tc, result := call.ToolCalls[i], first[2+i]
		want := ToolCall{ID: "guide_" + topic, Name: agentgw.GuideTool, Arguments: `{"topic":"` + topic + `"}`}
		if tc != want || result.ToolCallID != want.ID || result.Content != guideText(t, topic) {
			t.Fatalf("guide %d: call %+v, result %q…", i, tc, result.Content[:40])
		}
		chars += utf8.RuneCountInString(result.Content)
	}
	if len(h.tools.calls) != 0 || len(out.LookedAt) != 0 || len(out.Usage.Steps[0].Calls) != 0 {
		t.Fatalf("a loaded guide is not a tool call: ran %v, looked_at %v, calls %v", h.tools.calls, out.LookedAt, out.Usage.Steps[0].Calls)
	}
	if got := out.Usage.Steps[0].GuideChars; got != chars {
		t.Fatalf("guide_chars = %d, want %d", got, chars)
	}

	// The follow-up loads them again, after its own question.
	h.ask(t, "u1", map[string]any{"question": "make it daily", "conversation_id": "conv-g"})
	next := h.llm.reqs[1].Messages
	if roles(next) != "user,assistant,user,assistant,tool,tool" || next[2].Content != "make it daily" || next[3].ToolCalls[0].ID != "guide_dashboards" {
		t.Fatalf("follow-up messages = %s", roles(next))
	}
	turns := h.store.convs[conversationKey("u1", "ws-1", "conv-g")]
	if len(turns) != 2 || !reflect.DeepEqual(turns[1].Guides, []string{"dashboards", "workflows"}) {
		t.Fatalf("stored turns = %+v", turns)
	}
}

func TestNoGuideLoadsWhenReadGuideIsNotOffered(t *testing.T) {
	h := newHarness(t, Limits{}, say("a"))
	w, out := h.ask(t, "u1", map[string]any{"question": "add a workflow", "conversation_id": "conv-n"})
	if w.Code != http.StatusOK || roles(h.llm.reqs[0].Messages) != "user" || out.Usage.Steps[0].GuideChars != 0 {
		t.Fatalf("status %d, roles %s: a guide call to a tool not offered makes the conversation invalid", w.Code, roles(h.llm.reqs[0].Messages))
	}
	if turns := h.store.convs[conversationKey("u1", "ws-1", "conv-n")]; turns[0].Guides != nil {
		t.Fatalf("stored guides = %v", turns[0].Guides)
	}
}

func TestAPlanningToolRefusesWithoutItsGuideThenRuns(t *testing.T) {
	plan := ToolCall{ID: "c1", Name: "plan_workflow", Arguments: `{"name":"x"}`}
	again := ToolCall{ID: "c2", Name: "plan_workflow", Arguments: `{"name":"x"}`}
	h := newHarness(t, Limits{},
		callTools(plan, ToolCall{ID: "c3", Name: "list_pipelines", Arguments: `{}`}),
		callTools(again),
		say("Drafted."),
	)
	h.tools.extra = []string{agentgw.GuideTool, "plan_workflow"}
	// list_pipelines names a guide too, but only a planning tool waits for one.
	h.tools.guides = map[string]string{"plan_workflow": "workflows", "list_pipelines": "pipelines"}
	w, out := h.ask(t, "u1", map[string]any{"question": "do the usual", "conversation_id": "conv-r"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if roles(h.llm.reqs[0].Messages) != "user" {
		t.Fatalf("no guide was chosen for this question; roles = %s", roles(h.llm.reqs[0].Messages))
	}
	if len(h.tools.calls) != 2 || h.tools.calls[0].name != "list_pipelines" || h.tools.calls[1].name != "plan_workflow" {
		t.Fatalf("tool calls = %+v, want list_pipelines then the second plan_workflow", h.tools.calls)
	}
	refusal := h.llm.reqs[1].Messages[2]
	guide := guideText(t, "workflows")
	if refusal.ToolCallID != "c1" || !strings.HasPrefix(refusal.Content, "Not run: plan_workflow needs the workflows rules") || !strings.HasSuffix(refusal.Content, guide) {
		t.Fatalf("refusal = %q…", refusal.Content[:80])
	}
	steps := out.Usage.Steps
	if !reflect.DeepEqual(steps[0].Calls, []CallOutcome{{Tool: "plan_workflow", Outcome: "guide_missing"}, {Tool: "list_pipelines", Outcome: "ok", Status: 200}}) ||
		!reflect.DeepEqual(steps[1].Calls, []CallOutcome{{Tool: "plan_workflow", Outcome: "ok", Status: 200}}) {
		t.Fatalf("calls = %+v / %+v", steps[0].Calls, steps[1].Calls)
	}
	if len(out.LookedAt) != 2 || out.LookedAt[1].Tool != "plan_workflow" {
		t.Fatalf("looked_at = %+v: the refused call was not looked at", out.LookedAt)
	}
	if n := utf8.RuneCountInString(guide); steps[0].GuideChars != 0 || steps[1].GuideChars != n || steps[2].GuideChars != n {
		t.Fatalf("guide_chars by step = %d %d %d, want 0 %d %d", steps[0].GuideChars, steps[1].GuideChars, steps[2].GuideChars, n, n)
	}
	if turns := h.store.convs[conversationKey("u1", "ws-1", "conv-r")]; !reflect.DeepEqual(turns[0].Guides, []string{"workflows"}) {
		t.Fatalf("stored guides = %v", turns[0].Guides)
	}
}

func TestALoadedGuideLetsAPlanningToolRunAtOnce(t *testing.T) {
	h := newHarness(t, Limits{}, callTools(ToolCall{ID: "c1", Name: "plan_workflow", Arguments: `{}`}), say("Drafted."))
	h.tools.extra = []string{agentgw.GuideTool, "plan_workflow"}
	h.tools.guides = map[string]string{"plan_workflow": "workflows"}
	_, out := h.ask(t, "u1", map[string]any{"question": "email me when sales drop"})
	if len(h.tools.calls) != 1 || out.Usage.Steps[0].Calls[0].Outcome != "ok" {
		t.Fatalf("calls = %+v, outcome %+v", h.tools.calls, out.Usage.Steps[0].Calls)
	}
}

func TestTheModelsOwnReadGuideCounts(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%v", failed), func(t *testing.T) {
			h := newHarness(t, Limits{},
				callTools(ToolCall{ID: "c1", Name: agentgw.GuideTool, Arguments: `{"topic":"dashboards"}`}),
				callTools(ToolCall{ID: "c2", Name: "plan_dashboard", Arguments: `{}`}),
				say("Drafted."),
			)
			h.tools.extra = []string{agentgw.GuideTool, "plan_dashboard"}
			h.tools.guides = map[string]string{"plan_dashboard": "dashboards"}
			guide := guideText(t, "dashboards")
			h.tools.results[agentgw.GuideTool] = guide
			h.tools.errs[agentgw.GuideTool] = failed
			_, out := h.ask(t, "u1", map[string]any{"question": "hello"})
			steps := out.Usage.Steps
			if got := steps[0].Calls; len(got) != 1 || got[0].Tool != agentgw.GuideTool {
				t.Fatalf("the model's read_guide is a call: %+v", got)
			}
			want, n := "ok", utf8.RuneCountInString(guide)
			if failed {
				want = "guide_missing"
			}
			if got := steps[1].Calls[0].Outcome; got != want {
				t.Fatalf("plan_dashboard after read_guide: %s, want %s", got, want)
			}
			if failed {
				// A failed read loads nothing; the refusal then carries the guide.
				if steps[1].GuideChars != 0 || steps[2].GuideChars != n {
					t.Fatalf("guide_chars %d then %d, want 0 then %d", steps[1].GuideChars, steps[2].GuideChars, n)
				}
				return
			}
			if steps[1].GuideChars != n {
				t.Fatalf("guide_chars = %d, want %d", steps[1].GuideChars, n)
			}
		})
	}
}

// stepRouteMessages is llm-service's cap on one step's messages
// (StepRequest.messages, max_length=80).
const stepRouteMessages = 80

func TestTheLongestQuestionFitsTheStepRoute(t *testing.T) {
	limits := Limits{}.withDefaults()
	// Calls refused for their arguments run nothing, so no limit ends the
	// question early: every step but the last sends a full turn of calls.
	calls := make([]ToolCall, maxCallsPerTurn)
	for i := range calls {
		calls[i] = ToolCall{ID: fmt.Sprintf("c%d", i), Name: "list_pipelines", Arguments: `[]`}
	}
	script := make([]stepFunc, limits.Steps)
	for i := range script {
		script[i] = callTools(calls...)
	}
	store := &memStore{}
	h := newHarnessOn(t, store, Limits{}, script...)
	h.tools.extra = []string{agentgw.GuideTool}
	history := make([]Turn, limits.HistoryTurns)
	for i := range history {
		history[i] = Turn{Question: "q", Answer: "a", Guides: []string{"models"}}
	}
	if err := store.Save(t.Context(), conversationKey("u1", "ws-1", "conv-l"), history); err != nil {
		t.Fatal(err)
	}
	page := map[string]string{"kind": "run", "id": "0b5e8d3c-55a8-4c4b-9d55-6d1f2b6e2a10"}
	if w, _ := h.ask(t, "u1", map[string]any{"question": "a workflow with a chart", "page": page, "conversation_id": "conv-l"}); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	most := 0
	for _, req := range h.llm.reqs {
		most = max(most, len(req.Messages))
	}
	if want := 2*limits.HistoryTurns + 1 + 1 + maxGuides + (limits.Steps-1)*(1+maxCallsPerTurn); most != want || most > stepRouteMessages {
		t.Fatalf("the longest step sent %d messages (want %d), the route takes %d", most, want, stepRouteMessages)
	}
}
