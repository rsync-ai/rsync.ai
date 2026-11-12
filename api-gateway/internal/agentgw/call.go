package agentgw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
)

// toolCall is one tools/call: the caller's request and its checked arguments.
type toolCall struct {
	s    *Server
	c    *gin.Context
	args map[string]any // string -> string, integer -> int, array -> []map[string]any
}

func (t *toolCall) str(name string) string {
	v, _ := t.args[name].(string)
	return v
}

func (t *toolCall) num(name string) int {
	v, _ := t.args[name].(int)
	return v
}

// items is an array argument's checked objects; nil when it was left out.
func (t *toolCall) items(name string) []map[string]any {
	v, _ := t.args[name].([]map[string]any)
	return v
}

func (t *toolCall) setQuery(q url.Values, arg, param string) {
	if v := t.str(arg); v != "" {
		q.Set(param, v)
	}
}

// itemStr and itemNum read one field of an array argument's object.
func itemStr(item map[string]any, name string) string {
	v, _ := item[name].(string)
	return v
}

func itemNum(item map[string]any, name string) int {
	v, _ := item[name].(int)
	return v
}

// toolError is a failed call as the agent sees it (message, fixed text) and
// as the audit row records it (outcome, status).
type toolError struct {
	status  int
	outcome string
	message string
	// reason is the handler's own refusal text, scrubbed, kept for a 4xx only.
	// A read tool never shows it; a model tool adds it with explain, because a
	// write refused without its reason ("the name is taken", "a model that
	// writes to a table must be a read query") leaves the agent nothing to fix.
	reason string
}

func (e *toolError) Error() string { return e.message }

func argError(format string, a ...any) *toolError {
	return &toolError{status: http.StatusBadRequest, outcome: "invalid_arguments", message: "Invalid arguments: " + fmt.Sprintf(format, a...)}
}

var errCodeRe = regexp.MustCompile(`^[a-z_]{1,40}$`)

// get runs a REST handler in-process, as the caller, and decodes a 2xx JSON
// body into dst. The synthetic request carries the caller's context keys
// (user, workspace, role, token) and request context, the given path params
// and query, and nothing else: no header of the real request is copied, so
// the Authorization header never reaches a handler. A non-2xx answer becomes
// fixed text by status; the handler's own message is never passed on, only
// its error code when it is shaped like one.
func (t *toolCall) get(h gin.HandlerFunc, params gin.Params, query url.Values, dst any) error {
	return t.do(http.MethodGet, h, params, query, nil, dst)
}

// do is get for any method: a non-nil body goes as JSON, and a nil dst skips
// reading the answer.
func (t *toolCall) do(method string, h gin.HandlerFunc, params gin.Params, query url.Values, body, dst any) error {
	w := &bufferWriter{header: http.Header{}}
	sc := gin.CreateTestContextOnly(w, t.s.engine)
	target := "/agent-gateway/tool"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &toolError{status: http.StatusInternalServerError, outcome: "error", message: "rsync.ai could not run this tool."}
		}
		reqBody = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.c.Request.Context(), method, target, reqBody)
	if err != nil {
		return &toolError{status: http.StatusInternalServerError, outcome: "error", message: "rsync.ai could not run this tool."}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The caller's trusted client IP, not RemoteAddr (the proxy in front of
	// the gateway): the handler's own audit row records sc.ClientIP().
	req.RemoteAddr = net.JoinHostPort(t.c.ClientIP(), "0")
	sc.Request = req
	sc.Params = params
	for k, v := range t.c.Keys {
		sc.Set(k, v)
	}
	if t.s.opts.Mark != nil {
		t.s.opts.Mark(sc)
	}
	h(sc)

	status := sc.Writer.Status()
	if status < 200 || status > 299 {
		return handlerError(status, w.buf.Bytes())
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(w.buf.Bytes(), dst); err != nil {
		return &toolError{status: http.StatusBadGateway, outcome: "error", message: "rsync.ai returned an answer this tool could not read."}
	}
	return nil
}

func handlerError(status int, body []byte) *toolError {
	var msg string
	switch {
	case status == http.StatusBadRequest:
		msg = "rsync.ai refused the request as invalid."
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Neutral wording: an agent token and the in-app assistant (no token)
		// both read this, and a write tool gets it as well as a read.
		msg = "Refused: the user's workspace role does not allow this."
	case status == http.StatusNotFound:
		msg = "Not found in this workspace."
	case status == http.StatusTooManyRequests:
		msg = "rsync.ai is rate limiting these reads. Wait and retry."
	case status >= 500:
		msg = "rsync.ai could not answer this right now. Try again later."
	default:
		msg = fmt.Sprintf("rsync.ai answered with HTTP %d.", status)
	}
	var e struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	reason := ""
	if json.Unmarshal(body, &e) == nil {
		for _, code := range []string{e.Code, e.Error} {
			if errCodeRe.MatchString(code) {
				msg += " (code: " + code + ")"
				break
			}
		}
		if status >= 400 && status < 500 && !errCodeRe.MatchString(e.Error) {
			reason = maskNumbers(llmscrub.Scrub(cleanText(e.Error)))
		}
	}
	return &toolError{status: status, outcome: "error", message: msg, reason: reason}
}

// explain adds a refused write's reason to the text the agent reads.
func explain(err error) error {
	var te *toolError
	if errors.As(err, &te) && te.reason != "" {
		return &toolError{status: te.status, outcome: te.outcome, message: te.message + " " + te.reason}
	}
	return err
}

// parseArgs checks raw arguments against the tool's Args and returns them
// typed, with defaults filled in. An argument the tool does not declare is
// refused, so a typo is an error rather than a silently broader answer.
func parseArgs(tool *Tool, raw json.RawMessage) (map[string]any, error) {
	in := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, argError("arguments must be a JSON object")
		}
	}
	return parseObject(tool.Args, in, "", tool.Name)
}

// parseObject checks one object's fields against args: the call's arguments,
// or one item of an array argument, whose errors name it by path
// ("branches[2].condition").
func parseObject(args []Arg, in map[string]any, path, owner string) (map[string]any, error) {
	known := map[string]bool{}
	for _, a := range args {
		known[a.Name] = true
	}
	var unknown []string
	for k := range in {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, argError("unknown argument %q; %s takes %s", path+unknown[0], owner, argNames(args))
	}

	out := map[string]any{}
	for _, a := range args {
		name := path + a.Name
		v, present := in[a.Name]
		if !present || v == nil {
			if a.Required {
				return nil, argError("%s is required", name)
			}
			if a.Type == "integer" {
				out[a.Name] = a.Default
			}
			continue
		}
		switch a.Type {
		case "integer":
			n, ok := asInt(v)
			if !ok {
				return nil, argError("%s must be a whole number", name)
			}
			if n < a.Min || n > a.Max {
				return nil, argError("%s must be between %d and %d", name, a.Min, a.Max)
			}
			out[a.Name] = n
		case "array":
			items, err := parseItems(a, v, name)
			if err != nil {
				return nil, err
			}
			if len(items) == 0 && a.Required {
				return nil, argError("%s is required", name)
			}
			if len(items) > 0 {
				out[a.Name] = items
			}
		default:
			s, ok := v.(string)
			if !ok {
				return nil, argError("%s must be a string", name)
			}
			s = strings.TrimSpace(s)
			// An empty optional id is the same as leaving it out; a required
			// one is refused as missing below.
			if a.Format == "uuid" && s != "" {
				u, err := uuid.Parse(s)
				if err != nil {
					// Models often pass a name where an id goes: say where the id comes from.
					return nil, argError("%s must be an id (a uuid), not a name. %s", name, a.Description)
				}
				s = u.String()
			}
			if a.MaxLength > 0 && len([]rune(s)) > a.MaxLength {
				return nil, argError("%s must be at most %d characters", name, a.MaxLength)
			}
			if len(a.Enum) > 0 && s != "" {
				if s = enumValue(a, s); !contains(a.Enum, s) {
					return nil, argError("%s must be one of %s", name, strings.Join(a.Enum, ", "))
				}
			}
			if s == "" && a.Required {
				return nil, argError("%s is required", name)
			}
			if s == "" {
				continue
			}
			if !a.Raw {
				out[a.Name] = cleanText(s)
				continue
			}
			if at := hiddenRune(s); at > 0 {
				return nil, argError("%s has a control or invisible formatting character at character %d; remove it", name, at)
			}
			out[a.Name] = s
		}
	}
	return out, nil
}

// parseItems checks an array argument: at most MaxItems objects, each checked
// against Items. A model that sends the array as a JSON string (some write
// every argument as a string) has it read as the array it holds.
func parseItems(a Arg, v any, name string) ([]map[string]any, error) {
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, argError("%s must be a list of objects", name)
		}
	}
	list, ok := v.([]any)
	if !ok {
		return nil, argError("%s must be a list of objects", name)
	}
	if len(list) > a.MaxItems {
		return nil, argError("%s has %d items; at most %d", name, len(list), a.MaxItems)
	}
	out := make([]map[string]any, 0, len(list))
	for i, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, argError("%s[%d] must be an object", name, i)
		}
		parsed, err := parseObject(a.Items, obj, fmt.Sprintf("%s[%d].", name, i), name+"[]")
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
}

// hiddenRune is the position, counting characters from 1, of the first
// character a Raw argument may not carry: a control character other than a
// line break or tab, or a format character (bidi overrides, zero-width and tag
// characters) that would make the text a person reviews differ from the text
// that runs. 0 when there is none.
func hiddenRune(s string) int {
	i := 0
	for _, r := range s {
		i++
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return i
		}
	}
	return 0
}

// enumKey is the form an enum value and a synonym are written in: lower
// case, words joined by "_". "In progress", "in-progress" and "IN_PROGRESS"
// share one key.
func enumKey(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	}), "_")
}

// enumValue maps the word a model passes for an enum argument (often the
// asker's own: "successful", "Sources") onto the enum value it means. A word
// it does not know comes back as it was, for the enum check to refuse.
func enumValue(a Arg, s string) string {
	k := enumKey(s)
	if v, ok := a.Synonyms[k]; ok {
		return v
	}
	if contains(a.Enum, k) {
		return k
	}
	return s
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil || f != math.Trunc(f) || math.Abs(f) > 1e9 {
			return 0, false
		}
		return int(f), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	}
	return 0, false
}

func argNames(args []Arg) string {
	if len(args) == 0 {
		return "no arguments"
	}
	names := make([]string, len(args))
	for i, a := range args {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// bufferWriter is the http.ResponseWriter of an in-process handler call.
type bufferWriter struct {
	header http.Header
	buf    bytes.Buffer
}

func (w *bufferWriter) Header() http.Header         { return w.header }
func (w *bufferWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *bufferWriter) WriteHeader(int)             {}
