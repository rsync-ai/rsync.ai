package sentinel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
	log "github.com/sirupsen/logrus"
)

// notify.go — the bridge from a sentinel issue to a user-visible alert.
//
// Every sentinel issue already lands in two places: a row in
// sentinel_active_issues, and a SENTINEL_ALERT on pipeline.domain.events.
// Neither reaches a person. The first is a table somebody has to go and look
// at; the second paints the pipeline overview of a page nobody has open at
// 3am. The bell, Slack and email all hang off ONE topic —
// rsync.notifications — and until this file existed the only sentinel that
// published to it was the WAL watchdog (cdc_wal_watchdog.go:369). Every other
// detector was pull-only: the system knew the data had stopped moving and
// told nobody.
//
// Publishing happens at the two chokepoints every issue already flows through
// (CDCSentinel.emitCDCIssue, BatchSentinel.emitBatchIssue) rather than at the
// dozen call sites, so a detector added later is wired by construction.
//
// Duplicate suppression is the notifier's job, not ours: these loops re-detect
// the same condition every tick, and the api-gateway drops a repeat of the
// same (pipeline, code, action_url) for as long as the alert's episode lasts
// (notifier.go recentDuplicate). The episode is the issue row: a raise carries
// its detected_at as opened_at, and the row is deleted when the alert closes,
// so a relapse opens a new one and is pushed again. Re-emitting every tick is
// what gets an alert through when its first notification was lost. A raise
// without opened_at (episodeExempt, or a row that could not be written) keeps
// the 60-minute repeat (dedupWindowMinutes).

// sentinelNotifyTopic is the one topic the api-gateway's notifier consumes for
// user alerts. Spelled here rather than imported because the qualification to
// a wire name happens inside kafka.Manager.
const sentinelNotifyTopic = "rsync.notifications"

// instanceScopeID is the pipeline_id an alert carries when it is about the
// deployment rather than about one pipeline. The api-gateway matches it against
// notifier.go's instanceScopeIDs, stores the row with a NULL pipeline_id
// (migration 112) and fans it out to every active admin — the path #1159 added
// for healthwatch's "system" alerts, which had been failing an owner lookup and
// being dropped every single time it fired.
const instanceScopeID = "instance"

// instanceActionURL is where an instance alert sends the reader. A component is
// not reachable from /pipelines/<id>, and notificationActionURL would render
// "/pipelines/instance" — a 404 for the one alert class that has no pipeline.
const instanceActionURL = "/admin/health"

// sentinelAlert is the user-facing half of an issue: the catalog code the
// api-gateway renders curated copy from (catalog.go), and a short message in
// the user's terms.
//
// The message is built from metadata rather than reusing the issue's
// description on purpose. A description is written for whoever is going to fix
// this — it names connectors, consumer groups and containers, and
// sink_write_rejected's embeds the destination's raw error, which can quote the
// row that was rejected. That text belongs in the issues table and the domain
// event. It does not belong in an email.
type sentinelAlert struct {
	code    string
	message func(metadata map[string]interface{}) string
}

// notifiableIssueTypes maps an issue type to its curated alert. Everything here
// means the same thing to a user — "your data has stopped moving" — which is
// why each one is worth interrupting somebody for.
var notifiableIssueTypes = map[IssueType]sentinelAlert{
	IssueTypeConnectorDown: {
		code: "CDC_CONNECTOR_DOWN",
		message: func(m map[string]interface{}) string {
			return "The connector that captures changes from your source has stopped and did not come back after a restart. " +
				"No new changes are being captured until it is running again."
		},
	},
	// The three below are what the IssueDetector files about components, as opposed
	// to about pipelines. They had no copy because nothing published them: the
	// detector wrote to sentinel_active_issues and the healer and stopped there
	// (KI-COMPONENT-HEALTH-ALERTS-NOBODY).
	//
	// None of them quotes metadata["last_error"], deliberately. A component's last
	// error is whatever the probe got back, and for an infrastructure component that
	// is frequently a connection string with the password in it. It belongs in the
	// issues row and the log, not in an email — the same reason sink_write_rejected
	// summarises rather than quotes.
	IssueTypeInfrastructureDown: {
		code: "INFRASTRUCTURE_DOWN",
		message: func(m map[string]interface{}) string {
			label := componentLabel(metaString(m, "component_id"))
			if svc, ok := coreServiceImpact[label]; ok {
				return "The " + svc.name + " service this deployment depends on is not responding. " + svc.impact
			}
			msg := "A core service this deployment depends on is not responding"
			if label != "" {
				msg = "The " + label + " service this deployment depends on is not responding"
			}
			return msg + ". " + dataPathImpact
		},
	},
	IssueTypeMissingHeartbeat: {
		code: "WORKER_HEARTBEAT_LOST",
		message: func(m map[string]interface{}) string {
			msg := "A background worker has stopped reporting in"
			if n := componentLabel(metaString(m, "component_id")); n != "" {
				msg = "The " + n + " worker has stopped reporting in"
			}
			if d := humanDuration(metaInt(m, "time_since_heartbeat")); d != "" {
				msg += " for " + d
			}
			return msg + ". Whatever it was responsible for is not being picked up, and the pipelines that depend on it " +
				"will go quiet rather than fail."
		},
	},
	IssueTypeConsumerGroupClosed: {
		code: "CONSUMER_GROUP_CLOSED",
		message: func(m map[string]interface{}) string {
			msg := "A queue reader in this deployment has shut down"
			if n := componentLabel(metaString(m, "component_id")); n != "" {
				msg = "The queue reader " + n + " has shut down"
			}
			return msg + " and is no longer processing messages. Changes already captured are safe in the queue, " +
				"but they will stop arriving at their destination until it is running again."
		},
	},
	IssueTypeCaptureStopped: {
		code: "CDC_CAPTURE_STOPPED",
		message: func(m map[string]interface{}) string {
			return "The connector that captures changes from your source is not running. " +
				"No new changes are being captured until it is back, and the destination looking caught up does not mean it is."
		},
	},
	IssueTypeSourceStreamStalled: {
		code: "CDC_SOURCE_STREAM_STALLED",
		message: func(m map[string]interface{}) string {
			msg := "No changes have reached your destination"
			if d := humanDuration(metaInt(m, "stalled_for_seconds")); d != "" {
				msg += " for " + d
			}
			return msg + ", even though the source connector still reports itself healthy. " +
				"Either the source stopped sending changes or the connector lost its place in the change stream."
		},
	},
	IssueTypeSinkWorkerAbsent: {
		code: "SINK_WORKER_ABSENT",
		message: func(m map[string]interface{}) string {
			return "Nothing is writing this run's rows to your destination. The export is still reading from the source, " +
				"but the rows are queueing up instead of landing. The run will not fail on its own — it will go quiet."
		},
	},
	IssueTypeRowsParked: {
		code: "CDC_ROWS_PARKED",
		message: func(m map[string]interface{}) string {
			msg := "Some changes from your source were set aside instead of being written to the destination"
			if n := metaInt(m, "new_dlq_rows"); n > 0 {
				msg = fmt.Sprintf("%d change(s) from your source were set aside instead of being written to the destination", n)
			}
			if tables := metaStrings(m, "tables"); len(tables) > 0 {
				msg += " (" + strings.Join(tables, ", ")
				if more := metaInt(m, "more_tables"); more > 0 {
					msg += fmt.Sprintf(" and %d more", more)
				}
				msg += ")"
			}
			return msg + ". The destination is missing them, and they will not arrive on their own: " +
				"the pipeline has moved on past them."
		},
	},
	IssueTypeSinkWriteRejected: {
		code: "SINK_WRITE_REJECTED",
		message: func(m map[string]interface{}) string {
			msg := "Your destination is rejecting writes"
			if t := metaString(m, "table_name"); t != "" {
				msg += " for table " + t
			}
			if n := metaInt(m, "rejected_batches"); n > 0 {
				msg += fmt.Sprintf(" (%d batch(es) so far)", n)
			}
			return msg + ". Rows are being read from the source and are NOT arriving at the destination."
		},
	},
}

// notifiableAlertTypes covers issues whose stored severity understates them.
// The terminal sink-wedge escalation is the loudest thing the auto-restart loop
// can say and it rides in through emitLagIssue as a warning
// (cdc_sink_autorestart.go:190), so severity alone would never surface it.
var notifiableAlertTypes = map[string]sentinelAlert{
	// The sink-drain alarm escalated: its high_lag issue turns critical once the sink has
	// committed nothing for DefaultSinkDrainNotifyAfter (cdc_sink_drain.go). Keyed on the
	// alert type rather than added to notifiableIssueTypes because high_lag is shared with
	// the source-lag and auto-restart alarms, which must not inherit this copy.
	//
	// No consumer group in the copy, unlike the issue's description: a reader of an email
	// cannot act on a Kafka group name.
	"sink_not_draining": {
		code: "CDC_SINK_NOT_DRAINING",
		message: func(m map[string]interface{}) string {
			msg := "Changes from your source have stopped reaching the destination"
			if d := humanDuration(metaInt(m, "stalled_for_seconds")); d != "" {
				msg = "No changes from your source have reached the destination for " + d
			}
			msg += ". They are still being captured and are held safely"
			if n := metaInt(m, "total_lag"); n > 0 {
				msg += fmt.Sprintf(" (%d waiting)", n)
			}
			return msg + ", but nothing is writing them, and they will keep building up until the writer is running again."
		},
	},
	// The source-lag alarm escalated: its high_lag issue turns critical once source-side lag
	// has lasted, with the connector's committed position frozen, for
	// DefaultSourceLagNotifyAfter (cdc_source_lag.go). The duration is how long the position
	// has not moved, not how long the lag has been over its threshold: a connector that was
	// reading slowly before it stopped has been stuck for the shorter of the two.
	//
	// No slot or connector name, and no promise that nothing is lost: a MySQL server can
	// discard binlog that has not been read yet.
	"source_lag_sustained": {
		code: "CDC_SOURCE_LAG_HIGH",
		message: func(m map[string]interface{}) string {
			msg := "rsync.ai has stopped reading new changes from your source database"
			if d := humanDuration(metaInt(m, "frozen_for_seconds")); d != "" {
				msg = "rsync.ai has not read any new changes from your source database for " + d
			}
			msg += ", while changes there keep building up"
			if mb := metaInt(m, "lag_mb"); mb > 0 {
				msg += fmt.Sprintf(" (about %d MB so far)", mb)
			}
			return msg + ". Until reading resumes, they do not reach the destination."
		},
	},
	"sink_wedged_escalation": {
		code: "CDC_SINK_WEDGED",
		message: func(m map[string]interface{}) string {
			msg := "The writer for this pipeline is stuck and has not recovered"
			if n := metaInt(m, "attempts"); n > 0 {
				msg += fmt.Sprintf(" after %d automatic restarts", n)
			}
			return msg + ". Your changes are still being captured safely, but they are not reaching the destination " +
				"and will keep building up until somebody looks at it."
		},
	},
	// The respawn rung's terminal verdict (cdc_sink_respawn.go sinkRespawnEscalate): the sink
	// container holds no worker for this pipeline and restarting it did not bring one back.
	// Filed as a high_lag warning, so like the wedge it reaches a user only through this entry.
	"sink_absent": {
		code: "CDC_SINK_ABSENT",
		message: func(m map[string]interface{}) string {
			msg := "Nothing is writing this pipeline's changes to the destination"
			if n := metaInt(m, "attempts"); n > 0 {
				msg += fmt.Sprintf(", and %d automatic restarts did not bring the writer back", n)
			}
			return msg + ". Your changes are still being captured safely, but none reach the destination " +
				"until the writer is started again."
		},
	},
}

// sentinelAlertFor decides whether an issue is worth interrupting a user for,
// and with what copy.
//
// The default is to alert, not to stay quiet. A critical issue with no curated
// entry still publishes, and the api-gateway falls back to a humanized event
// type ("Sink Worker Absent") rather than dropping it — the same bias
// categories.go takes for an uncategorized code. Silence is how this whole
// class of bug happened; a slightly machine-shaped headline is recoverable.
func sentinelAlertFor(issueType IssueType, alertType string, severity IssueSeverity) (sentinelAlert, bool) {
	// Alert type first: it is the only thing that distinguishes a terminal
	// escalation from the lag alarm it borrows its type and severity from.
	if a, ok := notifiableAlertTypes[strings.TrimSpace(alertType)]; ok {
		return a, true
	}
	if severity != IssueSeverityCritical {
		return sentinelAlert{}, false
	}
	if a, ok := notifiableIssueTypes[issueType]; ok {
		return a, true
	}
	return sentinelAlert{}, true
}

// publishSentinelAlert emits one user notification for an issue, if that issue
// is one a user should hear about. Every argument but the manager comes
// straight from the issue the caller just persisted; openedAt is its row's
// detected_at, zero when the row could not be written, or when the caller
// knows the episode opened (emitCDCIssueOpenedAt).
func publishSentinelAlert(
	mgr *kafka.Manager,
	issueType IssueType,
	severity IssueSeverity,
	alertType, pipelineID, description string,
	metadata map[string]interface{},
	openedAt time.Time,
) {
	if strings.TrimSpace(pipelineID) == "" {
		return
	}
	notification, ok := buildSentinelNotification(issueType, severity, alertType, pipelineID, description, metadata)
	if !ok {
		return
	}
	stampEpisodeOpened(notification, openedAt)
	emitNotification(mgr, pipelineID, issueType, notification)
}

// episodeExempt are the alerts whose every raise is news, so each keeps the
// api-gateway's 60-minute repeat instead of being pushed once per episode. Keyed
// by what the gateway dedups on: the catalog code, or the event type without one.
// Every entry is a curated code with a reason (TestEpisodeExemptAlertsAreCuratedAndSayWhy).
var episodeExempt = map[string]string{
	"CDC_ROWS_PARKED": "raised again only when the parked total grows (checkRowsParked), " +
		"so each repeat is more changes missing from the destination",
	"CDC_CONNECTOR_DOWN": "raised once per escalation already (restart state's escalated flag), " +
		"and its row is kept across Stop and Resume (alertsClosedOnStop), so its detected_at " +
		"would date a failure after Resume to the episode before it",
	"CDC_SINK_WEDGED": "raised once per escalation already (sinkRestartEscalate), " +
		"and its row is kept across Stop and Resume (alertsClosedOnStop), so its detected_at " +
		"would date a wedge after Resume to the episode before it",
}

// stampEpisodeOpened says when the alert's episode opened, so the api-gateway
// drops its re-raises for the whole episode rather than for an hour. The zero time
// (the row was not written) and an exempt alert are left without one.
func stampEpisodeOpened(notification map[string]interface{}, openedAt time.Time) {
	if openedAt.IsZero() {
		return
	}
	code, _ := notification["_code"].(string)
	if code == "" {
		code, _ = notification["type"].(string)
	}
	if _, exempt := episodeExempt[code]; exempt {
		return
	}
	notification["opened_at"] = openedAt.UTC().Format(time.RFC3339)
}

// publishInstanceIssueAlert is the sentinel agent's side of the instance lane:
// an IssueDetector finding is about a component, so it has no pipeline to hang
// off and none of the pipeline-scoped publishers ever applied to it.
func (a *Agent) publishInstanceIssueAlert(issue *Issue) {
	if a == nil {
		return
	}
	publishInstanceAlert(a.kafkaManager, issue)
}

// publishInstanceAlert emits one user notification about a COMPONENT, scoped to
// the instance rather than to a pipeline.
//
// Separate from publishSentinelAlert because that function requires a pipeline id
// and returns early without one — correctly, for its callers, since a pipeline
// alert with no pipeline is a bug. Component issues have no pipeline by nature,
// so they take the instance scope instead and land on every admin's bell.
func publishInstanceAlert(mgr *kafka.Manager, issue *Issue) {
	if issue == nil {
		return
	}

	// Copied rather than annotated in place: this map belongs to the issue already
	// sitting in a.activeIssues, and triggerHealing reads it from another goroutine.
	metadata := make(map[string]interface{}, len(issue.Metadata)+2)
	for k, v := range issue.Metadata {
		metadata[k] = v
	}
	// The detectors put the component in the Issue struct, not in its metadata, and
	// the copy functions above only ever see metadata.
	metadata["component_id"] = issue.ComponentID
	metadata["component_type"] = string(issue.ComponentType)

	notification, ok := buildSentinelNotification(
		issue.Type, issue.Severity, "", instanceScopeID, issue.Description, metadata)
	if !ok {
		return
	}
	notification["action_url"] = instanceActionURL

	// Without this, the second component to fail is silently dropped. The
	// api-gateway dedups on (pipeline, code, action_url, subject) for 60 minutes,
	// and every instance alert shares the first three: "postgres is down" and
	// "kafka is down" would be one alert, and the one you got would be whichever
	// lost service was detected first.
	if se, ok := notification["error"].(*diagnose.StructuredError); ok && se != nil {
		se.DedupSubject = issue.ComponentID
	}

	emitNotification(mgr, instanceScopeID, issue.Type, notification)
}

// emitNotification marshals and produces one notification, and swallows its own
// failures. A nil manager is the unit-test shape (and the pre-wiring startup
// window), and sends nothing.
//
// Failures here are logged and swallowed: the durable record is the row the
// caller already wrote, and a sentinel loop must not stop detecting because a
// broker was briefly unavailable.
//
// It is a variable so a test can see what would be sent without a broker.
var emitNotification = func(mgr *kafka.Manager, key string, issueType IssueType, notification map[string]interface{}) {
	if mgr == nil {
		return
	}
	code, _ := notification["_code"].(string)
	delete(notification, "_code")

	b, err := json.Marshal(notification)
	if err != nil {
		log.WithError(err).WithField("pipeline_id", key).
			Warn("🛡️ sentinel: could not marshal user notification")
		return
	}
	if err := mgr.Produce(sentinelNotifyTopic, []byte(key), b); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"pipeline_id": key,
			"code":        code,
		}).Warn("🛡️ sentinel: could not publish user notification")
		return
	}
	log.WithFields(log.Fields{
		"pipeline_id": key,
		"code":        code,
		"issue_type":  string(issueType),
	}).Info("🛡️ sentinel: user notification published")
}

// buildSentinelNotification renders the rsync.notifications payload for an
// issue, or reports false when that issue is not one to interrupt anybody for.
// Split from the publish so the policy and the wire shape are testable without
// a broker — the sentinels hold a concrete *kafka.Manager, which is why nothing
// in this package unit-tests a produce.
//
// The returned map carries the resolved catalog code under "_code" for the
// caller's log line; publishSentinelAlert strips it before marshalling.
func buildSentinelNotification(
	issueType IssueType,
	severity IssueSeverity,
	alertType, pipelineID, description string,
	metadata map[string]interface{},
) (map[string]interface{}, bool) {
	alert, ok := sentinelAlertFor(issueType, alertType, severity)
	if !ok {
		return nil, false
	}

	// An uncurated critical falls back to the issue type as the event type,
	// which the api-gateway humanizes, and to the description it was given.
	code := alert.code
	message := description
	if alert.message != nil {
		message = alert.message(metadata)
	}

	notification := map[string]interface{}{
		"type":        string(issueType),
		"pipeline_id": pipelineID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  notificationActionURL(pipelineID),
		"message":     message,
		// Everything that gets this far means the data stopped. The stored
		// severity does not decide this: sink_wedged_escalation is filed as a
		// warning and is one of the worst things in here.
		"severity": "critical",
		"_code":    code,
	}
	if code != "" {
		// Audience matters: the api-gateway hides audience=developer rows from
		// the bell. These are all things the pipeline's owner has to act on.
		se := diagnose.NewStructuredError(code, diagnose.FailureTypeSystemError, diagnose.AudienceUser, message)
		se.Severity = diagnose.SeverityError
		notification["error"] = se
	}
	return notification, true
}

// dataPathImpact is what an INFRASTRUCTURE_DOWN alert says about a service on every
// pipeline's data path. It used to end "nothing will resume on its own once it is back",
// which was false: a streaming pipeline continues from its last position and a schedule
// runs at its next time. What can need a person is narrower, and this names it.
//
// The api-gateway's Kafka watch says the same thing in the same words
// (api-gateway/internal/handlers/core_service_watch.go dataPathImpact). The two are in
// different modules, so neither can import the other: change both or neither.
const dataPathImpact = "Pipelines cannot move data while it is down. Once it is back, streaming pipelines " +
	"continue from where they left off and schedules run at their next time, but a run that failed, " +
	"a pipeline that was stopped, or a scheduled time that passed during the outage may need you to start it again."

// coreServiceImpact is what an INFRASTRUCTURE_DOWN alert names a service and says stops
// while it is down, keyed by componentLabel. These are the services service_probes.go
// checks, and Kafka Connect, which only CDC capture uses. A label missing here —
// postgresql, kafka — gets dataPathImpact, which is right for those two and wrong for
// these: an LLM service outage does not stop a single pipeline, and telling every admin
// their data has stopped moving would be a false page they learn to ignore.
var coreServiceImpact = map[string]struct{ name, impact string }{
	"redis": {"Redis",
		"New pipeline runs cannot start or move forward while it is down: workers receive their planning, validation and execution steps through it."},
	"temporal": {"Temporal",
		"Manual and scheduled pipeline runs cannot start while it is down."},
	"temporal-adapter": {"Temporal adapter",
		"Pipeline runs cannot start or progress while it is down."},
	"connector-deployer": {"connector deployer",
		"Connectors cannot be started or restarted while it is down."},
	"tool-generator": {"tool generator",
		"Connectors cannot be started or restarted while it is down."},
	"llm-service": {"AI",
		"AI features are unavailable while it is down. Running pipelines keep moving data."},
	// Only CDC capture runs in Kafka Connect (infra_preflight.go probes it for CDC runs
	// alone), so a batch run is not affected and the alert must not say it is.
	"kafka-connect": {"Kafka Connect",
		"Streaming pipelines cannot capture new changes from their sources while it is down; scheduled and " +
			"manual runs are not affected. Once it is back, streaming pipelines continue from where they left off, " +
			"but one that was stopped during the outage may need you to start it again."},
}

// componentLabel turns a component id into something worth putting in a sentence.
// Component ids are "<type>:<name>" — "infrastructure:postgresql",
// "kafka_consumer:rsync.cdc-orders" — and the type is already carried by the copy
// that quotes the label.
func componentLabel(componentID string) string {
	id := strings.TrimSpace(componentID)
	if _, name, found := strings.Cut(id, ":"); found {
		return strings.TrimSpace(name)
	}
	return id
}

// metaString reads a string out of an issue's metadata, or "" when it is absent
// or some other type.
func metaString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// metaInt reads a count out of an issue's metadata. Sentinel metadata is built
// in Go and never round-trips through JSON before it gets here, so the widths
// are whatever the caller happened to use; float64 is covered anyway in case a
// future caller does decode one.
// metaStrings reads a list of names, whether the detector put a []string in the map or it
// came back from JSON as []interface{}.
func metaStrings(m map[string]interface{}, key string) []string {
	if m == nil {
		return nil
	}
	switch v := m[key].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

func metaInt(m map[string]interface{}, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

// humanDuration turns a span in seconds into the roughest unit that still says
// something true: a user does not need "4021 seconds", they need "over an hour".
func humanDuration(seconds int64) string {
	switch {
	case seconds <= 0:
		return ""
	case seconds < 120:
		return fmt.Sprintf("%d seconds", seconds)
	case seconds < 5400:
		return fmt.Sprintf("%d minutes", seconds/60)
	default:
		return fmt.Sprintf("%d hours", seconds/3600)
	}
}
