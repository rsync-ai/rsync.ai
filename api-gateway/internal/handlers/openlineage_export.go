package handlers

// The OpenLineage export (design § 5.2): a pull of a workspace's events as JSON lines, and
// a push of each new event to a target URL an admin sets. RegisterOpenLineage is its one
// registration point: the routes and the push hook.
//
// Both leave out private models: a push has no viewer, and the pull answers what the
// push sends.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/safehttp"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/rsync-ai/shared/crypto"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
)

const (
	openLineagePullDefaultLimit = 500
	openLineagePullMaxLimit     = 1000
	// OpenLineageNextCursorHeader carries the pull's next page, absent on the last.
	OpenLineageNextCursorHeader = "X-Next-Cursor"

	openLineageTargetURLMax     = 2048
	openLineageAuthHeaderMax    = 4096
	openLineagePushQueue        = 256
	openLineagePushAttempts     = 3
	openLineagePushTimeout      = 15 * time.Second
	openLineagePushFirstBackoff = time.Second

	openLineageStatusDelivered = "delivered"
	openLineageStatusFailed    = "failed"
	openLineageStatusRefused   = "refused"
)

// openLineageEventsSQL reads a workspace's events oldest first, by end then id. The
// filter is $2 (ids) or the window $3..$4 after the cursor $5,$6; private models are
// left out.
const openLineageEventsSQL = `
	SELECT e.id::text, e.workspace_id::text, e.job_kind,
	       COALESCE(e.saved_query_id::text, e.pipeline_id::text),
	       COALESCE(sq.name, p.name, ''), e.run_ref, e.event_type, e.started_at, e.ended_at,
	       e.inputs, e.outputs, e.sql_hash, COALESCE(sq.connection_id::text, '')
	FROM lineage_events e
	LEFT JOIN saved_queries sq ON sq.id = e.saved_query_id
	LEFT JOIN pipelines p ON p.id = e.pipeline_id
	WHERE e.workspace_id = $1::uuid
	  AND (e.saved_query_id IS NULL OR sq.visibility = 'workspace')
	  AND (cardinality($2::uuid[]) = 0 OR e.id = ANY($2::uuid[]))
	  AND ($3::timestamptz IS NULL OR e.ended_at >= $3::timestamptz)
	  AND ($4::timestamptz IS NULL OR e.ended_at < $4::timestamptz)
	  AND ($5::timestamptz IS NULL OR (e.ended_at, e.id) > ($5::timestamptz, $6::uuid))
	ORDER BY e.ended_at, e.id
	LIMIT $7`

// openLineageQuery is which of a workspace's events to read.
type openLineageQuery struct {
	IDs          []string
	Since, Until sql.NullTime
	AfterEnded   sql.NullTime
	AfterID      string
	Limit        int
}

// loadOpenLineageEvents reads a workspace's events with each model run's column lineage
// and each connection's naming.
func loadOpenLineageEvents(ctx context.Context, database *sql.DB, workspaceID string, q openLineageQuery) ([]openLineageEventRow, map[string]openLineageStore, error) {
	afterID := q.AfterID
	if afterID == "" {
		afterID = "00000000-0000-0000-0000-000000000000"
	}
	ids := pgdriver.StringArray{}
	if len(q.IDs) > 0 {
		ids = pgdriver.StringArray(q.IDs)
	}
	rows, err := database.QueryContext(ctx, openLineageEventsSQL,
		workspaceID, ids, q.Since, q.Until, q.AfterEnded, afterID, q.Limit)
	if err != nil {
		return nil, nil, err
	}
	var events []openLineageEventRow
	hashes := map[string]string{} // event id → model id + sql hash, for its columns
	for rows.Next() {
		var r openLineageEventRow
		var started sql.NullTime
		var inputs, outputs []byte
		var sqlHash string
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.JobKind, &r.JobID, &r.JobName, &r.RunRef, &r.EventType,
			&started, &r.EndedAt, &inputs, &outputs, &sqlHash, &r.ModelConnectionID); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if started.Valid {
			t := started.Time
			r.StartedAt = &t
		}
		// A skipped model run read and wrote nothing: it leaves as the run, with no
		// dataset, and no column lineage is looked up for it. The events recorded before
		// the recorder knew it carry the SQL's tables (migration 154 clears them).
		if lineageEventNamesTables(r.JobKind, r.EventType) {
			if err := json.Unmarshal(inputs, &r.Inputs); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if err := json.Unmarshal(outputs, &r.Outputs); err != nil {
				rows.Close()
				return nil, nil, err
			}
			if r.JobKind == lineageJobModel && sqlHash != "" {
				hashes[r.ID] = r.JobID + "\x00" + sqlHash
			}
		}
		events = append(events, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	rows.Close()

	columns, err := loadOpenLineageColumns(ctx, database, workspaceID, hashes)
	if err != nil {
		return nil, nil, err
	}
	conns := map[string]bool{}
	for i := range events {
		if c, ok := columns[hashes[events[i].ID]]; ok {
			events[i].Columns = c
		}
		for _, d := range append(append([]lineageDataset{}, events[i].Inputs...), events[i].Outputs...) {
			conns[d.ConnectionID] = true
		}
		if events[i].ModelConnectionID != "" {
			conns[events[i].ModelConnectionID] = true
		}
	}
	stores, err := loadOpenLineageStores(ctx, database, workspaceID, conns)
	if err != nil {
		return nil, nil, err
	}
	return events, stores, nil
}

// loadOpenLineageColumns reads the column lineage of each model version the events name,
// keyed by model id + "\x00" + sql hash.
func loadOpenLineageColumns(ctx context.Context, database *sql.DB, workspaceID string, hashes map[string]string) (map[string]*savedColumnLineage, error) {
	out := map[string]*savedColumnLineage{}
	if len(hashes) == 0 {
		return out, nil
	}
	want := map[string]bool{}
	models, sqlHashes := map[string]bool{}, map[string]bool{}
	for _, k := range hashes {
		want[k] = true
		model, hash, _ := strings.Cut(k, "\x00")
		models[model], sqlHashes[hash] = true, true
	}
	rows, err := database.QueryContext(ctx, `
		SELECT saved_query_id::text, sql_hash, status, result
		FROM model_column_lineage
		WHERE workspace_id = $1::uuid AND saved_query_id = ANY($2::uuid[]) AND sql_hash = ANY($3::text[])`,
		workspaceID, pgdriver.StringArray(mapKeys(models)), pgdriver.StringArray(mapKeys(sqlHashes)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var model, hash, status string
		var result []byte
		if err := rows.Scan(&model, &hash, &status, &result); err != nil {
			return nil, err
		}
		key := model + "\x00" + hash
		if !want[key] || (status != columnLineageStatusOK && status != columnLineageStatusPartial) {
			continue
		}
		var saved savedColumnLineage
		if json.Unmarshal(result, &saved) == nil {
			out[key] = &saved
		}
	}
	return out, rows.Err()
}

// loadOpenLineageStores names each connection's tables. The config is decrypted to read
// the allowlisted settings (openLineageConfigKeys) and dropped.
func loadOpenLineageStores(ctx context.Context, database *sql.DB, workspaceID string, conns map[string]bool) (map[string]openLineageStore, error) {
	out := map[string]openLineageStore{}
	ids := []string{}
	for id := range conns {
		if validUUID(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT id::text, COALESCE(connector_type, ''), COALESCE(config, '')
		FROM connections
		WHERE workspace_id = $1::uuid AND id = ANY($2::uuid[])`, workspaceID, pgdriver.StringArray(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, connectorType, enc string
		if err := rows.Scan(&id, &connectorType, &enc); err != nil {
			return nil, err
		}
		var cfg map[string]any
		if enc != "" {
			if plain, err := crypto.Decrypt(enc); err == nil {
				_ = json.Unmarshal(plain, &cfg)
			}
		}
		out[id] = openLineageStoreFor(id, connectorType, cfg)
	}
	return out, rows.Err()
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// openLineageCursor is the pull's place: the last event's end and id.
func encodeOpenLineageCursor(ended time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(ended.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeOpenLineageCursor(s string) (time.Time, string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", false
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || !validUUID(id) {
		return time.Time{}, "", false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", false
	}
	return t, id, true
}

func parseOpenLineageTime(s string) (sql.NullTime, bool) {
	if s == "" {
		return sql.NullTime{}, true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return sql.NullTime{}, false
	}
	return sql.NullTime{Time: t, Valid: true}, true
}

// PullOpenLineageEvents answers GET /lineage/openlineage?since=&until=&cursor=&limit=:
// the workspace's events, oldest first, one RunEvent per line. Admin: it lists every
// pipeline and model of the workspace at once.
func PullOpenLineageEvents(c *gin.Context) {
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	since, ok1 := parseOpenLineageTime(c.Query("since"))
	until, ok2 := parseOpenLineageTime(c.Query("until"))
	if !ok1 || !ok2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "since and until are RFC 3339 times"})
		return
	}
	q := openLineageQuery{Since: since, Until: until, Limit: openLineagePullDefaultLimit}
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		q.Limit = min(v, openLineagePullMaxLimit)
	}
	if cur := c.Query("cursor"); cur != "" {
		t, id, ok := decodeOpenLineageCursor(cur)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cursor is not one this endpoint gave"})
			return
		}
		q.AfterEnded, q.AfterID = sql.NullTime{Time: t, Valid: true}, id
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	events, stores, err := loadOpenLineageEvents(c.Request.Context(), database, workspaceID, q)
	if err != nil && !isUndefinedTable(err) {
		log.WithError(err).Error("openlineage: could not read the events")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read lineage events"})
		return
	}
	if len(events) == q.Limit {
		last := events[len(events)-1]
		c.Header(OpenLineageNextCursorHeader, encodeOpenLineageCursor(last.EndedAt, last.ID))
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, r := range events {
		if err := enc.Encode(buildOpenLineageEvent(r, stores)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode lineage events"})
			return
		}
	}
	c.Data(http.StatusOK, "application/x-ndjson", body.Bytes())
}

// openLineageTargetView is the target as the API shows it: never the auth header.
type openLineageTargetView struct {
	Configured      bool       `json:"configured"`
	URL             string     `json:"url"`
	HasAuthHeader   bool       `json:"has_auth_header"`
	Enabled         bool       `json:"enabled"`
	LastStatus      string     `json:"last_status"`
	LastStatusCode  *int       `json:"last_status_code"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`
	UpdatedAt       *time.Time `json:"updated_at"`
}

func readOpenLineageTarget(ctx context.Context, database *sql.DB, workspaceID string) (openLineageTargetView, error) {
	var v openLineageTargetView
	var code sql.NullInt64
	var delivered, updated sql.NullTime
	err := database.QueryRowContext(ctx, `
		SELECT url, auth_header_enc <> '', enabled, last_status, last_status_code, last_delivered_at, updated_at
		FROM openlineage_targets WHERE workspace_id = $1::uuid`, workspaceID,
	).Scan(&v.URL, &v.HasAuthHeader, &v.Enabled, &v.LastStatus, &code, &delivered, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return openLineageTargetView{}, nil
	}
	if err != nil {
		return v, err
	}
	v.Configured = true
	if code.Valid {
		n := int(code.Int64)
		v.LastStatusCode = &n
	}
	if delivered.Valid {
		v.LastDeliveredAt = &delivered.Time
	}
	if updated.Valid {
		v.UpdatedAt = &updated.Time
	}
	return v, nil
}

// GetOpenLineageTarget answers GET /lineage/openlineage/target. Admin.
func GetOpenLineageTarget(c *gin.Context) {
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	v, err := readOpenLineageTarget(c.Request.Context(), database, workspaceID)
	if err != nil && !isUndefinedTable(err) {
		log.WithError(err).Error("openlineage: could not read the target")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the OpenLineage target"})
		return
	}
	c.JSON(http.StatusOK, v)
}

// validateOpenLineageTargetURL accepts an http or https URL with a host and no user
// info. Whether its address is public is checked when it is dialed (safehttp).
func validateOpenLineageTargetURL(raw string) error {
	if raw == "" || len(raw) > openLineageTargetURLMax {
		return fmt.Errorf("url must be 1 to %d characters", openLineageTargetURLMax)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("url must be an http or https URL with a host")
	}
	if u.User != nil {
		return errors.New("url must not carry a user name or password; put a token in the auth header")
	}
	if err := safehttp.ValidateURL(u); err != nil {
		return errors.New("url must be an http or https URL with a host")
	}
	return nil
}

// validateOpenLineageAuthHeader accepts an Authorization header's value of one line.
func validateOpenLineageAuthHeader(v string) error {
	if len(v) > openLineageAuthHeaderMax {
		return fmt.Errorf("auth_header must be at most %d characters", openLineageAuthHeaderMax)
	}
	if strings.ContainsAny(v, "\r\n\x00") {
		return errors.New("auth_header must be one line")
	}
	return nil
}

// PutOpenLineageTarget answers PUT /lineage/openlineage/target with {url, enabled,
// auth_header}: auth_header absent keeps the stored one, "" clears it. Admin; audited
// without the header's value.
func PutOpenLineageTarget(c *gin.Context) {
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	var req struct {
		URL        string  `json:"url"`
		Enabled    *bool   `json:"enabled"`
		AuthHeader *string `json:"auth_header"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if err := validateOpenLineageTargetURL(req.URL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// "keep" leaves the stored ciphertext; otherwise $3 replaces it.
	keep, enc := req.AuthHeader == nil, ""
	if req.AuthHeader != nil {
		v := strings.TrimSpace(*req.AuthHeader)
		if err := validateOpenLineageAuthHeader(v); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if v != "" {
			var err error
			if enc, err = crypto.EncryptString(v); err != nil {
				log.WithError(err).Error("openlineage: could not encrypt the auth header")
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the OpenLineage target"})
				return
			}
		}
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	_, err := database.ExecContext(c.Request.Context(), `
		INSERT INTO openlineage_targets (workspace_id, url, auth_header_enc, enabled, updated_by, updated_at)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5, '')::uuid, NOW())
		ON CONFLICT (workspace_id) DO UPDATE SET
		    url = EXCLUDED.url,
		    auth_header_enc = CASE WHEN $6 THEN openlineage_targets.auth_header_enc ELSE EXCLUDED.auth_header_enc END,
		    enabled = EXCLUDED.enabled,
		    -- A new address has not been delivered to yet.
		    last_status = CASE WHEN openlineage_targets.url = EXCLUDED.url THEN openlineage_targets.last_status ELSE '' END,
		    last_status_code = CASE WHEN openlineage_targets.url = EXCLUDED.url THEN openlineage_targets.last_status_code END,
		    last_delivered_at = CASE WHEN openlineage_targets.url = EXCLUDED.url THEN openlineage_targets.last_delivered_at END,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = NOW()`,
		workspaceID, req.URL, enc, enabled, openLineageUserUUID(userID), keep)
	if err != nil {
		log.WithError(err).Error("openlineage: could not save the target")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save the OpenLineage target"})
		return
	}
	authChange := "kept"
	if !keep {
		authChange = map[bool]string{true: "set", false: "cleared"}[enc != ""]
	}
	logAudit(c, "openlineage_target.update", "workspace", workspaceID, gin.H{
		"url": req.URL, "enabled": enabled, "auth_header": authChange,
	})
	v, err := readOpenLineageTarget(c.Request.Context(), database, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read the OpenLineage target"})
		return
	}
	c.JSON(http.StatusOK, v)
}

// openLineageUserUUID is the user id when it is a UUID, "" otherwise (updated_by is
// NULL then).
func openLineageUserUUID(userID string) string {
	if validUUID(userID) {
		return userID
	}
	return ""
}

// DeleteOpenLineageTarget answers DELETE /lineage/openlineage/target. Admin; audited.
func DeleteOpenLineageTarget(c *gin.Context) {
	if _, ok := requireWorkspaceRole(c, security.WSAdmin); !ok {
		return
	}
	workspaceID, ok := resolveActiveWorkspace(c)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	if _, err := database.ExecContext(c.Request.Context(),
		`DELETE FROM openlineage_targets WHERE workspace_id = $1::uuid`, workspaceID); err != nil {
		log.WithError(err).Error("openlineage: could not delete the target")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete the OpenLineage target"})
		return
	}
	logAudit(c, "openlineage_target.delete", "workspace", workspaceID, nil)
	c.JSON(http.StatusOK, openLineageTargetView{})
}

// openLineagePusher posts each new event to its workspace's target. Its queue lives in
// memory: an event recorded while the queue is full, or before a restart, is not pushed
// and stays in the pull.
type openLineagePusher struct {
	database func() *sql.DB
	queue    chan []string
	client   *http.Client
	backoff  time.Duration
}

func newOpenLineagePusher(database func() *sql.DB) *openLineagePusher {
	client := safehttp.NewClient(openLineagePushTimeout)
	// A redirect is answered as it is, not followed with the auth header.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &openLineagePusher{database: database, queue: make(chan []string, openLineagePushQueue),
		client: client, backoff: openLineagePushFirstBackoff}
}

// enqueue is lineagePushHook: it never blocks.
func (p *openLineagePusher) enqueue(ids []string) {
	select {
	case p.queue <- ids:
	default:
		log.WithField("events", len(ids)).Warn("[OpenLineage] push queue full; these events stay in the pull only")
	}
}

func (p *openLineagePusher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ids := <-p.queue:
			if database := p.database(); database != nil {
				p.push(ctx, database, ids)
			}
		}
	}
}

// openLineageTarget is a workspace's push target, its auth header decrypted.
type openLineageTarget struct {
	URL        string
	AuthHeader string
}

// push sends the events with these ids to their workspaces' enabled targets.
func (p *openLineagePusher) push(ctx context.Context, database *sql.DB, ids []string) {
	rows, err := database.QueryContext(ctx, `
		SELECT e.workspace_id::text, array_agg(e.id::text ORDER BY e.ended_at, e.id), t.url, t.auth_header_enc
		FROM lineage_events e
		JOIN openlineage_targets t ON t.workspace_id = e.workspace_id AND t.enabled
		WHERE e.id = ANY($1::uuid[])
		GROUP BY e.workspace_id, t.url, t.auth_header_enc`, pgdriver.StringArray(ids))
	if err != nil {
		if !isUndefinedTable(err) {
			log.WithError(err).Warn("[OpenLineage] could not read the push targets")
		}
		return
	}
	type job struct {
		workspaceID string
		ids         []string
		target      openLineageTarget
	}
	var jobs []job
	for rows.Next() {
		var j job
		var idList pgStringList
		var enc string
		if err := rows.Scan(&j.workspaceID, &idList, &j.target.URL, &enc); err != nil {
			log.WithError(err).Warn("[OpenLineage] could not read a push target")
			continue
		}
		j.ids = idList
		if enc != "" {
			v, err := crypto.DecryptString(enc)
			if err != nil {
				log.WithError(err).WithField("workspace_id", j.workspaceID).Warn("[OpenLineage] could not decrypt a target's auth header; not pushed")
				continue
			}
			j.target.AuthHeader = v
		}
		jobs = append(jobs, j)
	}
	rows.Close()

	for _, j := range jobs {
		events, stores, err := loadOpenLineageEvents(ctx, database, j.workspaceID, openLineageQuery{IDs: j.ids, Limit: len(j.ids)})
		if err != nil {
			log.WithError(err).WithField("workspace_id", j.workspaceID).Warn("[OpenLineage] could not read the events to push")
			continue
		}
		var status string
		var code int
		for _, r := range events {
			body, err := json.Marshal(buildOpenLineageEvent(r, stores))
			if err != nil {
				continue
			}
			status, code = p.post(ctx, j.target, body)
			if status == openLineageStatusRefused || ctx.Err() != nil {
				break
			}
		}
		if status != "" {
			p.recordStatus(ctx, database, j.workspaceID, j.target.URL, status, code)
		}
	}
}

// post sends one event, retrying a network error, a 429 and a 5xx. It answers the
// delivery status and the last HTTP status code (0 without a response). The response
// body is read and dropped, never kept or logged.
func (p *openLineagePusher) post(ctx context.Context, t openLineageTarget, body []byte) (string, int) {
	wait := p.backoff
	code := 0
	for attempt := 1; attempt <= openLineagePushAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
		if err != nil {
			return openLineageStatusFailed, 0
		}
		req.Header.Set("Content-Type", "application/json")
		if t.AuthHeader != "" {
			req.Header.Set("Authorization", t.AuthHeader)
		}
		resp, err := p.client.Do(req)
		if err != nil {
			if errors.Is(err, safehttp.ErrBlockedAddress) {
				return openLineageStatusRefused, 0
			}
			code = 0
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			code = resp.StatusCode
			if code >= 200 && code < 300 {
				return openLineageStatusDelivered, code
			}
			if code != http.StatusTooManyRequests && code < 500 {
				return openLineageStatusFailed, code
			}
		}
		if attempt == openLineagePushAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return openLineageStatusFailed, code
		case <-time.After(wait):
		}
		wait *= 2
	}
	return openLineageStatusFailed, code
}

// recordStatus keeps the last delivery's outcome, unless the target changed meanwhile.
func (p *openLineagePusher) recordStatus(ctx context.Context, database *sql.DB, workspaceID, targetURL, status string, code int) {
	_, err := database.ExecContext(ctx, `
		UPDATE openlineage_targets
		SET last_status = $3, last_status_code = NULLIF($4, 0),
		    last_delivered_at = CASE WHEN $3 = 'delivered' THEN NOW() ELSE last_delivered_at END
		WHERE workspace_id = $1::uuid AND url = $2`, workspaceID, targetURL, status, code)
	if err != nil {
		log.WithError(err).WithField("workspace_id", workspaceID).Warn("[OpenLineage] could not record the delivery status")
	}
}

// pgStringList scans a Postgres text[] of plain values (ids), as array_agg returns it.
type pgStringList []string

func (l *pgStringList) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	case nil:
		*l = nil
		return nil
	default:
		return fmt.Errorf("pgStringList: cannot scan %T", src)
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	if s == "" {
		*l = []string{}
		return nil
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"`)
	}
	*l = parts
	return nil
}

// RegisterOpenLineage adds the OpenLineage export's routes to the authenticated API
// group and starts its push. The one place L3 is wired, so leaving it out leaves the
// export out (design open question 1: which edition ships it).
func RegisterOpenLineage(ctx context.Context, api *gin.RouterGroup) {
	pusher := newOpenLineagePusher(db.GetDB)
	lineagePushHook = pusher.enqueue
	go pusher.run(ctx)

	api.GET("/lineage/openlineage", PullOpenLineageEvents)
	api.GET("/lineage/openlineage/target", GetOpenLineageTarget)
	api.PUT("/lineage/openlineage/target", PutOpenLineageTarget)
	api.DELETE("/lineage/openlineage/target", DeleteOpenLineageTarget)
}
