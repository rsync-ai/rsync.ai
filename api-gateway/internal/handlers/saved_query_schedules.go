package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	// Embeds the IANA timezone database. The runtime image is alpine, which ships no
	// zoneinfo, so without this time.LoadLocation below fails for every zone except
	// UTC — and schedules are created with the BROWSER's zone by default, not UTC.
	//
	// Imported by THIS package rather than by package main on purpose: the test binary
	// does not link main, so an embed placed there would leave nextScheduleRun's
	// timezone tests passing on a developer laptop (which has system zoneinfo) and
	// failing in a container — the exact inversion of what the tests are for.
	_ "time/tzdata"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/robfig/cron"
	"github.com/rsync-ai/shared/pgdriver"
	log "github.com/sirupsen/logrus"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// ============================================================================
// Saved-query model schedules
// ============================================================================
// The HTTP layer over saved_query_schedules (migration 085). It mirrors
// pipeline_schedules.go — same Temporal Schedules API, same cron/interval spec,
// same SKIP overlap policy — and reuses ScheduleSpec and validateScheduleSpec
// outright so there is one schedule grammar in this codebase rather than two.
//
// Three things here are NOT in the pipeline version, all for the same reason: a
// pipeline's authority comes from the pipeline row, while a model's authority
// comes from a person, and people change.
//
//  1. Every mutation requires WSAdmin, not WSMember. Saving a query is a member act
//     (it mutates nothing); pointing one at a table and letting it rebuild that table
//     unattended is a DDL act. See modelRunMinRole.
//
//  2. Creating a schedule dry-runs the same authorization the fire path will apply,
//     so a schedule that could never fire successfully is rejected at create time
//     instead of failing silently at 3am.
//
//  3. Resume re-runs that check. An auto-pause is a security stop, and a resume click
//     must not be able to clear one — if the condition still holds, the resume is
//     refused with the reason.

// The two kinds of thing a model can be woken by. Stored on every upstream row rather
// than inferred from which of the two id columns is set, for the reason migration 100
// gives: a reader that has to work that out gets it wrong once and then reports a model
// trigger as a pipeline trigger forever.
const (
	upstreamKindPipeline = "pipeline"
	upstreamKindModel    = "model"
	// upstreamKindWorkflow is a model run a workflow's refresh_model step asked for
	// (migration 140): upstream_id is the workflow, origin_execution_id its run. Never a
	// trigger's upstream; only a run's.
	upstreamKindWorkflow = "workflow"
)

// scheduleUpstream is one thing a model rebuilds after. Name is joined for display and
// is ignored on write — the id is the edge.
type scheduleUpstream struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// SavedQuerySchedule is one durable schedule over a model.
type SavedQuerySchedule struct {
	ScheduleID   string       `json:"schedule_id"`
	SavedQueryID string       `json:"saved_query_id"`
	ScheduleType string       `json:"schedule_type"`
	ScheduleSpec ScheduleSpec `json:"schedule_spec"`

	// A clock schedule has a Temporal counterpart and no upstreams; an event trigger is
	// the other way round. Migration 100 still enforces the Temporal half in both
	// directions, but the "an event trigger has an upstream" half stopped being a CHECK
	// when the upstreams moved into a child table — a parent row cannot be constrained
	// by what does or does not reference it. Three weaker things replace it: the single
	// transaction each write path uses, validateModelScheduleSpec refusing an empty set,
	// and the fire path, which matches BY upstream and so cannot see a schedule that has
	// none. The last one is the only one a row written by some future path cannot get
	// around.
	TemporalScheduleID string             `json:"temporal_schedule_id,omitempty"`
	Upstreams          []scheduleUpstream `json:"upstreams,omitempty"`
	UpstreamPolicy     string             `json:"upstream_policy"`

	Status      string    `json:"status"`
	RunAsUserID string    `json:"run_as_user_id"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	PausedAt         *time.Time `json:"paused_at,omitempty"`
	PausedReason     string     `json:"paused_reason,omitempty"`
	AutoPausedAt     *time.Time `json:"auto_paused_at,omitempty"`
	AutoPausedReason string     `json:"auto_paused_reason,omitempty"`

	// Blocked is computed per request, never stored: an active schedule whose model
	// would be refused at fire time is not going to announce that on its own. Same
	// role Schedule.Blocked plays for pipelines.
	Blocked       bool   `json:"blocked,omitempty"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// EventDriven reports whether something upstream wakes this schedule rather than a
// clock. Every Temporal call site below branches on this rather than on
// TemporalScheduleID being empty: the two are equivalent today only because migrations
// 095 and 100 make them so, and a guard that reads the schedule's own type keeps saying
// what it means if that ever stops being true.
func (s *SavedQuerySchedule) EventDriven() bool { return s.ScheduleType == scheduleAfterUpstream }

type setMaterializationRequest struct {
	Materialization string `json:"materialization" binding:"required"`
	TargetTable     string `json:"target_table"`
	// Layer is the name of one of the workspace's layers (workspace_layers.go), in any
	// case, or "" to clear it. Absent keeps the stored value, so a caller that predates
	// layers does not wipe one an admin set.
	Layer *string `json:"layer"`
}

// resolveModelLayer finds the layer a model asks for among its own workspace's
// layers, by name ignoring case. "" resolves to no layer (nil). refusal is set
// when the workspace has no layer by that name, and lists the ones it has.
func resolveModelLayer(ctx context.Context, database *sql.DB, savedQueryID, raw string) (id *string, name, refusal string, err error) {
	want, ok := cleanLayerText(raw)
	if !ok {
		return nil, "", "a layer name cannot contain control characters", nil
	}
	if want == "" {
		return nil, "", "", nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT wl.id, wl.name FROM workspace_layers wl
		JOIN saved_queries sq ON sq.workspace_id = wl.workspace_id
		WHERE sq.id = $1
		ORDER BY wl.position`, savedQueryID)
	if err != nil {
		return nil, "", "", err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var lid, lname string
		if err := rows.Scan(&lid, &lname); err != nil {
			return nil, "", "", err
		}
		if strings.EqualFold(lname, want) {
			return &lid, lname, "", nil
		}
		names = append(names, lname)
	}
	if err := rows.Err(); err != nil {
		return nil, "", "", err
	}
	if len(names) == 0 {
		return nil, "", fmt.Sprintf("This workspace has no layers yet, so a model cannot go in %q. "+
			"An owner or admin adds layers in Workspace settings.", want), nil
	}
	return nil, "", fmt.Sprintf("This workspace has no layer named %q. Its layers are %s.",
		want, strings.Join(names, ", ")), nil
}

// scheduleAfterUpstream is the third schedule_type and the only one that is not a
// cadence. The model is woken by something upstream of it finishing rather than by a
// clock.
//
// It exists because a cron was the wrong instrument for what models are actually for.
// A model reads tables a pipeline writes, so scheduling it means guessing a time far
// enough after that pipeline usually lands — and both directions of a wrong guess are
// silent. Too early rebuilds yesterday's data and reports success; too late leaves the
// dashboard stale for exactly as long as the safety margin someone padded in.
//
// Migration 095 spelled this "after_pipeline" and stored one pipeline id on the schedule
// row. Migration 100 renamed the value because neither half of that name survived: an
// upstream can be another model, and there can be more than one.
const scheduleAfterUpstream = "after_upstream"

// maxScheduleUpstreams bounds one schedule's fan-in. A model reading from more than this
// many producers is a modelling problem the operator should see as a refusal rather than
// as a schedule that quietly waits on a set nobody can read off the page.
const maxScheduleUpstreams = 16

// Upstream policies (migration 104). 'any' rebuilds on every upstream completion, which is
// what every schedule did before the column existed. 'all' rebuilds only once EVERY upstream
// has succeeded since this model's last successful rebuild started.
//
// 'all' exists because 'any' is wrong for a real fan-in: a model joining two producers was
// rebuilt the moment the first finished, reading the second one's previous output, and then
// rebuilt again when the second finished. The first build was wasted at best and, for a
// consumer that read the table in between, a wrong answer.
const (
	upstreamPolicyAny = "any"
	upstreamPolicyAll = "all"
)

// normalizeUpstreamPolicy returns the stored form of a requested policy.
func normalizeUpstreamPolicy(scheduleType, policy string) (string, error) {
	policy = strings.TrimSpace(policy)
	if policy == "" {
		return upstreamPolicyAny, nil
	}
	if policy != upstreamPolicyAny && policy != upstreamPolicyAll {
		return "", fmt.Errorf("upstream_policy must be %q or %q", upstreamPolicyAny, upstreamPolicyAll)
	}
	// Refused rather than ignored, for the same reason upstreams are on a clock schedule:
	// a request asking a cadence to wait for all upstreams has no reading we could honour.
	if policy == upstreamPolicyAll && scheduleType != scheduleAfterUpstream {
		return "", fmt.Errorf("upstream_policy only applies to %s schedules", scheduleAfterUpstream)
	}
	return policy, nil
}

// maxTriggerChainDepth bounds how far one rebuild chain walks before it stops waking
// anything further.
//
// A backstop, not the cycle check: a loop is refused at write time by checkUpstreamCycle,
// and this is for the loop that gets in some other way. The depth rides along on the
// signal payload and on every model run, so each hop through Temporal adds to one count.
const maxTriggerChainDepth = 8

// chainDepthExceeded is the bound itself, named so a test can walk a ring against it
// without a database — the guard it fronts sits behind a live connection and a lookup.
func chainDepthExceeded(depth int) bool { return depth > maxTriggerChainDepth }

type createSavedQueryScheduleRequest struct {
	ScheduleType string       `json:"schedule_type" binding:"required"`
	ScheduleSpec ScheduleSpec `json:"schedule_spec" binding:"required"`

	// Upstreams is required for, and only meaningful to, after_upstream. Every entry is
	// authorized as a resource in its own right before it is stored: naming a pipeline or
	// a model in another workspace would otherwise turn this field into a cross-tenant
	// probe that reports, through this model's own run history, when that tenant's work
	// finishes.
	//
	// On update it is the COMPLETE set, not a delta. The dialog shows the whole set, so
	// the whole set is what comes back.
	Upstreams []scheduleUpstream `json:"upstreams,omitempty"`

	// UpstreamPolicy decides what a fan-in waits for; see upstreamPolicyAll. Only
	// meaningful to after_upstream. Empty means 'any', including on update, so a client
	// that predates the field keeps the behaviour every schedule had before it existed.
	UpstreamPolicy string `json:"upstream_policy,omitempty"`
}

type pauseSavedQueryScheduleRequest struct {
	Reason string `json:"reason,omitempty"`
}

// internalModelRunRequest is what the Temporal activity POSTs.
type internalModelRunRequest struct {
	ScheduleID string `json:"schedule_id"`

	// Trigger names which of the endpoint's two doors this call is for. Empty is the
	// clock path, which is what every schedule created before the event path existed
	// still sends, so those keep behaving exactly as they did. The only other accepted
	// value is scheduleAfterUpstream.
	Trigger string `json:"trigger"`

	// Depth is how many rebuilds already stand between the thing that started this chain
	// and this one. Carried on the wire because every hop goes through Temporal and leaves
	// no in-process frame to count: without it, maxTriggerChainDepth would bound nothing.
	Depth int `json:"depth,omitempty"`

	// Provenance of an event-path run (migration 104): the upstream completion that woke
	// it, that upstream's own run row when it is a model, the pipeline execution at the
	// root of the chain, and how many completions the adapter coalesced into this call.
	// All optional — an adapter that predates them still runs, it just records less.
	UpstreamKind  string `json:"upstream_kind,omitempty"`
	UpstreamID    string `json:"upstream_id,omitempty"`
	UpstreamRunID string `json:"upstream_run_id,omitempty"`
	ExecutionID   string `json:"execution_id,omitempty"`
	Coalesced     int    `json:"coalesced,omitempty"`

	// NextHop is modelNextHopCaller when the calling workflow, not this request, wakes
	// the models below the run (model_next_hop.go). Empty from an adapter that predates
	// it, which then gets the hop fired from here as before.
	NextHop string `json:"next_hop,omitempty"`
}

// ============================================================================
// Materialization settings
// ============================================================================

// SetSavedQueryMaterialization turns a saved query into a model, or back.
// PUT /api/v1/explorer/saved/:id/materialization
func SetSavedQueryMaterialization(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	if !requireVisibleSavedQuery(c, id, modelRunMinRole) {
		return
	}

	var req setMaterializationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Materialization))
	if mode != matNone && mode != matTable && mode != matStatement {
		c.JSON(http.StatusBadRequest, gin.H{"error": `materialization must be "none", "table" or "statement"`})
		return
	}
	if mode == matNone {
		// Leave target_table as it was. The model stops rebuilding; whatever table it
		// last produced stays exactly where it is, because deleting a user's data is
		// not something turning a toggle off should do. The layer goes: a query that
		// builds nothing is in no layer.
		if _, err := database.ExecContext(c.Request.Context(),
			`UPDATE saved_queries SET materialization = 'none', layer_id = NULL, updated_at = NOW() WHERE id = $1`, id); err != nil {
			log.WithError(err).Error("failed to clear materialization")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update saved query"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"materialization": matNone})
		return
	}

	// keepLayer is true when the request leaves the layer as it is. Resolved only
	// for a model: turning one off drops its layer, whatever the request names.
	var layerID *string
	layerName, keepLayer := "", req.Layer == nil
	if !keepLayer {
		var refusal string
		var err error
		if layerID, layerName, refusal, err = resolveModelLayer(c.Request.Context(), database, id, *req.Layer); err != nil {
			log.WithError(err).Error("failed to load the workspace's layers")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update saved query"})
			return
		} else if refusal != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": refusal})
			return
		}
	}

	// Only table mode has a destination to name. Asking a statement model for one was
	// the defect: a MERGE already names both its source and its target inside the SQL,
	// so there was no honest answer to give, and the modal would not let the query be
	// scheduled without one.
	target := ""
	if mode == matTable {
		schemaName, tableName, err := validateModelTarget(req.TargetTable)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		target = tableName
		if schemaName != "" {
			target = schemaName + "." + tableName
		}
	}

	// Refuse now what the fire path would refuse later. The model row is loaded rather
	// than trusting the request, so this classifies the SQL that will actually run.
	m, err := loadSavedQueryModel(c.Request.Context(), database, id)
	if err != nil {
		// requireVisibleSavedQuery already proved the row exists and is in reach, so a miss
		// here means the connection join failed — the connection is gone or has moved.
		c.JSON(http.StatusConflict, gin.H{"error": "this saved query's connection is no longer available in this workspace"})
		return
	}
	if !modelSupported(m.ConnectorType) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "materialized models are not supported for " + m.ConnectorType + " connections yet",
		})
		return
	}
	m.Materialization = mode
	m.TargetTable = target
	if _, refusal, err := authorizeModelRun(c.Request.Context(), database, m, c.GetString("user_id")); err != nil {
		// Undecided, not denied — a 400 here would tell the user their query is invalid
		// when the truth is that rsync.ai could not check.
		log.WithError(err).Error("failed to authorize materialization target")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify this query can be materialized; try again"})
		return
	} else if refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": refusal})
		return
	}

	// target_owned resets whenever the destination changes. Ownership is a claim about
	// one specific table name — carrying it across a rename would let the next run
	// issue a rename over a table this model never created. Switching to statement mode
	// clears it for the same reason from the other direction: the claim is about a table
	// this saved query is no longer rebuilding, and leaving it set would let a later
	// switch back to table mode inherit a licence to DROP without re-earning it.
	if _, err := database.ExecContext(c.Request.Context(), `
		UPDATE saved_queries
		SET materialization = $3,
		    target_table    = NULLIF($2, ''),
		    target_owned    = (target_table IS NOT DISTINCT FROM NULLIF($2, '')) AND target_owned,
		    layer_id        = CASE WHEN $4 THEN layer_id ELSE $5::uuid END,
		    updated_at      = NOW()
		WHERE id = $1
	`, id, target, mode, keepLayer, layerID); err != nil {
		if isForeignKeyViolation(err) {
			// The layer was deleted between the lookup above and this write.
			c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
				"The layer %q was just deleted. Reload and pick another.", layerName)})
			return
		}
		log.WithError(err).Error("failed to set materialization")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update saved query"})
		return
	}

	resp := gin.H{"materialization": mode, "target_table": target}
	if !keepLayer {
		resp["layer"] = layerName
	}
	c.JSON(http.StatusOK, resp)
}

// ============================================================================
// Manual run
// ============================================================================

// allowLongModelRun lifts this request's HTTP write deadline for the length of a rebuild.
//
// cmd/server/main.go sets WriteTimeout: 300s on the shared http.Server. That is a sane
// bound for the API surface -- it is sized to cover the chat path's CPU inference --
// and still six times too short for this one route, where modelRunTimeout budgets 30
// minutes. The deadline sits on the connection rather
// than the handler, so when it expires mid-rebuild the handler keeps running to
// completion while the response can no longer be written: the caller gets a bare EOF.
//
// For the scheduled path that is worse than a slow request. The Temporal activity is
// already patient (32m client timeout, 35m StartToCloseTimeout) so it reads the EOF as a
// failed attempt and retries — MaximumAttempts: 3 — and each retry starts ANOTHER
// rebuild of the same target while the first is still running inside the gateway. A
// long-but-healthy model becomes concurrent writers on one table, reported as a failure.
// (acquireModelRunLock now refuses the overlap, but the run is still falsely failed.)
//
// Raising the deadline on this route is the narrow fix; lowering the run budget or
// raising WriteTimeout globally would both be worse trades.
//
// Best effort by design: if the deadline cannot be lifted, the run should still be
// attempted. The failure mode is the status quo, not a new one.
func allowLongModelRun(c *gin.Context) {
	// A minute past the work budget, leaving room to serialize the response after the
	// last statement returns.
	deadline := time.Now().Add(modelRunTimeout + time.Minute)
	if err := http.NewResponseController(c.Writer).SetWriteDeadline(deadline); err != nil {
		log.WithError(err).Warn("model run: could not extend the HTTP write deadline; a rebuild over 2 minutes may be unable to report its result")
	}
}

// RunSavedQueryModel materializes a model once, now, as the calling user.
// POST /api/v1/explorer/saved/:id/run
func RunSavedQueryModel(c *gin.Context) {
	allowLongModelRun(c)

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	if !requireVisibleSavedQuery(c, id, modelRunMinRole) {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}

	res, err := runManualModel(c.Request.Context(), database, id, userID)
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("model run could not be attempted")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to run model"})
		return
	}

	switch res.Status {
	case "succeeded":
		c.JSON(http.StatusOK, res)
	case "skipped":
		// A refusal is the caller's problem to fix (wrong class, missing target,
		// unsupported connector), not a server fault.
		c.JSON(http.StatusBadRequest, res)
	default:
		// The statement reached the engine and the engine rejected it. 422 rather than
		// 500: rsync.ai did its job, the SQL did not.
		c.JSON(http.StatusUnprocessableEntity, res)
	}
}

// runManualModel is one run started by a person or an agent rather than a schedule:
// run, record, and continue the chain. Both manual doors — the request that waits for
// the result and the one that does not — go through it, so neither can stop recording
// its run or waking what depends on it.
func runManualModel(ctx context.Context, database *sql.DB, id, userID string) (*modelRunResult, error) {
	startedAt := time.Now()
	res, err := runSavedQueryModel(ctx, database, id, userID)
	if err != nil {
		return nil, err
	}
	runID := recordModelRunOutcome(ctx, database, id, res, modelRunAudit{
		Trigger:   triggerManual,
		ActorID:   userID,
		StartedAt: startedAt,
	})

	// A hand-run rebuild wakes what depends on it, exactly as a scheduled one does. Both
	// manual doors go through this one helper so neither can quietly stop doing it.
	fireDownstreamModelsAfterRun(res, id, runID, "", 0)
	return res, nil
}

// StartSavedQueryModelRun is RunSavedQueryModel for a caller that cannot hold a request
// open for the length of a rebuild — the agent gateway's run_model, whose MCP client
// times a tool call out long before a model's 30-minute budget runs out. It answers 202
// once the run has been handed to the background, and the outcome lands in the run
// history exactly as a waited-for run's does, so the caller reads it there.
//
// Everything that can be refused is refused here, synchronously, with the same gates
// and the same dry-run CreateSavedQuerySchedule does: a caller told "started" should
// find a run in the history, not discover that the model never could have run. What it
// cannot promise is the one transient refusal — another run of the model in flight —
// which the background run records as a skip.
//
// Not routed: the UI waits for its runs, and the only caller is in-process.
//
// A gateway that stops mid-run loses the run's outcome, as it would for a waiting
// caller whose connection the restart cut; the target table is never left half-built
// (the swap is atomic), and the next run sweeps the staging table.
func StartSavedQueryModelRun(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	if !requireVisibleSavedQuery(c, id, modelRunMinRole) {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}

	m, err := loadSavedQueryModel(c.Request.Context(), database, id)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "this saved query's connection is no longer available in this workspace"})
		return
	}
	if m.Materialization == matNone {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "this saved query is not a model; choose what a run should do before running it",
		})
		return
	}
	if _, refusal, err := authorizeModelRun(c.Request.Context(), database, m, userID); err != nil {
		log.WithError(err).Error("failed to authorize a background model run")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify this model can run; try again"})
		return
	} else if refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": refusal})
		return
	}

	launchModelRun(database, id, userID)
	c.JSON(http.StatusAccepted, gin.H{"status": "started", "model_id": id})
}

// launchModelRun is a seam so the handler can be tested without a goroutine racing the
// test's mock.
var launchModelRun = func(database *sql.DB, id, userID string) {
	go runModelInBackground(database, id, userID)
}

func runModelInBackground(database *sql.DB, id, userID string) {
	// Not the request's context: the request is over by the time this matters. Its own
	// budget instead, a minute past the run's.
	ctx, cancel := context.WithTimeout(context.Background(), modelRunTimeout+time.Minute)
	defer cancel()
	if _, err := runManualModel(ctx, database, id, userID); err != nil {
		log.WithError(err).WithField("model_id", id).Error("background model run could not be attempted")
	}
}

// ============================================================================
// Schedule CRUD
// ============================================================================

// CreateSavedQuerySchedule attaches a schedule to a model.
// POST /api/v1/explorer/saved/:id/schedule
func CreateSavedQuerySchedule(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	if !requireVisibleSavedQuery(c, id, modelRunMinRole) {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}

	var req createSavedQueryScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := validateModelScheduleSpec(req.ScheduleType, req.ScheduleSpec, req.Upstreams); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	policy, err := normalizeUpstreamPolicy(req.ScheduleType, req.UpstreamPolicy)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.ScheduleSpec.Timezone == "" {
		req.ScheduleSpec.Timezone = "UTC"
	}

	// An event trigger registers nothing with Temporal, so a scheduling-service outage
	// is not its problem. Checked after authorization either way, so an unauthorized
	// caller never learns that service's state (same ordering as CreatePipelineSchedule).
	eventDriven := req.ScheduleType == scheduleAfterUpstream
	if !eventDriven && temporalClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scheduling service not available"})
		return
	}

	if eventDriven && !authorizeUpstreams(c, req.Upstreams) {
		return
	}

	m, err := loadSavedQueryModel(c.Request.Context(), database, id)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "this saved query's connection is no longer available in this workspace"})
		return
	}
	if m.Materialization == matNone {
		// A plain saved query has no effect to repeat: nothing to write, and no consumer
		// subscribes to a result-delivery topic today. Scheduling one would build a thing
		// that quietly does nothing.
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "choose what a run of this query should do — write its results to a table, " +
				"or run it as a statement — before scheduling it",
		})
		return
	}
	// Dry-run the fire path's authorization against the identity that will run it.
	if _, refusal, err := authorizeModelRun(c.Request.Context(), database, m, userID); err != nil {
		log.WithError(err).Error("failed to authorize schedule creation")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify this query can be scheduled; try again"})
		return
	} else if refusal != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": refusal})
		return
	}
	if eventDriven && !checkUpstreamCycle(c, database, id, req.Upstreams) {
		return
	}
	if eventDriven && !refuseUnbuildableUpstreams(c, database, req.Upstreams) {
		return
	}

	scheduleID := uuid.New().String()
	specJSON, err := json.Marshal(req.ScheduleSpec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode schedule spec"})
		return
	}

	// NULL for an event trigger, which registers nothing with Temporal; migration 100
	// enforces that pairing in both directions, so this is not the last line of defence,
	// it is the readable one.
	//
	// The Temporal schedule ID is the same string as schedule_id, but the columns are
	// UUID and TEXT — one placeholder in both slots leaves Postgres with two conflicting
	// type deductions for it (42P08). Same value, separate placeholders.
	var temporalID any
	if !eventDriven {
		temporalID = scheduleID
	}
	// The row goes in behind a lock on the saved query it schedules, and the lock and
	// the INSERT are one transaction so the lock is still held when the row lands.
	//
	// Without this, becoming scheduled is invisible to the approval gate in
	// UpdateSavedQuery. That handler locks the saved_queries row and then asks whether
	// a schedule exists; an INSERT that commits between those two answers "no" for a
	// query that IS scheduled by the time the edit lands, and the SQL change goes in
	// ungated — on precisely the kind of query the gate exists for. Holding a lock on
	// saved_queries here makes the two mutually exclusive in both directions: whoever
	// gets the row first, the other one sees the finished state.
	//
	// FOR SHARE rather than FOR UPDATE because scheduling changes nothing about the
	// saved query itself. It only has to exclude an edit in flight, and two schedules
	// for one query are already impossible — the partial unique index below is what
	// enforces that, not this lock.
	//
	// The transaction stops at the INSERT. Registering with Temporal is a call to
	// another service, and holding a row lock across it would let a slow Temporal
	// block every edit to that query for as long as it took to answer.
	schedTx, err := database.BeginTx(c.Request.Context(), nil)
	if err != nil {
		log.WithError(err).Error("failed to begin saved query schedule insert")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
		return
	}
	defer func() { _ = schedTx.Rollback() }()

	var lockedQueryID string
	if err := schedTx.QueryRowContext(c.Request.Context(), `
		SELECT id FROM saved_queries WHERE id = $1 FOR SHARE
	`, id).Scan(&lockedQueryID); err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		log.WithError(err).Error("failed to lock saved query for schedule insert")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
		return
	}

	if _, err := schedTx.ExecContext(c.Request.Context(), `
		INSERT INTO saved_query_schedules
			(schedule_id, saved_query_id, schedule_type, schedule_spec, temporal_schedule_id, run_as_user_id, created_by, upstream_policy)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
	`, scheduleID, id, req.ScheduleType, specJSON, temporalID, userID, policy); err != nil {
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, gin.H{"error": "this query already has a schedule"})
			return
		}
		log.WithError(err).Error("failed to insert saved query schedule")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
		return
	}

	// In the same transaction as the row they hang off. A commit that landed the parent
	// without them would leave an after_upstream schedule nothing can ever wake, and
	// since 100 moved the upstream into a child table no CHECK can catch that shape.
	if eventDriven {
		if err := insertScheduleUpstreams(c.Request.Context(), schedTx, scheduleID, req.Upstreams); err != nil {
			log.WithError(err).Error("failed to insert saved query schedule upstreams")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
			return
		}
	}

	if err := schedTx.Commit(); err != nil {
		log.WithError(err).Error("failed to commit saved query schedule insert")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
		return
	}

	// Nothing to register for an event trigger: the row IS the subscription, and
	// FireModelsAfterPipeline reads it on every completion event.
	if !eventDriven {
		if err := createTemporalModelSchedule(c.Request.Context(), scheduleID, id, req.ScheduleType, req.ScheduleSpec); err != nil {
			// Roll the row back so a retry re-creates cleanly rather than colliding with
			// the partial one (mirrors attachScheduleForChat's cleanup).
			_, _ = database.ExecContext(c.Request.Context(),
				`DELETE FROM saved_query_schedules WHERE schedule_id = $1`, scheduleID)
			log.WithError(err).Error("failed to create Temporal schedule for model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create schedule"})
			return
		}
	}

	s, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return
	}
	c.JSON(http.StatusCreated, s)
}

// GetSavedQuerySchedule returns the model's schedule, if any.
// GET /api/v1/explorer/saved/:id/schedule
func GetSavedQuerySchedule(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	// Reading a schedule is a read: viewers may see that a model is scheduled.
	if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	s, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, s)
}

// ScheduledQuerySummary is one row of the workspace-wide Scheduled Queries view: a
// schedule joined to the model it drives. Flattened rather than nested because the
// view is a table — a caller that had to walk summary.query.connection.name to render
// a cell would be paying for a shape nothing else needs.
type ScheduledQuerySummary struct {
	ScheduleID   string `json:"schedule_id"`
	SavedQueryID string `json:"saved_query_id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`

	ConnectionID   string `json:"connection_id"`
	ConnectionName string `json:"connection_name,omitempty"`
	ConnectorType  string `json:"connector_type,omitempty"`

	ScheduleType string       `json:"schedule_type"`
	ScheduleSpec ScheduleSpec `json:"schedule_spec"`
	Status       string       `json:"status"`

	// Set only for schedule_type=after_upstream. Names are joined rather than left to the
	// client to resolve: this row already knows what it is waiting for, and a list that
	// renders "After an upstream runs" without saying WHICH is a cadence column that has
	// stopped answering the question the column exists to answer.
	Upstreams []scheduleUpstream `json:"upstreams,omitempty"`
	// UpstreamPolicy is what those upstreams mean together: "any" rebuilds on each one,
	// "all" waits for every one. Without it a client can only guess, and the guess it
	// falls back to is "any" — so a fan-in that waits is described as one that does not.
	UpstreamPolicy string `json:"upstream_policy"`

	Materialization string `json:"materialization"`
	TargetTable     string `json:"target_table,omitempty"`
	// Layer is the name of the model's workspace layer, or "" (workspace_layers.go).
	Layer string `json:"layer,omitempty"`

	// StatementClass is the class stored for the query's SQL. Advisory, exactly as it is
	// everywhere else — the fire path re-classifies the live SQL — but the Scheduled
	// Queries view offers an edit affordance, and a mode picker with no idea whether the
	// SQL reads or writes can only offer both and let the server refuse one.
	StatementClass string `json:"statement_class,omitempty"`

	// SupportsMaterialization is resolved from ConnectorType through the Explorer
	// capability table, so this view's edit affordance can disable the two modes that
	// this engine's rebuild path would refuse rather than offering all three and letting
	// the PUT decide. Same field, same resolver, same reason as on SavedQuery.
	SupportsMaterialization bool `json:"supports_materialization"`

	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastRunStatus string     `json:"last_run_status,omitempty"`
	LastRunError  string     `json:"last_run_error,omitempty"`

	// NextRunAt is derived from the stored spec on every request, never persisted and
	// never read back from Temporal. Persisting it would create a second clock that
	// drifts the moment a cadence changes, and asking Temporal would put a network
	// round trip per row into a list render. A paused schedule has no next run, so
	// this is nil rather than a time that will not happen.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`

	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	PausedAt         *time.Time `json:"paused_at,omitempty"`
	PausedReason     string     `json:"paused_reason,omitempty"`
	AutoPausedAt     *time.Time `json:"auto_paused_at,omitempty"`
	AutoPausedReason string     `json:"auto_paused_reason,omitempty"`
}

// ListSavedQuerySchedules returns every live schedule in the active workspace.
// GET /api/v1/explorer/schedules
//
// This is a COLLECTION endpoint, so it gates with requireWorkspaceRole plus
// resolveActiveWorkspace and binds workspace_id in the SQL itself. requireResourceRole
// is not usable here and the difference is not stylistic: that helper proves membership
// for ONE row id supplied by the caller, and the entire purpose of this route is to
// serve a caller who does not know the ids yet. Every existing schedule route is
// per-query, which is precisely why a user with a schedule had nowhere to go to see it.
func ListSavedQuerySchedules(c *gin.Context) {
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}
	if _, ok := requireWorkspaceRole(c, security.WSViewer); !ok {
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

	// The visibility predicate mirrors ListSavedQueries: a private query belonging to
	// another member must not become visible just because it happens to be scheduled.
	// LEFT JOIN on connections so a schedule survives its connection being deleted —
	// that is exactly the broken state an operator needs to be able to see.
	//
	// Upstreams are not joined here at all. A schedule can hold several since migration
	// 100, and joining a one-to-many into this SELECT would multiply every scheduled
	// query by its fan-in — the LIMIT 500 would then be counting upstream rows rather
	// than schedules, and a model with four producers would silently push three other
	// schedules off the page. They come back from one more workspace-scoped query below
	// and are attached by id.
	// ?saved_query_id= narrows the list to one model, for that model's own page. It is a
	// filter on this same visibility-checked query rather than a new per-id route, so a
	// private model of another member reads as absent here exactly as it does in the list.
	args := []any{workspaceID, userID}
	onlyQuery := ""
	if one := c.Query("saved_query_id"); one != "" {
		if _, err := uuid.Parse(one); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved_query_id"})
			return
		}
		args = append(args, one)
		onlyQuery = "AND s.saved_query_id = $3"
	}
	rows, err := database.QueryContext(c.Request.Context(), `
		SELECT s.schedule_id, s.saved_query_id, sq.name, COALESCE(sq.description, ''),
		       sq.connection_id::text, COALESCE(cn.name, ''), COALESCE(cn.connector_type, ''),
		       s.schedule_type, s.schedule_spec, s.status,
		       sq.materialization, COALESCE(sq.target_table, ''), sq.statement_class,
		       sq.last_run_at, COALESCE(sq.last_run_status, ''), COALESCE(sq.last_run_error, ''),
		       s.created_by::text, s.created_at, s.updated_at,
		       s.paused_at, COALESCE(s.paused_reason, ''),
		       s.auto_paused_at, COALESCE(s.auto_paused_reason, ''),
		       s.upstream_policy, COALESCE(wl.name, '')
		FROM saved_query_schedules s
		JOIN saved_queries sq ON sq.id = s.saved_query_id
		LEFT JOIN connections cn ON cn.id = sq.connection_id
		LEFT JOIN workspace_layers wl ON wl.id = sq.layer_id
		WHERE sq.workspace_id = $1
		  AND (sq.visibility = 'workspace' OR sq.created_by = $2)
		  AND s.status != 'deleted'
		  `+onlyQuery+`
		ORDER BY s.updated_at DESC
		LIMIT 500
	`, args...)
	if err != nil {
		log.WithError(err).Error("list saved query schedules")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list schedules"})
		return
	}
	defer rows.Close()

	now := time.Now()
	out := make([]ScheduledQuerySummary, 0)
	for rows.Next() {
		var s ScheduledQuerySummary
		var specJSON []byte
		var lastRunAt, pausedAt, autoPausedAt sql.NullTime
		if err := rows.Scan(&s.ScheduleID, &s.SavedQueryID, &s.Name, &s.Description,
			&s.ConnectionID, &s.ConnectionName, &s.ConnectorType,
			&s.ScheduleType, &specJSON, &s.Status,
			&s.Materialization, &s.TargetTable, &s.StatementClass,
			&lastRunAt, &s.LastRunStatus, &s.LastRunError,
			&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
			&pausedAt, &s.PausedReason,
			&autoPausedAt, &s.AutoPausedReason,
			&s.UpstreamPolicy, &s.Layer); err != nil {
			log.WithError(err).Error("scan saved query schedule")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list schedules"})
			return
		}
		s.LastRunError = storedRunErrorForDisplay(s.LastRunError)
		if err := json.Unmarshal(specJSON, &s.ScheduleSpec); err != nil {
			// An unreadable spec is worth surfacing as a row with no cadence rather than
			// dropping the schedule from the list, which would make it unfixable via UI.
			log.WithError(err).WithField("schedule_id", s.ScheduleID).Warn("unreadable schedule spec")
		}
		s.SupportsMaterialization = ResolveExplorerCapability(s.ConnectorType).SupportsMaterialization
		if lastRunAt.Valid {
			s.LastRunAt = &lastRunAt.Time
		}
		if pausedAt.Valid {
			s.PausedAt = &pausedAt.Time
		}
		if autoPausedAt.Valid {
			s.AutoPausedAt = &autoPausedAt.Time
		}
		if s.Status == "active" {
			s.NextRunAt = nextScheduleRun(s.ScheduleType, s.ScheduleSpec, now)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("iterate saved query schedules")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list schedules"})
		return
	}

	upstreams, err := listWorkspaceScheduleUpstreams(c.Request.Context(), database, workspaceID, userID)
	if err != nil {
		log.WithError(err).Error("list saved query schedule upstreams")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list schedules"})
		return
	}
	for i := range out {
		out[i].Upstreams = upstreams[out[i].ScheduleID]
	}

	// Soonest first, paused last. Sorted here rather than in SQL because next_run_at is
	// computed in Go; ORDER BY updated_at above only fixes the tie-break so the list is
	// stable between refreshes.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].NextRunAt, out[j].NextRunAt
		if a == nil || b == nil {
			return a != nil && b == nil
		}
		return a.Before(*b)
	})

	c.JSON(http.StatusOK, gin.H{"schedules": out, "count": len(out)})
}

// SavedQueryRun is one recorded attempt from saved_query_runs (migration 086).
type SavedQueryRun struct {
	RunID           string    `json:"run_id"`
	SavedQueryID    string    `json:"saved_query_id"`
	ScheduleID      string    `json:"schedule_id,omitempty"`
	TriggerSource   string    `json:"trigger_source"`
	Status          string    `json:"status"`
	StatementClass  string    `json:"statement_class,omitempty"`
	TargetTable     string    `json:"target_table,omitempty"`
	RowsAffected    *int64    `json:"rows_affected,omitempty"`
	Error           string    `json:"error,omitempty"`
	AutoPauseReason string    `json:"auto_pause_reason,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	DurationMS      int64     `json:"duration_ms"`
	RanAsUserID     string    `json:"ran_as_user_id,omitempty"`

	// Provenance of a triggered run, and why a skipped one did not rebuild (migration
	// 104). UpstreamName is empty when the upstream is gone, or is a private model the
	// caller cannot see — the kind still says a model woke it.
	UpstreamKind      string `json:"upstream_kind,omitempty"`
	UpstreamID        string `json:"upstream_id,omitempty"`
	UpstreamName      string `json:"upstream_name,omitempty"`
	UpstreamRunID     string `json:"upstream_run_id,omitempty"`
	OriginExecutionID string `json:"origin_execution_id,omitempty"`
	TriggerDepth      int    `json:"trigger_depth,omitempty"`
	CoalescedCount    int    `json:"coalesced_count,omitempty"`
	SkipReason        string `json:"skip_reason,omitempty"`
}

// ListSavedQueryRuns returns the recent attempt history for one model, newest first.
// GET /api/v1/explorer/saved/:id/runs
//
// Viewer-level, matching GetSavedQuerySchedule: seeing THAT a scheduled rebuild failed
// is a read, and withholding it from viewers is what makes a silent 3am failure silent.
func ListSavedQueryRuns(c *gin.Context) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}
	page, err := parseRunPage(c.Query("limit"), c.Query("status"), c.Query("before"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !requireVisibleSavedQuery(c, id, security.WSViewer) {
		return
	}
	userID, ok := resolveUserID(c)
	if !ok {
		return
	}

	// The upstream's name is joined rather than stored, so a rename shows the current
	// name. A model upstream is named only when the caller could see it: a private model
	// belongs to its author, and this is the one place its name would otherwise reach
	// another member. Every join also requires the upstream to still be in this model's
	// workspace, so one moved away since does not report its new tenant's name here. A
	// workflow upstream is a refresh_model step's run (migration 140).
	rows, err := database.QueryContext(c.Request.Context(), `
		SELECT r.run_id, r.saved_query_id, COALESCE(r.schedule_id::text, ''), r.trigger_source, r.status,
		       r.statement_class, r.target_table, r.rows_affected, COALESCE(r.error, ''),
		       COALESCE(r.auto_pause_reason, ''), r.started_at, r.finished_at,
		       COALESCE(r.ran_as_user_id::text, ''),
		       COALESCE(r.upstream_kind, ''), COALESCE(r.upstream_id::text, ''),
		       COALESCE(up.name, um.name, uw.name, ''),
		       COALESCE(r.upstream_run_id::text, ''), COALESCE(r.origin_execution_id, ''),
		       COALESCE(r.trigger_depth, 0), COALESCE(r.coalesced_count, 0),
		       COALESCE(r.skip_reason, '')
		FROM saved_query_runs r
		JOIN saved_queries sq ON sq.id = r.saved_query_id
		LEFT JOIN pipelines up ON r.upstream_kind = 'pipeline' AND up.id = r.upstream_id
		     AND up.workspace_id = sq.workspace_id
		LEFT JOIN saved_queries um ON r.upstream_kind = 'model' AND um.id = r.upstream_id
		     AND um.workspace_id = sq.workspace_id
		     AND (um.visibility = 'workspace' OR um.created_by = $2)
		LEFT JOIN workflows uw ON r.upstream_kind = 'workflow' AND uw.id = r.upstream_id
		     AND uw.workspace_id = sq.workspace_id
		WHERE r.saved_query_id = $1
		  AND ($3 = '' OR r.status = $3)
		  AND ($4::timestamptz IS NULL OR (r.finished_at, r.run_id) < ($4::timestamptz, $5::uuid))
		ORDER BY r.finished_at DESC, r.run_id DESC
		LIMIT $6
	`, id, userID, page.status, page.beforeAt, page.beforeRunID, page.limit+1)
	if err != nil {
		log.WithError(err).Error("list saved query runs")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}
	defer rows.Close()

	out := make([]SavedQueryRun, 0)
	for rows.Next() {
		var r SavedQueryRun
		var rowsAffected sql.NullInt64
		if err := rows.Scan(&r.RunID, &r.SavedQueryID, &r.ScheduleID, &r.TriggerSource, &r.Status,
			&r.StatementClass, &r.TargetTable, &rowsAffected, &r.Error,
			&r.AutoPauseReason, &r.StartedAt, &r.FinishedAt, &r.RanAsUserID,
			&r.UpstreamKind, &r.UpstreamID, &r.UpstreamName, &r.UpstreamRunID, &r.OriginExecutionID,
			&r.TriggerDepth, &r.CoalescedCount, &r.SkipReason); err != nil {
			log.WithError(err).Error("scan saved query run")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
			return
		}
		if rowsAffected.Valid {
			v := rowsAffected.Int64
			r.RowsAffected = &v
		}
		r.Error = storedRunErrorForDisplay(r.Error)
		r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.WithError(err).Error("iterate saved query runs")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list runs"})
		return
	}

	// One row past the page was asked for only to learn whether another page exists.
	resp := gin.H{}
	if len(out) > page.limit {
		out = out[:page.limit]
		last := out[len(out)-1]
		resp["next_cursor"] = encodeRunCursor(last.FinishedAt, last.RunID)
	}
	resp["runs"] = out
	resp["count"] = len(out)
	c.JSON(http.StatusOK, resp)
}

const (
	defaultRunPageSize = 50
	maxRunPageSize     = 200
)

// runPage is one keyset page of a model's run history. Keyset rather than OFFSET: runs
// keep landing while someone pages back, and an offset would then repeat or skip rows.
type runPage struct {
	limit       int
	status      string
	beforeAt    any // nil, or the cursor's finished_at
	beforeRunID any // nil, or the cursor's run_id
}

func parseRunPage(limit, status, before string) (runPage, error) {
	p := runPage{limit: defaultRunPageSize}
	if limit != "" {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > maxRunPageSize {
			return p, fmt.Errorf("limit must be between 1 and %d", maxRunPageSize)
		}
		p.limit = n
	}
	switch status {
	case "", "succeeded", "failed", "skipped":
		p.status = status
	default:
		return p, fmt.Errorf("status must be succeeded, failed or skipped")
	}
	if before != "" {
		at, runID, err := decodeRunCursor(before)
		if err != nil {
			return p, fmt.Errorf("invalid before cursor")
		}
		p.beforeAt, p.beforeRunID = at, runID
	}
	return p, nil
}

// The cursor is the last row's (finished_at, run_id). run_id breaks ties between runs
// finishing in the same microsecond, which a fan-out burst does produce.
func encodeRunCursor(at time.Time, runID string) string {
	return at.UTC().Format(time.RFC3339Nano) + "_" + runID
}

func decodeRunCursor(cursor string) (time.Time, string, error) {
	i := strings.LastIndex(cursor, "_")
	if i < 0 {
		return time.Time{}, "", fmt.Errorf("no separator")
	}
	at, err := time.Parse(time.RFC3339Nano, cursor[:i])
	if err != nil {
		return time.Time{}, "", err
	}
	if _, err := uuid.Parse(cursor[i+1:]); err != nil {
		return time.Time{}, "", err
	}
	return at, cursor[i+1:], nil
}

// PauseSavedQuerySchedule stops an operator's schedule firing.
// POST /api/v1/explorer/saved/:id/schedule/pause
func PauseSavedQuerySchedule(c *gin.Context) {
	database, id, s, ok := mutableSavedQuerySchedule(c)
	if !ok {
		return
	}
	var req pauseSavedQueryScheduleRequest
	_ = c.ShouldBindJSON(&req) // body is optional

	// The row is written FIRST, because the row is the authoritative kill switch:
	// RunSavedQueryModelInternal re-reads status on every fire and refuses anything that
	// is not 'active'. Calling Temporal first meant a Temporal blip returned 500 without
	// ever writing the row — telling the operator their pause had failed while the
	// schedule went right on rebuilding the table. autoPauseModelSchedule below already
	// had this ordering right; this is the same reasoning applied to a human pause.
	if _, err := database.ExecContext(c.Request.Context(), `
		UPDATE saved_query_schedules
		SET status = 'paused', paused_at = NOW(), paused_reason = NULLIF($2, '')
		WHERE schedule_id = $1
	`, s.ScheduleID, req.Reason); err != nil {
		log.WithError(err).Error("failed to record model schedule pause")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to pause schedule"})
		return
	}
	// An event trigger has nothing registered to pause; the row above is the whole
	// mechanism, and FireModelsAfterPipeline reads status on every completion event.
	if !s.EventDriven() {
		if err := pauseTemporalSchedule(c.Request.Context(), s.TemporalScheduleID, req.Reason); err != nil {
			// Best effort for the same reason it is best effort in autoPauseModelSchedule:
			// the row above already stops the fire path, so a Temporal that keeps waking this
			// schedule only wakes it into a refusal. A 500 here would be the lie — the
			// schedule is paused.
			log.WithError(err).WithField("schedule_id", s.ScheduleID).
				Warn("failed to pause Temporal schedule for model; the status check will keep refusing runs")
		}
	}

	out, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, out)
}

// ResumeSavedQuerySchedule restarts a paused schedule.
// POST /api/v1/explorer/saved/:id/schedule/resume
//
// This is the one place an auto-pause is allowed to end, and it only ends if the
// condition that caused it is actually gone. Re-running the fire path's own check
// here is what stops a resume click from clearing a security stop.
func ResumeSavedQuerySchedule(c *gin.Context) {
	database, id, s, ok := mutableSavedQuerySchedule(c)
	if !ok {
		return
	}

	m, err := loadSavedQueryModel(c.Request.Context(), database, id)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "this saved query's connection is no longer available in this workspace"})
		return
	}
	if m.Materialization == matNone {
		c.JSON(http.StatusConflict, gin.H{"error": "choose what a run of this query should do — write its results " +
			"to a table, or run it as a statement — before resuming this schedule"})
		return
	}
	// Checked against run_as_user_id, NOT the caller. The question is whether the
	// unattended fire would succeed, and an admin clicking Resume does not lend their
	// authority to a schedule that runs as someone else.
	if _, refusal, err := authorizeModelRun(c.Request.Context(), database, m, s.RunAsUserID); err != nil {
		// Refuse to resume on an unverified authorization — but say so honestly, rather
		// than reporting the run-as user as demoted.
		log.WithError(err).Error("failed to authorize schedule resume")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify this schedule is safe to resume; try again"})
		return
	} else if refusal != "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "this schedule cannot resume yet: " + refusal,
		})
		return
	}

	// An event trigger whose upstreams were all deleted was paused by that delete, and
	// resuming it would restore an active schedule that nothing can ever wake. Re-read
	// rather than trusting s.Upstreams: the loader swallows a failed upstream read, and an
	// empty list from a database blip must not be reported as "no upstreams".
	if s.EventDriven() {
		ups, err := loadScheduleUpstreams(c.Request.Context(), database, s.ScheduleID)
		if err != nil {
			log.WithError(err).Error("failed to read upstreams for schedule resume")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify this schedule is safe to resume; try again"})
			return
		}
		if len(ups) == 0 {
			c.JSON(http.StatusConflict, gin.H{
				"error": "every upstream this model rebuilt after has been deleted; edit the schedule to choose new upstreams",
			})
			return
		}
	}

	// Temporal first for a clock schedule: the row is what refuses runs, so writing it
	// before Temporal accepted the resume would advertise an active schedule that is
	// still paused upstream. An event trigger has no upstream to ask.
	if !s.EventDriven() {
		if err := resumeTemporalSchedule(c.Request.Context(), s.TemporalScheduleID); err != nil {
			log.WithError(err).Error("failed to resume Temporal schedule for model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resume schedule"})
			return
		}
	}
	if _, err := database.ExecContext(c.Request.Context(), `
		UPDATE saved_query_schedules
		SET status = 'active',
		    paused_at = NULL, paused_reason = NULL,
		    auto_paused_at = NULL, auto_paused_reason = NULL
		WHERE schedule_id = $1
	`, s.ScheduleID); err != nil {
		log.WithError(err).Error("failed to record model schedule resume")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resume schedule"})
		return
	}

	out, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, out)
}

// UpdateSavedQuerySchedule changes the cadence in place.
// PUT /api/v1/explorer/saved/:id/schedule
func UpdateSavedQuerySchedule(c *gin.Context) {
	database, id, s, ok := mutableSavedQuerySchedule(c)
	if !ok {
		return
	}

	var req createSavedQueryScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := validateModelScheduleSpec(req.ScheduleType, req.ScheduleSpec, req.Upstreams); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	policy, err := normalizeUpstreamPolicy(req.ScheduleType, req.UpstreamPolicy)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.ScheduleSpec.Timezone == "" {
		req.ScheduleSpec.Timezone = "UTC"
	}
	specJSON, err := json.Marshal(req.ScheduleSpec)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode schedule spec"})
		return
	}

	wantEvent := req.ScheduleType == scheduleAfterUpstream
	if wantEvent {
		// Re-authorized on every edit, not just at create: the point of the check is
		// that THIS caller may see THESE upstreams right now, and an edit is a new
		// request from a caller whose access may have changed since.
		if !authorizeUpstreams(c, req.Upstreams) {
			return
		}
		// Re-checked on every edit for a second reason as well. The graph moves under a
		// stored edge: A -> B was acyclic when it was written, and stays stored while
		// someone else adds B -> A. Checking only at create would let the second edit
		// close a ring the first one could not have seen.
		if !checkUpstreamCycle(c, database, id, req.Upstreams) {
			return
		}
		if !refuseUnbuildableUpstreams(c, database, req.Upstreams) {
			return
		}
	}
	if !wantEvent && temporalClient == nil {
		// Reachable now that mutableSavedQuerySchedule stopped demanding Temporal for
		// every mutation: this is an event trigger being converted back to a clock.
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scheduling service not available"})
		return
	}

	// Four transitions, and the ordering differs between them because the unsafe
	// partial state differs. The rule each branch follows: never leave a window where
	// something can fire that the operator believes they have changed.
	temporalID := s.TemporalScheduleID
	switch {
	case !s.EventDriven() && !wantEvent:
		// Temporal first: if it fails the stored spec still describes what is actually
		// firing. The reverse order would leave the DB claiming a cadence Temporal never
		// accepted.
		if err := updateTemporalSchedule(c.Request.Context(), s.TemporalScheduleID, id, req.ScheduleType, req.ScheduleSpec); err != nil {
			log.WithError(err).Error("failed to update Temporal schedule for model")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update schedule"})
			return
		}

	case s.EventDriven() && !wantEvent:
		// Register before storing. The reverse order would write a row claiming a cron
		// whose Temporal schedule then failed to create — a schedule that looks active
		// on the page and never fires again.
		if err := createTemporalModelSchedule(c.Request.Context(), s.ScheduleID, id, req.ScheduleType, req.ScheduleSpec); err != nil {
			log.WithError(err).Error("failed to create Temporal schedule while converting model trigger to a cadence")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update schedule"})
			return
		}
		temporalID = s.ScheduleID
	}

	var newTemporalID any
	if !wantEvent {
		newTemporalID = temporalID
	}

	// One cleanup for every way the write below can fail, because the compensating action
	// depends on which conversion this is and not on which statement failed.
	//
	// clock -> event needs none: nothing has been touched in Temporal yet on that branch,
	// so the old cadence is still registered and still firing, which matches the row that
	// is still stored.
	failWrite := func(err error, what string) {
		if s.EventDriven() && !wantEvent {
			// Undo the registration above so a retry does not collide with a Temporal
			// schedule that no row points at (mirrors the create path's cleanup).
			if delErr := deleteTemporalSchedule(c.Request.Context(), s.ScheduleID); delErr != nil {
				log.WithError(delErr).WithField("schedule_id", s.ScheduleID).
					Error("orphaned Temporal schedule: created for a conversion whose row write then failed")
			}
		}
		log.WithError(err).Error(what)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update schedule"})
	}

	// A transaction, which the single-column version of this did not need. The row and
	// its upstream set are now two statements, and a commit that landed one without the
	// other would leave the schedule firing on the previous edit's producers while the
	// page shows this edit's.
	editTx, err := database.BeginTx(c.Request.Context(), nil)
	if err != nil {
		failWrite(err, "failed to begin model schedule update")
		return
	}
	defer func() { _ = editTx.Rollback() }()

	if _, err := editTx.ExecContext(c.Request.Context(), `
		UPDATE saved_query_schedules
		SET schedule_type = $2, schedule_spec = $3, temporal_schedule_id = $4, upstream_policy = $5
		WHERE schedule_id = $1
	`, s.ScheduleID, req.ScheduleType, specJSON, newTemporalID, policy); err != nil {
		failWrite(err, "failed to persist model schedule update")
		return
	}

	// Replaced wholesale rather than diffed. The request carries the complete set the
	// operator was looking at, so a diff would be reconstructing an answer the caller
	// already sent. The unconditional DELETE is also what clears the set when an event
	// trigger is converted back to a cadence — the rows would otherwise survive their
	// schedule_type and start firing again the moment it was converted back.
	if _, err := editTx.ExecContext(c.Request.Context(),
		`DELETE FROM saved_query_schedule_upstreams WHERE schedule_id = $1`, s.ScheduleID); err != nil {
		failWrite(err, "failed to clear the previous upstreams of a model schedule")
		return
	}
	if wantEvent {
		if err := insertScheduleUpstreams(c.Request.Context(), editTx, s.ScheduleID, req.Upstreams); err != nil {
			failWrite(err, "failed to store the upstreams of a model schedule")
			return
		}
	}
	if err := editTx.Commit(); err != nil {
		failWrite(err, "failed to commit model schedule update")
		return
	}

	// Deregistered only after the row says event-driven. Between those two statements
	// the old cadence can still tick, and that tick is harmless: the internal endpoint
	// refuses to fire any row whose schedule_type is after_upstream.
	if !s.EventDriven() && wantEvent {
		if err := deleteTemporalSchedule(c.Request.Context(), s.TemporalScheduleID); err != nil {
			log.WithError(err).WithField("schedule_id", s.ScheduleID).
				Warn("failed to remove the Temporal schedule after converting to an event trigger; its ticks are refused by the type check")
		}
	}

	out, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, out)
}

// DeleteSavedQuerySchedule removes the schedule. The model and its table remain.
// DELETE /api/v1/explorer/saved/:id/schedule
func DeleteSavedQuerySchedule(c *gin.Context) {
	database, _, s, ok := mutableSavedQuerySchedule(c)
	if !ok {
		return
	}

	if !s.EventDriven() {
		if err := deleteTemporalSchedule(c.Request.Context(), s.TemporalScheduleID); err != nil {
			// Logged, not fatal: a Temporal schedule with no row behind it fires into the
			// internal endpoint, finds no schedule, and returns without executing. Refusing
			// to delete the row here would instead leave the user unable to reschedule.
			log.WithError(err).Warn("failed to delete Temporal schedule for model; marking the row deleted anyway")
		}
	}
	if _, err := database.ExecContext(c.Request.Context(), `
		UPDATE saved_query_schedules SET status = 'deleted' WHERE schedule_id = $1
	`, s.ScheduleID); err != nil {
		log.WithError(err).Error("failed to mark model schedule deleted")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete schedule"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// ============================================================================
// Internal fire endpoint
// ============================================================================

// RunSavedQueryModelInternal is what a Temporal schedule tick calls.
// POST /api/v1/internal/explorer/models/:id/run
//
// There is no session here — the caller is a service. The identity comes from the
// schedule's run_as_user_id and is re-resolved against workspace_members on every
// single fire, so a demotion or an offboarding takes effect at the next tick without
// anyone remembering to clean up schedules.
//
// A refusal auto-pauses. The reason it pauses rather than just failing is that every
// refusal this path produces is stable: the user's role will not un-demote itself, and
// the SQL will not re-classify itself. Retrying on a cron would be a loop that never
// terminates and never tells anyone.
func RunSavedQueryModelInternal(c *gin.Context) {
	allowLongModelRun(c)

	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return
	}

	var req internalModelRunRequest
	_ = c.ShouldBindJSON(&req)

	// A workflow's refresh_model step comes through the model's own refresh workflow,
	// whose commands step 5 left unchanged, so it carries the event door's trigger. It
	// has no schedule: the workflow run is its authority (workflow_step_models.go).
	if req.UpstreamKind == upstreamKindWorkflow {
		runWorkflowStepModel(c, database, id, req)
		return
	}

	if req.Trigger != "" && req.Trigger != scheduleAfterUpstream {
		// Refuse rather than fall through to the clock door. An unrecognised trigger is a
		// version skew between the adapter and this gateway, and quietly serving it from
		// the wrong door would record the run under the wrong trigger forever.
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown trigger"})
		return
	}
	eventPath := req.Trigger == scheduleAfterUpstream
	query, trigger := modelRunScheduleLookup(eventPath)

	// The schedule row is the authority, and it is matched on BOTH ids. A tick that
	// names a schedule belonging to a different query executes nothing.
	var scheduleID, runAsUserID, status string
	var temporalID sql.NullString
	err := database.QueryRowContext(c.Request.Context(), query,
		id, req.ScheduleID, scheduleAfterUpstream).
		Scan(&scheduleID, &temporalID, &runAsUserID, &status)
	if err == sql.ErrNoRows {
		// The schedule was deleted, or converted to the other kind, while something kept
		// firing the old one. Report success so Temporal stops retrying an activity that
		// can never succeed.
		reason := "no clock schedule for this model"
		if eventPath {
			reason = "no event trigger for this model"
		}
		c.JSON(http.StatusOK, gin.H{"status": "skipped", "reason": reason})
		return
	}
	if err != nil {
		log.WithError(err).Error("internal model run: failed to load schedule")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load schedule"})
		return
	}
	if status != "active" {
		c.JSON(http.StatusOK, gin.H{"status": "skipped", "reason": "schedule is " + status})
		return
	}

	var prov runProvenance
	if eventPath {
		prov = provenanceFor(modelRefreshSource{
			Kind:        req.UpstreamKind,
			ID:          req.UpstreamID,
			RunID:       req.UpstreamRunID,
			ExecutionID: req.ExecutionID,
			Depth:       req.Depth,
		}, req.Coalesced)

		// A fan-in on policy 'all' does not rebuild until every sibling is fresh. A lookup
		// error is a 500 so Temporal retries it: guessing either way would be wrong on a
		// real fan-in.
		//
		// The firing upstream is excluded by id alone, which the query matches against
		// either column, so an unknown kind (an adapter older or newer than this gateway)
		// costs the provenance but not the exclusion; without it the upstream that just
		// finished would count as stale against itself and 'all' would never fire. A
		// malformed id is dropped instead, or it would fail the uuid cast on every retry.
		firingID := ""
		if _, perr := uuid.Parse(req.UpstreamID); perr == nil {
			firingID = req.UpstreamID
		}
		if req.UpstreamID != "" && prov.sanitized().UpstreamID == "" {
			log.WithFields(log.Fields{"model_id": id, "upstream_kind": req.UpstreamKind, "upstream_id": req.UpstreamID}).
				Warn("internal model run: upstream kind or id not recognised, run history will not record what woke it")
		}
		waiting, err := unsatisfiedUpstreams(c.Request.Context(), database, scheduleID, firingID)
		if err != nil {
			log.WithError(err).WithField("model_id", id).Error("internal model run: failed to check the fan-in policy")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check upstream policy"})
			return
		}
		if len(waiting) > 0 {
			msg := waitingOnMessage(waiting)
			recordModelRunSkip(c.Request.Context(), database, id, scheduleID, runAsUserID,
				skipWaitingOnUpstreams, msg, prov)
			c.JSON(http.StatusOK, gin.H{
				"status":      "skipped",
				"skip_reason": skipWaitingOnUpstreams,
				"reason":      msg,
				"waiting_on":  len(waiting),
			})
			return
		}
	}

	startedAt := time.Now()
	res, err := runSavedQueryModel(c.Request.Context(), database, id, runAsUserID)
	if err != nil {
		log.WithError(err).WithField("model_id", id).Error("internal model run could not be attempted")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to run model"})
		return
	}
	runID := recordModelRunOutcome(c.Request.Context(), database, id, res, modelRunAudit{
		Trigger:    trigger,
		ScheduleID: scheduleID,
		ActorID:    runAsUserID,
		StartedAt:  startedAt,
		Provenance: prov,
	})

	if res.AutoPauseReason != "" {
		// Naturally empty for an event trigger, which has nothing registered to pause, so
		// the status write is the whole pause.
		autoPauseModelSchedule(c.Request.Context(), database, id, scheduleID, temporalID.String, res.AutoPauseReason)
	}

	// 200 even for a failed run: the activity did its job, and a non-2xx would make
	// Temporal retry a statement the engine already rejected on its merits.
	answerInternalModelRun(c, req, res, id, runID)
}

// modelRunScheduleLookup builds the schedule lookup for one of the internal run
// endpoint's two doors, and returns the trigger a run through that door is recorded
// under.
//
// The doors are exact complements over schedule_type, and they are generated from one
// operator instead of written out twice because two hand-written polarities drift —
// and both directions of drift are silent. An overlap means one pipeline completion
// rebuilds the model twice, once through each door, with two successful-looking rows in
// the history. A gap means a schedule that never fires and answers "skipped" forever.
//
// The exclusion is also what makes converting a cadence into an event trigger safe.
// Deregistering the old Temporal schedule is best effort — it can fail, and it is not
// even attempted until after the row is rewritten — so a stale registration may keep
// ticking against a row that is now event-driven. The clock door makes every such tick
// inert instead of a second, invisible trigger firing the model on the old cadence.
//
// schedule_type is BOUND as $3 and never interpolated; the operator is the only thing
// this function chooses.
func modelRunScheduleLookup(eventPath bool) (string, modelRunTrigger) {
	op, joins, tenancy := "!=", "", ""
	trigger := triggerScheduled
	if eventPath {
		op = "="
		trigger = triggerTriggered
		// The same tenancy check the fire path makes when it looks the models up, made
		// again when the rebuild actually runs. It belongs in both places: a signal can wait
		// through a task queue and up to three activity attempts between the two, so the
		// workspace an upstream was in when it completed is not necessarily the one it is
		// in when the model rebuilds.
		//
		// Stated as "no upstream is outside this model's workspace" rather than as a check
		// on the one that fired, because the signal does not name which one that was and a
		// fan-in schedule has several. The strict reading is the safe direction: a set that
		// has been split across workspaces stops rebuilding, which an operator sees, rather
		// than rebuilding on another tenant's data, which they do not. Neither write path
		// can produce that state — it takes a pipeline being moved after the fact, which is
		// exactly what this check has always been for.
		joins = "\n\t\tJOIN saved_queries sq ON sq.id = s.saved_query_id"
		tenancy = "\n\t\t  AND NOT EXISTS (" +
			"\n\t\t      SELECT 1 FROM saved_query_schedule_upstreams u" +
			"\n\t\t      LEFT JOIN pipelines up ON up.id = u.upstream_pipeline_id" +
			"\n\t\t      LEFT JOIN saved_queries um ON um.id = u.upstream_saved_query_id" +
			"\n\t\t      WHERE u.schedule_id = s.schedule_id" +
			"\n\t\t        AND COALESCE(up.workspace_id, um.workspace_id) IS DISTINCT FROM sq.workspace_id)"
	}
	// status is filtered only for 'deleted' here. A paused schedule is loaded and then
	// refused by the caller's status check, which reports which state stopped it.
	return fmt.Sprintf(`
		SELECT s.schedule_id, s.temporal_schedule_id, s.run_as_user_id::text, s.status
		FROM saved_query_schedules s%s
		WHERE s.saved_query_id = $1 AND s.schedule_id = $2
		  AND s.status != 'deleted' AND s.schedule_type %s $3%s
	`, joins, op, tenancy), trigger
}

// The Temporal half of the event trigger. backend-temporal-adapter is a separate Go
// module, so there is no shared type to import and these literals are the wire
// contract; they are named rather than inlined so a grep finds both ends of it.
const (
	upstreamFanOutWorkflowType = "UpstreamFanOutWorkflow"
	upstreamFanOutTaskQueue    = "pipeline-workflows"
)

// modelRefreshTarget is one model that something upstream should rebuild.
type modelRefreshTarget struct {
	ScheduleID  string
	ModelID     string
	RunAsUserID string
}

// modelRefreshSource is what just finished, and how deep into a rebuild chain we already
// are. Passed as one value rather than as loose ids because Depth has to travel with the
// other two through the dispatcher and into the signal payload; a bare extra int
// parameter is the kind of thing a later call site forgets to thread through, and the
// only symptom would be a bound that silently stops applying.
type modelRefreshSource struct {
	// Kind is upstreamKindPipeline or upstreamKindModel.
	Kind string
	ID   string
	// ExecutionID identifies the pipeline execution that started the chain. Carried
	// unchanged down every model hop below it; empty for a chain a model started.
	ExecutionID string
	// RunID is the saved_query_runs row of the model run that just finished. Empty for a
	// pipeline source, and for a model run whose history row could not be written.
	RunID string
	Depth int
}

// upstreamFanOut is one completion and everything waiting on it, handed to Temporal as
// a single unit.
//
// One value rather than loose arguments because the three parts are only meaningful
// together: the source carries the depth bound, the targets are what the bound is being
// applied to, and the occurrence is what stops the same completion fanning out twice.
type upstreamFanOut struct {
	// Occurrence names this particular completion of this particular upstream. It is what
	// makes the workflow id unique per completion, and it is the whole reason a redelivered
	// Kafka message does not rebuild everything a second time.
	Occurrence string
	Source     modelRefreshSource
	Targets    []modelRefreshTarget
}

// upstreamFanOutDispatch hands one completion's whole fan-out to Temporal.
type upstreamFanOutDispatch func(ctx context.Context, f upstreamFanOut) error

// newUpstreamFanOutDispatch resolves the dispatcher for this deployment, returning nil
// when there is no Temporal client. Replaced in tests.
var newUpstreamFanOutDispatch = temporalUpstreamFanOutDispatch

// temporalUpstreamFanOutDispatch starts one workflow per completion.
//
// A constructor because getTemporalClient dials when there is no client yet, and a nil
// return is that dial failing. That is a start that did not happen — there is no
// in-process path to fall back to any more (workflows-design.md § 4.2) — so
// startUpstreamFanOut retries it and records what was lost when it gives up.
//
// This used to be N signal-with-starts issued from a detached goroutine, one per model.
// It is one ExecuteWorkflow because that is what makes the fan-out durable: the gateway
// can die the instant this returns and every model still gets signalled, where before a
// restart after the third of nine signals left six models owed a rebuild with no record
// that they were owed one. The signal-with-start itself did not disappear — it moved into
// the adapter's dispatch child, which is the only place it can be while still coalescing
// (SignalModelRefreshActivity, backend-temporal-adapter upstream_fanout_workflow.go).
//
// run_as_user_id is deliberately not sent. The internal run endpoint re-resolves it from
// the schedule row on every call, so putting it in a workflow argument would freeze a
// permission where it would outlive the demotion that revoked it.
func temporalUpstreamFanOutDispatch() upstreamFanOutDispatch {
	tc := getTemporalClient()
	if tc == nil {
		return nil
	}
	return func(ctx context.Context, f upstreamFanOut) error {
		id := upstreamFanOutWorkflowID(f)
		_, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID:        id,
			TaskQueue: upstreamFanOutTaskQueue,
			// The projector delivers a completion at least once, and a redelivery used to
			// mean a second full fan-out. The id already names the completion, so refusing
			// a duplicate of one that succeeded makes the fan-out exactly-once per
			// completion. FAILED_ONLY rather than REJECT_DUPLICATE so a fan-out that could
			// not deliver is still allowed a second attempt under the same name.
			WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
			// No run timeout: the children own their own budget, and a timeout here would
			// only cut short a fan-out that was still delivering.
		}, upstreamFanOutWorkflowType, upstreamFanOutArgs(f))
		return err
	}
}

// upstreamFanOutArgs is the argument UpstreamFanOutWorkflow is started with: here, and in
// the /downstream answer a model run's own workflow starts it from (model_next_hop.go).
//
// map[string]any rather than a struct: the adapter is a separate Go module, so this is
// the wire contract with its UpstreamFanOutInput and there is no shared type to marshal.
// depth is a number and the adapter decodes it into an int.
func upstreamFanOutArgs(f upstreamFanOut) map[string]interface{} {
	targets := make([]map[string]interface{}, 0, len(f.Targets))
	for _, t := range f.Targets {
		targets = append(targets, map[string]interface{}{
			"schedule_id":    t.ScheduleID,
			"saved_query_id": t.ModelID,
		})
	}
	return map[string]interface{}{
		"upstream_kind":   f.Source.Kind,
		"upstream_id":     f.Source.ID,
		"execution_id":    f.Source.ExecutionID,
		"upstream_run_id": f.Source.RunID,
		"depth":           f.Source.Depth,
		"targets":         targets,
	}
}

// upstreamFanOutWorkflowID is the one place the id is spelled. One workflow per
// completion, which is what the occurrence in it means.
func upstreamFanOutWorkflowID(f upstreamFanOut) string {
	return "upstream-fanout:" + f.Source.Kind + ":" + f.Source.ID + ":" + f.Occurrence
}

// fanOutOccurrence names the completion that is fanning out.
//
// A pipeline completion has an execution id, and using it is what makes a redelivered
// Kafka message land on a workflow id that already exists instead of rebuilding
// everything again.
//
// A model-sourced hop has no execution of its own — modelRefreshSource.ExecutionID
// carries the pipeline execution that started the chain, which is the SAME value for
// every hop and for every later rebuild of that model. Reusing it as the occurrence would
// give successive rebuilds of one model the same workflow id, and the second would be
// refused as a duplicate: the chain would stop after one hop and nothing would report it.
// That is why the rule is by KIND and not by "is there an execution id": since the id is
// now threaded down model hops, a presence check would hit exactly that. A model hop is
// named by its own run row, which is unique per rebuild, and gets a fresh id when that row
// could not be written.
func fanOutOccurrence(src modelRefreshSource) string {
	if src.Kind == upstreamKindPipeline && src.ExecutionID != "" {
		return src.ExecutionID
	}
	if src.Kind == upstreamKindModel && src.RunID != "" {
		return src.RunID
	}
	return uuid.NewString()
}

// fanOutRetryBudget is how long a completion keeps trying to hand its fan-out to Temporal
// before the models below it are recorded as trigger_lost. A Temporal restart or rollout
// is seconds; past this it is an outage, and a row saying so beats a goroutine that
// holds a database connection waiting for it. Variables so a test can shrink the clock.
var (
	fanOutRetryBudget     = 30 * time.Second
	fanOutRetryFirstPause = time.Second
)

var (
	// errNoTemporalClient is getTemporalClient returning nil: its dial failed.
	errNoTemporalClient = errors.New("no Temporal client to hand the fan-out to")
	// errFanOutBreakerOpen is a start refused without dialling, inside the window after
	// one failed (G11).
	errFanOutBreakerOpen = errors.New("Temporal failed recently; not dialling it again yet")
)

// startUpstreamFanOut hands one completion's fan-out to Temporal, retrying inside
// fanOutRetryBudget, and returns nil once Temporal has it.
//
// All or nothing: there is one call, so a half-dispatched batch is not a state that can
// exist. There is no in-process path behind it any more (workflows-design.md § 4.2): an
// error here means the models below did not get this trigger, and the caller records
// that rather than dropping it. resolve is called on every attempt because
// getTemporalClient does not cache a failed dial, so the next attempt can reconnect.
func startUpstreamFanOut(ctx context.Context, b *fanOutBreaker, resolve func() upstreamFanOutDispatch, f upstreamFanOut) error {
	fields := log.Fields{"upstream_id": f.Source.ID, "occurrence": f.Occurrence, "models": len(f.Targets)}
	if !b.acquire() {
		return errFanOutBreakerOpen
	}
	ctx, cancel := context.WithTimeout(ctx, fanOutRetryBudget)
	defer cancel()

	var err error
	for pause := fanOutRetryFirstPause; ; pause *= 2 {
		if dispatch := resolve(); dispatch == nil {
			err = errNoTemporalClient
		} else {
			err = dispatch(ctx, f)
		}
		if err == nil || isFanOutAlreadyStarted(err) {
			break
		}
		log.WithError(err).WithFields(fields).Warn("upstream trigger: could not reach Temporal")
		if !waitOrDone(ctx, pause) {
			break
		}
	}
	b.observe(err)

	switch {
	case err == nil:
		log.WithFields(fields).Info("⏩ handed the fan-out to Temporal")
		return nil
	case isFanOutAlreadyStarted(err):
		// This completion has already been fanned out. "Already done", not "did not
		// happen": no trigger_lost row about models that are being rebuilt.
		log.WithFields(fields).Info("upstream trigger: this completion was already fanned out, not repeating it")
		return nil
	}
	return err
}

// waitOrDone waits d, and reports false instead when ctx ends first.
func waitOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// deliverFanOut starts one completion's fan-out and, when that fails, records every
// model below it as skipped with trigger_lost, so a model whose trigger never arrived
// says so instead of reading "nothing happened". Blocks for up to the retry budget; the
// callers run it on its own goroutine.
func deliverFanOut(database *sql.DB, b *fanOutBreaker, resolve func() upstreamFanOutDispatch, f upstreamFanOut) {
	err := startUpstreamFanOut(context.Background(), b, resolve, f)
	if err == nil {
		return
	}
	log.WithError(err).WithFields(log.Fields{"upstream_id": f.Source.ID, "occurrence": f.Occurrence, "models": len(f.Targets)}).
		Error("upstream trigger: could not hand the fan-out to Temporal; recording the models below as trigger_lost")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := recordSkippedDownstream(ctx, database, f.Source, skipTriggerLost); err != nil {
		log.WithError(err).WithField("upstream_id", f.Source.ID).
			Error("upstream trigger: could not record the models whose trigger was lost")
	}
}

// fanOutBreaker stops the fan-out re-dialling a Temporal that just failed (G11).
//
// A failed dial holds the client mutex for its whole deadline, so without it every
// completion during an outage paid that deadline in turn. One failed start opens it for
// window, and inside the window a start is refused at once — the caller records it as
// trigger_lost without dialling. After the window ONE start is let through as the probe
// while the rest are still refused; otherwise every completion that queued up would dial
// the still-down Temporal at once. The probe's answer closes the breaker or reopens it.
// "Already started" is Temporal answering, not failing, so it closes the breaker too.
//
// A probe that hangs is given up on after another window, so a lost probe cannot keep
// Temporal bypassed for good.
type fanOutBreaker struct {
	mu           sync.Mutex
	until        time.Time
	probing      bool
	probeExpires time.Time
	window       time.Duration
	now          func() time.Time
}

var fanOutTemporalBreaker = &fanOutBreaker{window: time.Minute, now: time.Now}

// acquire says whether this start may dial Temporal. Closed: always. Open: never.
// Window passed: only if no other probe is out.
func (b *fanOutBreaker) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.until.IsZero() {
		return true
	}
	if now.Before(b.until) {
		return false
	}
	if b.probing && now.Before(b.probeExpires) {
		return false
	}
	b.probing, b.probeExpires = true, now.Add(b.window)
	return true
}

func (b *fanOutBreaker) observe(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if err == nil || isFanOutAlreadyStarted(err) {
		b.until = time.Time{}
		return
	}
	b.until = b.now().Add(b.window)
}

// isFanOutAlreadyStarted reports whether Temporal refused the start because this
// completion already has a fan-out.
//
// Separate and named because it is the difference between "already done" and "did not
// happen", and those two take opposite actions: the first is success, the second is
// retried and then recorded as trigger_lost.
func isFanOutAlreadyStarted(err error) bool {
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &already)
}

// downstreamModelLookup builds the query that finds every model waiting on one upstream.
//
// The kind picks a column and a join rather than being bound as a parameter, because the
// two upstream columns are two different foreign keys and no single predicate covers
// both. Only the join and the column name are interpolated from a two-value constant —
// the id is still bound — which is the shape modelRunScheduleLookup already uses.
//
// "Fan-in" here means several producers feed one model, not that the model waits for all
// of them. A completion fires every schedule that lists it, and a model listing three
// pipelines rebuilds three times a night. There is no barrier state to wait on, and
// inventing one would mean deciding when a night is over. That question is answered by a
// deadline instead — saved_queries.freshness_deadline_seconds and the sweep in
// saved_query_freshness.go — which reports a table that stopped moving without having to
// decide when a batch was supposed to be complete.
func downstreamModelLookup(kind string) string {
	col, join := "u.upstream_pipeline_id", "JOIN pipelines up ON up.id = u.upstream_pipeline_id"
	if kind == upstreamKindModel {
		col, join = "u.upstream_saved_query_id", "JOIN saved_queries up ON up.id = u.upstream_saved_query_id"
	}
	// Both halves of the tenancy check are here, in the fire path, rather than trusted
	// from create time:
	//
	//   - sq.workspace_id = up.workspace_id — the schedule was authorized against the
	//     creator's active workspace when it was made, and an upstream can be moved
	//     afterwards. Without this, moving a pipeline into workspace B would keep
	//     rebuilding workspace A's model on B's data, and the run history would show it
	//     as a normal successful rebuild.
	//
	//   - status = 'active' — a paused trigger has no Temporal schedule to have been
	//     paused, so this predicate IS the pause for the event path.
	return fmt.Sprintf(`
		SELECT s.schedule_id, s.saved_query_id::text, s.run_as_user_id::text
		FROM saved_query_schedule_upstreams u
		JOIN saved_query_schedules s ON s.schedule_id = u.schedule_id
		JOIN saved_queries sq ON sq.id = s.saved_query_id
		%s
		WHERE %s = $1::uuid
		  AND s.schedule_type = 'after_upstream'
		  AND s.status = 'active'
		  AND sq.workspace_id = up.workspace_id
	`, join, col)
}

// FireModelsAfterPipeline rebuilds every model whose schedule waits on this pipeline.
// Assigned to EventProjector.OnPipelineCompleted in main.go.
//
// A pipeline is one of the two kinds of thing a model can wait on, so this is a wrapper
// over the shared path. It keeps its own name because the projector's callback field is
// about pipelines and nothing else, and because main.go should not have to know that the
// two kinds share an implementation.
func FireModelsAfterPipeline(ctx context.Context, pipelineID, executionID string) {
	fireDownstreamModels(ctx, modelRefreshSource{
		Kind:        upstreamKindPipeline,
		ID:          pipelineID,
		ExecutionID: executionID,
	})
}

// fireDownstreamModelsAfterRun continues a rebuild chain past a model that just finished,
// from the gateway: for Run now, and for an internal run whose caller did not take the
// hop itself (leavesHopToCaller). A run whose workflow takes the hop continues the chain
// through ListSavedQueryModelDownstreamInternal instead, by the same rules.
//
// Only a succeeded run propagates. A model whose rebuild failed has the data it had
// before, so firing its consumers would rebuild them on a snapshot that is one refresh
// stale while reporting a fresh run — the exact confusion the trigger exists to avoid.
//
// A run that did not succeed stops the chain, and says so: every model it would have
// woken gets a 'skipped' row naming why (G3). Before, they got nothing, and a model below
// a failed upstream had a history indistinguishable from "nothing happened".
func fireDownstreamModelsAfterRun(res *modelRunResult, modelID, runID, executionID string, depth int) {
	if src := nextChainSource(res, modelID, runID, executionID, depth); src != nil {
		fireDownstreamModels(context.Background(), *src)
		return
	}
	reason := cascadeSkipReason(res)
	if reason == "" {
		return
	}
	database := db.GetDB()
	if database == nil {
		return
	}
	if _, err := uuid.Parse(modelID); err != nil {
		return
	}
	// Its own goroutine: this is reached from request handlers, which answer first.
	root := modelRefreshSource{
		Kind:        upstreamKindModel,
		ID:          modelID,
		RunID:       runID,
		ExecutionID: executionID,
		Depth:       depth + 1,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := recordSkippedDownstream(ctx, database, root, reason); err != nil {
			log.WithError(err).WithField("upstream_id", modelID).
				Warn("upstream trigger: could not record the models a stopped upstream did not rebuild")
		}
	}()
}

// cascadeSkipReason is what a finished run that did not succeed tells the models below
// it, or "" when it should tell them nothing.
//
// A skip only cascades when it is a stable refusal (one that auto-pauses). The transient
// one — another run of this model already in flight — is not a stop: that other run
// finishes and wakes the same consumers itself, so a skip row here would say "not
// rebuilt" about a model that is about to be.
func cascadeSkipReason(res *modelRunResult) string {
	switch {
	case res == nil:
		return ""
	case res.Status == "failed" && res.CheckBlocked:
		return skipUpstreamCheckFailed
	case res.Status == "failed":
		return skipUpstreamFailed
	case res.Status == "skipped" && res.AutoPauseReason != "":
		return skipUpstreamSkipped
	}
	return ""
}

// recordSkippedDownstream writes a skip row for every model below a stopped upstream,
// all the way down: the direct consumers get reason, everything below them
// upstream_skipped, each row linked by upstream_run_id to the row of the model above it,
// so the history of a model three hops down can be walked back to the failure.
//
// Bounded twice, like the rebuild chain: by the same depth bound, and by a visited set,
// because a fan-in reachable along two paths is one model that did not rebuild once, not
// twice. Only active event triggers are reached (downstreamModelLookup), so a paused
// model below the failure records nothing — its pause is already its explanation.
//
// A failed lookup is returned: /downstream answers 500 on it so the workflow retries,
// where an empty answer would read as "nothing below". Rows written before it stay.
func recordSkippedDownstream(ctx context.Context, database *sql.DB, root modelRefreshSource, reason string) error {
	type hop struct {
		src    modelRefreshSource
		reason string
	}
	queue := []hop{{root, reason}}
	visited := map[string]bool{root.ID: true}
	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]
		if chainDepthExceeded(h.src.Depth) {
			continue
		}
		targets, err := lookupDownstreamModels(ctx, database, h.src.Kind, h.src.ID)
		if err != nil {
			return fmt.Errorf("look up the models below %s: %w", h.src.ID, err)
		}
		for _, t := range targets {
			if visited[t.ModelID] {
				continue
			}
			visited[t.ModelID] = true
			runID := recordModelRunSkip(ctx, database, t.ModelID, t.ScheduleID, t.RunAsUserID,
				h.reason, "", provenanceFor(h.src, 0))
			queue = append(queue, hop{modelRefreshSource{
				Kind:        upstreamKindModel,
				ID:          t.ModelID,
				RunID:       runID,
				ExecutionID: h.src.ExecutionID,
				Depth:       h.src.Depth + 1,
			}, skipUpstreamSkipped})
		}
	}
	return nil
}

// recordChainDepthSkips records the models a chain stopped short of at its depth bound
// (G7). The bound used to be a log line only, so the model past it simply never had a
// row, and its owner had no way to learn the chain was too deep rather than idle.
func recordChainDepthSkips(ctx context.Context, database *sql.DB, src modelRefreshSource) error {
	targets, err := lookupDownstreamModels(ctx, database, src.Kind, src.ID)
	if err != nil {
		return fmt.Errorf("look up the models below %s: %w", src.ID, err)
	}
	msg := fmt.Sprintf("the rebuild chain reached its limit of %d hops", maxTriggerChainDepth)
	for _, t := range targets {
		recordModelRunSkip(ctx, database, t.ModelID, t.ScheduleID, t.RunAsUserID,
			skipChainDepthExceeded, msg, provenanceFor(src, 0))
	}
	return nil
}

// lookupDownstreamModels runs downstreamModelLookup.
func lookupDownstreamModels(ctx context.Context, database *sql.DB, kind, id string) ([]modelRefreshTarget, error) {
	rows, err := database.QueryContext(ctx, downstreamModelLookup(kind), id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []modelRefreshTarget
	for rows.Next() {
		var f modelRefreshTarget
		if err := rows.Scan(&f.ScheduleID, &f.ModelID, &f.RunAsUserID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// unsatisfiedUpstreams returns the ids of the upstreams a policy='all' fan-in is still waiting on
// (G4), or returns none when the schedule is 'any' or every upstream is fresh.
//
// Fresh is measured from this model's last successful rebuild STARTED — started, not
// finished, because a sibling that landed while that build was running was not read by
// it. The upstream that is firing right now is fresh by definition and is excluded by
// id, since its completion may not have reached either table yet (a pipeline completion
// fires from the projector, and a model's run row is committed by a different request).
//
// The query gathers each upstream's facts and upstreamIsFresh judges them: a model on a
// succeeded run; a pipeline on a stored PIPELINE_COMPLETED event, the same event that
// fires it; a CDC pipeline, which never completes, also on its watermark or a drained
// sink. With no successful rebuild of this model yet, any of them counts.
//
// A stale model upstream still does not hold the fan-in when nothing is pending above it
// (modelAncestry.settled): it will not rebuild again until its own sources move, so
// waiting for it skipped the fan-in on every fire, forever. Seen on prod: two gold models
// skipped hourly for a week behind silver models on an idle CDC pipeline. The extra
// lookup runs only when a model upstream is stale.
//
// "Ever" survives retention: the run-history sweep deletes a batch run's events after
// PIPELINE_RUN_RETENTION_DAYS (90 by default) but keeps the run holding each pipeline's
// newest PIPELINE_COMPLETED (retention/pipeline_run_retention.go runIsKeptSQL), so a
// batch upstream idle for longer still reads as completed. The completion lookup keeps
// the baseline as its lower bound, so a CDC pipeline's many events are scanned no
// further back than the old NOT EXISTS scanned them.
func unsatisfiedUpstreams(ctx context.Context, database *sql.DB, scheduleID, firingUpstreamID string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `
		WITH me AS (
			SELECT s.schedule_id,
			       (SELECT MAX(r.started_at) FROM saved_query_runs r
			        WHERE r.saved_query_id = s.saved_query_id AND r.status = 'succeeded') AS baseline
			FROM saved_query_schedules s
			WHERE s.schedule_id = $1::uuid AND s.upstream_policy = 'all'
		)
		SELECT COALESCE(u.upstream_pipeline_id, u.upstream_saved_query_id)::text,
		       u.upstream_saved_query_id IS NOT NULL,
		       me.baseline,
		       (SELECT MAX(ur.finished_at) FROM saved_query_runs ur
		        WHERE ur.saved_query_id = u.upstream_saved_query_id AND ur.status = 'succeeded'),
		       (SELECT MAX(e.received_at) FROM pipeline_run_events e
		        WHERE e.pipeline_id = u.upstream_pipeline_id AND e.event_type = 'PIPELINE_COMPLETED'
		          AND e.received_at > COALESCE(me.baseline, '-infinity'::timestamptz)),
		       (p.id IS NOT NULL AND `+pipelineRowIsCDCSQL+`),
		       (SELECT MAX(w.source_time) FROM cdc_table_watermarks w WHERE w.pipeline_id = u.upstream_pipeline_id),
		       (SELECT l.measured_at FROM pipeline_sink_lag l
		        WHERE l.pipeline_id = u.upstream_pipeline_id AND l.total_lag = 0)
		FROM me
		JOIN saved_query_schedule_upstreams u ON u.schedule_id = me.schedule_id
		LEFT JOIN pipelines p ON p.id = u.upstream_pipeline_id
		WHERE COALESCE(u.upstream_pipeline_id, u.upstream_saved_query_id) IS DISTINCT FROM NULLIF($2, '')::uuid
		ORDER BY 1
	`, scheduleID, firingUpstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stale []upstreamFacts
	var roots []string
	for rows.Next() {
		var f upstreamFacts
		var baseline sql.NullTime
		if err := rows.Scan(&f.ID, &f.IsModel, &baseline, &f.ModelSucceededAt, &f.PipelineCompletedAt,
			&f.IsCDC, &f.WatermarkSourceTime, &f.SinkCaughtUpAt); err != nil {
			return nil, err
		}
		if upstreamIsFresh(f, baseline) {
			continue
		}
		stale = append(stale, f)
		if f.IsModel && f.ModelSucceededAt.Valid {
			roots = append(roots, f.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// A stale model upstream that has nothing pending above it will not rebuild again, so
	// waiting for it would skip this fan-in on every sibling's fire, forever. Its last
	// build is the newest its upstreams can give, so it counts as satisfied
	// (modelAncestry.settled). A model that has never built has nothing to read, and waits.
	var ancestry modelAncestry
	if len(roots) > 0 {
		if ancestry, err = loadModelAncestry(ctx, database, roots); err != nil {
			return nil, err
		}
	}
	var out []string
	for _, f := range stale {
		if f.IsModel && f.ModelSucceededAt.Valid && ancestry.settled(f.ID) {
			continue
		}
		out = append(out, f.ID)
	}
	return out, nil
}

// loadModelAncestry reads every model above roots (themselves included) along
// after_upstream schedules, with each one's baseline and its upstreams' newest output.
// A model with no upstreams to wait on (a cron, interval or manual model, or a deleted
// schedule) comes back with no edges, which settled reads as "unknown, so pending".
// UNION, not UNION ALL, so a ring ends the walk instead of looping (checkUpstreamCycle
// refuses one on save; this does not rely on it).
func loadModelAncestry(ctx context.Context, database *sql.DB, roots []string) (modelAncestry, error) {
	rows, err := database.QueryContext(ctx, `
		WITH RECURSIVE closure(model_id) AS (
			SELECT unnest($1::uuid[])
			UNION
			SELECT u.upstream_saved_query_id
			FROM closure c
			JOIN saved_query_schedules s ON s.saved_query_id = c.model_id
			     AND s.status <> 'deleted' AND s.schedule_type = 'after_upstream'
			JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
			WHERE u.upstream_saved_query_id IS NOT NULL
		), node AS (
			SELECT c.model_id,
			       (SELECT MAX(r.started_at) FROM saved_query_runs r
			        WHERE r.saved_query_id = c.model_id AND r.status = 'succeeded') AS baseline
			FROM closure c
		)
		SELECT n.model_id::text,
		       n.baseline,
		       COALESCE(u.upstream_pipeline_id, u.upstream_saved_query_id)::text,
		       u.upstream_saved_query_id IS NOT NULL,
		       (SELECT MAX(ur.finished_at) FROM saved_query_runs ur
		        WHERE ur.saved_query_id = u.upstream_saved_query_id AND ur.status = 'succeeded'),
		       (SELECT MAX(e.received_at) FROM pipeline_run_events e
		        WHERE e.pipeline_id = u.upstream_pipeline_id AND e.event_type = 'PIPELINE_COMPLETED'
		          AND e.received_at > COALESCE(n.baseline, '-infinity'::timestamptz)),
		       (p.id IS NOT NULL AND `+pipelineRowIsCDCSQL+`),
		       (SELECT MAX(w.landed_at) FROM cdc_table_watermarks w WHERE w.pipeline_id = u.upstream_pipeline_id)
		FROM node n
		LEFT JOIN saved_query_schedules s ON s.saved_query_id = n.model_id
		     AND s.status <> 'deleted' AND s.schedule_type = 'after_upstream'
		LEFT JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
		LEFT JOIN pipelines p ON p.id = u.upstream_pipeline_id
		ORDER BY 1, 3
	`, pgdriver.StringArray(roots))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := modelAncestry{}
	for rows.Next() {
		var id string
		var n ancestryNode
		var edgeID sql.NullString
		var e ancestryEdge
		if err := rows.Scan(&id, &n.Baseline, &edgeID, &e.IsModel, &e.ModelSucceededAt,
			&e.PipelineCompletedAt, &e.IsCDC, &e.LandedAt); err != nil {
			return nil, err
		}
		n.Edges = out[id].Edges
		if edgeID.Valid {
			e.ID = edgeID.String
			n.Edges = append(n.Edges, e)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// upstreamFacts is what the database knows about one upstream of a policy='all' fan-in:
// the newest moment of each kind that could make it fresh. unsatisfiedUpstreams reads
// them and upstreamIsFresh judges them, so the rule is testable without a database.
type upstreamFacts struct {
	ID      string
	IsModel bool
	// ModelSucceededAt is a model upstream's newest succeeded run, finished_at.
	ModelSucceededAt sql.NullTime
	// PipelineCompletedAt is a pipeline upstream's newest stored PIPELINE_COMPLETED.
	PipelineCompletedAt sql.NullTime
	// IsCDC: the pipeline streams (pipelineRowIsCDCSQL), so it never completes.
	IsCDC bool
	// WatermarkSourceTime is the newest source time the sink has landed across the
	// pipeline's tables (cdc_table_watermarks, migration 137), on the SOURCE clock.
	WatermarkSourceTime sql.NullTime
	// SinkCaughtUpAt is when the sentinel last measured the sink's lag at 0
	// (pipeline_sink_lag, migration 116), NULL while it is behind.
	SinkCaughtUpAt sql.NullTime
}

// upstreamIsFresh says whether an upstream has rebuilt since this model's last
// successful build started (baseline; NULL when there is none, so anything counts).
//
// A CDC pipeline never emits PIPELINE_COMPLETED, so it counts as fresh on either of two
// stand-ins: its watermark is past the baseline (new source data landed), or its sink
// was measured fully drained after the baseline (an idle source, whose last change the
// model can read). Without them an idle CDC source held an 'all' model forever
// (KI-CDC-UPSTREAM-ALL-NEVER-FRESH).
//
// Two limits, both accepted: the watermark is on the source's clock and the baseline on
// this database's, so skew between them moves the edge by that much; and a lag of 0
// cannot tell an idle source from a dead Debezium worker, whose topics drain to 0 too
// (that death raises its own capture_stopped issue, #1251).
func upstreamIsFresh(f upstreamFacts, baseline sql.NullTime) bool {
	after := func(t sql.NullTime) bool { return t.Valid && (!baseline.Valid || t.Time.After(baseline.Time)) }
	if f.IsModel {
		return after(f.ModelSucceededAt)
	}
	if after(f.PipelineCompletedAt) {
		return true
	}
	return f.IsCDC && (after(f.WatermarkSourceTime) || after(f.SinkCaughtUpAt))
}

// modelAncestry is every model above a fan-in's stale model upstreams, keyed by id, as
// loadModelAncestry reads it. settled judges it, so the rule is testable without a database.
type modelAncestry map[string]ancestryNode

// ancestryNode is one model above a fan-in: its own baseline and its upstreams.
type ancestryNode struct {
	// Baseline is this model's newest succeeded run, started_at; NULL when it has none.
	Baseline sql.NullTime
	Edges    []ancestryEdge
}

// ancestryEdge is one upstream of an ancestryNode and the newest output it has given.
type ancestryEdge struct {
	ID      string
	IsModel bool
	// ModelSucceededAt is a model upstream's newest succeeded run, finished_at.
	ModelSucceededAt sql.NullTime
	// PipelineCompletedAt is a pipeline upstream's newest PIPELINE_COMPLETED after the
	// node's baseline.
	PipelineCompletedAt sql.NullTime
	IsCDC               bool
	// LandedAt is a CDC pipeline's newest landing across its tables, on this database's
	// clock (cdc_table_watermarks.landed_at): the signal the trailing edge rebuilds on.
	LandedAt sql.NullTime
}

// settled says whether nothing is pending anywhere above model id: none of its upstreams
// has given output since its last build started, and the same holds for each model
// upstream, all the way up. Such a model will not rebuild until a new event arrives, and
// its last build is already the newest its sources can give.
//
// A CDC pipeline is pending on a landing after the baseline, not on a drained sink: an
// idle source's sink stays drained, and that would read as pending forever. A lagging
// sink is not pending either, or one wedged sink would hold the fan-in. A model with no
// upstreams to watch, one missing from the ancestry, and a ring are all unknown, so not
// settled: that keeps the old rule, which waited.
func (a modelAncestry) settled(id string) bool {
	memo := map[string]bool{}
	var walk func(id string) bool
	walk = func(id string) bool {
		if v, seen := memo[id]; seen {
			return v
		}
		memo[id] = false // in progress, so a ring reads as not settled
		n, ok := a[id]
		if !ok || len(n.Edges) == 0 {
			return false
		}
		after := func(t sql.NullTime) bool { return t.Valid && (!n.Baseline.Valid || t.Time.After(n.Baseline.Time)) }
		for _, e := range n.Edges {
			if e.IsModel && after(e.ModelSucceededAt) ||
				!e.IsModel && (after(e.PipelineCompletedAt) || e.IsCDC && after(e.LandedAt)) {
				return false
			}
		}
		for _, e := range n.Edges {
			if e.IsModel && !walk(e.ID) {
				return false
			}
		}
		memo[id] = true
		return true
	}
	return walk(id)
}

// waitingOnMessage says how many upstreams a fan-in is still waiting for, and not which.
// The message is stored on the run, and everyone who can see the model reads it; an
// upstream can be another member's private model, whose name that reader may not see.
func waitingOnMessage(ids []string) string {
	if len(ids) == 1 {
		return "waiting on 1 upstream that has not rebuilt since this model's last build"
	}
	return fmt.Sprintf("waiting on %d upstreams that have not rebuilt since this model's last build", len(ids))
}

// nextChainSource answers the only two questions a finished model run poses to the
// chain: does it continue, and one hop from where.
//
// Separate from its caller so both rules can be asserted without a database. Between
// them they are what terminates a ring: the depth carried here is what chainDepthExceeded
// eventually refuses, so a hop that forgot to increment would loop forever and a bound
// that never saw the increment would never fire.
func nextChainSource(res *modelRunResult, modelID, runID, executionID string, depth int) *modelRefreshSource {
	if res == nil || res.Status != "succeeded" {
		return nil
	}
	return &modelRefreshSource{
		Kind:        upstreamKindModel,
		ID:          modelID,
		RunID:       runID,
		ExecutionID: executionID,
		Depth:       depth + 1,
	}
}

// fireDownstreamModels hands every model waiting on whatever just finished to Temporal.
//
// Returns immediately. The start happens on its own goroutine, and that is not an
// optimization — the pipeline door reaches this from the projector's consume loop,
// before committing the Kafka offset, and a start retries for up to fanOutRetryBudget.
// Blocking there stops the projector fetching, and a consumer that stops fetching past
// the session timeout is evicted from the group, which stalls telemetry for every
// pipeline in the deployment and then redelivers the batch to whoever takes the partition.
//
// The passed context belongs to the caller and is cancelled at shutdown, so it is used
// for the lookup only.
func fireDownstreamModels(ctx context.Context, src modelRefreshSource) {
	database := db.GetDB()
	if database == nil {
		return
	}
	if _, err := uuid.Parse(src.ID); err != nil {
		return
	}

	// The chain bound, and the reason it is not redundant with checkUpstreamCycle. That
	// check refuses a ring at write time, which is the only place a person can be told
	// about it; this is the backstop for the ring that check could not see — one closed
	// between the check and the write by a concurrent edit, or by rows that predate the
	// check. Neither replaces the other: a depth bound alone would let a two-model ring
	// rebuild eight times before stopping, and a write-time check alone cannot bound a
	// chain that is long without being circular.
	if chainDepthExceeded(src.Depth) {
		log.WithFields(log.Fields{
			"upstream_kind": src.Kind,
			"upstream_id":   src.ID,
			"depth":         src.Depth,
		}).Warn("upstream trigger: rebuild chain hit its depth bound, stopping here")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := recordChainDepthSkips(ctx, database, src); err != nil {
				log.WithError(err).WithField("upstream_id", src.ID).
					Warn("upstream trigger: could not record the models past the depth bound")
			}
		}()
		return
	}

	toFire, err := lookupDownstreamModels(ctx, database, src.Kind, src.ID)
	if err != nil {
		log.WithError(err).WithField("upstream_id", src.ID).
			Error("upstream trigger: failed to look up downstream models")
		return
	}
	if len(toFire) == 0 {
		return
	}

	log.WithFields(log.Fields{
		"upstream_kind": src.Kind,
		"upstream_id":   src.ID,
		"execution_id":  src.ExecutionID,
		"depth":         src.Depth,
		"models":        len(toFire),
	}).Info("⚡ upstream completed: waking downstream models")

	// getTemporalClient may dial, and not blocking the projector's consume loop is this
	// function's whole contract, so the client is resolved on the goroutine too.
	go deliverFanOut(database, fanOutTemporalBreaker, newUpstreamFanOutDispatch, upstreamFanOut{
		Occurrence: fanOutOccurrence(src),
		Source:     src,
		Targets:    toFire,
	})
}

// autoPauseModelSchedule records a machine pause. It writes auto_paused_* rather than
// paused_*, keeping "an operator stopped this" and "the platform stopped this"
// distinguishable — the resume path treats them differently.
//
// Takes a context rather than the *gin.Context it used to, because the event path
// reaches it from a projector callback that has no request behind it. An empty
// temporalID means an event trigger, which has nothing registered to pause: the row
// written here IS the pause, and FireModelsAfterPipeline re-reads status per event.
//
// The owner is told (notifyModelSchedulePaused) once the row says paused: nothing else
// would bring them back to a schedule that has stopped running.
func autoPauseModelSchedule(ctx context.Context, database *sql.DB, modelID, scheduleID, temporalID, reason string) {
	res, err := database.ExecContext(ctx, `
		UPDATE saved_query_schedules
		SET status = 'paused', auto_paused_at = NOW(), auto_paused_reason = $2
		WHERE schedule_id = $1
	`, scheduleID, reason)
	if err != nil {
		log.WithError(err).WithField("schedule_id", scheduleID).Error("failed to record model schedule auto-pause")
	} else if n, _ := res.RowsAffected(); n > 0 {
		notifyModelSchedulePaused(modelID, scheduleID)
	}
	if temporalClient == nil || temporalID == "" {
		return
	}
	if err := pauseTemporalSchedule(ctx, temporalID, "auto-paused: "+reason); err != nil {
		// The DB row already says paused and the internal endpoint re-reads status on
		// every tick, so the schedule is stopped even if Temporal keeps waking it.
		log.WithError(err).WithField("schedule_id", scheduleID).
			Warn("failed to pause Temporal schedule after auto-pause; the status check will keep refusing runs")
	}
	log.WithFields(log.Fields{"schedule_id": scheduleID, "reason": reason}).
		Warn("⏸️  model schedule auto-paused")
}

// ============================================================================
// Helpers
// ============================================================================

// authorizeUpstreams checks the caller against every producer in a proposed set.
//
// One refusal refuses the whole request rather than dropping the upstreams the caller
// cannot see. A schedule that silently waits on fewer producers than the operator asked
// for is a model that quietly goes stale, and the operator would have no way to tell
// that from a producer that simply has not run.
//
// requireResourceRole writes its own response, so a false return means the reply has
// already been sent. So does savedQueryVisibleToCaller.
func authorizeUpstreams(c *gin.Context, upstreams []scheduleUpstream) bool {
	for _, u := range upstreams {
		id := strings.TrimSpace(u.ID)
		// A saved query needs both halves. A pipeline needs only the role gate:
		// pipelines have no per-member visibility, so membership IS permission there.
		if u.Kind == upstreamKindModel {
			if !requireVisibleSavedQuery(c, id, security.WSViewer) {
				return false
			}
			continue
		}
		if _, ok := requireResourceRole(c, "pipelines", id, security.WSViewer); !ok {
			return false
		}
	}
	return true
}

// requireVisibleSavedQuery is the complete gate for a saved query reached by id: workspace
// membership and role, AND the visibility half requireResourceRole does not do.
//
// It exists as one function because the two halves were separable, and everything that
// called only the first half was wrong. Reaching a model by id — its schedule, its run
// history, its materialization, a manual run — asked only whether the caller belonged to
// the workspace holding it, so a member could read, pause, retarget, run and delete
// another member's PRIVATE model's schedule: ids that answer 404 on every direct read.
// New per-id endpoints should call this rather than requireResourceRole.
//
// Both halves write their own response, so a false return means the reply has been sent.
func requireVisibleSavedQuery(c *gin.Context, id string, min security.WorkspaceRole) bool {
	if _, ok := requireResourceRole(c, "saved_queries", id, min); !ok {
		return false
	}
	return savedQueryVisibleToCaller(c, id)
}

// savedQueryVisibleToCaller checks the half requireResourceRole does not: that this member may
// SEE this saved query, not merely that they belong to the workspace holding it.
//
// requireResourceRole proves membership and role, by design and by its own comment — there
// is no created_by fallback in it. Visibility is a separate column (migration 084): a
// private saved query belongs to its author, and `visibility = 'workspace' OR created_by =
// $n` is the predicate every read of the row already uses.
//
// Prefer requireVisibleSavedQuery, which runs both halves. Call this one directly only
// where the role gate has already run against a different resource.
//
// The refusal is the one loadSavedQuery gives a row the caller may not see: 404 "not
// found", identical to the answer for an id that does not exist, so a member cannot probe
// which private ids are real. workspace_id is bound too, rather than trusted from the role
// gate above, so the predicate is complete wherever this is called from.
func savedQueryVisibleToCaller(c *gin.Context, id string) bool {
	userID, ok := resolveUserID(c)
	if !ok {
		return false
	}
	activeWS := c.GetString(ctxWorkspaceID)
	if activeWS == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return false
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return false
	}

	var visible int
	err := database.QueryRowContext(c.Request.Context(), `
		SELECT 1
		FROM saved_queries
		WHERE id = $1 AND workspace_id = $2
		  AND (visibility = 'workspace' OR created_by = $3)
	`, id, activeWS, userID).Scan(&visible)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return false
	}
	if err != nil {
		log.WithError(err).Error("failed to check whether a saved query is visible to this caller")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return false
	}
	return true
}

// checkUpstreamCycle refuses a set of upstreams that would close a rebuild ring.
//
// Walks UP from each proposed model upstream and asks whether the model being scheduled
// is already an ancestor. Only model upstreams can close a ring: a pipeline is never
// downstream of a model, so a pipeline edge is always a leaf on this side of the graph.
//
// UNION rather than UNION ALL, deliberately. The recursion is over stored rows, and if
// one of those already forms a ring — written before this check existed, or by a path
// that bypasses it — UNION ALL would revisit it forever and hang the request. UNION
// stops at the first repeat, which is exactly the termination guarantee a graph with no
// integrity constraint against rings needs.
//
// A deleted schedule's edges are not part of the graph. Delete is a soft delete, so its
// upstream rows stay behind, and walking them refused a perfectly acyclic schedule for a
// ring that nothing could ever fire. A PAUSED schedule's edges do count: resuming it is
// one click, and that click must not be the thing that closes a ring.
//
// Writes its own response, so a false return means the reply has already been sent.
func checkUpstreamCycle(c *gin.Context, database *sql.DB, modelID string, upstreams []scheduleUpstream) bool {
	for _, u := range upstreams {
		if u.Kind != upstreamKindModel {
			continue
		}
		start := strings.TrimSpace(u.ID)
		// The one-hop ring, which the recursion below would also catch but which is worth
		// naming separately: its error message is the one an operator is most likely to
		// see and the least likely to understand from a generic "would create a cycle".
		if start == modelID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "a model cannot wait on itself"})
			return false
		}
		var reaches bool
		if err := database.QueryRowContext(c.Request.Context(), `
			WITH RECURSIVE ancestors(model_id) AS (
				SELECT $1::uuid
				UNION
				SELECT u.upstream_saved_query_id
				FROM ancestors a
				JOIN saved_query_schedules s ON s.saved_query_id = a.model_id AND s.status <> 'deleted'
				JOIN saved_query_schedule_upstreams u ON u.schedule_id = s.schedule_id
				WHERE u.upstream_saved_query_id IS NOT NULL
			)
			SELECT EXISTS (SELECT 1 FROM ancestors WHERE model_id = $2::uuid)
		`, start, modelID).Scan(&reaches); err != nil {
			log.WithError(err).WithField("model_id", modelID).
				Error("failed to check a model trigger chain for cycles")
			// Refused rather than allowed. The check cannot be re-run after the write, and
			// a ring that gets stored rebuilds until the depth bound catches it on every
			// upstream completion, forever.
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate the upstreams"})
			return false
		}
		if reaches {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "that upstream already depends on this model, so the two would rebuild each other",
			})
			return false
		}
	}
	return true
}

// pauseTriggersOrphanedBy pauses every active after_upstream schedule whose ONLY
// remaining upstream is the pipeline or model about to be deleted.
//
// The upstream rows cascade with the thing they point at (migration 100), and the fire path
// matches schedules BY upstream, so a schedule left with none is active on the page and can
// never fire again: the silent broken state an auto-pause exists to make loud. A schedule
// that still has another upstream keeps running on that one and is left alone.
//
// Only ACTIVE schedules are marked. One the user already paused is left paused with no
// auto_paused_reason and zero upstreams; nothing reports it until they try to resume, which
// refuses an after_upstream schedule with no upstreams and asks them to choose new ones.
//
// Must run inside the delete's transaction and BEFORE the delete: after it the rows that
// say which schedules depended on the upstream are already gone, and outside it a delete
// that rolls back would leave schedules paused for an upstream that still exists.
//
// "IS DISTINCT FROM" in the second test rather than "<>": a row for the other kind of
// upstream holds NULL in this column, and it is exactly such a row that must count as a
// surviving upstream.
func pauseTriggersOrphanedBy(ctx context.Context, ex modelExecer, kind, upstreamID string) (int64, error) {
	col := "upstream_saved_query_id"
	if kind == upstreamKindPipeline {
		col = "upstream_pipeline_id"
	}
	res, err := ex.ExecContext(ctx, `
		UPDATE saved_query_schedules s
		SET status = 'paused', auto_paused_at = NOW(),
		    auto_paused_reason = 'an upstream it depended on was deleted, so nothing can wake it; ' ||
		        'edit the schedule to choose new upstreams'
		WHERE s.status = 'active'
		  AND s.schedule_type = 'after_upstream'
		  AND EXISTS (SELECT 1 FROM saved_query_schedule_upstreams u
		              WHERE u.schedule_id = s.schedule_id AND u.`+col+` = $1::uuid)
		  AND NOT EXISTS (SELECT 1 FROM saved_query_schedule_upstreams u
		                  WHERE u.schedule_id = s.schedule_id AND u.`+col+` IS DISTINCT FROM $1::uuid)
	`, upstreamID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// refuseUnbuildableUpstreams refuses a model upstream that can never complete a build.
//
// A model upstream wakes its downstreams when a run of it succeeds, and a run of a plain
// saved query (materialization 'none') is refused before it starts, so it never succeeds.
// Accepting one stored an active schedule that could not fire, and nothing on the page said
// so. Checked on every edit, not only at create, because the upstream's materialization can
// have been cleared since the edge was first written.
//
// The refusal names the upstream. Both callers run authorizeUpstreams first, so the caller
// has already been shown to see it; a new caller must do the same.
//
// Writes its own response, so a false return means the reply has already been sent.
func refuseUnbuildableUpstreams(c *gin.Context, database *sql.DB, upstreams []scheduleUpstream) bool {
	for _, u := range upstreams {
		if u.Kind != upstreamKindModel {
			continue
		}
		var name, materialization string
		err := database.QueryRowContext(c.Request.Context(), `
			SELECT name, materialization FROM saved_queries WHERE id = $1::uuid
		`, strings.TrimSpace(u.ID)).Scan(&name, &materialization)
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return false
		}
		if err != nil {
			log.WithError(err).WithField("upstream_id", u.ID).Error("failed to read an upstream model's materialization")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate the upstreams"})
			return false
		}
		if materialization == matNone {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("%q does not write a table or run a statement, so it never completes a build "+
					"and could never wake this model; give it a materialization first", name),
			})
			return false
		}
	}
	return true
}

// insertScheduleUpstreams writes one set of stored upstream edges.
//
// Takes modelExecer so the same implementation serves the create path, which passes the
// transaction already holding a lock on the model, and the update path, which opens its
// own. The edges must land in the same transaction as the row that owns them.
//
// ON CONFLICT DO NOTHING against the two partial unique indexes from migration 100.
// validateModelScheduleSpec already refuses a duplicate inside one request, so this only
// covers the concurrent case: two edits adding the same producer at once, where the
// second should be a no-op rather than an error the operator cannot act on.
func insertScheduleUpstreams(ctx context.Context, ex modelExecer, scheduleID string, upstreams []scheduleUpstream) error {
	for _, u := range upstreams {
		var pipelineID, savedQueryID any
		if u.Kind == upstreamKindModel {
			savedQueryID = strings.TrimSpace(u.ID)
		} else {
			pipelineID = strings.TrimSpace(u.ID)
		}
		if _, err := ex.ExecContext(ctx, `
			INSERT INTO saved_query_schedule_upstreams
				(schedule_id, upstream_kind, upstream_pipeline_id, upstream_saved_query_id)
			VALUES ($1, $2, $3::uuid, $4::uuid)
			ON CONFLICT DO NOTHING
		`, scheduleID, u.Kind, pipelineID, savedQueryID); err != nil {
			return err
		}
	}
	return nil
}

// upstreamRowsQuery is the shared projection behind the two readers below. Both need the
// producer's display name, and both get it from whichever of the two LEFT JOINs matched
// — an upstream row has exactly one non-null id by the CHECK in migration 100.
const upstreamRowsQuery = `
	SELECT u.schedule_id::text, u.upstream_kind,
	       COALESCE(u.upstream_pipeline_id::text, u.upstream_saved_query_id::text, ''),
	       COALESCE(p.name, m.name, '')
	FROM saved_query_schedule_upstreams u
	LEFT JOIN pipelines p ON p.id = u.upstream_pipeline_id
	LEFT JOIN saved_queries m ON m.id = u.upstream_saved_query_id
`

// scanUpstreamRows reads the shared projection into a map keyed by schedule id.
func scanUpstreamRows(rows *sql.Rows) (map[string][]scheduleUpstream, error) {
	out := make(map[string][]scheduleUpstream)
	for rows.Next() {
		var scheduleID string
		var u scheduleUpstream
		if err := rows.Scan(&scheduleID, &u.Kind, &u.ID, &u.Name); err != nil {
			return nil, err
		}
		out[scheduleID] = append(out[scheduleID], u)
	}
	return out, rows.Err()
}

// loadScheduleUpstreams reads the stored upstreams of one schedule.
func loadScheduleUpstreams(ctx context.Context, database *sql.DB, scheduleID string) ([]scheduleUpstream, error) {
	rows, err := database.QueryContext(ctx, upstreamRowsQuery+`
		WHERE u.schedule_id = $1
		ORDER BY u.upstream_kind, 4
	`, scheduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID, err := scanUpstreamRows(rows)
	if err != nil {
		return nil, err
	}
	return byID[scheduleID], nil
}

// listWorkspaceScheduleUpstreams reads the upstreams of every schedule the list endpoint
// can show, in one query.
//
// One query rather than one per schedule, and separate from the list query rather than
// joined into it. Joining a one-to-many would multiply the schedule rows by their
// upstream count, which would make the list's LIMIT 500 a limit on upstream rows instead
// of on schedules — a workspace with a few wide fan-ins would start losing schedules off
// the end of its own list page. Passing the ids back as an array is the other obvious
// shape and is not available: this module runs on pgx, which has no pq.Array.
//
// The visibility predicate is a copy of the list query's, for the same reason it is there:
// a private query belonging to another member must not become visible, and it must not
// leak the name of a pipeline it waits on either.
func listWorkspaceScheduleUpstreams(ctx context.Context, database *sql.DB, workspaceID, userID string) (map[string][]scheduleUpstream, error) {
	rows, err := database.QueryContext(ctx, upstreamRowsQuery+`
		JOIN saved_query_schedules s ON s.schedule_id = u.schedule_id
		JOIN saved_queries sq ON sq.id = s.saved_query_id
		WHERE sq.workspace_id = $1
		  AND (sq.visibility = 'workspace' OR sq.created_by = $2)
		  AND s.status != 'deleted'
		ORDER BY u.upstream_kind, 4
	`, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUpstreamRows(rows)
}

// validateModelScheduleSpec is validateScheduleSpec plus the one schedule type that
// only models have.
//
// Deliberately a wrapper rather than a third case inside validateScheduleSpec. That
// function is shared with pipeline schedules (pipeline_schedules.go:161 and :652,
// attachScheduleForChat, pipelines.go:2092), and every one of those callers goes
// straight on to build a Temporal schedule from whatever it accepted.
// createTemporalSchedule has no branch for a non-cadence type, so it would not fail —
// it would hand Temporal an empty client.ScheduleSpec and register a schedule that
// never fires, and nothing on that path would report anything wrong.
//
// TestNextScheduleRun_AcceptsEverySpecValidationAllows pins the same boundary from the
// other side: everything the shared validator admits must have a computable next run,
// and an event trigger deliberately has none.
func validateModelScheduleSpec(scheduleType string, spec ScheduleSpec, upstreams []scheduleUpstream) error {
	if scheduleType == scheduleAfterUpstream {
		// The empty set is refused here rather than by a CHECK. Migration 100 moved the
		// upstream into a child table, and a parent row cannot be constrained by what
		// does or does not reference it, so this is one of the three places that replaced
		// that constraint — and the only one that can say why in words.
		if len(upstreams) == 0 {
			return fmt.Errorf("choose at least one pipeline or model this should rebuild after")
		}
		if len(upstreams) > maxScheduleUpstreams {
			return fmt.Errorf("a model can wait on at most %d upstreams", maxScheduleUpstreams)
		}
		seen := make(map[string]bool, len(upstreams))
		for _, u := range upstreams {
			if u.Kind != upstreamKindPipeline && u.Kind != upstreamKindModel {
				return fmt.Errorf("each upstream needs a kind of %q or %q", upstreamKindPipeline, upstreamKindModel)
			}
			id := strings.TrimSpace(u.ID)
			if _, err := uuid.Parse(id); err != nil {
				return fmt.Errorf("each upstream needs a valid %s id", u.Kind)
			}
			// Refused rather than deduplicated. Two entries for one producer means the
			// caller and this handler disagree about what the set is, and silently
			// collapsing them would hide that from whoever sent it.
			if seen[u.Kind+":"+id] {
				return fmt.Errorf("the same upstream is listed twice")
			}
			seen[u.Kind+":"+id] = true
		}
		return nil
	}

	// Rejected rather than ignored: a request carrying both a cron and an upstream has
	// two readings, and silently keeping the cron would schedule the model on the one
	// the caller did not ask for. Nothing at the column level refuses this combination
	// since 100 — a cadence row simply has no upstream rows — which makes this check the
	// whole of that rule rather than the readable copy of it.
	if len(upstreams) > 0 {
		return fmt.Errorf("upstreams only apply to %s schedules", scheduleAfterUpstream)
	}
	return validateScheduleSpec(scheduleType, spec)
}

// nextScheduleRun computes when a schedule fires next, from the same stored spec the
// Temporal schedule was built from. Returns nil when the answer is unknowable — an
// unparseable cron, an unknown timezone, a type with no cadence — because a wrong time
// on a schedule page is worse than a blank one: a user reads "next run 03:00" as a
// promise and stops checking.
//
// Local computation rather than ScheduleHandle.Describe: Describe is a network call per
// schedule, and this runs once per row of a list. The tradeoff is that this reproduces
// Temporal's own tick arithmetic instead of asking it, so both branches below must keep
// matching createTemporalModelSchedule.
func nextScheduleRun(scheduleType string, spec ScheduleSpec, from time.Time) *time.Time {
	switch scheduleType {
	case "cron":
		if spec.Cron == "" {
			return nil
		}
		// Same 5-field parser validateScheduleSpec accepts with, so anything that was
		// allowed in is parseable here.
		parsed, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec.Cron)
		if err != nil {
			return nil
		}
		loc := time.UTC
		if tz := strings.TrimSpace(spec.Timezone); tz != "" && tz != "UTC" {
			// Requires the embedded IANA database (main.go imports time/tzdata) — the
			// gateway image is alpine, which ships no zoneinfo of its own.
			l, err := time.LoadLocation(tz)
			if err != nil {
				log.WithError(err).WithField("timezone", tz).Warn("unknown schedule timezone; next run unavailable")
				return nil
			}
			loc = l
		}
		next := parsed.Next(from.In(loc))
		if next.IsZero() {
			return nil
		}
		utc := next.UTC()
		return &utc

	case "interval":
		if spec.EverySeconds <= 0 {
			return nil
		}
		// Temporal aligns interval ticks to the Unix epoch, not to when the schedule was
		// created, so the next tick is the next epoch multiple — NOT from + every.
		every := int64(spec.EverySeconds)
		next := time.Unix(((from.UTC().Unix()/every)+1)*every, 0).UTC()
		return &next
	}
	return nil
}

// mutableSavedQuerySchedule is the shared preamble for every schedule mutation:
// validate the id, require admin on a saved query the caller may SEE, and load the live
// schedule.
func mutableSavedQuerySchedule(c *gin.Context) (*sql.DB, string, *SavedQuerySchedule, bool) {
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return nil, "", nil, false
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid saved query id"})
		return nil, "", nil, false
	}
	if !requireVisibleSavedQuery(c, id, modelRunMinRole) {
		return nil, "", nil, false
	}
	s, ok := loadSavedQuerySchedule(c, database, id)
	if !ok {
		return nil, "", nil, false
	}
	// Load first, then decide whether Temporal is even involved. This check used to
	// come before the load, which meant a scheduling-service outage made an event
	// trigger unpausable and undeletable — a schedule that has no Temporal counterpart
	// at all, held hostage by Temporal being down.
	if !s.EventDriven() && temporalClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "scheduling service not available"})
		return nil, "", nil, false
	}
	return database, id, s, true
}

// loadSavedQuerySchedule reads the live schedule for a saved query and computes
// Blocked. It filters on saved_query_id alone, so the caller must already have passed
// requireVisibleSavedQuery — the ROLE half by itself is what let a member read, pause,
// retarget and delete another member's private model's schedule.
func loadSavedQuerySchedule(c *gin.Context, database *sql.DB, savedQueryID string) (*SavedQuerySchedule, bool) {
	var s SavedQuerySchedule
	var specJSON []byte
	var pausedAt, autoPausedAt sql.NullTime
	var pausedReason, autoPausedReason sql.NullString
	// Nullable since 095: an event trigger has no Temporal schedule. Scanning it into a
	// plain string panics on that shape.
	var temporalID sql.NullString

	err := database.QueryRowContext(c.Request.Context(), `
		SELECT s.schedule_id, s.saved_query_id, s.schedule_type, s.schedule_spec, s.temporal_schedule_id,
		       s.status, s.run_as_user_id::text, s.created_by::text, s.created_at, s.updated_at,
		       s.paused_at, s.paused_reason, s.auto_paused_at, s.auto_paused_reason, s.upstream_policy
		FROM saved_query_schedules s
		WHERE s.saved_query_id = $1 AND s.status != 'deleted'
	`, savedQueryID).Scan(&s.ScheduleID, &s.SavedQueryID, &s.ScheduleType, &specJSON, &temporalID,
		&s.Status, &s.RunAsUserID, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&pausedAt, &pausedReason, &autoPausedAt, &autoPausedReason, &s.UpstreamPolicy)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no schedule for this saved query"})
		return nil, false
	}
	if err != nil {
		log.WithError(err).Error("failed to load saved query schedule")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load schedule"})
		return nil, false
	}

	if err := json.Unmarshal(specJSON, &s.ScheduleSpec); err != nil {
		log.WithError(err).WithField("schedule_id", s.ScheduleID).Warn("unreadable schedule spec")
	}
	s.TemporalScheduleID = temporalID.String
	// A second query rather than a join: the upstreams are one-to-many, and folding them
	// into the row above would return one copy of the schedule per upstream for a caller
	// that scans exactly one.
	if ups, upErr := loadScheduleUpstreams(c.Request.Context(), database, s.ScheduleID); upErr != nil {
		// Not fatal. The schedule itself loaded, and a page that renders it without its
		// upstream list is recoverable; refusing the whole read is not.
		log.WithError(upErr).WithField("schedule_id", s.ScheduleID).
			Warn("could not read this schedule's upstreams")
	} else {
		s.Upstreams = ups
	}
	if pausedAt.Valid {
		s.PausedAt = &pausedAt.Time
	}
	s.PausedReason = pausedReason.String
	if autoPausedAt.Valid {
		s.AutoPausedAt = &autoPausedAt.Time
	}
	s.AutoPausedReason = autoPausedReason.String

	if s.Status == "active" {
		if m, mErr := loadSavedQueryModel(c.Request.Context(), database, savedQueryID); mErr != nil {
			s.Blocked, s.BlockedReason = true, "this saved query's connection is no longer available in this workspace"
		} else if m.Materialization == matNone {
			s.Blocked, s.BlockedReason = true, "this query neither writes to a table nor runs as a statement, so scheduled runs do nothing"
		} else if _, refusal, aErr := authorizeModelRun(c.Request.Context(), database, m, s.RunAsUserID); aErr != nil {
			// This field is advisory UI text. An undecided check is not evidence the
			// schedule is blocked, and rendering a guess as "the run-as user is no longer
			// a member" is exactly the false accusation this split exists to prevent.
			log.WithError(aErr).WithField("schedule_id", s.ScheduleID).
				Warn("could not evaluate whether a model schedule is blocked; reporting it as unblocked")
		} else if refusal != "" {
			s.Blocked, s.BlockedReason = true, refusal
		}
	}

	return &s, true
}

// createTemporalModelSchedule registers the schedule with Temporal. Same policies as
// the pipeline version: SKIP overlap so a slow rebuild is never stacked on itself, and
// a 1-minute catchup window so a brief worker outage does not replay a backlog.
func createTemporalModelSchedule(ctx context.Context, scheduleID, savedQueryID, scheduleType string, spec ScheduleSpec) error {
	scheduleSpec := client.ScheduleSpec{}
	switch scheduleType {
	case "cron":
		scheduleSpec.CronExpressions = []string{spec.Cron}
		if spec.Timezone != "" {
			scheduleSpec.TimeZoneName = spec.Timezone
		}
	case "interval":
		scheduleSpec.Intervals = []client.ScheduleIntervalSpec{
			{Every: time.Duration(spec.EverySeconds) * time.Second},
		}
	}

	input := map[string]interface{}{
		"schedule_id":    scheduleID,
		"saved_query_id": savedQueryID,
	}

	_, err := temporalClient.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:   scheduleID,
		Spec: scheduleSpec,
		Action: &client.ScheduleWorkflowAction{
			Workflow:  "ScheduledModelRunWorkflow",
			TaskQueue: "pipeline-workflows",
			Args:      []interface{}{input},
			// The workflow only fires an HTTP activity and waits for the answer; the
			// rebuild itself runs inside the API gateway under modelRunTimeout (30m).
			//
			// This has to sit clear of the activity's own StartToCloseTimeout (35m), not
			// level with it. When the two coincide, a rebuild that overruns trips both at
			// the same instant and Temporal records the run timeout — an opaque failure
			// with no indication of which statement was still running. With headroom the
			// innermost timeout always fires first, so the history says the activity timed
			// out, which is the diagnosable answer.
			//
			// The headroom also has to fit the retries the policy actually promises. Those
			// are for fast failures — a gateway restart, a brief network fault — which
			// exhaust their three attempts in about a minute. A second FULL-LENGTH rebuild
			// deliberately does not fit: re-running a 30-minute build inside the same tick
			// would put two rebuilds of one target back to back for a fault the next tick
			// will retry anyway.
			WorkflowRunTimeout: modelRunTimeout + 15*time.Minute,
		},
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: 1 * time.Minute,
		Paused:        false,
	})
	return err
}
