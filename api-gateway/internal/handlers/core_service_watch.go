package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// The orchestrator's sentinel alerts every admin when a core service it depends on stops
// answering (backend-orchestrator sentinel/service_probes.go). Two outages it cannot
// report, and this watch covers from the gateway side:
//
//   - the orchestrator itself. A process cannot alert on its own death, and every
//     pipeline goes quiet with it.
//   - Kafka. The sentinel sees Kafka go down, but its alert is a Kafka message to
//     rsync.notifications — the one path Kafka being down takes out. This watch
//     delivers in-process through the notifier (Publish persists, dedups and sends),
//     so it needs no broker.
//
// Postgres and the gateway are the two it cannot cover: the notifier stores the alert
// and looks its recipients up in Postgres before sending, and a dead gateway runs no
// watch. Those need an uptime check from outside the deployment (self-hosting.md).

const (
	// coreServiceWatchInterval matches the sentinel's infrastructure tick, so both
	// sides take about the same time to call something down.
	coreServiceWatchInterval = 30 * time.Second
	// coreServiceWatchThreshold is how many consecutive failed checks make an outage.
	// One refused dial during a restart or a rolling deploy is not one.
	coreServiceWatchThreshold = 3
	// coreServiceWatchGrace is how long after the gateway starts before a failure
	// counts. On a stack coming up, the gateway can be serving before the orchestrator
	// has finished starting, and that is not an outage to page every admin about.
	coreServiceWatchGrace = 2 * time.Minute
)

// coreServiceCheck watches one service. failures and alerted belong to the single
// goroutine StartCoreServiceWatch runs, so they need no lock.
type coreServiceCheck struct {
	// subject is the alert's dedup subject, in the sentinel's component-id form so the
	// two producers' alerts about different services never share a dedup key.
	subject string
	name    string
	impact  string
	probe   func() serviceHealth

	failures int
	// downSince is when the current run of failed checks began; the resolved notice
	// sends it as opened_at.
	downSince time.Time
	// alerted is set once this outage has been delivered, so one outage is one alert.
	// It clears once the "responding again" notice has been delivered too.
	alerted bool
}

// dataPathImpact is what the Kafka outage alert says stops, and what does not. It used to
// end "nothing will resume on its own once it is back", which was false: a streaming
// pipeline continues from its last position and a schedule runs at its next time.
//
// The sentinel's outage alert says the same thing in the same words
// (backend-orchestrator/internal/agents/sentinel/notify.go dataPathImpact). The two are in
// different modules, so neither can import the other: change both or neither.
const dataPathImpact = "Pipelines cannot move data while it is down. Once it is back, streaming pipelines " +
	"continue from where they left off and schedules run at their next time, but a run that failed, " +
	"a pipeline that was stopped, or a scheduled time that passed during the outage may need you to start it again."

func defaultCoreServiceChecks() []*coreServiceCheck {
	return []*coreServiceCheck{
		{
			subject: "infrastructure:orchestrator",
			name:    "orchestrator",
			// Scoped to what is certain. Capture (Debezium) and the sink workers are
			// separate processes and may keep moving data; what stops is control through
			// the orchestrator (pipeline_cdc.go) and the sentinel that watches pipelines.
			impact: "CDC pipelines cannot be paused, resumed or restarted while it is down, and nothing watches the running ones: " +
				"the checks that detect a stalled pipeline and alert on it run inside it.",
			// /health answers 200 whenever the process is serving, whatever state its
			// subsystems are in, so a failure here is the process, not a dependency.
			probe: func() serviceHealth {
				return probeHTTPService("orchestrator", strings.TrimRight(orchestratorBaseURL(), "/")+"/health")
			},
		},
		{
			subject: "infrastructure:kafka",
			name:    "Kafka",
			impact:  dataPathImpact,
			probe:   checkKafka,
		},
	}
}

// StartCoreServiceWatch runs the watch until ctx is cancelled. With no publisher it does
// nothing: an outage it could not deliver is not worth probing for.
func StartCoreServiceWatch(ctx context.Context, p assessmentPublisher) {
	if p == nil {
		return
	}
	checks := defaultCoreServiceChecks()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(coreServiceWatchGrace):
		}
		ticker := time.NewTicker(coreServiceWatchInterval)
		defer ticker.Stop()
		for {
			for _, c := range checks {
				c.observe(ctx, p)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// observe runs one check and delivers the alert when this is the threshold-th failure in a
// row and the outage has not been delivered yet.
func (c *coreServiceCheck) observe(ctx context.Context, p assessmentPublisher) {
	h := c.probe()
	if h.Status == "up" {
		c.failures = 0
		if c.alerted {
			log.WithField("service", c.name).Info("core service watch: responding again")
			// Tell the admins the outage they were paged for is over. A notice that
			// cannot be delivered leaves the outage marked as alerted, so the next
			// tick sends it again; the notifier drops one that is not wanted
			// (api-gateway notifier/resolved.go), so sending is always safe.
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := p.Publish(pctx, coreServiceResolvedPayload(c)); err != nil {
				log.WithError(err).WithField("service", c.name).Warn("core service watch: could not deliver the recovery notice")
				return
			}
		}
		c.alerted = false
		return
	}

	c.failures++
	if c.failures == 1 {
		c.downSince = time.Now()
	}
	if c.failures < coreServiceWatchThreshold || c.alerted {
		return
	}

	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := p.Publish(pctx, coreServiceDownPayload(c)); err != nil {
		// Not marked delivered, so the next tick tries again. When the notifier cannot
		// store the alert, Postgres is usually the reason, and that is the one outage
		// no alert from inside the deployment can reach anybody about.
		log.WithError(err).WithField("service", c.name).Warn("core service watch: could not deliver the outage alert")
		return
	}
	c.alerted = true
	log.WithFields(log.Fields{"service": c.name, "failures": c.failures, "error": h.Error}).
		Error("core service watch: service is not responding; admins alerted")
}

// coreServiceDownPayload is the rsync.notifications-shaped event the sentinel publishes for
// INFRASTRUCTURE_DOWN (sentinel/notify.go buildSentinelNotification + publishInstanceAlert):
// instance scope, so it reaches every admin, and the same code, so it has the same copy
// and category and is muted by the same setting.
//
// The probe's error is not in it. For Kafka it names broker addresses, and neither
// producer puts a raw error in a notification.
func coreServiceDownPayload(c *coreServiceCheck) []byte {
	message := "The " + c.name + " service this deployment depends on is not responding. " + c.impact
	raw, _ := json.Marshal(map[string]interface{}{
		"type":        "infrastructure_down",
		"pipeline_id": "instance",
		"message":     message,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  "/admin/health",
		"severity":    "critical",
		"error": map[string]interface{}{
			"failure_type":  "system_error",
			"code":          "INFRASTRUCTURE_DOWN",
			"severity":      "error",
			"audience":      "user",
			"user_message":  message,
			"dedup_subject": c.subject,
		},
	})
	return raw
}

// coreServiceResolvedPayload is the "responding again" notice for the alert above: the
// same instance scope, action_url and dedup subject, so the notifier pairs it with that
// service's INFRASTRUCTURE_DOWN and with no other, and opened_at so it counts how long the
// outage lasted (sentinel/resolved_notice.go publishInstanceResolved sends the same shape).
func coreServiceResolvedPayload(c *coreServiceCheck) []byte {
	message := "The " + c.name + " service is responding again."
	body := map[string]interface{}{
		"type":        "alert_resolved",
		"pipeline_id": "instance",
		"message":     message,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
		"action_url":  "/admin/health",
		"severity":    "info",
		"error": map[string]interface{}{
			"failure_type":  "transient_error",
			"code":          "INFRASTRUCTURE_DOWN_RESOLVED",
			"severity":      "info",
			"audience":      "user",
			"user_message":  message,
			"dedup_subject": c.subject,
		},
	}
	if !c.downSince.IsZero() {
		body["opened_at"] = c.downSince.UTC().Format(time.RFC3339)
	}
	raw, _ := json.Marshal(body)
	return raw
}
