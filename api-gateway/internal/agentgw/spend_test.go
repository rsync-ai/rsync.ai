package agentgw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"api-gateway/internal/services"

	"github.com/gin-gonic/gin"
)

// The route's middleware charges one call to the token's rate limit per HTTP
// request. A batch carries several tools/call in one request, so each one
// after the first is charged through Spend; one Spend refuses is answered as
// rate limited and runs no handler.
func TestBatch_EachToolCallSpendsTheLimit(t *testing.T) {
	ran := 0
	h := leakyHandlers()
	inner := h.GetExecution
	h.GetExecution = func(c *gin.Context) { ran++; inner(c) }
	spent := 0
	var rows []auditRow
	s, err := New(Options{
		Handlers: h,
		Actor:    func(*gin.Context) services.Actor { return readerActor },
		Audit: func(_ *gin.Context, tool, rt, rid, outcome string, status int) {
			rows = append(rows, auditRow{tool, rt, rid, outcome, status})
		},
		Spend:   func(*gin.Context) bool { spent++; return spent <= 1 },
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/mcp", s.Handle)

	callMsg := func(id int) string {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": "get_run_status", "arguments": map[string]any{"run_id": runID}}})
		return string(raw)
	}

	// One tools/call alone: the middleware's charge covers it.
	if w := post(r, callMsg(1)); w.Code != http.StatusOK || spent != 0 || ran != 1 {
		t.Fatalf("single call: %d spent=%d ran=%d", w.Code, spent, ran)
	}

	// A ping, then three calls: the 2nd call spends one (allowed), the 3rd is refused.
	ran, rows = 0, nil
	w := post(r, "["+`{"jsonrpc":"2.0","id":0,"method":"ping"}`+","+callMsg(1)+","+callMsg(2)+","+callMsg(3)+"]")
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 4 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if spent != 2 || ran != 2 {
		t.Fatalf("spent=%d ran=%d, want 2 and 2", spent, ran)
	}
	last, _ := out[3]["result"].(map[string]any)
	if !isError(last) || !strings.Contains(resultText(last), "rate limiting") {
		t.Fatalf("the call past the limit: %v", out[3])
	}
	if got := rows[len(rows)-1]; got != (auditRow{"get_run_status", "execution", runID, "rate_limited", http.StatusTooManyRequests}) {
		t.Fatalf("audit = %+v", rows)
	}
}
