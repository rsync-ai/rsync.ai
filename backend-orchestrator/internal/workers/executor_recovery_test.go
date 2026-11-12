package workers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/agents/executor"
)

// TestSuggestRecoveryActionCDCProvisioning pins KI-CDC-HEALER-RETRY-PK: the
// synchronous classifier used by executeWithHealer must treat CDC provisioning
// failures as non-retryable, exactly as CLAUDE.md and pkg/diagnose already do
// ("CDC provisioning errors (publication missing, slot, WAL level, PK missing,
// table not found) → ActionEscalate").
//
// Before the fix none of these matched any rule, so they fell through to the
// "unrecognized error shape" default and came back as retry_with_backoff for the
// first two attempts — two full backoff cycles that re-ran the same validation,
// hit the same missing PRIMARY KEY, and failed with the identical message.
func TestSuggestRecoveryActionCDCProvisioning(t *testing.T) {
	w := &ExecutorWorker{}

	// Verbatim messages from the code that emits them, so a reworded error
	// breaks this test instead of silently reopening the bug.
	cases := []struct {
		name string
		msg  string
	}{
		// No longer emitted: the executor stopped refusing keyless CDC tables on
		// 2026-09-27. Kept because runs recorded before then still carry it.
		{"missing primary key", "CDC requires PRIMARY KEY for DB destinations; missing PK on: public.events"},
		// internal/cdc/{postgresql,mysql,sqlserver,oracle}.go PK validators
		{"table absent from postgres source", "table not found in source postgresql: public.driverb_big"},
		{"table absent from mysql source", "table not found in source mysql: appdb.orders"},
		{"table absent from sqlserver source", "table not found in source sqlserver: dbo.orders"},
		{"table absent from oracle source", "table not found in source oracle: HR.ORDERS"},
		// PostgreSQL logical-replication provisioning
		{"publication missing", `ERROR: publication does not exist (SQLSTATE 42704)`},
		{"replication slot failure", "failed to create replication slot rsync_slot_2cb: all replication slots are in use"},
		{"slot already exists", "replication slot already exists: rsync_slot_2cb"},
		// SQL Server capture instances
		{"cdc not enabled on database", "cannot enable cdc on database appdb: needs sysadmin/db_owner"},
		{"agent down", "SQL Server Agent is not running; CDC capture will not advance"},
		{"capture instance missing", "capture instance dbo_orders does not exist"},
		// Oracle LogMiner prerequisites
		{"archivelog off", "ORACLE_ARCHIVELOG_DISABLED: database is not in ARCHIVELOG mode"},
		{"supplemental logging off", "supplemental logging is not enabled for table HR.ORDERS"},
		{"logminer grants missing", "ORACLE_LOGMINER_PRIVS_MISSING: LogMiner requires EXECUTE_CATALOG_ROLE"},
		// MongoDB change streams
		{"standalone mongod", "change streams require a replica set; server is not a replica set"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// attempt_count 0 is the worst case: the old code's retry budget was
			// still wide open, so the fall-through returned retry_with_backoff.
			errorContext := map[string]interface{}{
				"error_message": tc.msg,
				"attempt_count": 0,
			}
			got := w.suggestRecoveryAction(context.Background(), errorContext)
			if got != "fail" {
				t.Fatalf("suggestRecoveryAction(%q) = %q; want \"fail\" (a retry can never provision this)", tc.msg, got)
			}
			if errorContext["reasoning"] == "" || errorContext["reasoning"] == nil {
				t.Fatal("no reasoning recorded for the operator")
			}
		})
	}
}

// TestSuggestRecoveryActionStillRetriesTransient guards the other direction: the
// fail-fast block above must not swallow the genuinely transient shapes that the
// retry budget exists for. A source database that is briefly unreachable during
// PK validation really can succeed on the next attempt, so it must NOT be caught
// by the CDC-provisioning rule even though pkg/diagnose escalates it.
func TestSuggestRecoveryActionStillRetriesTransient(t *testing.T) {
	w := &ExecutorWorker{}

	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"connect timeout", "dial tcp 10.0.0.4:5432: i/o timeout", "retry_with_backoff"},
		{"connection reset", "read tcp: connection reset by peer", "retry_with_backoff"},
		{"source unreachable during pk validation", "failed to connect to postgresql for pk validation: server temporarily unavailable", "retry_with_backoff"},
		{"batch too large", "request payload too large", "retry_smaller_batch"},
		{"bad row", "invalid data in column created_at", "skip_and_continue"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errorContext := map[string]interface{}{
				"error_message": tc.msg,
				"attempt_count": 0,
			}
			got := w.suggestRecoveryAction(context.Background(), errorContext)
			if got != tc.want {
				t.Fatalf("suggestRecoveryAction(%q) = %q; want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// A setting the run needs is missing (generator-trust phase 2). The generated
// ClickUp connector, run with its list ID blank, failed on prod 2026-10-05 with
// the message below. It matched no rule, fell to the attemptCount<2 default and
// came back retry_with_backoff: the same request re-sent with the same blank
// setting, as if it were a network blip. Bug class: a deterministic config
// failure classified as transient. The asynchronous healer already asks the user
// for the setting (pkg/diagnose user-config rule → ActionRequestUserConfig).
func TestSuggestRecoveryActionMissingConfigIsNotRetried(t *testing.T) {
	w := &ExecutorWorker{}
	for _, msg := range []string{
		// Verbatim from prod (agents/executor/executor.go batch-export failure,
		// with the connector's quoted names scrubbed).
		"batch export failed (partial sync, 1 rows landed before error): Missing required path parameter '[redacted]' for endpoint '[redacted]'",
		// The same class in the shapes pkg/diagnose's user-config rule matches.
		"Missing required parameter: list_id",
		"required field 'workspace_id' is empty",
		"invalid configuration: base_url is not set",
		"config validation failed: database is required",
	} {
		t.Run(msg, func(t *testing.T) {
			errorContext := map[string]interface{}{"error_message": msg, "attempt_count": 0}
			if got := w.suggestRecoveryAction(context.Background(), errorContext); got != "fail" {
				t.Fatalf("suggestRecoveryAction(%q) = %q; want \"fail\" — a retry re-sends the same blank setting", msg, got)
			}
			reasoning, _ := errorContext["reasoning"].(string)
			if !strings.Contains(strings.ToLower(reasoning), "setting") {
				t.Fatalf("reasoning %q does not tell the user a setting is missing", reasoning)
			}
		})
	}
}

// A run aimed at a destination resource that takes no writes fails before the
// sink starts (agents/executor/dest_writable_check.go). The same target refuses
// again on every attempt, so a retry only delays telling the user which
// resources it can write.
func TestSuggestRecoveryActionDestinationNotWritableIsNotRetried(t *testing.T) {
	msg := `Failed to start sink: destination resource "products" is not writable on t-clickup: ` +
		`it takes writes to list_task. Name one as the destination (e.g. "… as list_task")`
	errorContext := map[string]interface{}{"error_message": msg, "attempt_count": 0}
	if got := (&ExecutorWorker{}).suggestRecoveryAction(context.Background(), errorContext); got != "fail" {
		t.Fatalf("suggestRecoveryAction = %q; want \"fail\" — the same target refuses again", got)
	}
	if reasoning, _ := errorContext["reasoning"].(string); !strings.Contains(reasoning, "writable") {
		t.Fatalf("reasoning %q does not say the destination resource is not writable", reasoning)
	}
}

// The real path: the executor must run once, not three times.
func TestHealerRunsAMissingConfigFailureOnce(t *testing.T) {
	runs := 0
	w := &ExecutorWorker{runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
		runs++
		return executor.ExecutorResponse{Status: "failed",
			Error: "batch export failed (partial sync, 1 rows landed before error): Missing required path parameter '[redacted]' for endpoint '[redacted]'"}
	}}
	w.executeWithHealer(context.Background(), stopTestTask(), 0)
	if runs != 1 {
		t.Fatalf("executor ran %d times, want 1 — a missing setting was retried", runs)
	}
}

// A destination that is out of space (generator-trust phase 2). The free Atlas
// cluster used as the prod test destination rejects writes with "you are over
// your space quota"; a bare "quota exceeded" is the same class from other
// stores. Space does not come back by waiting, so it is not retried. "Quota
// exceeded" with an explicit 429 is a rate limit (Google says "Quota exceeded
// for quota metric … 429"), covered below.
func TestSuggestRecoveryActionDestinationCapacityIsNotRetried(t *testing.T) {
	w := &ExecutorWorker{}
	for _, msg := range []string{
		"write failed: you are over your space quota, using 513 MB of 512 MB",
		"insert into events: quota exceeded",
		"pq: could not extend file: No space left on device",
		"disk full",
		"storage full: bucket rsync-landing",
	} {
		t.Run(msg, func(t *testing.T) {
			errorContext := map[string]interface{}{"error_message": msg, "attempt_count": 0}
			if got := w.suggestRecoveryAction(context.Background(), errorContext); got != "fail" {
				t.Fatalf("suggestRecoveryAction(%q) = %q; want \"fail\" — space does not come back by retrying", msg, got)
			}
		})
	}
}

// A rate limit (generator-trust phase 2). It fell to the default and was
// retried after attemptCount*2 seconds — zero on the first attempt — so the
// retry hit the same limit window. It gets its own action with a 60 s floor.
func TestSuggestRecoveryActionRateLimitWaitsOutTheWindow(t *testing.T) {
	w := &ExecutorWorker{}
	for _, msg := range []string{
		// shared/mcp-connectors/base_connector.py _page_error, after its own retries.
		"batch export failed (partial sync, 0 rows landed before error): Request failed with status 429",
		"HTTP 429 Too Many Requests",
		"Too Many Requests",
		"API rate limit exceeded for installation",
		"googleapi: Error 429: Quota exceeded for quota metric 'Read requests' (status 429)",
	} {
		t.Run(msg, func(t *testing.T) {
			errorContext := map[string]interface{}{"error_message": msg, "attempt_count": 0}
			if got := w.suggestRecoveryAction(context.Background(), errorContext); got != "retry_after_rate_limit" {
				t.Fatalf("suggestRecoveryAction(%q) = %q; want retry_after_rate_limit", msg, got)
			}
		})
	}
	// A row count is not a status code.
	errorContext := map[string]interface{}{"error_message": "wrote 1429 of 2000 rows: invalid data in column x", "attempt_count": 0}
	if got := w.suggestRecoveryAction(context.Background(), errorContext); got == "retry_after_rate_limit" {
		t.Fatal("a row count containing 429 was read as a rate limit")
	}
}

func TestHealerBackoffFloorsARateLimitAtOneMinute(t *testing.T) {
	cases := []struct {
		action  string
		attempt int
		want    time.Duration
	}{
		{"retry_with_backoff", 0, 0},
		{"retry_with_backoff", 2, 4 * time.Second},
		{"retry_after_rate_limit", 0, 60 * time.Second},
		{"retry_after_rate_limit", 2, 60 * time.Second},
	}
	for _, tc := range cases {
		if got := healerBackoff(tc.action, tc.attempt); got != tc.want {
			t.Fatalf("healerBackoff(%q, %d) = %v, want %v", tc.action, tc.attempt, got, tc.want)
		}
	}
}

// The rate-limit wait honours a Stop and re-runs only after the window.
func TestHealerRetriesARateLimitAfterTheWait(t *testing.T) {
	runs := 0
	w := &ExecutorWorker{runTaskFn: func(context.Context, executor.ExecutorTask) executor.ExecutorResponse {
		runs++
		if runs == 1 {
			return executor.ExecutorResponse{Status: "failed", Error: "Request failed with status 429"}
		}
		return executor.ExecutorResponse{Status: "success"}
	}}
	saved := rateLimitBackoffFloor
	rateLimitBackoffFloor = 10 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoffFloor = saved })

	if resp := w.executeWithHealer(context.Background(), stopTestTask(), 0); resp.Status != "success" || runs != 2 {
		t.Fatalf("status=%q runs=%d, want success after one retry", resp.Status, runs)
	}
}
