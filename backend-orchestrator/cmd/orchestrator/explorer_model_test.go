package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// A model statement runs for minutes, past the orchestrator server's WriteTimeout.
// Here the server's timeouts are shrunk to 200 ms (read) / 300 ms (write) and the
// statement takes 700 ms: modelStatementContext must keep the response deliverable for
// that long. The short ReadTimeout shows it does not cut a running handler off.
func TestModelStatementContext_OutlastsTheServerTimeouts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const statement = 700 * time.Millisecond

	serve := func(lift bool) (int, string, error) {
		r := gin.New()
		r.POST("/q", func(c *gin.Context) {
			ctx := c.Request.Context()
			if lift {
				var cancel func()
				ctx, cancel, _ = modelStatementContext(c, 60)
				defer cancel()
			}
			select {
			case <-time.After(statement):
				c.String(http.StatusOK, "done")
			case <-ctx.Done():
				c.String(http.StatusGatewayTimeout, "cancelled: %v", ctx.Err())
			}
		})
		srv := httptest.NewUnstartedServer(r)
		srv.Config.ReadTimeout = 200 * time.Millisecond
		srv.Config.WriteTimeout = 300 * time.Millisecond
		srv.Start()
		defer srv.Close()

		resp, err := srv.Client().Post(srv.URL+"/q", "application/json", nil)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), err
	}

	// Control: without the lift the server's timeouts cut the statement off, so this
	// test can fail.
	if code, body, err := serve(false); err == nil && code == http.StatusOK {
		t.Fatalf("control: a %s statement got %d %q under 200 ms/300 ms server timeouts; the test cannot tell the lift apart", statement, code, body)
	}
	code, body, err := serve(true)
	if err != nil || code != http.StatusOK || body != "done" {
		t.Fatalf("lifted: got (%d, %q, %v), want (200, \"done\", nil)", code, body, err)
	}
}

func TestModelStatementContext_Budget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		in   int
		want int
	}{
		{120, 120},
		{0, int(modelStatementMax / time.Second)},
		{-5, int(modelStatementMax / time.Second)},
		{int(2 * modelStatementMax / time.Second), int(modelStatementMax / time.Second)},
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/q", nil)
		ctx, cancel, got := modelStatementContext(c, tc.in)
		deadline, ok := ctx.Deadline()
		cancel()
		if got != tc.want {
			t.Errorf("timeout_seconds %d: budget %d, want %d", tc.in, got, tc.want)
		}
		wantDeadline := time.Duration(tc.want)*time.Second + modelStatementMargin
		if left := time.Until(deadline); !ok || left > wantDeadline || left < wantDeadline-5*time.Second {
			t.Errorf("timeout_seconds %d: ctx deadline in %s, want about %s", tc.in, left, wantDeadline)
		}
	}
}
