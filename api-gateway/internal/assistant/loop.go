package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"api-gateway/internal/agentgw"
	"api-gateway/internal/handlers"
	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// One question: model turns until the model answers without asking for a
// tool, bounded by the step, tool-call and size limits. Every bound ends in an
// answer the user can read, never a hang: the last turn is sent with
// tool_choice "none", and a model that asks for a tool anyway gets a fixed
// answer naming what was looked at.

var callIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

const (
	maxArgumentChars = 4000 // the step route's cap on one call's arguments
	maxCallsPerTurn  = 12   // the step route's cap on calls in one message
	maxStoredAnswer  = 8000
	// maxToolCorrections: turns that call only made-up tools the model is
	// told about and asked again, before the question ends.
	maxToolCorrections = 2
)

// LookedAt is one tool the assistant ran for this question.
type LookedAt struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	OK        bool            `json:"ok"`
	// ResourceID is the id a successful call named or made: the page shows a
	// plan_pipeline's plan as a card from it.
	ResourceID string `json:"resource_id,omitempty"`
	// Sample is a sample_rows call's row and withheld-column counts.
	Sample *SampleSummary `json:"sample,omitempty"`
	// SQL is a generate_sql call's SQL: the person sees it, the model does not.
	SQL    *SQLDraft `json:"sql,omitempty"`
	callID string
}

// ModelInfo names the model that answered.
type ModelInfo struct {
	Entry string `json:"entry"`
	Label string `json:"label"`
	Model string `json:"model"`
}

// Answer is the response to one question.
type Answer struct {
	ConversationID string     `json:"conversation_id"`
	Answer         string     `json:"answer"`
	Model          ModelInfo  `json:"model"`
	LookedAt       []LookedAt `json:"looked_at"`
	// Stopped is "limit" when the question hit a limit before the model
	// finished; the answer then says so.
	Stopped   string `json:"stopped,omitempty"`
	CostCents int    `json:"cost_cents"`
	Usage     Usage  `json:"usage"`
	// guides are the topic guides in the conversation, which the next
	// question loads again.
	guides []string
}

// Usage is what one question sent and how each tool call ended, turn by turn,
// so scripts/assistant_eval.py can measure a prompt change. Token counts are
// the provider's own.
type Usage struct {
	InputTokens     int         `json:"input_tokens"`
	CachedTokens    int         `json:"cached_tokens"`
	OutputTokens    int         `json:"output_tokens"`
	ReasoningTokens int         `json:"reasoning_tokens"`
	Steps           []StepUsage `json:"steps"`
}

// StepUsage is one model turn: its tokens, its request's characters by part,
// and the tool calls the model asked for in it. GuideChars is the part of
// Chars.Tool that is topic guides, so a report shows the rules apart from
// what the tools returned.
type StepUsage struct {
	InputTokens     int           `json:"input_tokens"`
	CachedTokens    int           `json:"cached_tokens"`
	OutputTokens    int           `json:"output_tokens"`
	ReasoningTokens int           `json:"reasoning_tokens"`
	Chars           SentChars     `json:"chars"`
	GuideChars      int           `json:"guide_chars"`
	Calls           []CallOutcome `json:"calls"`
}

// CallOutcome is how one tool call ended. A call the agent gateway ran has
// its audit row's outcome and status ("ok" 200, "forbidden" 403,
// "pipeline_not_found" 404, "invalid_arguments" 400 ...). The loop names the
// calls it did not pass on: "unknown_tool", "limit", "invalid_arguments" (400),
// "dropped" (past the calls one turn may make), "too_large" (the result did
// not fit), "values_in_conversation", "guide_missing" (a planning tool called
// before its guide was read: it answers with the guide); sample_rows and
// generate_sql end "ok" or "error". The guides the loop loads itself are not
// calls.
type CallOutcome struct {
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
	Status  int    `json:"status,omitempty"`
}

// wholeCents is a question's cost rounded up to a whole cent. The steps' exact
// costs are summed first: each one rounded up charged a question of three
// 0.0004¢ steps 3¢. The sum is first rounded to a millionth of a cent, where
// every price falls (llm-service src/utils/llm_cost.py), so float error never
// adds a cent: 0.2 + 0.4 + 0.3 + 0.1 is 1.0000000000000002 in float64.
func wholeCents(c float64) int {
	return int(math.Ceil(math.Round(c*1e6) / 1e6))
}

func (u *Usage) step(r *StepResponse, guideChars int) {
	u.InputTokens += r.Usage.PromptTokens
	u.CachedTokens += r.Usage.CachedTokens
	u.OutputTokens += r.Usage.CompletionTokens
	u.ReasoningTokens += r.Usage.ReasoningTokens
	u.Steps = append(u.Steps, StepUsage{
		InputTokens: r.Usage.PromptTokens, CachedTokens: r.Usage.CachedTokens,
		OutputTokens: r.Usage.CompletionTokens, ReasoningTokens: r.Usage.ReasoningTokens,
		Chars: r.Chars, GuideChars: guideChars, Calls: []CallOutcome{},
	})
}

// call records how a call in the latest turn ended.
func (u *Usage) call(tool, outcome string, status int) {
	last := &u.Steps[len(u.Steps)-1]
	last.Calls = append(last.Calls, CallOutcome{Tool: safeName(tool), Outcome: outcome, Status: status})
}

type question struct {
	actor    services.Actor
	text     string
	entry    string
	pageKind string
	pageID   string
	history  []Turn
	// model is the entry's model and key, set by ask: generate_sql writes
	// with it.
	model handlers.AskModel
}

func (s *Service) ask(c *gin.Context, ctx context.Context, q question) (*Answer, error) {
	key, model, _, err := s.opts.Keys.Get(ctx, q.entry)
	if err != nil {
		return nil, err
	}
	q.model = handlers.AskModel{Entry: q.entry, Model: model, Key: handlers.AskKey(key)}
	specs := s.opts.Tools.ToolSpecs()
	tools := make([]Tool, 0, len(specs)+2)
	offered := make(map[string]bool, len(specs)+2)
	guideOf := make(map[string]string, len(specs))
	for _, t := range specs {
		if t.Name == SampleRowsTool || t.Name == GenerateSQLTool {
			continue // only the assistant's own reads values or writes SQL
		}
		tools = append(tools, Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
		offered[t.Name] = true
		guideOf[t.Name] = t.Guide
	}
	sampling := s.sampleRowsOpen(ctx, q.actor.WorkspaceID, q.entry)
	if sampling {
		tools = append(tools, sampleRowsSpec)
		offered[SampleRowsTool] = true
	}
	if s.opts.SQL != nil {
		tools = append(tools, generateSQLSpec)
		offered[GenerateSQLTool] = true
	}
	// needsConnection: the tools that take a connection_id, and whether
	// they must. A failed call of one is told the workspace's connections
	// when its id is not one of them (connectionHint).
	needsConnection := map[string]bool{}
	for _, t := range tools {
		if takes, required := connectionArg(t.InputSchema); takes {
			needsConnection[t.Name] = required
		}
	}
	var conns workspaceConnections

	msgs := s.historyMessages(q.history, q.entry, sampling)
	msgs = append(msgs, Message{Role: "user", Content: q.text})
	// A tool that acts on a URL (generate_connector) checks the URL is in the
	// user's own words, so one the model found or made up is refused.
	c.Set(services.CtxUserText, userWords(q))
	out := &Answer{LookedAt: []LookedAt{}, Usage: Usage{Steps: []StepUsage{}}}
	// cost is the steps' exact cost; only the answer's total is rounded.
	var cost float64
	ran, read, sampled := 0, 0, 0
	// valuesRead: a sample_rows result is in the conversation, so every turn
	// the model writes after it may quote a value.
	valuesRead := false
	usedIDs := map[string]bool{}
	corrections := 0
	askedAgain := false
	var failed failedCalls
	var guides guidesLoaded
	if offered[agentgw.GuideTool] {
		msgs = append(msgs, guides.preload(chooseGuides(q), usedIDs)...)
	}
	defer func() { out.guides = guides.topics }()

	for step := 1; ; step++ {
		// The tools do not watch the context, so a stopped question is caught
		// here and before each tool.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		last := step >= s.limits.Steps || ran >= s.limits.ToolCalls || read >= s.limits.ToolBudgetChars
		choice := "auto"
		if last {
			choice = "none"
		}
		req := StepRequest{
			Entry: q.entry, APIKey: secret(key), Model: model, UserID: q.actor.UserID,
			PageKind: q.pageKind, PageID: q.pageID,
			Messages: msgs, Tools: tools, ToolChoice: choice, MaxTokens: 1024,
		}
		var resp *StepResponse
		for {
			r, err := s.opts.LLM.Step(ctx, req)
			if err != nil {
				return nil, err
			}
			cost += r.CostCents
			out.CostCents = wholeCents(cost)
			out.Usage.step(r, guides.chars)
			resp = r
			// Gemini sometimes ends a turn with no words and no tool call, and
			// answers when asked again (the accuracy eval's s05, 2026-10-11).
			// One such turn a question is asked again before finalAnswer says so.
			if askedAgain || !emptyTurn(r) {
				break
			}
			askedAgain = true
		}
		out.Model = ModelInfo{Entry: resp.Entry, Label: resp.Label, Model: resp.Model}
		applyDataResults(out, resp.DataResults)
		content := strings.TrimSpace(resp.Content)
		if len(resp.ToolCalls) == 0 {
			out.Answer = finalAnswer(content, resp.FinishReason, failed)
			return out, nil
		}
		if last {
			for _, tc := range resp.ToolCalls {
				out.Usage.call(tc.Name, "limit", 0)
			}
			out.Stopped = "limit"
			out.Answer = limitAnswer(content, len(out.LookedAt))
			return out, nil
		}

		turn := Message{Role: "assistant", Content: resp.Content}
		if valuesRead {
			turn.Data = DataSampleRows
		}
		var results []Message
		var unknown []string
		for i, tc := range resp.ToolCalls {
			if i >= maxCallsPerTurn {
				for _, rest := range resp.ToolCalls[i:] {
					out.Usage.call(rest.Name, "dropped", 0)
				}
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !offered[tc.Name] {
				// A call to a tool the model was not given would make the next
				// turn's conversation invalid; it is dropped, not answered.
				out.LookedAt = append(out.LookedAt, LookedAt{Tool: safeName(tc.Name), Arguments: json.RawMessage(`{}`)})
				out.Usage.call(tc.Name, "unknown_tool", 0)
				unknown = append(unknown, safeName(tc.Name))
				continue
			}
			id := tc.ID
			if !callIDPattern.MatchString(id) || usedIDs[id] {
				id = fmt.Sprintf("call_%d_%d", step, i)
			}
			usedIDs[id] = true
			args, argsOK := objectArguments(tc.Arguments)
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{ID: id, Name: tc.Name, Arguments: args, Signature: tc.Signature})

			var text string
			// callFailed: the call ran, or was refused, with an error the
			// model read. An empty last turn lists these.
			callFailed := false
			// toolRan: the tool itself ran, so its error may be about the
			// connection the call named.
			toolRan := false
			isSample := tc.Name == SampleRowsTool
			isSQL := tc.Name == GenerateSQLTool
			switch {
			case ran >= s.limits.ToolCalls || read >= s.limits.ToolBudgetChars:
				text = "Not run: the limit of tool calls for one question was reached. Answer from what you have."
				if isSample || isSQL {
					text = sampleError(text)
				}
				out.Usage.call(tc.Name, "limit", 0)
			case !argsOK:
				text = "Not run: the arguments must be a JSON object of at most 4000 characters."
				if isSample || isSQL {
					text = sampleError(text)
				}
				out.Usage.call(tc.Name, "invalid_arguments", http.StatusBadRequest)
				callFailed = true
			case strings.HasPrefix(tc.Name, "plan_") && guideOf[tc.Name] != "" && !guides.has(guideOf[tc.Name]):
				// A draft is made only with its area's rules in the
				// conversation: the refusal carries them, so the next call runs.
				topic := guideOf[tc.Name]
				guide, _ := agentgw.GuideText(topic)
				text = "Not run: " + tc.Name + " needs the " + topic + " rules, which follow. " +
					"Read them, then call " + tc.Name + " again.\n\n" + guide
				guides.add(topic, guide)
				out.Usage.call(tc.Name, "guide_missing", 0)
			case isSample && sampled >= maxSampleCalls:
				text = sampleError("Not run: sample_rows runs at most 3 times per question. Answer from what you have.")
				out.Usage.call(tc.Name, "limit", 0)
			case isSample:
				ran++
				sampled++
				toolRan = true
				var isErr bool
				var summary *SampleSummary
				text, isErr, summary = s.runSampleRows(c, q, args)
				callFailed = isErr
				read += len(text)
				out.LookedAt = append(out.LookedAt, LookedAt{
					Tool: tc.Name, Arguments: json.RawMessage(args), OK: !isErr, Sample: summary, callID: id,
				})
				out.Usage.call(tc.Name, okOrError(isErr), 0)
			case isSQL && (valuesRead || valuesInConversation(msgs)):
				// The question it sends goes to the Explorer's steps, which
				// take names only: none may carry a value the model read.
				text = sampleError("Not run: sample values are in this conversation, so SQL cannot be written in it. " +
					"Tell the person to ask for the SQL in a new conversation.")
				out.Usage.call(tc.Name, "values_in_conversation", 0)
				callFailed = true
			case isSQL:
				ran++
				toolRan = true
				var isErr bool
				var draft *SQLDraft
				text, isErr, draft = s.runGenerateSQL(c, q, args)
				callFailed = isErr
				read += len(text)
				out.LookedAt = append(out.LookedAt, LookedAt{
					Tool: tc.Name, Arguments: json.RawMessage(args), OK: !isErr, SQL: draft, callID: id,
				})
				out.Usage.call(tc.Name, okOrError(isErr), 0)
			default:
				ran++
				toolRan = true
				r := s.opts.Tools.Run(c, tc.Name, json.RawMessage(args))
				text = r.Text
				isErr, made, outcome := r.IsError, r.ResourceID, r.Outcome
				if len(text) > s.limits.ToolResultChars {
					// A long list is cut, its totals kept; only what cannot
					// be cut is replaced.
					if fitted, ok := fitResult(text, s.limits.ToolResultChars); ok && !isErr {
						text = fitted
					} else {
						text = fmt.Sprintf("The result was too large to read (%d characters). Ask for less, for example one pipeline or a smaller limit.", len(text))
						isErr = true
						outcome = "too_large"
					}
				}
				out.Usage.call(tc.Name, outcome, r.Status)
				callFailed = isErr
				read += len(text)
				if tc.Name == agentgw.GuideTool && !isErr {
					guides.add(guideArg(args), text)
				}
				looked := LookedAt{Tool: tc.Name, Arguments: json.RawMessage(args), OK: !isErr}
				if !isErr {
					looked.ResourceID = made
					if made != "" {
						s.meterSQL(c, tc.Name, args)
					}
				}
				out.LookedAt = append(out.LookedAt, looked)
			}
			if callFailed {
				failed.add(tc.Name, text)
				required, takes := needsConnection[tc.Name]
				if toolRan && takes && offered[listConnectionsTool] {
					if hint := connectionHint(c, s.opts.Tools, &conns, tc.Name, required, args, hintLimit(tc.Name, text)); hint != "" {
						before := len(text)
						text = withHint(text, hint)
						read += len(text) - before
					}
				}
			}
			if text == "" {
				text = "(empty result)"
			}
			result := Message{Role: "tool", ToolCallID: id, Content: text}
			if isSample {
				// Every sample_rows result is marked, an error too: the guard
				// refuses an unmarked one.
				result.Data = DataSampleRows
				valuesRead = true
			}
			results = append(results, result)
		}
		if len(turn.ToolCalls) == 0 {
			// Every call named a tool that does not exist. The model is told
			// which do and asked again; the made-up calls stay out of the
			// conversation, which the step route would refuse.
			if corrections < maxToolCorrections {
				corrections++
				if content != "" {
					msgs = append(msgs, turn)
				}
				msgs = append(msgs, Message{Role: "user", Content: unknownToolNote(unknown, tools, valuesRead)})
				continue
			}
			out.Answer = content
			if out.Answer == "" {
				out.Answer = "The model asked for a tool rsync.ai does not have. Try rephrasing the question."
			}
			return out, nil
		}
		msgs = append(msgs, turn)
		msgs = append(msgs, results...)
	}
}

// sqlTools are the tools whose SQL the model writes, by the argument naming
// the connection it runs on. One a tool saved counts as one NL→SQL query.
var sqlTools = map[string]string{
	"plan_workflow": "connection_id",
	"create_model":  "connection_id",
}

// meterSQL counts a tool's saved SQL on the Usage page. args passed the tool's
// own checks; a connection id it cannot read is counted without one.
func (s *Service) meterSQL(c *gin.Context, tool, args string) {
	connArg, ok := sqlTools[tool]
	if !ok || s.opts.MeterSQL == nil {
		return
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(args), &in)
	raw, _ := in[connArg].(string)
	connectionID := ""
	if id, err := uuid.Parse(strings.TrimSpace(raw)); err == nil {
		connectionID = id.String()
	}
	s.opts.MeterSQL(c, connectionID)
}

// objectArguments returns the call's arguments when they are a JSON object
// the step route accepts, else "{}" and false.
func objectArguments(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", true
	}
	var obj map[string]json.RawMessage
	if len(raw) > maxArgumentChars || json.Unmarshal([]byte(raw), &obj) != nil || obj == nil {
		return "{}", false
	}
	return raw, true
}

func okOrError(isErr bool) string {
	if isErr {
		return "error"
	}
	return "ok"
}

func safeName(name string) string {
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// unknownToolNote tells a model whose every call named a tool it was not
// given which tools it has. Once a sample_rows result is in the conversation
// the made-up names are not repeated: the note is a user message, never
// marked as data, and a name the model wrote then may quote a value.
func unknownToolNote(unknown []string, tools []Tool, valuesRead bool) string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	sort.Strings(names)
	what := "that tool does not exist here"
	if !valuesRead {
		what = unknown[0] + " is not a tool here"
		if len(unknown) > 1 {
			what = strings.Join(unknown, ", ") + " are not tools here"
		}
	}
	return "Not run: " + what + ". The tools are: " + strings.Join(names, ", ") + ". Call one of them by its exact name, or answer."
}

// emptyTurn is a turn that ended by the model's choice with nothing in it: no
// words, no tool call, not cut off at the length limit or by the provider's filter.
func emptyTurn(r *StepResponse) bool {
	return len(r.ToolCalls) == 0 && strings.TrimSpace(r.Content) == "" &&
		r.FinishReason != "length" && r.FinishReason != "content_filter"
}

// finalAnswer is the answer the model's last turn gives: its own words, or,
// when it wrote none, why. finish is the provider's stop reason in OpenAI's
// names, which llm-service maps Anthropic's to.
func finalAnswer(content, finish string, failed failedCalls) string {
	if content != "" {
		if finish == "length" {
			return content + "\n\n(The answer was cut off: the model reached its length limit.)"
		}
		return content
	}
	lead, next := "The model returned an empty answer.", "Try asking again."
	switch finish {
	case "length":
		lead, next = "The model ran out of room before it wrote an answer.", "Ask a narrower question."
	case "content_filter":
		lead = "The model provider declined to answer."
	}
	if len(failed) == 0 {
		return lead + " " + next
	}
	var b strings.Builder
	b.WriteString(lead)
	b.WriteString(" These calls failed:\n")
	shown := failed
	if len(shown) > maxFailuresShown {
		shown = shown[:maxFailuresShown]
	}
	for _, f := range shown {
		b.WriteString("- " + f + "\n")
	}
	if more := len(failed) - len(shown); more > 0 {
		fmt.Fprintf(&b, "- and %d more\n", more)
	}
	b.WriteString("\nTry rephrasing the question.")
	return b.String()
}

// maxFailuresShown and maxFailureChars keep an empty answer's list short.
const (
	maxFailuresShown = 3
	maxFailureChars  = 240
)

// failedCalls are the failed calls of one question, as "tool: message", each
// once.
type failedCalls []string

// add records a failed call by the error the model read: the message of a
// sample_rows or generate_sql error, a tool's own text otherwise.
func (f *failedCalls) add(tool, text string) {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(text), &e) == nil && e.Error != "" {
		text = e.Error
	}
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > maxFailureChars {
		text = string(r[:maxFailureChars]) + "…"
	}
	if text == "" {
		text = "no message"
	}
	line := safeName(tool) + ": " + text
	for _, seen := range *f {
		if seen == line {
			return
		}
	}
	*f = append(*f, line)
}

func limitAnswer(content string, lookedAt int) string {
	if content != "" {
		return content
	}
	return fmt.Sprintf("I looked at %d things but reached the limit for one question before finishing. "+
		"Ask a narrower question, for example about one pipeline.", lookedAt)
}

// userWords is what the user typed in this conversation: the earlier
// questions and this one. Never an answer, which the model wrote.
func userWords(q question) string {
	var b strings.Builder
	for _, t := range q.history {
		b.WriteString(t.Question)
		b.WriteByte('\n')
	}
	b.WriteString(q.text)
	return b.String()
}

// historyMessages turns the earlier questions into messages, newest kept,
// within the history limits. An answer formed from sample rows goes only to
// the entry that read them, and only while sample rows are open; otherwise a
// fixed line stands in for it.
func (s *Service) historyMessages(turns []Turn, entry string, sampling bool) []Message {
	if len(turns) > s.limits.HistoryTurns {
		turns = turns[len(turns)-s.limits.HistoryTurns:]
	}
	total := 0
	start := len(turns)
	for start > 0 {
		n := len(turns[start-1].Question) + len(turns[start-1].Answer)
		if total+n > s.limits.HistoryChars {
			break
		}
		total += n
		start--
	}
	msgs := make([]Message, 0, 2*(len(turns)-start))
	for _, t := range turns[start:] {
		answer := Message{Role: "assistant", Content: t.Answer}
		if t.Samples != "" {
			answer.Data = DataSampleRows
			if !sampling || t.Samples != entry {
				answer = Message{Role: "assistant", Content: withheldAnswer}
			}
		}
		msgs = append(msgs, Message{Role: "user", Content: t.Question}, answer)
	}
	return msgs
}

// remember appends the finished question to the conversation. samples is the
// entry that read sample rows for the answer, "" when none did; guides are
// the topic guides it loaded.
func (s *Service) remember(ctx context.Context, key string, turns []Turn, q, a, samples string, guides []string) error {
	if len(a) > maxStoredAnswer {
		a = strings.ToValidUTF8(a[:maxStoredAnswer], "")
	}
	turns = append(turns, Turn{Question: q, Answer: a, Samples: samples, Guides: guides})
	if len(turns) > s.limits.HistoryTurns {
		turns = turns[len(turns)-s.limits.HistoryTurns:]
	}
	return s.opts.Store.Save(ctx, key, turns)
}
