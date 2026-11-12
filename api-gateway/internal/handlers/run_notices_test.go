package handlers

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Shared by the run-notice tests (pipeline_stop_notice, pipeline_wait_alert,
// model_run_alert, workflow_run_alert): a publisher that keeps what it was sent,
// with the publish run in line so a test reads it back without waiting.

type runNoticeRecorder struct {
	mu   sync.Mutex
	sent [][]byte
}

func (r *runNoticeRecorder) Publish(_ context.Context, raw []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, append([]byte(nil), raw...))
	return nil
}

// notices decodes every payload sent so far.
func (r *runNoticeRecorder) notices(t *testing.T) []map[string]interface{} {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]interface{}, 0, len(r.sent))
	for _, raw := range r.sent {
		var m map[string]interface{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("run notice is not JSON: %v (%s)", err, raw)
		}
		out = append(out, m)
	}
	return out
}

// captureRunNotices wires a recorder and runs every publish in line, for the
// length of the test.
func captureRunNotices(t *testing.T) *runNoticeRecorder {
	t.Helper()
	rec := &runNoticeRecorder{}
	prevGo := runNoticeGo
	prev := currentRunNoticeNotifier()
	runNoticeGo = func(f func()) { f() }
	SetRunNoticeNotifier(rec)
	t.Cleanup(func() {
		runNoticeGo = prevGo
		SetRunNoticeNotifier(prev)
	})
	return rec
}

// noticeError is the payload's structured-error envelope.
func noticeError(t *testing.T, n map[string]interface{}) map[string]interface{} {
	t.Helper()
	env, ok := n["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("run notice has no error envelope: %v", n)
	}
	return env
}

func sortedKeys(m map[string]interface{}) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// assertMetadataOnly pins the whole key set of a notice, so a field carrying an
// error's text, a row, a count or who pressed the button cannot be added
// without a test saying so. subjectKey is the one id key it routes on.
func assertMetadataOnly(t *testing.T, n map[string]interface{}, subjectKey string) {
	t.Helper()
	keys := []string{"action_url", "error", "message", "timestamp", "type", subjectKey}
	sort.Strings(keys)
	if got, want := sortedKeys(n), strings.Join(keys, ","); got != want {
		t.Errorf("notice keys = %s, want exactly %s", got, want)
	}
	if got, want := sortedKeys(noticeError(t, n)), "audience,code,dedup_subject,failure_type,severity,user_message"; got != want {
		t.Errorf("envelope keys = %s, want exactly %s", got, want)
	}
	if n["type"] != "structured_error_notification" {
		t.Errorf("type = %v; the notifier renders catalog copy only for the structured envelope", n["type"])
	}
	if noticeError(t, n)["audience"] != "user" {
		t.Errorf("audience = %v, want user", noticeError(t, n)["audience"])
	}
}

// A nil publisher is the state of every binary and test that did not wire one.
// It must be a no-op, and must not start a goroutine for nothing.
func TestRunNotice_NilPublisherSendsNothing(t *testing.T) {
	prevGo := runNoticeGo
	prev := currentRunNoticeNotifier()
	started := 0
	runNoticeGo = func(f func()) { started++; f() }
	SetRunNoticeNotifier(nil)
	t.Cleanup(func() { runNoticeGo = prevGo; SetRunNoticeNotifier(prev) })

	publishRunNotice(runNotice{PipelineID: wsScopePipeline, Code: "PIPELINE_RUN_STOPPED"})
	if started != 0 {
		t.Fatalf("a nil publisher started %d publishes", started)
	}
}
