package handlers

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// KI-AUDIT-ROW-LOST-NON-UUID-RESOURCE-ID: audit_logs.resource_id is a uuid
// column, but some audited resources are not rows with a uuid — the
// notification-channel settings are the single row "1", a channel test names
// "slack" or "email". logAudit bound those raw, Postgres rejected the insert
// (22P02) and the audit row was lost, only a Warn in the gateway log. A
// resource without a uuid now writes a NULL resource_id and keeps its name in
// details.resource_ref, so the row lands.

// jsonDetails matches the details argument when it is JSON holding every
// wanted key with the wanted value.
type jsonDetails map[string]interface{}

func (want jsonDetails) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		return false
	}
	for k, w := range want {
		if got[k] != w {
			return false
		}
	}
	return true
}

func auditTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set("user_id", wsScopeUser)
	c.Set(ctxWorkspaceID, wsScopeWS)
	c.Set("trace_id", "trace-abc")
	return c
}

func TestLogAudit_ResourceWithoutAUUIDStillWritesTheRow(t *testing.T) {
	cases := []struct {
		name       string
		resourceID string
		details    interface{}
		want       jsonDetails
	}{
		// notification_channels.go: the settings row and a channel test, both
		// with gin.H details (a named type the old map check missed, so they
		// also lost their trace_id).
		{"settings row", "1", gin.H{"email_enabled": true},
			jsonDetails{"resource_ref": "1", "email_enabled": true, "trace_id": "trace-abc"}},
		{"channel test", "email", gin.H{"channel": "email", "success": true},
			jsonDetails{"resource_ref": "email", "channel": "email", "trace_id": "trace-abc"}},
		{"plain map", "slack", map[string]interface{}{"channel": "slack"},
			jsonDetails{"resource_ref": "slack", "channel": "slack", "trace_id": "trace-abc"}},
		{"no details", "not-a-uuid", nil,
			jsonDetails{"resource_ref": "not-a-uuid", "trace_id": "trace-abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := wsScopeMockDB(t)
			defer cleanup()
			mock.ExpectExec(`INSERT INTO audit_logs`).
				WithArgs(wsScopeUser, "admin_update_notification_channels", "notification_channels",
					nil, tc.want, sqlmock.AnyArg(), wsScopeWS).
				WillReturnResult(sqlmock.NewResult(1, 1))

			logAudit(auditTestContext(), "admin_update_notification_channels", "notification_channels", tc.resourceID, tc.details)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("a resource id that is not a uuid must bind NULL and move to details.resource_ref: %v", err)
			}
		})
	}
}

// Control: a uuid resource id is still bound as the resource_id, with no
// resource_ref, and gin.H details still get the trace_id.
func TestLogAudit_UUIDResourceIDIsKept(t *testing.T) {
	mock, cleanup := wsScopeMockDB(t)
	defer cleanup()
	mock.ExpectExec(`INSERT INTO audit_logs`).
		WithArgs(wsScopeUser, "update_pipeline", "pipeline", wsScopePipeline,
			jsonDetails{"name": "p", "trace_id": "trace-abc", "resource_ref": nil}, sqlmock.AnyArg(), wsScopeWS).
		WillReturnResult(sqlmock.NewResult(1, 1))

	logAudit(auditTestContext(), "update_pipeline", "pipeline", wsScopePipeline, gin.H{"name": "p"})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a uuid resource id must be kept as resource_id: %v", err)
	}
}
