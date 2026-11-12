package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"api-gateway/internal/config"
	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/backend-orchestrator/pkg/llmscrub"
	log "github.com/sirupsen/logrus"

	"github.com/rsync-ai/shared/sqlexec/mask"
)

// Ask rsync.ai's sample rows (docs/security/llm-egress.md, "Sample rows"). A model
// sees a value only while two switches are on, both off by default:
//
//   - a platform admin approved the model entry (Admin > Settings > AI models):
//     the instance_settings row llmSampleRowsSetting;
//   - the workspace owner set workspaces.llm_data_mode to sample_rows
//     (Workspace settings > Data).
//
// The assistant package reads both before it offers its sample_rows tool, and
// llm-service's egress guard reads them again before each send. The rsync.ai MCP
// server (agentgw) has no such tool: an agent token never gets a row value.
//
// Above both sits the install: while config.LLMSampleRowsAllowed() is false (the
// self-host compose and chart), no model counts as approved and neither switch
// can be turned on.

const (
	llmSampleRowsSetting = "llm_sample_rows_models"
	// LLMDataNamesOnly is the default: names, types and ids, never a value.
	LLMDataNamesOnly = "names_only"
	// LLMDataSampleRows lets Ask rsync.ai read a few sample rows with an approved model.
	LLMDataSampleRows = "sample_rows"

	assistantSampleRows       = 5
	assistantSampleValueRunes = 64
	assistantSampleTimeout    = 5 * time.Second
)

// ErrSampleRowsDisabled is SetApproved's answer to an approval on an install
// that does not allow sample rows; the admin route turns it into a 403.
var ErrSampleRowsDisabled = errors.New("sample rows are turned off on this install")

const sampleRowsDisabledMessage = "Sample rows are turned off on this install: Ask rsync.ai sends a model names only."

// sampleRowsEntries are the assistant entries an admin can approve. A
// self-hosted model is not one: there is no self-hosted assistant entry.
var sampleRowsEntries = []string{"claude", "chatgpt", "gemini"}

// AssistantSamples is the assistant package's Samples: the two switches and
// the sampler. Every read error counts as off.
type AssistantSamples struct{}

// Approved returns the entries an admin approved for sample rows: none on an
// install that does not allow them, whatever an admin approved before.
func (AssistantSamples) Approved(ctx context.Context) (map[string]bool, error) {
	if !config.LLMSampleRowsAllowed() {
		return map[string]bool{}, nil
	}
	database := db.GetDB()
	if database == nil {
		return nil, errors.New("database not available")
	}
	var raw string
	err := database.QueryRowContext(ctx, `SELECT value FROM instance_settings WHERE key = $1`, llmSampleRowsSetting).Scan(&raw)
	if err == sql.ErrNoRows {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSampleRowsEntries(raw), nil
}

func parseSampleRowsEntries(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		for _, e := range sampleRowsEntries {
			if part == e {
				out[e] = true
			}
		}
	}
	return out
}

func joinSampleRowsEntries(set map[string]bool) string {
	var parts []string
	for _, e := range sampleRowsEntries {
		if set[e] {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, ",")
}

// SetApproved approves or withdraws one entry, as admin by, and returns the
// approved set. The row is locked, so two admins changing different entries
// at once both land.
func (AssistantSamples) SetApproved(ctx context.Context, entry string, allowed bool, by string) (map[string]bool, error) {
	known := false
	for _, e := range sampleRowsEntries {
		known = known || e == entry
	}
	if !known {
		return nil, fmt.Errorf("unknown model entry %q", entry)
	}
	// Withdrawing stays possible, so an admin can still clear an old approval.
	if allowed && !config.LLMSampleRowsAllowed() {
		return nil, ErrSampleRowsDisabled
	}
	database := db.GetDB()
	if database == nil {
		return nil, errors.New("database not available")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	updatedBy := sql.NullString{String: by, Valid: by != ""}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO instance_settings (key, value, updated_by, updated_at)
		VALUES ($1, '', $2, NOW())
		ON CONFLICT (key) DO NOTHING
	`, llmSampleRowsSetting, updatedBy); err != nil {
		return nil, err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM instance_settings WHERE key = $1 FOR UPDATE`, llmSampleRowsSetting).Scan(&raw); err != nil {
		return nil, err
	}
	set := parseSampleRowsEntries(raw)
	if allowed {
		set[entry] = true
	} else {
		delete(set, entry)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE instance_settings SET value = $2, updated_by = $3, updated_at = NOW() WHERE key = $1
	`, llmSampleRowsSetting, joinSampleRowsEntries(set), updatedBy); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return set, nil
}

// Mode returns the workspace's llm_data_mode.
func (AssistantSamples) Mode(ctx context.Context, workspaceID string) (string, error) {
	return workspaceLLMDataMode(ctx, workspaceID)
}

func workspaceLLMDataMode(ctx context.Context, workspaceID string) (string, error) {
	if workspaceID == "" {
		return LLMDataNamesOnly, errors.New("no workspace")
	}
	database := db.GetDB()
	if database == nil {
		return LLMDataNamesOnly, errors.New("database not available")
	}
	var mode string
	err := database.QueryRowContext(ctx, `SELECT llm_data_mode FROM workspaces WHERE id = $1`, workspaceID).Scan(&mode)
	if err != nil {
		return LLMDataNamesOnly, err
	}
	return mode, nil
}

// SampleReadError is a sample read that failed, with a message a model may read.
type SampleReadError struct{ Msg string }

func (e *SampleReadError) Error() string { return e.Msg }

// Read returns at most five sample rows of table in one of the active
// workspace's connections, keeping only what a model may read. The connection
// loads as the table preview loads it (loadSampleConnection), scoped to the
// workspace, and the read is logged as the preview's is. Then:
//
//   - at most five rows; a string longer than 64 characters is cut, and an
//     object or array becomes its type name;
//   - a column whose name looks secret or personal is withheld
//     (assistantWithholdsColumnName);
//   - a column a PII scan flagged on this table is withheld (pii_scan_results);
//   - when columns is set, only those columns.
//
// llm-service's egress guard reads the values again before any is sent.
// withheld names the columns left out, never their values.
func (AssistantSamples) Read(c *gin.Context, connectionID, table string, columns []string) ([]string, []map[string]interface{}, []string, error) {
	database := db.GetDB()
	if database == nil {
		return nil, nil, nil, &SampleReadError{"the database is not available"}
	}
	wsID := activeWorkspaceID(c)
	if wsID == "" {
		return nil, nil, nil, &SampleReadError{"connection not found"}
	}
	connectorType, config, err := loadSampleConnection(database, connectionID, wsID)
	switch {
	case errors.Is(err, errSampleConnNotFound):
		return nil, nil, nil, &SampleReadError{"connection not found in this workspace"}
	case err != nil:
		log.WithError(err).WithField("connection_id", connectionID).Warn("assistant sample_rows: connection load failed")
		return nil, nil, nil, &SampleReadError{"could not load the connection"}
	}

	flagged, err := scanFlaggedColumns(c.Request.Context(), database, connectionID, table)
	if err != nil {
		// The scan's verdicts cannot be read, so nothing is sampled.
		log.WithError(err).WithField("connection_id", connectionID).Warn("assistant sample_rows: PII scan results unreadable")
		return nil, nil, nil, &SampleReadError{"could not check this table's PII scan, so no rows were read"}
	}

	logConnectionAccess(c, connectionID, "sample_data", true, "")
	ctx, cancel := context.WithTimeout(c.Request.Context(), assistantSampleTimeout)
	defer cancel()
	result, err := sampleDataFromConnection(ctx, connectorType, config, table, assistantSampleRows)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, nil, nil, &SampleReadError{"reading sample rows took longer than 5 seconds"}
		}
		return nil, nil, nil, &SampleReadError{llmscrub.Scrub(cleanSampleError(err))}
	}
	cols, rows, withheld := trimAssistantSample(result, columns, flagged)
	return cols, rows, withheld, nil
}

// scanFlaggedColumns returns the lowercased columns a PII scan flagged on this
// connection's table, matched by its full name or its name after the last dot.
func scanFlaggedColumns(ctx context.Context, database *sql.DB, connectionID, table string) (map[string]bool, error) {
	bare := table
	if i := strings.LastIndex(table, "."); i >= 0 {
		bare = table[i+1:]
	}
	rows, err := database.QueryContext(ctx, `
		SELECT DISTINCT column_name FROM pii_scan_results
		WHERE connection_id = $1 AND (table_name = $2 OR table_name = $3)
	`, connectionID, table, bare)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		out[strings.ToLower(col)] = true
	}
	return out, rows.Err()
}

// assistantPersonalNames are column-name words for personal data that the
// preview's mask.IsPIIColumnName leaves alone, because a preview shows them to a
// person who may read the source anyway. A model is a third party.
var assistantPersonalNames = []string{
	"first_name", "firstname", "last_name", "lastname", "full_name", "fullname", "surname",
	"maiden", "address", "street", "postcode", "postal", "zip_code", "zipcode",
	"birth", "ip_address", "ip_addr", "salary", "iban", "account_number",
}

// assistantWithholdsColumnName reports whether a column is withheld from a
// model by its name alone.
func assistantWithholdsColumnName(col string) bool {
	if mask.IsSecretColumnName(col) || mask.IsPIIColumnName(col) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(col))
	for _, w := range assistantPersonalNames {
		if strings.Contains(lower, w) {
			return true
		}
	}
	for _, tok := range mask.ColumnNameTokens(col) {
		if tok == "dob" || tok == "ip" {
			return true
		}
	}
	return false
}

func trimAssistantSample(result *SampleResult, want []string, flagged map[string]bool) ([]string, []map[string]interface{}, []string) {
	cols := append([]string(nil), result.Columns...)
	if len(cols) == 0 && len(result.Rows) > 0 {
		for k := range result.Rows[0] {
			cols = append(cols, k)
		}
		sort.Strings(cols)
	}
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[strings.ToLower(strings.TrimSpace(w))] = true
	}
	var keep, withheld []string
	for _, col := range cols {
		lower := strings.ToLower(col)
		if len(wanted) > 0 && !wanted[lower] {
			continue
		}
		if assistantWithholdsColumnName(col) || flagged[lower] {
			withheld = append(withheld, col)
			continue
		}
		keep = append(keep, col)
	}
	n := len(result.Rows)
	if n > assistantSampleRows {
		n = assistantSampleRows
	}
	rows := make([]map[string]interface{}, 0, n)
	for _, r := range result.Rows[:n] {
		row := make(map[string]interface{}, len(keep))
		for _, col := range keep {
			row[col] = assistantSampleValue(r[col])
		}
		rows = append(rows, row)
	}
	if keep == nil {
		keep = []string{}
	}
	if withheld == nil {
		withheld = []string{}
	}
	return keep, rows, withheld
}

func assistantSampleValue(v interface{}) interface{} {
	switch t := v.(type) {
	case nil, bool, int, int32, int64, float32, float64:
		return t
	case string:
		return cutRunes(t, assistantSampleValueRunes)
	case []byte:
		return cutRunes(string(t), assistantSampleValueRunes)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case map[string]interface{}:
		return "(object)"
	case []interface{}:
		return "(array)"
	default:
		return cutRunes(fmt.Sprint(t), assistantSampleValueRunes)
	}
}

func cutRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// llmDataView is GET/PUT /api/v1/workspace/llm-data.
type llmDataView struct {
	Mode           string   `json:"mode"`
	ApprovedModels []string `json:"approved_models"`
	CanEdit        bool     `json:"can_edit"`
}

func llmDataResponse(c *gin.Context, mode string) {
	approved, err := AssistantSamples{}.Approved(c.Request.Context())
	if err != nil {
		log.WithError(err).Warn("llm data mode: approved models unreadable")
		approved = map[string]bool{}
	}
	list := []string{}
	for _, e := range sampleRowsEntries {
		if approved[e] {
			list = append(list, e)
		}
	}
	role := security.WorkspaceRole(c.GetString(ctxWorkspaceRole))
	c.JSON(http.StatusOK, llmDataView{Mode: mode, ApprovedModels: list, CanEdit: role.Meets(security.WSOwner)})
}

// GetWorkspaceLLMData handles GET /api/v1/workspace/llm-data: whether Ask
// rsync.ai may read sample rows in the active workspace, and with which models.
// Any member may read it: the assistant panel shows it to everyone who asks.
func GetWorkspaceLLMData(c *gin.Context) {
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	mode, err := workspaceLLMDataMode(c.Request.Context(), workspaceID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		log.WithError(err).Error("get workspace llm data mode")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the setting"})
		return
	}
	llmDataResponse(c, mode)
}

// SetWorkspaceLLMData handles PUT /api/v1/workspace/llm-data with
// {"mode": "names_only" | "sample_rows"}. Owner only: it decides whether row
// values from this workspace's connections may reach a third-party model.
func SetWorkspaceLLMData(c *gin.Context) {
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSOwner); !ok {
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.Mode != LLMDataNamesOnly && req.Mode != LLMDataSampleRows) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be names_only or sample_rows"})
		return
	}
	if req.Mode == LLMDataSampleRows && !config.LLMSampleRowsAllowed() {
		c.JSON(http.StatusForbidden, gin.H{"error": "sample_rows_disabled", "message": sampleRowsDisabledMessage})
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	// The old mode is read, then the new one written, as two statements in one
	// transaction. One UPDATE reading the old value through a FOR UPDATE CTE
	// returned NULL: Postgres evaluates the CTE after the update and skips the
	// row the same statement changed, so every save committed, then answered
	// 500 with no audit row.
	ctx := c.Request.Context()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("set workspace llm data mode: begin")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the setting"})
		return
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT llm_data_mode FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&previous)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE workspaces SET llm_data_mode = $2 WHERE id = $1`, workspaceID, req.Mode)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		log.WithError(err).Error("set workspace llm data mode")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the setting"})
		return
	}
	logAudit(c, "workspace.llm_data_mode_update", "workspace", workspaceID, map[string]interface{}{
		"from": previous,
		"to":   req.Mode,
	})
	llmDataResponse(c, req.Mode)
}
