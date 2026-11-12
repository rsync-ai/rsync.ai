package notifier

import (
	"context"
	"database/sql"
	"errors"

	"api-gateway/internal/safehttp"
)

// Workflows (handlers/workflow_runtime.go) send through the same transports as
// alerts: an SSRF-guarded post to a Slack incoming webhook, and the admin's
// SMTP server. The workflow names the webhook (a delivery channel, stored per
// workspace); the text is already escaped and capped by package workflow.

// ErrSMTPNotConfigured is returned when the admin has not set up SMTP email.
var ErrSMTPNotConfigured = errors.New("SMTP email is not set up on this rsync.ai")

// PostSlackText posts one message to a Slack incoming webhook. An error never
// carries the webhook URL, which is the credential.
func PostSlackText(ctx context.Context, webhookURL, text string) error {
	return sendSlackWebhook(ctx, safehttp.NewClient(slackTimeout),
		SlackChannel{Enabled: true, WebhookURL: webhookURL}, map[string]interface{}{"text": text})
}

// SMTPConfigured reports whether the admin turned SMTP email on.
func SMTPConfigured(ctx context.Context, db *sql.DB) bool {
	cfg, err := CachedChannelConfig(ctx, db)
	return err == nil && cfg.Email.Enabled
}

// SendSMTPText sends one plain-text email through the admin's SMTP settings,
// with any files attached after the text.
func SendSMTPText(ctx context.Context, db *sql.DB, to, subject, body string, atts ...Attachment) error {
	cfg, err := CachedChannelConfig(ctx, db)
	if err != nil {
		return err
	}
	if !cfg.Email.Enabled {
		return ErrSMTPNotConfigured
	}
	return sendSMTP(ctx, cfg.Email, to, emailMessage{Subject: subject, Body: body, Attachments: atts})
}
