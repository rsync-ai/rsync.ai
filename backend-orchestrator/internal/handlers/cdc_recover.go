package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/backend-orchestrator/internal/cdc"
	log "github.com/sirupsen/logrus"
)

type CDCRecoverRequest struct {
	Operator string `json:"operator" binding:"required"`
	Reason   string `json:"reason" binding:"required"`

	// SnapshotMode controls Debezium snapshot.mode:
	// - "initial": full snapshot then stream
	// - "recovery": resume a partial snapshot
	// - "never": streaming only
	SnapshotMode string `json:"snapshot_mode"`

	// ResetOffsets attempts to reset Kafka Connect stored offsets for the connector.
	// This is required when forcing a re-snapshot after binlog/WAL position loss.
	ResetOffsets bool `json:"reset_offsets"`

	// DryRun returns validations + planned actions without side effects.
	DryRun bool `json:"dry_run"`
}

type connectorStatusPayload struct {
	Name      string `json:"name"`
	Connector struct {
		State string `json:"state"`
	} `json:"connector"`
	Tasks []struct {
		ID    int    `json:"id"`
		State string `json:"state"`
	} `json:"tasks"`
}

// RecoverCDCPipeline is an operator-initiated, guarded recovery endpoint.
//
// POST /api/v1/cdc/pipelines/:pipeline_id/recover
func RecoverCDCPipeline(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strings.ToLower(strings.TrimSpace(os.Getenv("CDC_RECOVERY_ENABLED"))) != "true" {
			c.JSON(http.StatusForbidden, gin.H{
				"success": false,
				"error":   "cdc_recovery_disabled",
				"message": "CDC recovery endpoint is disabled (set CDC_RECOVERY_ENABLED=true to enable).",
			})
			return
		}

		pipelineID := strings.TrimSpace(c.Param("pipeline_id"))
		if pipelineID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "pipeline_id is required"})
			return
		}

		// SECURITY (tenant isolation): gate cross-tenant CDC recovery/re-provision.
		if !assertPipelineOwnerForHandlers(c, db, pipelineID) {
			return
		}

		var req CDCRecoverRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		snapshotMode := normalizeSnapshotMode(req.SnapshotMode)
		if snapshotMode == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error":   "invalid_snapshot_mode",
				"message": "snapshot_mode must be one of: initial, recovery, never",
			})
			return
		}

		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), cdcRecoverBudget)
		defer cancel()

		connectorName, err := findConnectorName(ctx, db, pipelineID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
			return
		}

		connectURL := strings.TrimRight(getKafkaConnectURL(), "/")
		status, statusCode, statusErr := getConnectorStatus(ctx, connectURL, connectorName)
		if statusErr != nil {
			c.JSON(http.StatusBadGateway, gin.H{
				"success": false,
				"error":   statusErr.Error(),
				"result": gin.H{
					"connector_name": connectorName,
					"status_code":    statusCode,
				},
			})
			return
		}

		connState := strings.ToUpper(strings.TrimSpace(status.Connector.State))
		taskFailed := false
		for _, t := range status.Tasks {
			if strings.ToUpper(strings.TrimSpace(t.State)) == "FAILED" {
				taskFailed = true
				break
			}
		}
		if !req.DryRun && !(connState == "FAILED" || taskFailed) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error":   "invalid_state",
				"message": "Recovery is only allowed when connector or a task is in FAILED state (use restart for RUNNING/PAUSED).",
				"result": gin.H{
					"connector_state": connState,
					"tasks":           status.Tasks,
				},
			})
			return
		}

		connCfg, err := fetchKafkaConnectConfig(ctx, connectURL, connectorName)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": err.Error()})
			return
		}

		dbType := inferDebeziumDatabaseType(connCfg)
		sourceConnID, err := findPipelineSourceConnectionID(ctx, db, pipelineID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": err.Error()})
			return
		}

		tables := parseTableIncludeList(connCfg)
		if len(tables) == 0 {
			// Fallback: allow operator to proceed, but warn loudly.
			log.WithFields(log.Fields{
				"pipeline_id":    pipelineID,
				"connector_name": connectorName,
			}).Warn("CDC recover: table.include.list empty")
		}

		plan := gin.H{
			"connector_name":  connectorName,
			"connector_state": connState,
			"db_type":         dbType,
			"snapshot_mode":   snapshotMode,
			"reset_offsets":   req.ResetOffsets,
			"table_count":     len(tables),
		}

		// Postgres source: ensure publication/slot are provisioned and align config.
		if dbType == "postgresql" {
			dbName := strings.TrimSpace(fmt.Sprint(connCfg["database.dbname"]))
			rcfg := cdc.CDCResourceConfig{
				PipelineID:   pipelineID,
				ConnectionID: sourceConnID,
				DatabaseType: "postgresql",
				Database:     dbName,
			}
			pgMgr := cdc.NewPostgreSQLManager(db)
			if !req.DryRun {
				if _, perr := pgMgr.ProvisionResources(ctx, rcfg, tables); perr != nil {
					c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("failed to provision postgresql CDC resources: %v", perr)})
					return
				}
			}
			connCfg["publication.name"] = cdc.GenerateResourceName(rcfg, "publication")
			connCfg["slot.name"] = cdc.GenerateResourceName(rcfg, "replication_slot")
			connCfg["publication.autocreate.mode"] = "disabled"
			plan["publication_name"] = connCfg["publication.name"]
			plan["slot_name"] = connCfg["slot.name"]
		}

		// Update snapshot mode.
		connCfg["snapshot.mode"] = snapshotMode

		if req.DryRun {
			recoveryID := uuid.NewString()
			c.JSON(http.StatusOK, gin.H{
				"success":     true,
				"dry_run":     true,
				"recovery_id": recoveryID,
				"pipeline_id": pipelineID,
				"plan":        plan,
			})
			return
		}

		recoveryID := uuid.NewString()
		reason := strings.TrimSpace(req.Reason)
		log.WithFields(log.Fields{
			"recovery_id":    recoveryID,
			"pipeline_id":    pipelineID,
			"connector_name": connectorName,
			"operator":       strings.TrimSpace(req.Operator),
			"snapshot_mode":  snapshotMode,
			"reset_offsets":  req.ResetOffsets,
			"reason":         reason,
		}).Warn("CDC recovery requested")

		// Every Kafka Connect answer from here on is checked: a refused step answers
		// non-2xx and says where the connector was left, never "recovered".
		offsetReset := gin.H{"attempted": req.ResetOffsets, "success": false}
		fail := func(status int, code, message, connectorState string) {
			log.WithFields(log.Fields{
				"recovery_id": recoveryID, "pipeline_id": pipelineID, "connector_name": connectorName,
				"error": code, "connector_state": connectorState,
			}).Warn("CDC recovery failed: " + message)
			c.JSON(status, gin.H{
				"success":         false,
				"error":           code,
				"message":         message,
				"recovery_id":     recoveryID,
				"pipeline_id":     pipelineID,
				"connector":       connectorName,
				"connector_state": connectorState,
				"offsets":         offsetReset,
			})
		}
		// restore starts the connector again after a failure that changed nothing,
		// putting it back as the operator found it, and says where it was left.
		parked := "paused"
		restore := func() string {
			if code, err := putConnectNoBody(ctx, connectURL, "/connectors/"+connectorName+"/resume"); err != nil || code < 200 || code >= 300 {
				return parked
			}
			return "resumed"
		}

		// Park the connector first. An offset reset needs it STOPPED: Kafka Connect's
		// offsets API (KIP-875) refuses a paused connector. Without a reset a pause
		// is enough, and the restart below brings the FAILED tasks back.
		if req.ResetOffsets {
			outcome, status, why := stopConnector(ctx, connectURL, connectorName)
			if status == http.StatusOK && outcome == "absent" {
				status, why = http.StatusNotFound, fmt.Sprintf("connector %s no longer exists in Kafka Connect", connectorName)
			}
			if status != http.StatusOK {
				fail(status, "connector_park_failed", why, "unchanged")
				return
			}
			parked = outcome
		} else if status, why := connectStepFailure(putConnectNoBody(ctx, connectURL, "/connectors/"+connectorName+"/pause")); status != 0 {
			fail(status, "connector_park_failed", "pause "+why, "unchanged")
			return
		}

		if req.ResetOffsets {
			if err := deleteConnectorOffsets(ctx, connectURL, connectorName); err != nil {
				// The offsets are intact, so nothing has changed yet.
				fail(http.StatusBadGateway, "offset_reset_failed", err.Error(), restore())
				return
			}
			offsetReset["success"] = true
		}

		// Apply updated config (snapshot.mode + optional pg publication/slot alignment).
		if err := putKafkaConnectConfig(ctx, connectURL, connectorName, connCfg); err != nil {
			if req.ResetOffsets {
				// The offsets are already gone. Started now, the connector would run its
				// old snapshot.mode from no position — a snapshot nobody chose — so it
				// stays parked until the recovery is run again.
				fail(http.StatusBadGateway, "config_update_failed",
					err.Error()+"; its offsets are already cleared, so the connector was left "+parked+": run the recovery again", parked)
				return
			}
			fail(http.StatusBadGateway, "config_update_failed", err.Error(), restore())
			return
		}

		// A stopped connector has no tasks; resuming it starts fresh ones from the new
		// config. A paused one keeps its FAILED tasks until they are restarted.
		restartWhy := ""
		if parked == "paused" {
			if status, why := connectStepFailure(postConnectNoBody(ctx, connectURL, "/connectors/"+connectorName+"/restart?includeTasks=true&onlyFailed=false")); status != 0 {
				restartWhy = "restart " + why
			}
		}
		if status, why := connectStepFailure(putConnectNoBody(ctx, connectURL, "/connectors/"+connectorName+"/resume")); status != 0 {
			fail(status, "connector_resume_failed", "resume "+why+"; the connector was left "+parked, parked)
			return
		}
		if restartWhy != "" {
			fail(http.StatusBadGateway, "connector_restart_failed",
				restartWhy+"; the connector was resumed with the new config, but its failed tasks were not restarted", "resumed")
			return
		}

		message := fmt.Sprintf("CDC recovery applied: %s started again with snapshot.mode=%s.", connectorName, snapshotMode)
		if req.ResetOffsets {
			message = fmt.Sprintf("CDC recovery applied: %s offsets cleared and the connector started again with snapshot.mode=%s.", connectorName, snapshotMode)
		}
		c.JSON(http.StatusOK, gin.H{
			"success":         true,
			"recovery_id":     recoveryID,
			"pipeline_id":     pipelineID,
			"connector":       connectorName,
			"connector_state": "resumed",
			"plan":            plan,
			"offsets":         offsetReset,
			"message":         message,
		})
	}
}

// connectStepFailure reads a Kafka Connect call's answer: 0 when it succeeded,
// else the status to answer and why.
func connectStepFailure(code int, err error) (int, string) {
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Sprintf("failed: kafka connect unreachable: %v", err)
	}
	if code < 200 || code >= 300 {
		return http.StatusBadGateway, fmt.Sprintf("refused by kafka connect (HTTP %d)", code)
	}
	return 0, ""
}

func normalizeSnapshotMode(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return "initial"
	}
	switch s {
	case "initial", "recovery":
		return s
	case "never", "none", "streaming_only", "streaming-only":
		return "never"
	default:
		return ""
	}
}

func parseTableIncludeList(connCfg map[string]interface{}) []string {
	return connectorIncludeList(connCfg)
}

func getConnectorStatus(ctx context.Context, connectURL, connectorName string) (*connectorStatusPayload, int, error) {
	u := fmt.Sprintf("%s/connectors/%s/status", strings.TrimRight(connectURL, "/"), connectorName)
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("kafka connect status error (status %d): %s", resp.StatusCode, string(body))
	}
	var payload connectorStatusPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, resp.StatusCode, err
	}
	return &payload, resp.StatusCode, nil
}

func deleteConnectorOffsets(ctx context.Context, connectURL, connectorName string) error {
	// Kafka Connect supports DELETE /connectors/{name}/offsets on modern versions.
	u := fmt.Sprintf("%s/connectors/%s/offsets", strings.TrimRight(connectURL, "/"), connectorName)
	httpClient := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("offset reset not supported or failed (status %d): %s", resp.StatusCode, string(body))
}

func putConnectNoBody(ctx context.Context, connectURL, path string) (int, error) {
	u := strings.TrimRight(connectURL, "/") + path
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func postConnectNoBody(ctx context.Context, connectURL, path string) (int, error) {
	u := strings.TrimRight(connectURL, "/") + path
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// cdcRecoverBudget bounds one recovery, every Kafka Connect step included. It runs
// detached from the request (context.WithoutCancel), so a caller that goes away — a closed
// tab, a gateway that stops waiting — cannot cancel the recovery between two steps and
// leave the connector stopped. The gateway's cdcRecoverProxyTimeout still outlasts it, so
// the caller hears how the recovery ended.
const cdcRecoverBudget = 90 * time.Second
