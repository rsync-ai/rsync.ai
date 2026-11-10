package sentinel

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Tests for the core-service probes (service_probes.go) and for what happens to the
// issue they raise: it has to stay open while the service is down, close when it comes
// back, and say in the alert what actually stops.

// fullProbeEnv is every address serviceProbesFromEnv reads, set.
func fullProbeEnv(key string) string {
	return map[string]string{
		"REDIS_ADDRESS":                         "redis.internal:6380",
		"TEMPORAL_ADDRESS":                      "temporal:7233",
		"SENTINEL_PROBE_TEMPORAL_ADAPTER_URL":   "http://temporal-adapter:8082",
		"SENTINEL_PROBE_CONNECTOR_DEPLOYER_URL": "http://connector-deployer:5011",
		"TOOL_GENERATOR_URL":                    "http://tool-generator:8000",
		"SENTINEL_PROBE_LLM_SERVICE_URL":        "http://llm-service:5000",
	}[key]
}

func probeIDs(probes []*serviceProbe) []string {
	ids := make([]string, 0, len(probes))
	for _, p := range probes {
		ids = append(ids, p.componentID)
	}
	sort.Strings(ids)
	return ids
}

func closeProbes(probes []*serviceProbe) {
	(&HealthMonitor{serviceProbes: probes}).closeServiceProbes()
}

// Redis is the one probe that runs everywhere, because the orchestrator cannot start
// without it. Every other probe exists only where its address is configured: a probe that
// guessed a compose service name on a Helm install would page every admin about a
// service that was never deployed.
func TestOnlyRedisIsProbedWithoutConfiguredAddresses(t *testing.T) {
	probes := serviceProbesFromEnv(func(string) string { return "" })
	defer closeProbes(probes)

	if got := probeIDs(probes); len(got) != 1 || got[0] != "infrastructure:redis" {
		t.Fatalf("probes with no addresses configured = %v, want only infrastructure:redis", got)
	}
	if probes[0].target != "redis:6379" {
		t.Errorf("redis probe target = %q, want the compose default the correlation client falls back to", probes[0].target)
	}
}

func TestEveryConfiguredCoreServiceIsProbed(t *testing.T) {
	probes := serviceProbesFromEnv(fullProbeEnv)
	defer closeProbes(probes)

	want := []string{
		"infrastructure:connector-deployer",
		"infrastructure:llm-service",
		"infrastructure:redis",
		"infrastructure:temporal",
		"infrastructure:temporal-adapter",
		"infrastructure:tool-generator",
	}
	if got := probeIDs(probes); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("probes = %v, want %v", got, want)
	}

	targets := map[string]string{}
	for _, p := range probes {
		targets[p.componentID] = p.target
	}
	// Each HTTP probe must hit the route that service really serves; a 404 would read as
	// an outage on every tick.
	for id, want := range map[string]string{
		"infrastructure:temporal-adapter":   "http://temporal-adapter:8082/version",
		"infrastructure:connector-deployer": "http://connector-deployer:5011/healthz",
		"infrastructure:tool-generator":     "http://tool-generator:8000/health",
		"infrastructure:llm-service":        "http://llm-service:5000/health",
		"infrastructure:temporal":           "temporal:7233",
		"infrastructure:redis":              "redis.internal:6380",
	} {
		if targets[id] != want {
			t.Errorf("%s target = %q, want %q", id, targets[id], want)
		}
	}
}

// The Redis probe has to look where the workers look (InitCorrelationClient), or it
// reports on a Redis nothing uses.
func TestTheRedisProbeResolvesTheCorrelationClientsAddress(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"REDIS_ADDRESS wins", map[string]string{"REDIS_ADDRESS": "a:1", "REDIS_ADDR": "b:2"}, "a:1"},
		{"REDIS_ADDR is the fallback", map[string]string{"REDIS_ADDR": "b:2"}, "b:2"},
		{"the compose name is the last resort", map[string]string{}, "redis:6379"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := redisServiceProbe(func(k string) string { return c.env[k] })
			defer p.close()
			if p.target != c.want {
				t.Errorf("target = %q, want %q", p.target, c.want)
			}
		})
	}
}

// The target is stored in component metadata, which /admin/health and the monitoring API
// both return.
func TestAProbeTargetNeverCarriesCredentials(t *testing.T) {
	p := httpServiceProbe(http.DefaultClient, "infrastructure:llm-service", "http://svc:sup3rs3cret@llm-service:5000/", "/health")
	if strings.Contains(p.target, "sup3rs3cret") || strings.Contains(p.target, "svc:") {
		t.Errorf("target keeps the userinfo: %q", p.target)
	}
	if p.target != "http://llm-service:5000/health" {
		t.Errorf("target = %q, want the URL with only its userinfo removed", p.target)
	}
	if got := redactURLUserinfo("http://[::1:5000/%zz"); got != "" {
		t.Errorf("an unparseable URL was stored as %q; it could be the secret", got)
	}
}

// /healthz answers 503 when the deployer cannot reach Docker. It cannot start a connector
// then, so that is down, not up — two-sided so a check that passed everything fails too.
func TestAnHTTPProbeCountsANon2xxAsDown(t *testing.T) {
	status := http.StatusOK
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()

	p := httpServiceProbe(srv.Client(), "infrastructure:connector-deployer", srv.URL+"/", "/healthz")

	if err := p.check(context.Background()); err != nil {
		t.Fatalf("200 graded as down: %v", err)
	}
	if gotPath != "/healthz" {
		t.Errorf("probed %q, want /healthz", gotPath)
	}

	status = http.StatusServiceUnavailable
	if err := p.check(context.Background()); err == nil {
		t.Fatal("503 graded as up; a deployer that cannot reach Docker would never be reported")
	}
}

func TestATCPProbeSeesAClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	p := tcpServiceProbe("infrastructure:temporal", addr)

	if err := p.check(context.Background()); err != nil {
		t.Fatalf("an open port graded as down: %v", err)
	}
	ln.Close()
	if err := p.check(context.Background()); err == nil {
		t.Fatal("a closed port graded as up")
	}
}

// One refused dial during a restart is not an outage. The misses before the threshold are
// recorded degraded (visible, not paged); the threshold-th is unhealthy (paged); a single
// success resets the count, so the next blip starts from zero again.
func TestAProbeIsReportedDownOnlyAfterConsecutiveFailures(t *testing.T) {
	var fail bool
	p := &serviceProbe{
		componentID: "infrastructure:temporal",
		targetKey:   "address",
		target:      "temporal:7233",
		check: func(context.Context) error {
			if fail {
				return errors.New("connection refused")
			}
			return nil
		},
	}
	h := &HealthMonitor{componentHealth: map[string]*ComponentHealth{}, serviceProbes: []*serviceProbe{p}}
	statusAfterTick := func() HealthStatus {
		h.checkServiceProbes(context.Background())
		return h.componentHealth[p.componentID].Status
	}

	fail = true
	for i := 1; i < serviceFailureThreshold; i++ {
		if got := statusAfterTick(); got != HealthStatusDegraded {
			t.Fatalf("failure %d recorded %q, want degraded", i, got)
		}
	}
	if got := statusAfterTick(); got != HealthStatusUnhealthy {
		t.Fatalf("failure %d recorded %q, want unhealthy", serviceFailureThreshold, got)
	}
	if got := statusAfterTick(); got != HealthStatusUnhealthy {
		t.Fatalf("a continuing outage recorded %q, want unhealthy", got)
	}

	fail = false
	if got := statusAfterTick(); got != HealthStatusHealthy {
		t.Fatalf("recovery recorded %q, want healthy", got)
	}
	if last := h.componentHealth[p.componentID].LastError; last != "" {
		t.Errorf("a recovered service still reports %q", last)
	}

	fail = true
	if got := statusAfterTick(); got != HealthStatusDegraded {
		t.Fatalf("the first miss after a recovery recorded %q, want degraded — the count was not reset", got)
	}
	if got := h.componentHealth[p.componentID].Metadata["address"]; got != "temporal:7233" {
		t.Errorf("metadata address = %v, want the probe target", got)
	}
}

// A recovered component's finding has to close, in memory and in the table — otherwise
// handleDetectedIssue sees the stale entry on the next outage and never alerts it.
//
// The degraded component is the other side: between probe misses is not a recovery. Its
// ids must not be in the DELETE (the exact WithArgs below would not match) and its issue
// must stay open.
func TestAHealthyComponentClosesItsFindingsAndADegradedOneDoesNot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	back := &ComponentHealth{ComponentID: "infrastructure:redis", Status: HealthStatusHealthy}
	wobbling := &ComponentHealth{ComponentID: "infrastructure:temporal", Status: HealthStatusDegraded}
	backIssue := generateIssueID(back.ComponentID, IssueTypeInfrastructureDown)
	wobblingIssue := generateIssueID(wobbling.ComponentID, IssueTypeInfrastructureDown)

	a := &Agent{
		db: db,
		activeIssues: map[string]*Issue{
			backIssue:     {ID: backIssue, ComponentID: back.ComponentID, Type: IssueTypeInfrastructureDown},
			wobblingIssue: {ID: wobblingIssue, ComponentID: wobbling.ComponentID, Type: IssueTypeInfrastructureDown},
		},
	}

	var args []driver.Value
	for _, it := range recoverableIssueTypes {
		args = append(args, generateIssueID(back.ComponentID, it))
	}
	mock.ExpectExec(`DELETE FROM sentinel_active_issues WHERE id IN`).
		WithArgs(args...).
		WillReturnResult(sqlmock.NewResult(0, 1))

	a.resolveRecoveredIssues(context.Background(), []*ComponentHealth{back, wobbling, nil})

	if _, open := a.activeIssues[backIssue]; open {
		t.Error("the recovered service's issue is still open; its next outage would never alert")
	}
	if _, open := a.activeIssues[wobblingIssue]; !open {
		t.Error("a degraded service's issue was closed; degraded is between probe misses, not a recovery")
	}
	if a.issuesResolved != 1 {
		t.Errorf("issuesResolved = %d, want 1", a.issuesResolved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the sentinel_active_issues row was not deleted as expected: %v", err)
	}
}

// An infrastructure outage is not something the healer can repair, and it is already
// delivered: publishInstanceIssueAlert sends the INFRASTRUCTURE_DOWN push before healing is
// considered. It used to map to a webhook "heal" that needed SENTINEL_ALERT_WEBHOOK_URL,
// which no deployment sets, so every outage also wrote a failed healing result and logged
// "Healing action failed" for a repair nobody could attempt. Now the healer has no action
// for it: nothing is graded, and the issue stays open until the component is healthy again.
func TestAnInfrastructureOutageIsNotGradedAsAFailedHeal(t *testing.T) {
	t.Setenv("SENTINEL_ALERT_WEBHOOK_URL", "") // unset, as on every deployment

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	config := DefaultSentinelConfig()
	issue := &Issue{
		ID:            generateIssueID("infrastructure:redis", IssueTypeInfrastructureDown),
		Type:          IssueTypeInfrastructureDown,
		Severity:      IssueSeverityCritical,
		ComponentID:   "infrastructure:redis",
		ComponentType: ComponentTypeInfrastructure,
	}
	a := &Agent{
		config:       config,
		activeIssues: map[string]*Issue{issue.ID: issue},
		healer:       NewHealer(nil, nil, config, nil, nil),
		logger:       NewAuditLogger(db, config),
		ctx:          context.Background(),
	}
	if action := a.healer.DetermineAction(issue); action != "" {
		t.Errorf("infrastructure down maps to healing action %q; the healer cannot repair an outage", action)
	}

	a.triggerHealing(issue)

	if a.healingActions != 0 {
		t.Errorf("healingActions = %d, want 0: an outage was graded as a healing attempt", a.healingActions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected statements: %v", err)
	}
	if _, open := a.activeIssues[issue.ID]; !open {
		t.Error("the issue was closed while the service is still down; its recovery would never be seen")
	}
	if a.issuesResolved != 0 {
		t.Errorf("issuesResolved = %d, want 0", a.issuesResolved)
	}
}

// Every service the probes can report has its own impact line. The generic line says
// pipelines cannot move data, which is false for the AI service and would be a page every
// admin learns to ignore.
func TestEveryProbedServiceSaysWhatStops(t *testing.T) {
	probes := serviceProbesFromEnv(fullProbeEnv)
	defer closeProbes(probes)
	if len(probes) == 0 {
		t.Fatal("vacuity floor: no probes built")
	}

	const secret = "sup3rs3cret"
	for _, p := range probes {
		label := componentLabel(p.componentID)
		svc, ok := coreServiceImpact[label]
		if !ok {
			t.Errorf("%s is probed but has no entry in coreServiceImpact; its alert would claim pipelines stopped", p.componentID)
			continue
		}
		n, ok := buildSentinelNotification(IssueTypeInfrastructureDown, IssueSeverityCritical, "", instanceScopeID,
			"Component is unhealthy: "+secret,
			map[string]interface{}{
				"component_id":   p.componentID,
				"component_type": string(ComponentTypeInfrastructure),
				"last_error":     "dial tcp: redis://:" + secret + "@redis:6379",
			})
		if !ok {
			t.Fatalf("%s: infrastructure down is not publishable", p.componentID)
		}
		if got := codeOf(t, n); got != "INFRASTRUCTURE_DOWN" {
			t.Errorf("%s: code = %q", p.componentID, got)
		}
		msg, _ := n["message"].(string)
		if !strings.Contains(msg, svc.name) || !strings.Contains(msg, svc.impact) {
			t.Errorf("%s: message %q does not carry its service name and impact", p.componentID, msg)
		}
		b, _ := json.Marshal(n)
		if strings.Contains(string(b), secret) {
			t.Errorf("%s: the alert quotes the probe's raw error: %s", p.componentID, b)
		}
	}

	llm, _ := buildSentinelNotification(IssueTypeInfrastructureDown, IssueSeverityCritical, "", instanceScopeID, "",
		map[string]interface{}{"component_id": "infrastructure:llm-service"})
	if msg, _ := llm["message"].(string); strings.Contains(msg, "cannot move data") {
		t.Errorf("the AI service alert says pipelines stopped: %q", msg)
	}
	// The data-path services keep the data-path sentence.
	pg, _ := buildSentinelNotification(IssueTypeInfrastructureDown, IssueSeverityCritical, "", instanceScopeID, "",
		map[string]interface{}{"component_id": "infrastructure:postgresql"})
	if msg, _ := pg["message"].(string); !strings.Contains(msg, "cannot move data") {
		t.Errorf("a postgres outage no longer says pipelines stopped: %q", msg)
	}
}

// infrastructureComponentIDs is every "infrastructure:<name>" id this package's code names,
// plus every probe built from a full environment. Read from the source rather than listed,
// so a component added later cannot slip past the outage-copy check below.
func infrastructureComponentIDs(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	probes := serviceProbesFromEnv(fullProbeEnv)
	defer closeProbes(probes)
	for _, p := range probes {
		seen[p.componentID] = true
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`"infrastructure:[a-z0-9-]+"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllString(string(src), -1) {
			seen[strings.Trim(m, `"`)] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// An outage alert used to end "nothing will resume on its own once it is back", and the
// connector services' said a stopped connector "will not come back on its own". Both were
// false: once the service is back a streaming pipeline continues from where it left off and
// a schedule runs at its next time. What can need a person is narrower, and the alert names
// it. Checked for every infrastructure component the package can raise an outage for, and
// for ids it has no entry for, which take the fallback copy.
func TestNoOutageAlertClaimsNothingRecovers(t *testing.T) {
	ids := append(infrastructureComponentIDs(t), "infrastructure:a-service-added-later", "")
	// Six probes, postgresql, kafka and kafka-connect, and the two appended above.
	if len(ids) < 11 {
		t.Fatalf("vacuity floor: only %d component ids to check: %v", len(ids), ids)
	}
	message := func(id string) string {
		n, ok := buildSentinelNotification(IssueTypeInfrastructureDown, IssueSeverityCritical, "", instanceScopeID, "",
			map[string]interface{}{"component_id": id})
		if !ok {
			t.Fatalf("%q: infrastructure down is not publishable", id)
		}
		msg, _ := n["message"].(string)
		return msg
	}
	for _, id := range ids {
		msg := message(id)
		if msg == "" {
			t.Errorf("%q: the alert has no message", id)
		}
		for _, claim := range []string{"on its own", "nothing will resume"} {
			if strings.Contains(msg, claim) {
				t.Errorf("%q: the alert says nothing recovers (%q): %s", id, claim, msg)
			}
		}
	}

	// The data path, and any id with no entry, say what continues and what may need a person.
	for _, id := range []string{"infrastructure:postgresql", "infrastructure:kafka", "infrastructure:a-service-added-later", ""} {
		msg := message(id)
		for _, want := range []string{"cannot move data", "continue from where they left off", "start it again"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q: the alert does not say %q: %s", id, want, msg)
			}
		}
	}

	// Kafka Connect runs only CDC capture (infra_preflight.go probes it for CDC runs alone),
	// so its outage must not say every pipeline has stopped.
	msg := message("infrastructure:kafka-connect")
	if strings.Contains(msg, "cannot move data") {
		t.Errorf("the Kafka Connect alert says every pipeline stopped: %s", msg)
	}
	for _, want := range []string{"Kafka Connect", "Streaming pipelines", "start it again"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the Kafka Connect alert does not say %q: %s", want, msg)
		}
	}
}
