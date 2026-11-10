package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rsync-ai/shared/crypto"
	log "github.com/sirupsen/logrus"
)

// The Checks tab's API (data-checks-design.md § 6, § 9):
//
//	GET    /explorer/saved/:id/checks                             a model's checks and their last results
//	POST   /explorer/saved/:id/checks                             add a check
//	PUT    /explorer/saved/:id/checks/:checkId                    replace a check
//	DELETE /explorer/saved/:id/checks/:checkId                    remove a check
//	POST   /explorer/saved/:id/checks/test                        Test now: run them on the live table
//	GET    /explorer/saved/:id/runs/:runId/checks                 what each check saw on one run
//	POST   /explorer/saved/:id/checks/pending/:pendingId/approve  apply a proposed custom_sql check
//	POST   /explorer/saved/:id/checks/pending/:pendingId/reject
//
// Who may change a check is who may edit the model: its creator or a workspace admin.
// A custom_sql check whose query is new or changed on a scheduled model does not apply:
// it runs as the schedule's run_as user and may read any table on the connection, so it
// becomes a proposal in saved_query_pending_edits (kind 'check') for an admin, the queue
// the model's own SQL uses (096, 138).

// modelCheckHistoryLen is how many of a check's results the Checks tab shows.
const modelCheckHistoryLen = 20

// checkTarget is what a check is judged against: the model's engine and its live table.
type checkTarget struct {
	dialect string
	owned   bool
	// table is the quoted live target, as a run's warnings and custom_sql see it.
	table string
	// refusal is why this model cannot have checks, or "".
	refusal string
	// connectorType is the connection's type, so the Checks tab can name the engine
	// that cannot hold a table back.
	connectorType string
}

// resolveCheckTarget says whether a model can have checks and, if so, against what. A
// check reads the model's own table, so a model must build one on a SQL engine.
func resolveCheckTarget(materialization, target string, owned bool, connectorType string) checkTarget {
	switch {
	case isStatementModel(materialization):
		return checkTarget{refusal: "a statement model writes to tables its SQL names, so it has no table of its own to check"}
	case materialization == "" || materialization == matNone:
		return checkTarget{refusal: "only a model that builds a table can have checks"}
	case isDocumentConnector(connectorType):
		return checkTarget{refusal: "checks are not supported on document databases yet"}
	}
	dialect := modelDialect(connectorType)
	if dialect == "" {
		return checkTarget{refusal: "checks are not supported for " + connectorType + " connections yet"}
	}
	schemaName, tableName, err := validateModelTarget(target)
	if err != nil {
		return checkTarget{refusal: "the model's target table is not valid: " + err.Error()}
	}
	return checkTarget{dialect: dialect, owned: owned, table: quoteModelIdent(dialect, schemaName, tableName),
		connectorType: connectorType}
}

// checkTargetColumns reads what resolveCheckTarget needs. The connection is joined on
// workspace too, as loadSavedQueryModel does.
const checkTargetSelect = `
	SELECT sq.materialization, COALESCE(sq.target_table, ''), sq.target_owned,
	       COALESCE(c.connector_type, '')
	FROM saved_queries sq
	LEFT JOIN connections c ON c.id = sq.connection_id AND c.workspace_id = sq.workspace_id
	WHERE sq.id = $1`

func scanCheckTarget(row *sql.Row) (checkTarget, error) {
	var mat, target, ct string
	var owned bool
	if err := row.Scan(&mat, &target, &owned, &ct); err != nil {
		return checkTarget{}, err
	}
	return resolveCheckTarget(mat, target, owned, ct), nil
}

// modelCheckView is one check as the Checks tab shows it.
type modelCheckView struct {
	ID       string           `json:"check_id"`
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Columns  []string         `json:"columns"`
	Params   modelCheckParams `json:"params"`
	SQLText  string           `json:"sql_text,omitempty"`
	Severity string           `json:"severity"`
	Enabled  bool             `json:"enabled"`
	// History is the check's last results, newest first; History[0] is its last result.
	History []modelCheckHistoryEntry `json:"history"`
}

// modelCheckHistoryEntry is what a check saw on one run.
type modelCheckHistoryEntry struct {
	RunID      string    `json:"run_id"`
	FinishedAt time.Time `json:"finished_at"`
	Status     string    `json:"status"`
	Severity   string    `json:"severity"`
	Observed   *int64    `json:"observed,omitempty"`
	Threshold  string    `json:"threshold,omitempty"`
	Message    string    `json:"message,omitempty"`
}

// modelCheckProposal is an open custom_sql check proposal.
type modelCheckProposal struct {
	ID         string          `json:"id"`
	CheckID    string          `json:"check_id,omitempty"`
	SQLText    string          `json:"sql_text"`
	BaseSQL    string          `json:"base_sql_text"`
	Check      json.RawMessage `json:"check"`
	Note       string          `json:"note,omitempty"`
	ProposedBy string          `json:"proposed_by"`
	ProposedAt time.Time       `json:"proposed_at"`
}

// modelCheckProposalPayload is the rest of a proposed check, beside its SQL: what an
// approval writes.
type modelCheckProposalPayload struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Columns  []string         `json:"columns"`
	Params   modelCheckParams `json:"params"`
	Severity string           `json:"severity"`
	Enabled  bool             `json:"enabled"`
}

// modelCheckRequest is a check as a client sends it, with a note for the approver.
type modelCheckRequest struct {
	modelCheckInput
	Note string `json:"note,omitempty"`
}

func parseCheckRouteIDs(c *gin.Context, extra string) (id, other string, ok bool) {
	id = strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return "", "", false
	}
	if extra != "" {
		other = strings.TrimSpace(c.Param(extra))
		if _, err := uuid.Parse(other); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + strings.TrimSuffix(extra, "Id") + " id"})
			return "", "", false
		}
	}
	return id, other, true
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// ListModelChecks is the Checks tab: each check with its last results, whether this
// model's engine can hold a table back, and the open custom_sql proposals.
// GET /api/v1/explorer/saved/:id/checks
func ListModelChecks(c *gin.Context) {
	id, _, ok := parseCheckRouteIDs(c, "")
	if !ok {
		return
	}
	if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	database := db.GetDB()
	ctx := c.Request.Context()
	fail := func(err error, what string) {
		log.WithError(err).WithField("model_id", id).Error("list model checks: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the model's checks"})
	}

	target, err := scanCheckTarget(database.QueryRowContext(ctx, checkTargetSelect, id))
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		fail(err, "model")
		return
	}

	rows, err := database.QueryContext(ctx, `SELECT `+modelCheckColumns+`
		FROM saved_query_checks WHERE saved_query_id = $1 ORDER BY created_at, name`, id)
	if err != nil {
		fail(err, "checks")
		return
	}
	checks := []modelCheckView{}
	index := map[string]int{}
	for rows.Next() {
		chk, enabled, err := scanModelCheck(rows)
		if err != nil {
			rows.Close()
			fail(err, "scan check")
			return
		}
		cols := chk.Columns
		if cols == nil {
			cols = []string{}
		}
		index[chk.ID] = len(checks)
		checks = append(checks, modelCheckView{ID: chk.ID, Name: chk.Name, Type: chk.Type, Columns: cols,
			Params: chk.Params, SQLText: chk.SQLText, Severity: chk.Severity, Enabled: enabled,
			History: []modelCheckHistoryEntry{}})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err, "checks")
		return
	}

	if len(checks) > 0 {
		// A result whose check was removed has no check to sit under, so it is left
		// out here; the run's own results still show it.
		hist, err := database.QueryContext(ctx, `
			SELECT check_id, run_id, finished_at, status, severity, observed, threshold, message
			FROM (
				SELECT r.check_id::text AS check_id, r.run_id::text AS run_id, sr.finished_at,
				       r.status, r.severity, r.observed, COALESCE(r.threshold, '') AS threshold,
				       COALESCE(r.message, '') AS message,
				       ROW_NUMBER() OVER (PARTITION BY r.check_id ORDER BY sr.finished_at DESC, r.run_id DESC) AS n
				FROM saved_query_check_results r
				JOIN saved_query_runs sr ON sr.run_id = r.run_id
				WHERE sr.saved_query_id = $1 AND r.check_id IS NOT NULL
			) h
			WHERE n <= $2
			ORDER BY check_id, n`, id, modelCheckHistoryLen)
		if err != nil {
			fail(err, "history")
			return
		}
		for hist.Next() {
			var checkID string
			var e modelCheckHistoryEntry
			var observed sql.NullInt64
			if err := hist.Scan(&checkID, &e.RunID, &e.FinishedAt, &e.Status, &e.Severity, &observed,
				&e.Threshold, &e.Message); err != nil {
				hist.Close()
				fail(err, "scan history")
				return
			}
			if observed.Valid {
				v := observed.Int64
				e.Observed = &v
			}
			if i, ok := index[checkID]; ok {
				checks[i].History = append(checks[i].History, e)
			}
		}
		hist.Close()
		if err := hist.Err(); err != nil {
			fail(err, "history")
			return
		}
	}

	pending, err := loadOpenCheckProposals(ctx, database, id)
	if err != nil {
		fail(err, "proposals")
		return
	}

	resp := gin.H{
		"checks":     checks,
		"pending":    pending,
		"max_checks": maxChecksPerModel,
		"can_block":  target.refusal == "" && modelCheckCanBlock(target.dialect, target.owned),
	}
	if target.refusal != "" {
		resp["unsupported_reason"] = target.refusal
	} else if !modelCheckCanBlock(target.dialect, target.owned) {
		resp["cannot_block_reason"] = cannotBlockMessage
		resp["connector_type"] = target.connectorType
	}
	c.JSON(http.StatusOK, resp)
}

func loadOpenCheckProposals(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}, modelID string) ([]modelCheckProposal, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id::text, COALESCE(check_id::text, ''), sql_text, base_sql_text, check_payload::text,
		       COALESCE(note, ''), proposed_by::text, proposed_at
		FROM saved_query_pending_edits
		WHERE saved_query_id = $1 AND kind = 'check' AND status = 'pending'
		ORDER BY proposed_at`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []modelCheckProposal{}
	for rows.Next() {
		var p modelCheckProposal
		var payload string
		if err := rows.Scan(&p.ID, &p.CheckID, &p.SQLText, &p.BaseSQL, &payload, &p.Note,
			&p.ProposedBy, &p.ProposedAt); err != nil {
			return nil, err
		}
		p.Check = json.RawMessage(payload)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListModelRunChecks is what each check saw on one run of the model.
// GET /api/v1/explorer/saved/:id/runs/:runId/checks
func ListModelRunChecks(c *gin.Context) {
	id, runID, ok := parseCheckRouteIDs(c, "runId")
	if !ok {
		return
	}
	if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	// The run is joined on the model, so a run id from another model reads as empty.
	rows, err := db.GetDB().QueryContext(c.Request.Context(), `
		SELECT COALESCE(r.check_id::text, ''), r.check_name, r.check_type, r.severity, r.status,
		       r.observed, COALESCE(r.threshold, ''), COALESCE(r.message, '')
		FROM saved_query_check_results r
		JOIN saved_query_runs sr ON sr.run_id = r.run_id AND sr.saved_query_id = $1
		WHERE r.run_id = $2
		ORDER BY r.check_name`, id, runID)
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("list a run's check results")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the run's check results"})
		return
	}
	defer rows.Close()
	results := []modelCheckResult{}
	for rows.Next() {
		var r modelCheckResult
		var observed sql.NullInt64
		if err := rows.Scan(&r.CheckID, &r.Name, &r.Type, &r.Severity, &r.Status, &observed,
			&r.Threshold, &r.Message); err != nil {
			log.WithError(err).WithField("model_id", id).Error("scan a run's check result")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the run's check results"})
			return
		}
		if observed.Valid {
			v := observed.Int64
			r.Observed = &v
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).WithField("model_id", id).Error("list a run's check results")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load the run's check results"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run_id": runID, "results": results})
}

// ---------------------------------------------------------------------------
// Changing
// ---------------------------------------------------------------------------

// requireCheckEditor is the gate for changing a model's checks: who may edit the model.
func requireCheckEditor(c *gin.Context, id string) (userID string, ok bool) {
	userID, ok = resolveUserID(c)
	if !ok {
		return "", false
	}
	role, ok := requireResourceRole(c, "saved_queries", id, security.WSMember)
	if !ok {
		return "", false
	}
	existing, ok := loadSavedQuery(c, id, userID)
	if !ok {
		return "", false
	}
	if existing.CreatedBy != userID && !role.Meets(security.WSAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the model's creator or a workspace admin can change its checks"})
		return "", false
	}
	return userID, true
}

// lockCheckTarget locks the model's row for the rest of the transaction and reads what
// a check is judged against from it. saved_queries first, then the checks and the
// pending edits: the order every path that holds both takes (saved_queries.go
// reviewSavedQueryPendingEdit).
func lockCheckTarget(c *gin.Context, tx *sql.Tx, id string) (checkTarget, bool) {
	target, err := scanCheckTarget(tx.QueryRowContext(c.Request.Context(), checkTargetSelect+` FOR UPDATE OF sq`, id))
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return target, false
	}
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("lock model for a check change")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to change the model's checks"})
		return target, false
	}
	if target.refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": target.refusal})
		return target, false
	}
	return target, true
}

// lockModelCheck reads one of the model's checks under a row lock.
func lockModelCheck(ctx context.Context, tx *sql.Tx, modelID, checkID string) (modelCheck, bool, error) {
	return scanModelCheck(tx.QueryRowContext(ctx, `SELECT `+modelCheckColumns+`
		FROM saved_query_checks WHERE check_id = $1 AND saved_query_id = $2 FOR UPDATE`, checkID, modelID))
}

// CreateModelCheck adds a check to a model.
// POST /api/v1/explorer/saved/:id/checks
func CreateModelCheck(c *gin.Context) { saveModelCheck(c, false) }

// UpdateModelCheck replaces one of a model's checks. enabled, when left out, keeps its
// current value.
// PUT /api/v1/explorer/saved/:id/checks/:checkId
func UpdateModelCheck(c *gin.Context) { saveModelCheck(c, true) }

func saveModelCheck(c *gin.Context, update bool) {
	extra := ""
	if update {
		extra = "checkId"
	}
	id, checkID, ok := parseCheckRouteIDs(c, extra)
	if !ok {
		return
	}
	userID, ok := requireCheckEditor(c, id)
	if !ok {
		return
	}
	var req modelCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := c.Request.Context()
	tx, err := db.GetDB().BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("begin model check change")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to change the model's checks"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	failed := func(err error, what string) {
		log.WithError(err).WithField("model_id", id).Error("model check change: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to change the model's checks"})
	}

	target, ok := lockCheckTarget(c, tx, id)
	if !ok {
		return
	}
	chk, err := normalizeModelCheck(req.modelCheckInput, target.dialect, target.owned, target.table)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	enabled := true
	var prev *modelCheck
	if update {
		p, prevEnabled, err := lockModelCheck(ctx, tx, id, checkID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "check not found"})
			return
		}
		if err != nil {
			failed(err, "lock check")
			return
		}
		prev, enabled = &p, prevEnabled
		// An approval writes the whole proposed check, so an edit landing before it
		// would be undone without a word. Settle the proposal first.
		var open bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM saved_query_pending_edits
			               WHERE saved_query_id = $1 AND check_id = $2 AND kind = 'check' AND status = 'pending')`,
			id, checkID).Scan(&open); err != nil {
			failed(err, "proposal probe")
			return
		}
		if open {
			c.JSON(http.StatusConflict, gin.H{"error": "this check has a change waiting for approval — it must be approved or rejected first"})
			return
		}
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_query_checks WHERE saved_query_id = $1`, id).Scan(&n); err != nil {
			failed(err, "count checks")
			return
		}
		if n >= maxChecksPerModel {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a model can have at most 20 checks"})
			return
		}
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// The gate (design § 6): only a query nobody has approved for this check yet, and
	// only on a model a schedule runs as somebody else.
	gated := chk.Type == checkCustomSQL &&
		(prev == nil || prev.Type != checkCustomSQL || prev.SQLText != chk.SQLText)
	if gated {
		scheduled, err := savedQueryIsScheduled(tx, id)
		if err != nil {
			// Not knowing is not "unscheduled": applying would skip the gate.
			failed(err, "schedule probe")
			return
		}
		gated = scheduled
	}
	if gated {
		proposeModelCheck(c, tx, id, userID, checkID, prev, chk, enabled, strings.TrimSpace(req.Note))
		return
	}

	colsJSON, paramsJSON, err := encodeModelCheck(chk)
	if err != nil {
		failed(err, "encode check")
		return
	}
	if update {
		_, err = tx.ExecContext(ctx, `
			UPDATE saved_query_checks
			SET name = $3, check_type = $4, columns = ARRAY(SELECT jsonb_array_elements_text($5::jsonb)),
			    params = $6::jsonb, sql_text = NULLIF($7, ''), severity = $8, enabled = $9,
			    updated_by = $10, updated_at = NOW()
			WHERE check_id = $1 AND saved_query_id = $2`,
			checkID, id, chk.Name, chk.Type, colsJSON, paramsJSON, chk.SQLText, chk.Severity, enabled, userID)
	} else {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO saved_query_checks
				(saved_query_id, name, check_type, columns, params, sql_text, severity, enabled, created_by, updated_by)
			VALUES ($1, $2, $3, ARRAY(SELECT jsonb_array_elements_text($4::jsonb)), $5::jsonb, NULLIF($6, ''), $7, $8, $9, $9)
			RETURNING check_id::text`,
			id, chk.Name, chk.Type, colsJSON, paramsJSON, chk.SQLText, chk.Severity, enabled, userID).Scan(&checkID)
	}
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "this model already has a check named " + chk.Name})
		return
	}
	if err != nil {
		failed(err, "write check")
		return
	}
	if err := tx.Commit(); err != nil {
		failed(err, "commit")
		return
	}

	action := "saved_query.check_create"
	status := http.StatusCreated
	if update {
		action, status = "saved_query.check_update", http.StatusOK
	}
	logAudit(c, action, "saved_query", id, map[string]interface{}{
		"check_id": checkID, "check_type": chk.Type, "severity": chk.Severity, "enabled": enabled,
	})
	cols := chk.Columns
	if cols == nil {
		cols = []string{}
	}
	c.JSON(status, modelCheckView{ID: checkID, Name: chk.Name, Type: chk.Type, Columns: cols, Params: chk.Params,
		SQLText: chk.SQLText, Severity: chk.Severity, Enabled: enabled, History: []modelCheckHistoryEntry{}})
}

func encodeModelCheck(chk modelCheck) (cols, params string, err error) {
	columns := chk.Columns
	if columns == nil {
		columns = []string{}
	}
	cb, err := json.Marshal(columns)
	if err != nil {
		return "", "", err
	}
	pb, err := json.Marshal(chk.Params)
	if err != nil {
		return "", "", err
	}
	return string(cb), string(pb), nil
}

// proposeModelCheck records a custom_sql check on a scheduled model as a proposal and
// commits. Nothing about the check changes until an admin approves; an edited check
// keeps running its approved query meanwhile. The transaction arrives holding the
// model's row lock.
func proposeModelCheck(c *gin.Context, tx *sql.Tx, id, userID, checkID string, prev *modelCheck, chk modelCheck, enabled bool, note string) {
	ctx := c.Request.Context()
	failed := func(err error, what string) {
		log.WithError(err).WithField("model_id", id).Error("propose model check: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to propose this check"})
	}
	// Caught here, not at approval: a proposal that can never apply is a request the
	// proposer can still fix.
	var taken bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM saved_query_checks
		               WHERE saved_query_id = $1 AND name = $2 AND check_id::text <> $3)`,
		id, chk.Name, checkID).Scan(&taken); err != nil {
		failed(err, "name probe")
		return
	}
	if taken {
		c.JSON(http.StatusConflict, gin.H{"error": "this model already has a check named " + chk.Name})
		return
	}

	payload, err := json.Marshal(modelCheckProposalPayload{Name: chk.Name, Type: chk.Type, Columns: chk.Columns,
		Params: chk.Params, Severity: chk.Severity, Enabled: enabled})
	if err != nil {
		failed(err, "encode proposal")
		return
	}
	base := ""
	if prev != nil {
		base = prev.SQLText
	}
	var pendingID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO saved_query_pending_edits
			(saved_query_id, kind, check_id, check_payload, sql_text, statement_class, base_sql_text, note, proposed_by)
		VALUES ($1, 'check', NULLIF($2, '')::uuid, $3::jsonb, $4, 'read', $5, NULLIF($6, ''), $7)
		RETURNING id::text`,
		id, checkID, string(payload), chk.SQLText, base, note, userID).Scan(&pendingID)
	if isUniqueViolation(err) {
		// One open proposal per check (138): two rewrites of one query have no merge.
		c.JSON(http.StatusConflict, gin.H{"error": "this check already has a change waiting for approval — it must be approved or rejected first"})
		return
	}
	if err != nil {
		failed(err, "insert proposal")
		return
	}
	if err := tx.Commit(); err != nil {
		failed(err, "commit")
		return
	}
	logAudit(c, "saved_query.propose_check_edit", "saved_query", id, map[string]interface{}{
		"pending_edit_id": pendingID, "check_id": checkID, "check_name": chk.Name,
	})
	c.JSON(http.StatusOK, gin.H{
		"pending_approval": gin.H{
			"id":       pendingID,
			"check_id": checkID,
			"reason":   "This model is on a schedule, and a custom SQL check runs as the schedule's user, so the query needs a workspace admin's approval before it takes effect.",
		},
	})
}

// DeleteModelCheck removes a check. Its past results stay, under their name.
// DELETE /api/v1/explorer/saved/:id/checks/:checkId
func DeleteModelCheck(c *gin.Context) {
	id, checkID, ok := parseCheckRouteIDs(c, "checkId")
	if !ok {
		return
	}
	if _, ok := requireCheckEditor(c, id); !ok {
		return
	}
	var name string
	err := db.GetDB().QueryRowContext(c.Request.Context(), `
		DELETE FROM saved_query_checks WHERE check_id = $1 AND saved_query_id = $2
		RETURNING name`, checkID, id).Scan(&name)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "check not found"})
		return
	}
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("delete model check")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove the check"})
		return
	}
	logAudit(c, "saved_query.check_delete", "saved_query", id, map[string]interface{}{
		"check_id": checkID, "check_name": name,
	})
	c.JSON(http.StatusOK, gin.H{"check_id": checkID, "deleted": true})
}

// ---------------------------------------------------------------------------
// Approving a proposed custom_sql check
// ---------------------------------------------------------------------------

// ApproveModelCheckEdit applies a proposed custom_sql check. Admin only.
// POST /api/v1/explorer/saved/:id/checks/pending/:pendingId/approve
func ApproveModelCheckEdit(c *gin.Context) { reviewModelCheckProposal(c, true) }

// RejectModelCheckEdit discards a proposed custom_sql check, keeping it as history.
// Admin only.
// POST /api/v1/explorer/saved/:id/checks/pending/:pendingId/reject
func RejectModelCheckEdit(c *gin.Context) { reviewModelCheckProposal(c, false) }

// reviewModelCheckProposal is approve and reject. Self-approval is allowed and recorded,
// for the reason reviewSavedQueryPendingEdit gives.
func reviewModelCheckProposal(c *gin.Context, approve bool) {
	id, pendingID, ok := parseCheckRouteIDs(c, "pendingId")
	if !ok {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	if _, ok := requireResourceRole(c, "saved_queries", id, security.WSAdmin); !ok {
		return
	}
	if _, ok := loadSavedQuery(c, id, userID); !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	ctx := c.Request.Context()
	tx, err := db.GetDB().BeginTx(ctx, nil)
	if err != nil {
		log.WithError(err).Error("begin model check review")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to review this change"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	failed := func(err error, what string) {
		log.WithError(err).WithField("model_id", id).Error("model check review: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to review this change"})
	}

	// saved_queries first, then pending_edits, as every path holding both. A rejection
	// writes only its verdict, so it takes no model lock (reviewSavedQueryPendingEdit).
	var target checkTarget
	if approve {
		if target, ok = lockCheckTarget(c, tx, id); !ok {
			return
		}
	}

	var checkID, sqlText, baseSQL, payloadJSON, proposedBy string
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(check_id::text, ''), sql_text, base_sql_text, check_payload::text, proposed_by::text
		FROM saved_query_pending_edits
		WHERE id = $1 AND saved_query_id = $2 AND kind = 'check' AND status = 'pending'
		FOR UPDATE`, pendingID, id).Scan(&checkID, &sqlText, &baseSQL, &payloadJSON, &proposedBy)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "there is no such check change waiting for approval"})
		return
	}
	if err != nil {
		failed(err, "load proposal")
		return
	}

	status := "rejected"
	if approve {
		status = "approved"
		var p modelCheckProposalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			failed(err, "decode proposal")
			return
		}
		enabled := p.Enabled
		// Judged again against the model as it is now: its engine or table may have
		// changed since the proposal, and what lands must pass today's rules.
		chk, err := normalizeModelCheck(modelCheckInput{Name: p.Name, Type: p.Type, Columns: p.Columns,
			Params: p.Params, SQLText: sqlText, Severity: p.Severity, Enabled: &enabled},
			target.dialect, target.owned, target.table)
		if err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "this change can no longer apply: " + err.Error() + " — reject it and propose it again"})
			return
		}
		if checkID, ok = applyApprovedModelCheck(c, tx, id, userID, proposedBy, checkID, baseSQL, chk, enabled); !ok {
			return
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE saved_query_pending_edits
		SET status = $2, reviewed_by = $3, reviewed_at = NOW(), review_note = NULLIF($4, '')
		WHERE id = $1`, pendingID, status, userID, strings.TrimSpace(req.Note)); err != nil {
		failed(err, "record verdict")
		return
	}
	if err := tx.Commit(); err != nil {
		failed(err, "commit")
		return
	}
	logAudit(c, "saved_query.review_check_edit", "saved_query", id, map[string]interface{}{
		"pending_edit_id": pendingID,
		"verdict":         status,
		"check_id":        checkID,
		"self_reviewed":   proposedBy == userID,
	})
	c.JSON(http.StatusOK, gin.H{"pending_edit_id": pendingID, "status": status, "applied": approve, "check_id": checkID})
}

// applyApprovedModelCheck writes an approved check inside the review's transaction and
// returns its id: checkID's, or the new check's when checkID is "". It writes the
// response on refusal.
func applyApprovedModelCheck(c *gin.Context, tx *sql.Tx, id, userID, proposedBy, checkID, baseSQL string, chk modelCheck, enabled bool) (string, bool) {
	ctx := c.Request.Context()
	failed := func(err error, what string) (string, bool) {
		log.WithError(err).WithField("model_id", id).Error("apply approved check: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to review this change"})
		return "", false
	}
	colsJSON, paramsJSON, err := encodeModelCheck(chk)
	if err != nil {
		return failed(err, "encode")
	}

	if checkID != "" {
		cur, _, err := lockModelCheck(ctx, tx, id, checkID)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusConflict, gin.H{"error": "the check this change edits was removed — reject the change"})
			return "", false
		}
		if err != nil {
			return failed(err, "lock check")
		}
		// The diff the reviewer saw was against base_sql_text; a check that moved since
		// would land something nobody compared.
		if cur.SQLText != baseSQL {
			c.JSON(http.StatusConflict, gin.H{"error": "the check changed after this was proposed — reject it and propose it again", "code": "stale_write"})
			return "", false
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE saved_query_checks
			SET name = $3, check_type = $4, columns = ARRAY(SELECT jsonb_array_elements_text($5::jsonb)),
			    params = $6::jsonb, sql_text = NULLIF($7, ''), severity = $8, enabled = $9,
			    updated_by = $10, updated_at = NOW()
			WHERE check_id = $1 AND saved_query_id = $2`,
			checkID, id, chk.Name, chk.Type, colsJSON, paramsJSON, chk.SQLText, chk.Severity, enabled, userID)
	} else {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM saved_query_checks WHERE saved_query_id = $1`, id).Scan(&n); err != nil {
			return failed(err, "count checks")
		}
		if n >= maxChecksPerModel {
			c.JSON(http.StatusConflict, gin.H{"error": "the model already has 20 checks — remove one, then approve"})
			return "", false
		}
		// The proposer wrote it; the approver let it in.
		err = tx.QueryRowContext(ctx, `
			INSERT INTO saved_query_checks
				(saved_query_id, name, check_type, columns, params, sql_text, severity, enabled, created_by, updated_by)
			VALUES ($1, $2, $3, ARRAY(SELECT jsonb_array_elements_text($4::jsonb)), $5::jsonb, NULLIF($6, ''), $7, $8, $9, $10)
			RETURNING check_id::text`,
			id, chk.Name, chk.Type, colsJSON, paramsJSON, chk.SQLText, chk.Severity, enabled, proposedBy, userID).Scan(&checkID)
	}
	if isUniqueViolation(err) {
		c.JSON(http.StatusConflict, gin.H{"error": "this model already has a check named " + chk.Name + " — reject the change or rename that check"})
		return "", false
	}
	if err != nil {
		return failed(err, "write check")
	}
	return checkID, true
}

// ---------------------------------------------------------------------------
// Test now
// ---------------------------------------------------------------------------

// TestModelChecks runs every saved check on the model's live table, as the person
// clicking, and records nothing (design § 9). A check is a read — a custom_sql one
// passes the gate an Explorer read does — so the floor is a viewer's, as a read in the
// Explorer is.
//
// With {"check": {...}} it tests that one check instead, unsaved, as it would save: the
// Add check form's Test. Saving it and clicking Test now would run the same read, so a
// draft takes the role saving does.
// POST /api/v1/explorer/saved/:id/checks/test
func TestModelChecks(c *gin.Context) {
	allowLongModelRun(c)
	id, _, ok := parseCheckRouteIDs(c, "")
	if !ok {
		return
	}
	var req struct {
		Check *modelCheckInput `json:"check"`
	}
	if c.Request.Body != nil {
		if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}
	if req.Check != nil {
		if _, ok := requireCheckEditor(c, id); !ok {
			return
		}
	} else if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	database := db.GetDB()
	ctx := c.Request.Context()

	m, err := loadSavedQueryModel(ctx, database, id)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("test checks: load model")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to test the checks"})
		return
	}
	target := resolveCheckTarget(m.Materialization, m.TargetTable, m.TargetOwned, m.ConnectorType)
	if target.refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": target.refusal})
		return
	}
	var checks []modelCheck
	if req.Check != nil {
		chk, err := normalizeModelCheck(*req.Check, target.dialect, target.owned, target.table)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		checks = []modelCheck{chk}
	} else if checks, err = loadModelChecks(ctx, database, id, false); err != nil {
		log.WithError(err).WithField("model_id", id).Error("test checks: load checks")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to test the checks"})
		return
	}
	if len(checks) == 0 {
		c.JSON(http.StatusOK, gin.H{"results": []modelCheckResult{}})
		return
	}
	var baseline *int64
	for _, chk := range checks {
		if chk.Type == checkRowCountChange {
			if baseline, err = loadCheckBaseline(ctx, database, id); err != nil {
				log.WithError(err).WithField("model_id", id).Error("test checks: load baseline")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to test the checks"})
				return
			}
			break
		}
	}
	// What a run would do with each one: a block this engine cannot hold is a warning.
	if !modelCheckCanBlock(target.dialect, target.owned) {
		for i := range checks {
			checks[i].Severity = checkSeverityWarn
		}
	}

	rd, closeReader, refusal := openCheckReader(ctx, m, target.dialect)
	if refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": refusal})
		return
	}
	defer closeReader()
	runCtx, cancel := context.WithTimeout(ctx, modelRunTimeout)
	defer cancel()
	results := runModelChecks(runCtx, rd, target.dialect, target.table, checks, baseline)
	c.JSON(http.StatusOK, gin.H{"results": results, "tested_at": time.Now().UTC()})
}

// openCheckReader connects to the model's engine the way its run does. The refusal says
// the stored connection is unusable without saying how: the underlying error is about
// credentials, and goes to the log.
func openCheckReader(ctx context.Context, m *savedQueryModel, dialect string) (checkReader, func(), string) {
	const unusable = "this model's connection settings could not be used"
	configJSON, err := crypto.DecryptString(m.ConfigEncrypted)
	if err != nil {
		log.WithError(err).WithField("connection_id", m.ConnectionID).Error("test checks: could not decrypt connection config")
		return nil, nil, unusable
	}
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		log.WithError(err).WithField("connection_id", m.ConnectionID).Error("test checks: could not parse connection config")
		return nil, nil, unusable
	}
	if !modelDialectRunsOnDriver(dialect) {
		return engineCheckReader{runner: newModelEngineRunner(dialect, m.ConnectorType, config)}, func() {}, ""
	}
	dsn, err := modelDSN(dialect, config)
	if err != nil {
		log.WithError(err).WithField("connection_id", m.ConnectionID).Error("test checks: could not build a DSN")
		return nil, nil, unusable
	}
	conn, err := openExplorerDB(dialect, dsn)
	if err != nil {
		log.WithError(err).WithField("connection_id", m.ConnectionID).Error("test checks: could not open the connection")
		return nil, nil, unusable
	}
	conn.SetMaxOpenConns(1)
	// Each check reads in its own read-only transaction, under a server-side timeout
	// (read_only_reads.go): a check's SQL is a read, and runs as one.
	dbc, err := conn.Conn(ctx)
	if err != nil {
		_ = conn.Close()
		log.WithError(err).WithField("connection_id", m.ConnectionID).Error("test checks: could not open the connection")
		return nil, nil, unusable
	}
	closeAll := func() { _ = dbc.Close(); _ = conn.Close() }
	return readOnlyCheckReader{conn: dbc, dialect: dialect, timeout: modelRunTimeout}, closeAll, ""
}

// ---------------------------------------------------------------------------
// Planning (plan_model_checks)
// ---------------------------------------------------------------------------

// checkPlanTargetSelect is checkTargetSelect with the connection, whose cached schema
// the agent tool checks a plan's columns against.
const checkPlanTargetSelect = `
	SELECT sq.materialization, COALESCE(sq.target_table, ''), sq.target_owned,
	       COALESCE(c.connector_type, ''), COALESCE(c.id::text, '')
	FROM saved_queries sq
	LEFT JOIN connections c ON c.id = sq.connection_id AND c.workspace_id = sq.workspace_id
	WHERE sq.id = $1`

// modelChecksPlan is what a proposed set of checks would do on this model: each check
// as it would save, or why it would not. Only plan_model_checks reads it.
type modelChecksPlan struct {
	ConnectionID string `json:"connection_id"`
	TargetSchema string `json:"target_schema"`
	TargetTable  string `json:"target_table"`
	// ColumnsCaseSensitive is whether a check's column must match the table's in case:
	// Postgres compares a quoted name exactly, the other engines without case
	// (checkColumnPresent).
	ColumnsCaseSensitive bool                `json:"columns_case_sensitive"`
	CanBlock             bool                `json:"can_block"`
	CannotBlockReason    string              `json:"cannot_block_reason,omitempty"`
	Existing             int                 `json:"existing"`
	MaxChecks            int                 `json:"max_checks"`
	Checks               []plannedModelCheck `json:"checks"`
}

// plannedModelCheck is one proposed check as it would save.
type plannedModelCheck struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Columns  []string `json:"columns"`
	Severity string   `json:"severity"`
	// Refusal is why the Checks tab would refuse this check, or "" when it would save.
	Refusal string `json:"refusal,omitempty"`
}

// PlanModelChecks judges a proposed set of checks as the Checks tab would save them, and
// saves nothing (design § 8). A person adds them on the Checks tab. Not routed: the
// only caller is the plan_model_checks tool, in-process, and it checks each column
// against the connection's cached schema itself. A check left without a severity on an
// engine that cannot hold a table back is planned as a warning, as the Checks tab fixes
// it there.
// POST (in-process) /explorer/saved/:id/checks/plan
func PlanModelChecks(c *gin.Context) {
	id, _, ok := parseCheckRouteIDs(c, "")
	if !ok {
		return
	}
	if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	var req struct {
		Checks []modelCheckInput `json:"checks"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(req.Checks) == 0 || len(req.Checks) > maxChecksPerModel {
		c.JSON(http.StatusBadRequest, gin.H{"error": "send 1 to 20 checks"})
		return
	}
	database := db.GetDB()
	ctx := c.Request.Context()
	fail := func(err error, what string) {
		log.WithError(err).WithField("model_id", id).Error("plan model checks: " + what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to plan the model's checks"})
	}

	var mat, rawTarget, connectorType, connectionID string
	var owned bool
	err := database.QueryRowContext(ctx, checkPlanTargetSelect, id).
		Scan(&mat, &rawTarget, &owned, &connectorType, &connectionID)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err != nil {
		fail(err, "model")
		return
	}
	target := resolveCheckTarget(mat, rawTarget, owned, connectorType)
	if target.refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "this model cannot have checks: " + target.refusal})
		return
	}
	schemaName, tableName, _ := validateModelTarget(rawTarget) // resolveCheckTarget has validated it

	rows, err := database.QueryContext(ctx, `SELECT name FROM saved_query_checks WHERE saved_query_id = $1`, id)
	if err != nil {
		fail(err, "checks")
		return
	}
	taken := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			fail(err, "scan check")
			return
		}
		taken[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err, "checks")
		return
	}

	canBlock := modelCheckCanBlock(target.dialect, target.owned)
	plan := modelChecksPlan{
		ConnectionID: connectionID, TargetSchema: schemaName, TargetTable: tableName,
		ColumnsCaseSensitive: target.dialect == "postgres",
		CanBlock:             canBlock, Existing: len(taken), MaxChecks: maxChecksPerModel,
		Checks: make([]plannedModelCheck, 0, len(req.Checks)),
	}
	if !canBlock {
		plan.CannotBlockReason = cannotBlockMessage
	}
	room := maxChecksPerModel - len(taken)
	planned := map[string]bool{}
	for _, in := range req.Checks {
		if strings.TrimSpace(in.Severity) == "" && !canBlock {
			in.Severity = checkSeverityWarn
		}
		chk, err := normalizeModelCheck(in, target.dialect, target.owned, target.table)
		p := plannedModelCheck{Name: chk.Name, Type: chk.Type, Columns: chk.Columns, Severity: chk.Severity}
		switch {
		case err != nil:
			p.Refusal = err.Error()
		case taken[chk.Name]:
			p.Refusal = "this model already has a check named " + chk.Name
		case planned[chk.Name]:
			p.Refusal = "two of the checks are named " + chk.Name
		case room <= 0:
			p.Refusal = "a model can have at most 20 checks"
		default:
			planned[chk.Name] = true
			room--
		}
		if p.Columns == nil {
			p.Columns = []string{}
		}
		plan.Checks = append(plan.Checks, p)
	}
	c.JSON(http.StatusOK, plan)
}
