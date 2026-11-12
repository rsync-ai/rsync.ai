package notifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A failed unattended workflow run's alert is about the workflow, not a pipeline
// (handlers/workflow_run_alert.go): it goes to whoever the workflow runs as, is
// labelled with the workflow's name, and stores workflow_id so the bell can scope
// it to the workflow's workspace.

const (
	alertWorkflow    = "3b7e2f10-6c4d-4a8e-9f21-0d5c8b7a6e43"
	alertWorkflowRun = "9a1d3c5e-7f20-4b46-8d8a-1c3e5f7a9b0d"
	alertRunAs       = "22222222-2222-2222-2222-222222222222"
)

func workflowRunFailedEvent(t *testing.T, workflowID string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"type":        "structured_error_notification",
		"workflow_id": workflowID,
		"message":     "A scheduled or pipeline-triggered run of this workflow failed.",
		"action_url":  "/workflows/" + alertWorkflow,
		"error": map[string]interface{}{
			"failure_type":  "workflow_run_failed",
			"code":          CodeWorkflowRunFailed,
			"severity":      "error",
			"audience":      "user",
			"user_message":  "A scheduled or pipeline-triggered run of this workflow failed.",
			"dedup_subject": alertWorkflowRun,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The run-as user first (the workflow acts as them), then whoever made it, then the
// instance's admins: a workflow whose people are both gone still tells someone.
func TestWorkflowAlertRoutesToTheWorkflowOwner(t *testing.T) {
	cases := []struct {
		name             string
		runAs, createdBy interface{}
		admins           []string
		want             []string
	}{
		{name: "the user it runs as", runAs: alertRunAs, createdBy: emitterOwner, want: []string{alertRunAs}},
		{name: "its maker when it runs as nobody", runAs: nil, createdBy: emitterOwner, want: []string{emitterOwner}},
		{name: "the admins when both are gone", runAs: nil, createdBy: nil,
			admins: []string{testAdminA, testAdminB}, want: []string{testAdminA, testAdminB}},
	}
	if len(cases) == 0 {
		t.Fatal("no routing cases")
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearNotifierEnv(t)
			InvalidateChannelCache()
			t.Cleanup(InvalidateChannelCache)
			mockDB, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer mockDB.Close()

			actionURL := "/workflows/" + alertWorkflow
			dedupKey := makeDedupKey(workflowDedupScope(alertWorkflow), CodeWorkflowRunFailed, actionURL, alertWorkflowRun)
			metas := make([]map[string]interface{}, len(tc.want))

			mock.ExpectQuery(`SELECT run_as_user_id::text, created_by::text, COALESCE\(name, ''\) FROM workflows WHERE id = \$1`).
				WithArgs(alertWorkflow).
				WillReturnRows(sqlmock.NewRows([]string{"run_as_user_id", "created_by", "name"}).
					AddRow(tc.runAs, tc.createdBy, "nightly_close"))
			if tc.admins != nil {
				rows := sqlmock.NewRows([]string{"id"})
				for _, a := range tc.admins {
					rows.AddRow(a)
				}
				mock.ExpectQuery(`FROM users\s+WHERE role = 'admin'`).WillReturnRows(rows)
			}
			mock.ExpectQuery(`SELECT id::text FROM pipeline_notifications\s+WHERE dedup_key = \$1`).
				WithArgs(dedupKey).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			mock.ExpectBegin()
			for i, recipient := range tc.want {
				// No saved_query_id column: a workflow's alert is stored like an
				// instance alert, with a NULL pipeline_id and its id in metadata.
				mock.ExpectExec(`INSERT INTO pipeline_notifications\s+\(id, pipeline_id, user_id, type, severity, title, message, action_url, metadata, delivery_status, dedup_key, created_at\)`).
					WithArgs(sqlmock.AnyArg(), nil, recipient, "structured_error_notification", severityCritical,
						"nightly_close failed",
						"A scheduled or pipeline-triggered run of this workflow failed.",
						actionURL, jsonCapture{&metas[i]}, dedupKey).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectCommit()
			mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
			// Each recipient gets its own delivery_status write; only the first
			// carries the instance-wide channels, so only its status is pinned.
			mock.ExpectExec(`UPDATE pipeline_notifications`).
				WithArgs(StatusSuppressed, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))
			for range tc.want[1:] {
				mock.ExpectExec(`UPDATE pipeline_notifications`).WillReturnResult(sqlmock.NewResult(0, 1))
			}

			if err := NewEmitter(mockDB).Publish(context.Background(), workflowRunFailedEvent(t, alertWorkflow)); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sqlmock expectations: %v", err)
			}
			for _, meta := range metas {
				if meta["category"] != CategoryRunStatus || meta["error_code"] != CodeWorkflowRunFailed ||
					meta["workflow_id"] != alertWorkflow || meta["workflow_name"] != "nightly_close" ||
					meta["action_label"] != "Open workflow" {
					t.Errorf("metadata = %v", meta)
				}
				for _, key := range []string{"pipeline_name", "saved_query_id", "model_name"} {
					if _, ok := meta[key]; ok {
						t.Errorf("a workflow alert carries %s: %v", key, meta)
					}
				}
			}
		})
	}
}

// A workflow that is gone has nobody to route to; redelivering cannot change that.
// Nor can an id that is not a uuid, and the lookup is by the canonical spelling,
// which is what the bell compares as text.
func TestWorkflowAlertForADeletedWorkflowIsPermanent(t *testing.T) {
	clearNotifierEnv(t)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(`FROM workflows WHERE id = \$1`).WithArgs(alertWorkflow).WillReturnError(sql.ErrNoRows)

	err = NewEmitter(mockDB).Publish(context.Background(), workflowRunFailedEvent(t, "{"+strings.ToUpper(alertWorkflow)+"}"))
	if !isPermanent(err) {
		t.Fatalf("err = %v, want a permanent error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the workflow was not looked up by its canonical id: %v", err)
	}
	if err := NewEmitter(mockDB).Publish(context.Background(), workflowRunFailedEvent(t, "nightly")); !isPermanent(err) {
		t.Fatalf("err = %v, want a non-uuid workflow id to be permanent", err)
	}
}

// An install with no active admin, for a workflow whose people are both gone, has
// nobody to tell: a clean drop, never a retry that wedges the topic.
func TestWorkflowAlertWithNobodyToTellIsDropped(t *testing.T) {
	clearNotifierEnv(t)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(`FROM workflows WHERE id = \$1`).WithArgs(alertWorkflow).
		WillReturnRows(sqlmock.NewRows([]string{"run_as_user_id", "created_by", "name"}).AddRow(nil, nil, "nightly_close"))
	mock.ExpectQuery(`FROM users\s+WHERE role = 'admin'`).WillReturnRows(sqlmock.NewRows([]string{"id"}))

	if err := NewEmitter(mockDB).Publish(context.Background(), workflowRunFailedEvent(t, alertWorkflow)); err != nil {
		t.Fatalf("want a clean drop, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("nothing else should have been queried: %v", err)
	}
}

func TestWorkflowAlertsAreLabelledWithTheWorkflow(t *testing.T) {
	n := &Notifier{appBaseURL: "https://app.example"}
	p := notificationPayload{WorkflowID: alertWorkflow, ActionURL: "/workflows/" + alertWorkflow, Message: "m"}
	r := resolve(CodeWorkflowRunFailed, "structured_error_notification", resolveNotifierTopics().notify, "error",
		map[string]string{"workflow": "nightly_close"})
	if r.Title != "nightly_close failed" || r.Severity != severityCritical || r.WorkflowName != "nightly_close" {
		t.Errorf("rendered = %+v", r)
	}

	raw, _ := json.Marshal(n.slackPayload(p, r, false))
	if !strings.Contains(string(raw), `"title":"Workflow","value":"nightly_close"`) || strings.Contains(string(raw), `"Pipeline"`) {
		t.Errorf("slack payload = %s", raw)
	}
	body := n.alertEmail(p, r, "footer").Body
	if !strings.Contains(body, "Workflow: nightly_close") || strings.Contains(body, "Pipeline:") {
		t.Errorf("email body = %q", body)
	}
	// Without a name the id stands in, never a pipeline label.
	if label, name := alertSubject(p, Rendered{}); label != "Workflow" || name != alertWorkflow {
		t.Errorf("unnamed subject = %q %q", label, name)
	}
	// A workflow's window is its own: the same code and link on a pipeline or a
	// model must not swallow it.
	if workflowDedupScope(alertWorkflow) == modelDedupScope(alertWorkflow) || workflowDedupScope(alertWorkflow) == alertWorkflow {
		t.Errorf("workflow dedup scope %q collides", workflowDedupScope(alertWorkflow))
	}
}
