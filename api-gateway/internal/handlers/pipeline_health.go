package handlers

import (
	"database/sql"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"api-gateway/internal/db"
	"api-gateway/internal/security"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

// PipelineHealth answers "why is this pipeline stale?" in one read: the state
// the pipeline list shows, what (if anything) is holding it, the last run and
// the last success, and the schedule. It carries states, times and classes
// only — never a row count, a byte size or the raw error text — so the agent
// gateway can return it as it is (docs/internal/agent-gateway-design.md §6).
type PipelineHealth struct {
	PipelineID    string                  `json:"pipeline_id"`
	Name          string                  `json:"name"`
	Mode          string                  `json:"mode"`  // cdc | batch
	State         string                  `json:"state"` // the list page's derived status
	Blocking      *PipelineBlocking       `json:"blocking,omitempty"`
	LastRun       *PipelineHealthRun      `json:"last_run,omitempty"`
	LastSuccessAt *time.Time              `json:"last_success_at,omitempty"`
	Schedule      *PipelineHealthSchedule `json:"schedule,omitempty"`
	CheckedAt     time.Time               `json:"checked_at"`
}

// PipelineBlocking is the one reason the pipeline is not moving, most
// specific first. Hint is fixed text chosen by Kind (and ErrorClass), never
// text read from the database.
type PipelineBlocking struct {
	Kind       string     `json:"kind"`
	ReasonType string     `json:"reason_type,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Hint       string     `json:"hint"`
}

type PipelineHealthRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ErrorClass string     `json:"error_class,omitempty"`
}

type PipelineHealthSchedule struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	Cron         string `json:"cron,omitempty"`
	EverySeconds int64  `json:"every_seconds,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
}

var (
	// A blocking_reason_type is a code the workflow writes ("paused_by_user");
	// anything that is not code-shaped is dropped rather than passed on.
	healthCodeRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	healthCronRe     = regexp.MustCompile(`^[A-Za-z0-9*/,?#@ \-]{1,100}$`)
	healthTimezoneRe = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)
)

// SafeCode, SafeCron and SafeTimezone return s when it has the shape its field
// should have, and "" otherwise. The agent gateway passes schedule and reason
// fields through them, so it and this read accept exactly the same values.
func SafeCode(s string) string     { return shaped(healthCodeRe, s) }
func SafeCron(s string) string     { return shaped(healthCronRe, s) }
func SafeTimezone(s string) string { return shaped(healthTimezoneRe, s) }

func shaped(re *regexp.Regexp, s string) string {
	if s = strings.TrimSpace(s); re.MatchString(s) {
		return s
	}
	return ""
}

// GetPipelineHealth handles GET /api/v1/pipelines/:id/health. Any member of
// the pipeline's workspace may read it; a pipeline in another workspace is 404.
func GetPipelineHealth(c *gin.Context) {
	pipelineID := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(pipelineID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Pipeline not found"})
		return
	}
	workspaceID, ok := requirePipelineWorkspaceRole(c, pipelineID, security.WSViewer)
	if !ok {
		return
	}
	database := db.GetDB()
	if database == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Database not connected"})
		return
	}

	// One read. The derived status is the list page's own CASE, so the agent
	// and the list can never disagree about whether a pipeline failed; it
	// reads the aliases p, pp, le and so, which this query provides.
	query := `
WITH le AS (
  SELECT e.id AS execution_id, e.status AS execution_status, e.start_time AS started_at,
         e.end_time AS completed_at, e.error_message
  FROM executions e
  WHERE e.pipeline_id = $1
  ORDER BY e.start_time DESC
  LIMIT 1
),
so AS (
  SELECT ps.schedule_id, ps.schedule_type, ps.status AS schedule_status,
         ps.schedule_spec->>'cron' AS cron, ps.schedule_spec->>'every_seconds' AS every_seconds,
         COALESCE(ps.schedule_spec->>'timezone', 'UTC') AS timezone
  FROM pipeline_schedules ps
  WHERE ps.pipeline_id = $1 AND ps.status != 'deleted'
  ORDER BY ps.created_at DESC
  LIMIT 1
)
SELECT p.name, ` + pipelineRowIsCDCSQL + ` AS is_cdc,
       pp.status, pp.blocking_reason_type, pp.updated_at,
       le.execution_id, le.execution_status, le.started_at, le.completed_at, le.error_message,
       (SELECT MAX(COALESCE(s.end_time, s.start_time)) FROM executions s
         WHERE s.pipeline_id = p.id AND s.status IN ('success', 'completed')) AS last_success_at,
       so.schedule_type, so.schedule_status, so.cron, so.every_seconds, so.timezone,
       EXISTS (
         SELECT 1 FROM pipeline_dependencies d
         JOIN pipeline_dependency_health h ON h.dependency_id = d.id
         WHERE d.pipeline_id = p.id AND h.status = 'unhealthy'
       ) AS dependency_unhealthy,
       ` + pipelineDerivedStatusCaseSQL + ` AS derived_status
FROM pipelines p
LEFT JOIN pipeline_progress pp ON pp.pipeline_id = p.id
LEFT JOIN le ON TRUE
LEFT JOIN so ON TRUE
WHERE p.id = $1 AND p.workspace_id = $2
`
	var (
		name, derived                                          string
		isCDC, depUnhealthy                                    bool
		ppStatus, ppReason                                     sql.NullString
		ppUpdated                                              sql.NullTime
		execID, execStatus, execError                          sql.NullString
		execStart, execEnd, lastSuccess                        sql.NullTime
		schedType, schedStatus, schedCron, schedEvery, schedTZ sql.NullString
	)
	err := database.QueryRow(query, pipelineID, workspaceID).Scan(
		&name, &isCDC,
		&ppStatus, &ppReason, &ppUpdated,
		&execID, &execStatus, &execStart, &execEnd, &execError,
		&lastSuccess,
		&schedType, &schedStatus, &schedCron, &schedEvery, &schedTZ,
		&depUnhealthy, &derived,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "Pipeline not found"})
		return
	}
	if err != nil {
		log.WithError(err).WithField("pipeline_id", pipelineID).Warn("pipeline health read failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Could not read the pipeline's health"})
		return
	}

	h := PipelineHealth{
		PipelineID: pipelineID,
		Name:       name,
		Mode:       "batch",
		State:      derived,
		CheckedAt:  time.Now().UTC().Truncate(time.Second),
	}
	if isCDC {
		h.Mode = "cdc"
	}
	if lastSuccess.Valid {
		t := lastSuccess.Time.UTC()
		h.LastSuccessAt = &t
	}
	if execID.Valid {
		run := &PipelineHealthRun{
			ID:         execID.String,
			Status:     execStatus.String,
			ErrorClass: RunErrorClass(execStatus.String, execError.String),
		}
		if execStart.Valid {
			t := execStart.Time.UTC()
			run.StartedAt = &t
		}
		if execEnd.Valid {
			t := execEnd.Time.UTC()
			run.EndedAt = &t
		}
		h.LastRun = run
	}
	if schedType.Valid {
		s := &PipelineHealthSchedule{Type: schedType.String, Status: schedStatus.String}
		s.Cron = SafeCron(schedCron.String)
		if n, err := strconv.ParseFloat(strings.TrimSpace(schedEvery.String), 64); err == nil && n > 0 {
			s.EverySeconds = int64(n)
		}
		s.Timezone = SafeTimezone(schedTZ.String)
		h.Schedule = s
	}
	h.Blocking = pipelineBlocking(h, ppStatus.String, ppReason.String, ppUpdated, depUnhealthy)

	c.JSON(http.StatusOK, h)
}

// pipelineBlocking picks the one reason the pipeline is not moving, or nil.
func pipelineBlocking(h PipelineHealth, ppStatus, ppReason string, ppUpdated sql.NullTime, depUnhealthy bool) *PipelineBlocking {
	since := func(t sql.NullTime) *time.Time {
		if !t.Valid {
			return nil
		}
		u := t.Time.UTC()
		return &u
	}
	switch {
	case strings.TrimSpace(ppStatus) == "waiting_for_user":
		b := &PipelineBlocking{
			Kind:  "waiting_for_user",
			Since: since(ppUpdated),
			Hint:  "A run is waiting for a person to answer a prompt on the pipeline page (for example, which tables to copy). Scheduled runs are skipped until someone answers.",
		}
		b.ReasonType = SafeCode(ppReason)
		return b
	case h.State == "paused":
		return &PipelineBlocking{Kind: "paused", Hint: "The pipeline is paused. A person resumes it on the pipeline page."}
	case h.State == "stopped":
		return &PipelineBlocking{Kind: "stopped", Hint: "The pipeline was stopped. A person starts it again on the pipeline page."}
	case h.Mode == "cdc" && depUnhealthy:
		return &PipelineBlocking{Kind: "dependency_unhealthy", Hint: "A service the change stream depends on (capture, connector or sink) is unhealthy, so no changes are arriving."}
	case h.State == "failed":
		b := &PipelineBlocking{Kind: "failed", Hint: runErrorHints["unclassified"]}
		if h.LastRun != nil {
			if b.ErrorClass = h.LastRun.ErrorClass; b.ErrorClass == "" {
				b.ErrorClass = "unclassified"
			}
			b.Hint = runErrorHints[b.ErrorClass]
			b.Since = h.LastRun.EndedAt
		}
		return b
	case h.LastRun == nil && h.Mode == "batch" && (h.Schedule == nil || h.Schedule.Status != "active"):
		return &PipelineBlocking{Kind: "never_run", Hint: "The pipeline has never run and has no active schedule. A person runs it from the pipeline page."}
	case h.Mode == "batch" && h.Schedule != nil && h.Schedule.Status != "active":
		return &PipelineBlocking{Kind: "schedule_inactive", Hint: "The pipeline's schedule is not active, so it runs only when a person runs it."}
	case h.Mode == "batch" && h.Schedule == nil && h.State == "idle":
		return &PipelineBlocking{Kind: "no_schedule", Hint: "The pipeline has no schedule, so it runs only when a person runs it."}
	}
	return nil
}

// Run error classes. A class is all the agent gateway ever says about why a
// run failed: the raw error text can quote row values (a failed INSERT) or
// connection details.
var runErrorHints = map[string]string{
	"auth":             "The source or destination rejected the credentials. A person reconnects or re-enters them on the connection page, then tests the connection.",
	"permission":       "The database user lacks a privilege the run needs. Grant it, then re-run.",
	"network":          "rsync.ai could not reach a host. Check the host, port and firewall, and that the database is up.",
	"timeout":          "A step ran out of time. Re-run; if it repeats, check the load on the source.",
	"schema":           "A table or column the run expected is missing or has changed, or a value did not fit its destination column's type. Check the pipeline's tables and its schema changes.",
	"cdc_setup":        "Change capture is not set up on the source (publication, replication slot, WAL or binlog settings, or a primary key). See the connector's CDC setup guide.",
	"quota":            "A plan or provider limit was reached. Wait, or raise the limit.",
	"rows_not_written": "The run read rows it did not write to the destination (rsync.ai's post-run check). Open the run's table stats on the pipeline page.",
	"orchestration":    "rsync's own run machinery failed. Re-run; if it repeats, report it with the run id.",
	"cancelled":        "The run was stopped by a person or by rsync.ai.",
	"unclassified":     "The error matched no known class. A person can read it on the run's page.",
}

// RunErrorHint is the fixed next-step text for a class from RunErrorClass.
func RunErrorHint(class string) string {
	if h, ok := runErrorHints[class]; ok {
		return h
	}
	return runErrorHints["unclassified"]
}

// runErrorRules are matched against the lower-cased error text; the first
// match wins, so the specific classes come before the general ones
// ("permission denied for replication" is CDC setup, not a missing grant).
// HTTP codes are matched as whole words so an id containing 401 is not auth.
// The text is matched after runErrorWrappers are removed.
var runErrorRules = []struct {
	class string
	re    *regexp.Regexp
}{
	{"cancelled", regexp.MustCompile(`\b(cancell?ed by|stopped by user|workflow (execution )?cancell?ed)\b`)},
	{"cdc_setup", regexp.MustCompile(`replication slot|\bpublication\b|wal_level|logical (replication|decoding)|replica identity|\bbinlog|change stream|\boplog\b|replication role|(no|missing|without a|requires a) primary key|primary key (is )?required`)},
	{"auth", regexp.MustCompile(`authentication failed|\bunauthori[sz]ed\b|\b401\b|invalid credentials|invalid_grant|invalid_client|token (has )?expired|expired token|login failed|invalid api key|\brole "?[a-z0-9_.@-]+"? does not exist`)},
	{"permission", regexp.MustCompile(`permission denied|\bforbidden\b|\b403\b|access denied|insufficient privilege|not authori[sz]ed|must be (the )?(owner|superuser)`)},
	{"quota", regexp.MustCompile(`\bquota\b|rate limit|too many requests|\b429\b|resource[ _]exhausted|plan limit`)},
	{"timeout", regexp.MustCompile(`timeout|timed out|deadline exceeded`)},
	{"network", regexp.MustCompile(`connection refused|no such host|connection reset|network is unreachable|no route to host|dial tcp|could not connect|could not translate host|tls handshake|broken pipe`)},
	{"schema", regexp.MustCompile(`does not exist|no such table|(unknown|undefined) (column|table)|invalid input syntax|type mismatch|incompatible type|schema mismatch|cannot cast|column .* (not found|missing)|` +
		`(could not|cannot|failed to|tried to) convert|conversion failed|could not parse|invalid literal|value too long|data truncated|` +
		`(integer|bigint|smallint|value) out of range|out of range (for type|value)|numeric (field|value) overflow`)},
	{"rows_not_written", regexp.MustCompile(`partially dropped rows|dropped rows|silent[_ ](partial[_ ])?drop|rows? (were )?not written|\bdlq\b`)},
	{"orchestration", regexp.MustCompile(`\bworkflow\b|\btemporal\b|\bactivity\b|heartbeat|\bpanic\b`)},
}

// runErrorWrappers are the text Temporal wraps around every activity and
// workflow error. They name the machinery, not the cause, so matching them
// made every wrapped error "orchestration" whatever it said inside.
var runErrorWrappers = regexp.MustCompile(`activity error \([^)]*\):?\s*|workflow execution error \([^)]*\):?\s*|\(type: [a-z0-9_#]+, retryable: (true|false)\)`)

var runFailedStatuses = map[string]bool{
	"failed": true, "error": true, "credential_check_failed": true,
	"silent_drop_detected": true, "silent_partial_drop_detected": true,
	"cancelled": true, "canceled": true, "stopped": true,
}

// RunErrorClass names why a run failed from its status and error text. It
// returns "" for a run that did not fail. The text is only matched, never
// returned.
func RunErrorClass(status, message string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	if !runFailedStatuses[s] {
		return ""
	}
	switch s {
	case "credential_check_failed":
		return "auth"
	case "silent_drop_detected", "silent_partial_drop_detected":
		return "rows_not_written"
	case "cancelled", "canceled", "stopped":
		return "cancelled"
	}
	m := strings.ToLower(message)
	if strings.TrimSpace(m) == "" {
		return "unclassified"
	}
	if inner := runErrorWrappers.ReplaceAllString(m, " "); strings.TrimSpace(inner) != "" {
		m = inner
	}
	for _, rule := range runErrorRules {
		if rule.re.MatchString(m) {
			return rule.class
		}
	}
	return "unclassified"
}
