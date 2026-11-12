package sentinel

import (
	"strings"
	"time"

	"github.com/rsync-ai/backend-orchestrator/internal/kafka"
	"github.com/rsync-ai/backend-orchestrator/pkg/diagnose"
)

// resolved_notice.go — "rsync.ai no longer sees this problem" (decision D2).
//
// An alert this package pushed to the owner and then cleared by itself is followed by
// one short info notice, so nobody is left watching a problem that is gone. Only the
// alerts on resolvedKinds below have one; every other clear stays silent, on purpose.
//
// This file only says WHEN an alert closed and what it was. Whether the notice is
// delivered is the api-gateway's call (notifier/resolved.go): it drops one for an
// alert nobody was told about or that was open under five minutes, and sends the rest
// to the same recipients, channels and mutes as the alert. So a producer here may
// publish for every clear of a pushed kind and leave the policy to one place.

// resolvedEventType is the notification type of every resolved notice. The catalog
// renders them by the envelope's code, so the type only has to be stable.
const resolvedEventType = "alert_resolved"

// resolvedKind is one clearable alert and the notice that answers it.
type resolvedKind struct {
	// issueType is the alert's issue type, for the log line only.
	issueType IssueType
	// resolvedCode is the catalog code of the notice (api-gateway notifier/catalog.go).
	// Named so it cannot be mistaken for a raise code by TestEverySentinelCodeHasCopy.
	resolvedCode string
	message      string
	// onlyCritical: the alert was pushed only at critical severity, so a row cleared
	// at a lower one never reached anybody.
	onlyCritical bool
}

// The resolved notices a pipeline-scoped alert can have, keyed by what the issue id is
// built from. Each message states only what is true once the alert has cleared.
var (
	resolvedStalled = resolvedKind{
		issueType:    IssueTypeSourceStreamStalled,
		resolvedCode: "CDC_SOURCE_STREAM_STALLED_RESOLVED",
		message:      "Changes from your source are reaching your destination again.",
		onlyCritical: true,
	}
	resolvedCapture = resolvedKind{
		issueType:    IssueTypeCaptureStopped,
		resolvedCode: "CDC_CAPTURE_STOPPED_RESOLVED",
		message:      "The connector that captures changes from your source is running again.",
		onlyCritical: true,
	}
	resolvedConnector = resolvedKind{
		issueType:    IssueTypeConnectorDown,
		resolvedCode: "CDC_CONNECTOR_DOWN_RESOLVED",
		message:      "The connector that captures changes from your source is running again.",
		onlyCritical: true,
	}
	resolvedSinkDrain = resolvedKind{
		issueType:    IssueTypeHighLag,
		resolvedCode: "CDC_SINK_NOT_DRAINING_RESOLVED",
		message:      "Changes are reaching your destination again.",
		onlyCritical: true,
	}
	// The WAL alert is pushed at warning too, so any severity counts.
	resolvedWAL = resolvedKind{
		issueType:    IssueTypeHighLag,
		resolvedCode: "CDC_WAL_PRESSURE_RESOLVED",
		message:      "The change log kept for this pipeline's replication slot is back to a safe size.",
	}
)

// resolvedKindForIssue returns the resolved notice for a pipeline-scoped lag-issue id,
// or false when clearing that id is not announced. Exact ids, not prefixes: the source
// lag, absent sink and wedged sink alerts sit next to these and are not in the allowlist.
func resolvedKindForIssue(issueID, pipelineID string) (resolvedKind, bool) {
	if strings.TrimSpace(pipelineID) == "" {
		return resolvedKind{}, false
	}
	switch issueID {
	case sourceStalledIssueID(pipelineID):
		return resolvedStalled, true
	case captureStoppedIssueID(pipelineID):
		return resolvedCapture, true
	case connectorIssueID(pipelineID):
		return resolvedConnector, true
	case sinkLagIssueID(pipelineID):
		return resolvedSinkDrain, true
	}
	return resolvedKind{}, false
}

// buildResolvedNotification renders the rsync.notifications payload for a cleared
// alert. subject must equal the alert's dedup subject ("" for a pipeline alert, the
// component id for an instance one) and actionURL its action_url, or the api-gateway
// cannot tell which alert this answers. openedAt is when the episode began; the zero
// time is left out.
func buildResolvedNotification(k resolvedKind, message, pipelineID, actionURL, subject string, openedAt time.Time) map[string]interface{} {
	if message == "" {
		message = k.message
	}
	se := diagnose.NewStructuredError(k.resolvedCode, diagnose.FailureTypeTransientError, diagnose.AudienceUser, message)
	se.Severity = diagnose.SeverityInfo
	se.DedupSubject = subject

	n := map[string]interface{}{
		"type":        resolvedEventType,
		"pipeline_id": pipelineID,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  actionURL,
		"message":     message,
		"severity":    "info",
		"error":       se,
		"_code":       k.resolvedCode,
	}
	if !openedAt.IsZero() {
		n["opened_at"] = openedAt.UTC().Format(time.RFC3339)
	}
	return n
}

// publishResolved sends the notice for a cleared alert. key is the Kafka key: the
// pipeline id, or the slot name for the WAL watchdog, matching how its alert was keyed
// so the two stay ordered on one partition. A nil manager is the unit-test shape.
//
// It is a variable so a test can see what would be sent without a broker.
var publishResolved = func(mgr *kafka.Manager, key string, k resolvedKind, pipelineID, actionURL, subject, message string, openedAt time.Time) {
	if mgr == nil {
		return
	}
	emitNotification(mgr, key, k.issueType, buildResolvedNotification(k, message, pipelineID, actionURL, subject, openedAt))
}

// resolvedInfrastructure answers an INFRASTRUCTURE_DOWN instance alert. It is the only
// instance-lane notice: the other component findings (a lost worker heartbeat, a closed
// queue reader) are not on the allowlist.
var resolvedInfrastructure = resolvedKind{
	issueType:    IssueTypeInfrastructureDown,
	resolvedCode: "INFRASTRUCTURE_DOWN_RESOLVED",
	message:      "A core service this deployment depends on is responding again.",
	onlyCritical: true,
}

// publishInstanceResolved tells every admin a core service that was reported down is
// back. issue is the finding resolveRecoveredIssues just closed. Nothing is sent for a
// finding that is not an INFRASTRUCTURE_DOWN that was pushed (sentinelAlertFor decides
// that, as it did for the alert), and the subject is the component id the alert used,
// so the api-gateway pairs it with that service's alert and no other.
func publishInstanceResolved(mgr *kafka.Manager, issue *Issue) {
	if issue == nil || issue.Type != IssueTypeInfrastructureDown {
		return
	}
	if _, pushed := sentinelAlertFor(issue.Type, "", issue.Severity); !pushed {
		return
	}
	message := resolvedInfrastructure.message
	if label := componentLabel(issue.ComponentID); label != "" {
		if svc, ok := coreServiceImpact[label]; ok {
			message = "The " + svc.name + " service is responding again."
		} else {
			message = "The " + label + " service is responding again."
		}
	}
	publishResolved(mgr, instanceScopeID, resolvedInfrastructure, instanceScopeID, instanceActionURL,
		issue.ComponentID, message, issue.DetectedAt)
}
