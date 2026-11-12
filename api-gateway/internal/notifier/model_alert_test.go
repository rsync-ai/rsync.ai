package notifier

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// A data check's alert is about a model, not a pipeline (data-checks-design.md § 7):
// it goes to the model's owner, is labelled with the model's name, and is stored with
// saved_query_id so the bell can scope it to the model's workspace.

const (
	alertModel    = "5d1c4a52-0f6e-4f3b-9b7e-6a7d2c1e9f01"
	alertModelRun = "8e0f1b2c-3d4e-4f50-8a6b-7c8d9e0f1a2b"
)

func modelCheckEvent(t *testing.T, code, severity string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]interface{}{
		"type":           "data_check",
		"saved_query_id": alertModel,
		"message":        `Check "has rows" (row_count) failed: saw 0, expected at least 1.`,
		"action_url":     "/models/" + alertModel + "?tab=checks&run=" + alertModelRun,
		"error": map[string]interface{}{
			"failure_type":  "DATA_CHECK",
			"code":          code,
			"severity":      severity,
			"audience":      "user",
			"user_message":  `Check "has rows" (row_count) failed: saw 0, expected at least 1.`,
			"dedup_subject": alertModelRun,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDataCheckAlertCopy(t *testing.T) {
	notify := resolveNotifierTopics().notify
	blocked := resolve(CodeCheckBlocked, "data_check", notify, "error", map[string]string{"model": "orders_daily"})
	if blocked.Title != "A data check held back orders_daily" || blocked.Severity != severityCritical ||
		blocked.ActionLabel != "View run" || blocked.ModelName != "orders_daily" {
		t.Errorf("blocked = %+v", blocked)
	}
	if !strings.Contains(blocked.Impact, "stays live") {
		t.Errorf("a blocked alert must say the old table stays live: %q", blocked.Impact)
	}
	warned := resolve(CodeCheckWarned, "data_check", notify, "warning", map[string]string{"model": "orders_daily"})
	if warned.Title != "A data check warned on orders_daily" || warned.Severity != severityWarning {
		t.Errorf("warned = %+v", warned)
	}
	if got := resolve(CodeCheckBlocked, "data_check", notify, "error", nil).Title; got != "A data check held back your model" {
		t.Errorf("unnamed model title = %q", got)
	}
	for _, code := range []string{CodeCheckBlocked, CodeCheckWarned} {
		if got := CategoryFor(code, "data_check"); got != CategoryDataQuality {
			t.Errorf("CategoryFor(%s) = %q, want %q so its own mute applies", code, got, CategoryDataQuality)
		}
	}
	if !IsCategory(CategoryDataQuality) {
		t.Errorf("%q is not a listed category, so no settings page can mute it", CategoryDataQuality)
	}
}

func TestEmitterPublishPersistsAModelCheckAlert(t *testing.T) {
	clearNotifierEnv(t)
	InvalidateChannelCache()
	t.Cleanup(InvalidateChannelCache)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()

	actionURL := "/models/" + alertModel + "?tab=checks&run=" + alertModelRun
	dedupKey := makeDedupKey(modelDedupScope(alertModel), CodeCheckBlocked, actionURL, alertModelRun)
	var meta map[string]interface{}

	mock.ExpectQuery(`SELECT created_by::text, COALESCE\(name, ''\) FROM saved_queries WHERE id = \$1`).
		WithArgs(alertModel).
		WillReturnRows(sqlmock.NewRows([]string{"created_by", "name"}).AddRow(emitterOwner, "orders_daily"))
	mock.ExpectQuery(`SELECT id::text FROM pipeline_notifications\s+WHERE dedup_key = \$1`).
		WithArgs(dedupKey).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO pipeline_notifications\s+\(id, pipeline_id, user_id, type, severity, title, message, action_url, metadata, delivery_status, dedup_key, created_at, saved_query_id\)`).
		WithArgs(sqlmock.AnyArg(), nil, emitterOwner, "data_check", severityCritical,
			"A data check held back orders_daily",
			`Check "has rows" (row_count) failed: saw 0, expected at least 1.`,
			actionURL, jsonCapture{&meta}, dedupKey, alertModel).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`FROM notification_channel_settings`).WillReturnRows(sqlmock.NewRows(channelColumns))
	mock.ExpectExec(`UPDATE pipeline_notifications`).
		WithArgs(StatusSuppressed, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := NewEmitter(mockDB).Publish(context.Background(), modelCheckEvent(t, CodeCheckBlocked, "error")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
	if meta["category"] != CategoryDataQuality || meta["error_code"] != CodeCheckBlocked ||
		meta["model_name"] != "orders_daily" || meta["saved_query_id"] != alertModel || meta["action_label"] != "View run" {
		t.Errorf("metadata = %v", meta)
	}
	if _, ok := meta["pipeline_name"]; ok {
		t.Errorf("a model alert carries a pipeline name: %v", meta)
	}
}

// The bell finds a model alert's workspace by comparing the stored id as text with
// saved_queries.id::text, so an id in any other spelling would hide the alert.
func TestModelAlertStoresTheCanonicalModelID(t *testing.T) {
	clearNotifierEnv(t)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	raw, _ := json.Marshal(map[string]interface{}{
		"type": "data_check", "saved_query_id": "{" + strings.ToUpper(alertModel) + "}", "message": "m",
	})
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1`).WithArgs(alertModel).WillReturnError(sql.ErrNoRows)
	if err := NewEmitter(mockDB).Publish(context.Background(), raw); !isPermanent(err) {
		t.Fatalf("err = %v, want the lookup by the canonical id to end permanently", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the model was not looked up by its canonical id: %v", err)
	}
	notUUID, _ := json.Marshal(map[string]interface{}{"type": "data_check", "saved_query_id": "orders", "message": "m"})
	if err := NewEmitter(mockDB).Publish(context.Background(), notUUID); !isPermanent(err) {
		t.Fatalf("err = %v, want a non-uuid model id to be permanent", err)
	}
}

// A model that is gone has no owner to route to; redelivering cannot change that.
func TestModelAlertForADeletedModelIsPermanent(t *testing.T) {
	clearNotifierEnv(t)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer mockDB.Close()
	mock.ExpectQuery(`FROM saved_queries WHERE id = \$1`).WithArgs(alertModel).WillReturnError(sql.ErrNoRows)

	err = NewEmitter(mockDB).Publish(context.Background(), modelCheckEvent(t, CodeCheckWarned, "warning"))
	if !isPermanent(err) {
		t.Fatalf("err = %v, want a permanent error", err)
	}
}

func TestModelAlertsAreLabelledWithTheModel(t *testing.T) {
	n := &Notifier{appBaseURL: "https://app.example"}
	p := notificationPayload{SavedQueryID: alertModel, ActionURL: "/models/" + alertModel, Message: "m"}
	r := Rendered{Title: "A data check held back orders_daily", ActionLabel: "View run", Severity: severityCritical, ModelName: "orders_daily"}

	raw, _ := json.Marshal(n.slackPayload(p, r, false))
	if !strings.Contains(string(raw), `"title":"Model","value":"orders_daily"`) || strings.Contains(string(raw), `"Pipeline"`) {
		t.Errorf("slack payload = %s", raw)
	}
	body := n.alertEmail(p, r, "footer").Body
	if !strings.Contains(body, "Model: orders_daily") || strings.Contains(body, "Pipeline:") {
		t.Errorf("email body = %q", body)
	}
}
