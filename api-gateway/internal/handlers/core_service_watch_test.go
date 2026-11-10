package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type recordingPublisher struct {
	sent [][]byte
	err  error
}

func (r *recordingPublisher) Publish(_ context.Context, raw []byte) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, raw)
	return nil
}

// downAlerts is the outage alerts among what was sent, so a test about alerting is not
// miscounted by the "responding again" notice that follows a recovery.
func (r *recordingPublisher) downAlerts() int {
	n := 0
	for _, raw := range r.sent {
		if strings.Contains(string(raw), `"type":"infrastructure_down"`) {
			n++
		}
	}
	return n
}

func (r *recordingPublisher) resolved() [][]byte {
	var out [][]byte
	for _, raw := range r.sent {
		if strings.Contains(string(raw), `"type":"alert_resolved"`) {
			out = append(out, raw)
		}
	}
	return out
}

func fakeCheck(up *bool) *coreServiceCheck {
	return &coreServiceCheck{
		subject: "infrastructure:orchestrator",
		name:    "orchestrator",
		impact:  "Impact sentence.",
		probe: func() serviceHealth {
			if *up {
				return serviceHealth{Service: "orchestrator", Status: "up"}
			}
			return serviceHealth{Service: "orchestrator", Status: "down",
				Error: "dial tcp kafka-user:sup3rs3cret@10.0.0.7:9092: connection refused"}
		},
	}
}

// One refused dial is not an outage, and one outage is one alert: nothing before the
// threshold, exactly one at it, nothing more while it lasts, and a fresh one for the
// next outage after a recovery.
func TestTheWatchAlertsOncePerOutageAfterTheThreshold(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	ctx := context.Background()

	for i := 1; i < coreServiceWatchThreshold; i++ {
		c.observe(ctx, pub)
		if pub.downAlerts() != 0 {
			t.Fatalf("alerted after %d failure(s); the threshold is %d", i, coreServiceWatchThreshold)
		}
	}
	c.observe(ctx, pub)
	if pub.downAlerts() != 1 {
		t.Fatalf("%d alerts at the threshold, want 1", pub.downAlerts())
	}
	c.observe(ctx, pub)
	c.observe(ctx, pub)
	if pub.downAlerts() != 1 {
		t.Fatalf("%d alerts for one continuing outage, want 1", pub.downAlerts())
	}

	up = true
	c.observe(ctx, pub)
	up = false
	for i := 0; i < coreServiceWatchThreshold-1; i++ {
		c.observe(ctx, pub)
	}
	if pub.downAlerts() != 1 {
		t.Fatalf("alerted before the threshold after a recovery; the failure count was not reset")
	}
	c.observe(ctx, pub)
	if pub.downAlerts() != 2 {
		t.Fatalf("%d alerts after a second outage, want 2 — the recovery did not re-arm the alert", pub.downAlerts())
	}
}

// A delivery that failed is not a delivered alert. Marking it sent anyway would make the
// one attempt that failed the only attempt.
func TestAFailedDeliveryIsRetriedNextTick(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{err: errors.New("notifier: persist: connection refused")}
	ctx := context.Background()

	for i := 0; i < coreServiceWatchThreshold; i++ {
		c.observe(ctx, pub)
	}
	if c.alerted {
		t.Fatal("a failed delivery was recorded as delivered")
	}

	pub.err = nil
	c.observe(ctx, pub)
	if pub.downAlerts() != 1 {
		t.Fatalf("%d alerts after the notifier recovered, want 1", pub.downAlerts())
	}
}

// The payload has to be the sentinel's INFRASTRUCTURE_DOWN shape: instance scope so it
// reaches every admin, the catalog code for its copy and category, a per-service dedup
// subject so two outages are two alerts — and never the probe's raw error.
func TestTheOutageAlertIsAnInstanceWideInfrastructureDown(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	for i := 0; i < coreServiceWatchThreshold; i++ {
		c.observe(context.Background(), pub)
	}
	if len(pub.sent) != 1 {
		t.Fatalf("no alert to inspect (%d sent)", len(pub.sent))
	}
	raw := pub.sent[0]
	if strings.Contains(string(raw), "sup3rs3cret") || strings.Contains(string(raw), "10.0.0.7") {
		t.Errorf("the alert carries the probe's raw error: %s", raw)
	}

	var got struct {
		Type       string `json:"type"`
		PipelineID string `json:"pipeline_id"`
		Message    string `json:"message"`
		ActionURL  string `json:"action_url"`
		Error      struct {
			Code         string `json:"code"`
			Audience     string `json:"audience"`
			UserMessage  string `json:"user_message"`
			DedupSubject string `json:"dedup_subject"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"type":          {got.Type, "infrastructure_down"},
		"pipeline_id":   {got.PipelineID, "instance"},
		"action_url":    {got.ActionURL, "/admin/health"},
		"code":          {got.Error.Code, "INFRASTRUCTURE_DOWN"},
		"audience":      {got.Error.Audience, "user"},
		"dedup_subject": {got.Error.DedupSubject, "infrastructure:orchestrator"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if !strings.Contains(got.Message, "orchestrator") || !strings.Contains(got.Message, "Impact sentence.") {
		t.Errorf("message does not name the service and what stops: %q", got.Message)
	}
	if got.Error.UserMessage != got.Message {
		t.Errorf("user_message %q differs from message %q", got.Error.UserMessage, got.Message)
	}
}

// Both watched services, each with copy and a subject of its own. A shared subject would
// fold "Kafka is down" into "the orchestrator is down" inside the notifier's dedup window.
func TestEveryWatchedServiceHasItsOwnSubjectAndCopy(t *testing.T) {
	checks := defaultCoreServiceChecks()
	want := map[string]bool{"infrastructure:orchestrator": false, "infrastructure:kafka": false}
	for _, c := range checks {
		if _, ok := want[c.subject]; !ok {
			t.Errorf("unexpected watched subject %q", c.subject)
			continue
		}
		if want[c.subject] {
			t.Errorf("subject %q is watched twice", c.subject)
		}
		want[c.subject] = true
		if c.name == "" || c.impact == "" || c.probe == nil {
			t.Errorf("%s has no name, impact or probe", c.subject)
		}
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("%s is not watched", s)
		}
	}
}

func TestTheWatchDoesNothingWithoutAPublisher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	StartCoreServiceWatch(ctx, nil) // must not panic or start probing
}

// The Kafka outage used to say "nothing will resume on its own once it is back", which is
// false: once Kafka answers, a streaming pipeline continues from where it left off and a
// schedule runs at its next time. It now says what can need a person instead, in the same
// words as the sentinel's outage alert (sentinel notify.go dataPathImpact).
func TestNoWatchedServiceClaimsNothingRecovers(t *testing.T) {
	checks := defaultCoreServiceChecks()
	if len(checks) == 0 {
		t.Fatal("vacuity floor: no watched services")
	}
	sawKafka := false
	for _, c := range checks {
		for _, claim := range []string{"on its own", "nothing will resume"} {
			if strings.Contains(c.impact, claim) {
				t.Errorf("%s: the alert says nothing recovers (%q): %s", c.subject, claim, c.impact)
			}
		}
		if c.subject == "infrastructure:kafka" {
			sawKafka = true
			for _, want := range []string{"cannot move data", "continue from where they left off", "start it again"} {
				if !strings.Contains(c.impact, want) {
					t.Errorf("the Kafka alert does not say %q: %s", want, c.impact)
				}
			}
		}
	}
	if !sawKafka {
		t.Error("Kafka is no longer watched, so the check above covered nothing")
	}
}

func (c *coreServiceCheck) failTimes(ctx context.Context, p assessmentPublisher, up *bool, n int) {
	*up = false
	for i := 0; i < n; i++ {
		c.observe(ctx, p)
	}
}

// An outage the admins were paged for ends with one notice saying so, for the same
// service (dedup subject), at the same place (instance scope, /admin/health), carrying when
// the outage began, and never the probe's raw error.
func TestARecoveryAfterAnAlertSendsOneResolvedNotice(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	ctx := context.Background()

	c.failTimes(ctx, pub, &up, coreServiceWatchThreshold)
	if pub.downAlerts() != 1 {
		t.Fatalf("no alert to answer (%d sent)", pub.downAlerts())
	}
	// Fixed in the past, then one more failure: opened_at must be the start of the run of
	// failures, not the most recent one.
	started := time.Now().Add(-12 * time.Minute).UTC().Truncate(time.Second)
	c.downSince = started
	c.failTimes(ctx, pub, &up, 1)

	up = true
	c.observe(ctx, pub)
	c.observe(ctx, pub) // still up: said once
	got := pub.resolved()
	if len(got) != 1 {
		t.Fatalf("%d resolved notices, want 1", len(got))
	}
	raw := got[0]
	if strings.Contains(string(raw), "sup3rs3cret") || strings.Contains(string(raw), "10.0.0.7") {
		t.Errorf("the notice carries the probe's raw error: %s", raw)
	}
	var n struct {
		Type       string `json:"type"`
		PipelineID string `json:"pipeline_id"`
		ActionURL  string `json:"action_url"`
		Severity   string `json:"severity"`
		Message    string `json:"message"`
		OpenedAt   string `json:"opened_at"`
		Error      struct {
			Code         string `json:"code"`
			Severity     string `json:"severity"`
			Audience     string `json:"audience"`
			DedupSubject string `json:"dedup_subject"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"type":          {n.Type, "alert_resolved"},
		"pipeline_id":   {n.PipelineID, "instance"},
		"action_url":    {n.ActionURL, "/admin/health"},
		"severity":      {n.Severity, "info"},
		"code":          {n.Error.Code, "INFRASTRUCTURE_DOWN_RESOLVED"},
		"error.sev":     {n.Error.Severity, "info"},
		"audience":      {n.Error.Audience, "user"},
		"dedup_subject": {n.Error.DedupSubject, "infrastructure:orchestrator"},
		"opened_at":     {n.OpenedAt, started.Format(time.RFC3339)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
	if !strings.Contains(n.Message, "orchestrator") {
		t.Errorf("message does not name the service: %q", n.Message)
	}
}

// The episode's opened_at is the FIRST failure of the run, taken by observe itself: the
// notifier counts "open for five minutes" from it, so a value that moved forward with
// each failed probe would make every outage look younger than it is.
func TestTheOutageStartIsTheFirstFailureNotTheLatest(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	ctx := context.Background()

	before := time.Now()
	c.observe(ctx, pub)
	first := c.downSince
	if first.IsZero() || first.Before(before) || time.Since(first) > 5*time.Second {
		t.Fatalf("downSince = %v after the first failed probe, want about now", first)
	}
	time.Sleep(20 * time.Millisecond)
	c.failTimes(ctx, pub, &up, coreServiceWatchThreshold+1)
	if !c.downSince.Equal(first) {
		t.Errorf("downSince moved from %v to %v while the outage went on", first, c.downSince)
	}

	// A recovery and a new outage start a new episode.
	up = true
	c.observe(ctx, pub)
	up = false
	time.Sleep(20 * time.Millisecond)
	c.observe(ctx, pub)
	if !c.downSince.After(first) {
		t.Errorf("a new outage kept the old start %v (now %v)", first, c.downSince)
	}
}

// A blip the admins were never paged for is not announced as over.
func TestARecoveryWithNoAlertSendsNothing(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	ctx := context.Background()

	c.failTimes(ctx, pub, &up, coreServiceWatchThreshold-1)
	up = true
	c.observe(ctx, pub)
	if len(pub.sent) != 0 {
		t.Fatalf("%d notices for an outage that never alerted, want 0", len(pub.sent))
	}
}

// A recovery notice that could not be delivered is retried, and until it lands the
// outage stays marked as alerted, so the next outage is not mistaken for this one.
func TestAFailedResolvedNoticeIsRetriedNextTick(t *testing.T) {
	up := false
	c := fakeCheck(&up)
	pub := &recordingPublisher{}
	ctx := context.Background()

	c.failTimes(ctx, pub, &up, coreServiceWatchThreshold)
	pub.err = errors.New("notifier: persist: connection refused")
	up = true
	c.observe(ctx, pub)
	if !c.alerted {
		t.Fatal("a recovery notice that failed was recorded as delivered")
	}

	pub.err = nil
	c.observe(ctx, pub)
	if len(pub.resolved()) != 1 {
		t.Fatalf("%d resolved notices after the notifier recovered, want 1", len(pub.resolved()))
	}
	if c.alerted {
		t.Error("the outage is still marked alerted after its recovery notice was delivered")
	}
}
