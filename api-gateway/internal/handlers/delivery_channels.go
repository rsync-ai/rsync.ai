package handlers

// Delivery channels: the named places a workflow sends to (migration 130). A
// Slack channel is one incoming webhook, stored encrypted because the URL is
// the credential; an email channel is a list of up to 20 addresses. Only a
// workspace owner or admin adds, changes or deletes one; anyone in the
// workspace sees their names, so Ask rsync.ai can pick one by name. A webhook
// URL is never returned by any route.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	deliveryChannelNameMax = 100
	deliveryEmailMax       = 20
	deliveryKindSlack      = "slack"
	deliveryKindEmail      = "email"
)

// DeliveryChannel is a channel as the API shows it. Recipients is filled for
// owners and admins only; a webhook URL is never shown.
type DeliveryChannel struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	RecipientCount int       `json:"recipient_count"`
	Recipients     []string  `json:"recipients,omitempty"`
	UsedBy         []string  `json:"used_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type deliveryChannelRequest struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	WebhookURL string   `json:"webhook_url"`
	Recipients []string `json:"recipients"`
}

// normalizeRecipients parses, lower-cases and de-duplicates email addresses.
func normalizeRecipients(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addr, err := mail.ParseAddress(raw)
		if err != nil || !strings.Contains(addr.Address, ".") {
			return nil, fmt.Errorf("%q is not an email address", raw)
		}
		a := strings.ToLower(addr.Address)
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("an email channel needs at least one address")
	}
	if len(out) > deliveryEmailMax {
		return nil, fmt.Errorf("an email channel can have at most %d addresses; this one has %d", deliveryEmailMax, len(out))
	}
	return out, nil
}

func validDeliveryChannelName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("a channel needs a name, such as #finance or Finance team")
	}
	if len([]rune(name)) > deliveryChannelNameMax {
		return "", fmt.Errorf("a channel name may be at most %d characters", deliveryChannelNameMax)
	}
	return name, nil
}

// ListDeliveryChannels lists the workspace's delivery channels.
// GET /api/v1/delivery-channels
func ListDeliveryChannels(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	role, ok := requireWorkspaceRole(c, security.WSViewer)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	channels, err := loadDeliveryChannels(c.Request.Context(), database, ws, role.Meets(security.WSAdmin))
	if err != nil {
		log.WithError(err).Error("[delivery_channels] list failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list delivery channels"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"channels": channels, "can_manage": role.Meets(security.WSAdmin)})
}

func loadDeliveryChannels(ctx context.Context, database *sql.DB, ws string, withRecipients bool) ([]DeliveryChannel, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT dc.id, dc.name, dc.kind, array_to_string(dc.email_recipients, E'\x1f'), dc.created_at, dc.updated_at,
		       COALESCE(string_agg(w.name, E'\x1f' ORDER BY w.name), '')
		FROM delivery_channels dc
		LEFT JOIN (SELECT DISTINCT workflow_id, channel_id FROM workflow_channels) wc ON wc.channel_id = dc.id
		LEFT JOIN workflows w ON w.id = wc.workflow_id
		WHERE dc.workspace_id = $1
		GROUP BY dc.id
		ORDER BY lower(dc.name)
	`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeliveryChannel{}
	for rows.Next() {
		var ch DeliveryChannel
		var recipientsJoined, usedByJoined string
		if err := rows.Scan(&ch.ID, &ch.Name, &ch.Kind, &recipientsJoined, &ch.CreatedAt, &ch.UpdatedAt,
			&usedByJoined); err != nil {
			return nil, err
		}
		recipients, usedBy := splitUnit(recipientsJoined), splitUnit(usedByJoined)
		ch.RecipientCount = len(recipients)
		if withRecipients {
			ch.Recipients = recipients
		}
		ch.UsedBy = usedBy
		out = append(out, ch)
	}
	return out, rows.Err()
}

// CreateDeliveryChannel adds a Slack or email channel. Owner or admin.
// POST /api/v1/delivery-channels
func CreateDeliveryChannel(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	var req deliveryChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name, err := validDeliveryChannelName(req.Name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var webhook *string
	recipients := []string{}
	switch kind := strings.ToLower(strings.TrimSpace(req.Kind)); kind {
	case deliveryKindSlack:
		url := strings.TrimSpace(req.WebhookURL)
		if err := validateSlackWebhookURL(url); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		enc, err := crypto.EncryptString(url)
		if err != nil {
			log.WithError(err).Error("[delivery_channels] encrypt webhook failed")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store the webhook"})
			return
		}
		webhook = &enc
		req.Kind = kind
	case deliveryKindEmail:
		if recipients, err = normalizeRecipients(req.Recipients); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Kind = kind
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be slack or email"})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	var id string
	err = database.QueryRowContext(c.Request.Context(), `
		INSERT INTO delivery_channels (workspace_id, name, kind, slack_webhook_encrypted, email_recipients, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, ws, name, req.Kind, webhook, pgdriver.StringArray(recipients), userID).Scan(&id)
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("This workspace already has a channel named %q.", name)})
		return
	}
	if err != nil {
		log.WithError(err).Error("[delivery_channels] insert failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add the channel"})
		return
	}
	logAudit(c, "create_delivery_channel", "delivery_channel", id, map[string]interface{}{
		"name": name, "kind": req.Kind, "recipients": len(recipients),
	})
	c.JSON(http.StatusCreated, gin.H{"id": id})
}

// UpdateDeliveryChannel renames a channel, replaces its webhook (an empty
// webhook_url keeps the stored one) or its addresses. Owner or admin.
// PUT /api/v1/delivery-channels/:id
func UpdateDeliveryChannel(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	var req deliveryChannelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ctx := c.Request.Context()
	var kind string
	if err := database.QueryRowContext(ctx,
		`SELECT kind FROM delivery_channels WHERE id = $1 AND workspace_id = $2`, id, ws).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
			return
		}
		log.WithError(err).Error("[delivery_channels] load failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the channel"})
		return
	}
	name, err := validDeliveryChannelName(req.Name)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var res sql.Result
	switch kind {
	case deliveryKindSlack:
		if url := strings.TrimSpace(req.WebhookURL); url != "" {
			if err := validateSlackWebhookURL(url); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			enc, err := crypto.EncryptString(url)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store the webhook"})
				return
			}
			res, err = database.ExecContext(ctx, `UPDATE delivery_channels SET name = $3, slack_webhook_encrypted = $4,
				updated_at = NOW() WHERE id = $1 AND workspace_id = $2`, id, ws, name, enc)
		} else {
			res, err = database.ExecContext(ctx, `UPDATE delivery_channels SET name = $3, updated_at = NOW()
				WHERE id = $1 AND workspace_id = $2`, id, ws, name)
		}
	default:
		recipients, rerr := normalizeRecipients(req.Recipients)
		if rerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": rerr.Error()})
			return
		}
		res, err = database.ExecContext(ctx, `UPDATE delivery_channels SET name = $3, email_recipients = $4,
			updated_at = NOW() WHERE id = $1 AND workspace_id = $2`, id, ws, name, pgdriver.StringArray(recipients))
	}
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("This workspace already has a channel named %q.", name)})
		return
	}
	if err != nil {
		log.WithError(err).Error("[delivery_channels] update failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update the channel"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	logAudit(c, "update_delivery_channel", "delivery_channel", id, map[string]interface{}{"name": name})
	c.JSON(http.StatusOK, gin.H{"id": id})
}

// DeleteDeliveryChannel deletes a channel no workflow uses. Owner or admin.
// DELETE /api/v1/delivery-channels/:id
func DeleteDeliveryChannel(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ctx := c.Request.Context()
	var usersJoined string
	if err := database.QueryRowContext(ctx, `
		SELECT COALESCE(string_agg(w.name, E'\x1f' ORDER BY w.name), '')
		FROM (SELECT DISTINCT workflow_id, channel_id FROM workflow_channels) wc
		JOIN workflows w ON w.id = wc.workflow_id
		WHERE wc.channel_id = $1 AND w.workspace_id = $2
	`, id, ws).Scan(&usersJoined); err != nil {
		log.WithError(err).Error("[delivery_channels] usage check failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the channel"})
		return
	}
	if users := splitUnit(usersJoined); len(users) > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
			"This channel is used by %s. Delete those workflows first, or change where they send.", backtickList(users))})
		return
	}
	res, err := database.ExecContext(ctx, `DELETE FROM delivery_channels WHERE id = $1 AND workspace_id = $2`, id, ws)
	if isForeignKeyViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "A workflow started using this channel. Reload and try again."})
		return
	}
	if err != nil {
		log.WithError(err).Error("[delivery_channels] delete failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the channel"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	logAudit(c, "delete_delivery_channel", "delivery_channel", id, nil)
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// TestDeliveryChannel sends one test message through a channel. Owner or admin.
// POST /api/v1/delivery-channels/:id/test
func TestDeliveryChannel(c *gin.Context) {
	ws, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not connected"})
		return
	}
	ch, err := loadChannelForSend(c.Request.Context(), database, ws, c.Param("id"))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("[delivery_channels] load for test failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the channel"})
		return
	}
	msg := renderedMessage{
		Subject: "[rsync.ai] Test message",
		Slack:   "*rsync.ai test message*\nWorkflows that send to " + ch.Name + " will post here.",
		Text:    "Workflows that send to " + ch.Name + " will email this address.\n\n--\nAutomated message from rsync.ai",
		HTML: "<p>Workflows that send to <b>" + htmlEscape(ch.Name) + "</b> will email this address.</p>" +
			"<p style=\"color:#71717a\">Automated message from rsync.ai</p>",
	}
	outcome := deliverToChannel(c.Request.Context(), database, ch, msg)
	logAudit(c, "test_delivery_channel", "delivery_channel", ch.ID, map[string]interface{}{
		"kind": ch.Kind, "status": outcome.Status,
	})
	if outcome.Status != deliveryStatusSent {
		c.JSON(http.StatusFailedDependency, gin.H{"error": outcome.Error, "delivery": outcome})
		return
	}
	c.JSON(http.StatusOK, gin.H{"delivery": outcome})
}

// sendChannel is a channel with its secret decrypted, for sending only.
type sendChannel struct {
	ID         string
	Name       string
	Kind       string
	WebhookURL string
	SecretErr  error
	Recipients []string
}

func loadChannelForSend(ctx context.Context, database *sql.DB, ws, id string) (*sendChannel, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, sql.ErrNoRows
	}
	var (
		ch       sendChannel
		enc      sql.NullString
		emailsTo string
	)
	if err := database.QueryRowContext(ctx, `
		SELECT id, name, kind, slack_webhook_encrypted, array_to_string(email_recipients, E'\x1f')
		FROM delivery_channels WHERE id = $1 AND workspace_id = $2
	`, id, ws).Scan(&ch.ID, &ch.Name, &ch.Kind, &enc, &emailsTo); err != nil {
		return nil, err
	}
	ch.Recipients = splitUnit(emailsTo)
	if ch.Kind == deliveryKindSlack {
		url, err := crypto.DecryptString(enc.String)
		if err != nil {
			ch.SecretErr = errors.New("the stored Slack webhook could not be decrypted; an admin should enter it again")
		}
		ch.WebhookURL = url
	}
	return &ch, nil
}

// splitUnit splits a list joined with the ASCII unit separator, which cannot
// occur in an email address or a name typed into a form. This module runs on
// pgx, which has no pq.Array, so arrays come back joined.
func splitUnit(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, "\x1f") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// isForeignKeyViolation reports a Postgres foreign_key_violation (23503).
func isForeignKeyViolation(err error) bool {
	return pgdriver.SQLState(err) == "23503"
}
