package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
)

// A model key saved in Admin > Settings > AI models (assistant_model_keys,
// migration 125) is rotated with every other stored secret: after
// ENCRYPTION_KEYS gains a new primary, the rotation re-encrypts it under that
// key, and the old key can then be dropped.
func TestRotateEncryptionKeysRotatesAssistantModelKeys(t *testing.T) {
	const (
		oldKey = "unit-test-encryption-key-old-0123456789"
		newKey = "unit-test-encryption-key-new-0123456789"
		apiKey = "unit-test-provider-key-canary"
	)
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", oldKey)
	oldCipher, err := crypto.EncryptString(apiKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENCRYPTION_KEYS", newKey+","+oldKey)

	mock := withMockGlobalDB(t)
	mock.MatchExpectationsInOrder(false)
	var rotated string
	mock.ExpectQuery(`SELECT entry, key_encrypted FROM assistant_model_keys`).
		WillReturnRows(sqlmock.NewRows([]string{"entry", "key_encrypted"}).AddRow("claude", oldCipher))
	mock.ExpectExec(`UPDATE assistant_model_keys SET key_encrypted = \$1 WHERE entry = \$2`).
		WithArgs(captureJSONArg(&rotated), "claude").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// The other tables hold nothing here.
	for _, table := range []string{"connections", "oauth_tokens", "oauth_tokens", "oauth_apps", "notification_channel_settings", "notification_channel_settings", "delivery_channels", "openlineage_targets"} {
		mock.ExpectQuery(`FROM ` + table + ` WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "secret"}))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/rotate", AdminRotateEncryptionKeys)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/rotate", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Stats map[string]rotateEncryptionStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if st := body.Stats["assistant_model_keys.key_encrypted"]; st.Scanned != 1 || st.Rotated != 1 || st.Failed != 0 {
		t.Fatalf("assistant_model_keys stats = %+v", st)
	}
	if rotated == "" || rotated == oldCipher {
		t.Fatalf("the key was not re-encrypted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Under the new key alone, the rotated value still reads; the old one no longer does.
	t.Setenv("ENCRYPTION_KEYS", newKey)
	if got, err := crypto.DecryptString(rotated); err != nil || got != apiKey {
		t.Fatalf("rotated value under the new key: %v", err)
	}
	if _, err := crypto.DecryptString(oldCipher); err == nil {
		t.Fatalf("control: the old value should not read under the new key alone")
	}
}

// A workflow's Slack channel (delivery_channels, migration 130) holds an
// incoming-webhook URL, which is the credential; it is rotated with every
// other stored secret.
func TestRotateEncryptionKeysRotatesDeliveryChannelWebhooks(t *testing.T) {
	const (
		oldKey  = "unit-test-encryption-key-old-0123456789"
		newKey  = "unit-test-encryption-key-new-0123456789"
		webhook = "https://hooks.slack.com/services/T000/B000/unit-test-canary"
		chID    = "11111111-0000-4000-8000-000000000001"
	)
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", oldKey)
	oldCipher, err := crypto.EncryptString(webhook)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENCRYPTION_KEYS", newKey+","+oldKey)

	mock := withMockGlobalDB(t)
	mock.MatchExpectationsInOrder(false)
	var rotated string
	mock.ExpectQuery(`SELECT id::text, slack_webhook_encrypted FROM delivery_channels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "secret"}).AddRow(chID, oldCipher))
	mock.ExpectExec(`UPDATE delivery_channels SET slack_webhook_encrypted = \$1`).
		WithArgs(captureJSONArg(&rotated), chID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for _, table := range []string{"connections", "oauth_tokens", "oauth_tokens", "oauth_apps",
		"notification_channel_settings", "notification_channel_settings", "assistant_model_keys", "openlineage_targets"} {
		mock.ExpectQuery(`FROM ` + table + ` WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "secret"}))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/rotate", AdminRotateEncryptionKeys)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/rotate", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Stats map[string]rotateEncryptionStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if st := body.Stats["delivery_channels.slack_webhook_encrypted"]; st.Scanned != 1 || st.Rotated != 1 || st.Failed != 0 {
		t.Fatalf("delivery_channels stats = %+v", st)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENCRYPTION_KEYS", newKey)
	if got, err := crypto.DecryptString(rotated); err != nil || got != webhook {
		t.Fatalf("rotated webhook under the new key: %v", err)
	}
}

// A workspace's OpenLineage push target (openlineage_targets, migration 153) holds an
// auth header, which is the credential; it is rotated with every other stored secret,
// so the push still reads it after the old key is dropped.
func TestRotateEncryptionKeysRotatesOpenLineageAuthHeaders(t *testing.T) {
	const (
		oldKey = "unit-test-encryption-key-old-0123456789"
		newKey = "unit-test-encryption-key-new-0123456789"
		header = "Bearer unit-test-openlineage-canary"
		wsID   = "11111111-0000-4000-8000-000000000002"
	)
	t.Setenv("ENCRYPTION_KEY", "")
	t.Setenv("ENCRYPTION_KEYS", oldKey)
	oldCipher, err := crypto.EncryptString(header)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENCRYPTION_KEYS", newKey+","+oldKey)

	mock := withMockGlobalDB(t)
	mock.MatchExpectationsInOrder(false)
	var rotated string
	mock.ExpectQuery(`SELECT workspace_id::text, auth_header_enc FROM openlineage_targets`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "secret"}).AddRow(wsID, oldCipher))
	mock.ExpectExec(`UPDATE openlineage_targets SET auth_header_enc = \$1 WHERE workspace_id = \$2::uuid`).
		WithArgs(captureJSONArg(&rotated), wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	for _, table := range []string{"connections", "oauth_tokens", "oauth_tokens", "oauth_apps",
		"notification_channel_settings", "notification_channel_settings", "assistant_model_keys", "delivery_channels"} {
		mock.ExpectQuery(`FROM ` + table + ` WHERE`).WillReturnRows(sqlmock.NewRows([]string{"id", "secret"}))
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/rotate", AdminRotateEncryptionKeys)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/rotate", strings.NewReader(`{}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Stats map[string]rotateEncryptionStats `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if st := body.Stats["openlineage_targets.auth_header_enc"]; st.Scanned != 1 || st.Rotated != 1 || st.Failed != 0 {
		t.Fatalf("openlineage_targets stats = %+v", st)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENCRYPTION_KEYS", newKey)
	if got, err := crypto.DecryptString(rotated); err != nil || got != header {
		t.Fatalf("rotated auth header under the new key: %v", err)
	}
	if _, err := crypto.DecryptString(oldCipher); err == nil {
		t.Fatalf("control: the old value should not read under the new key alone")
	}
}
