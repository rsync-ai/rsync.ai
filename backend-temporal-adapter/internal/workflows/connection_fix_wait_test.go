package workflows

import (
	"encoding/json"
	"strings"
	"testing"
)

// A run that stopped on a missing connection setting parked on a "Configure
// connections" dialog reading "Source: Unknown": the wait never said which
// connection or which setting, and its Resume sent no IDs, which blanked the
// ones the run had. These pin the wait details and the resume to the
// connection that failed.

// executorOutputViaJSON is the executor's Result as the workflow receives it:
// decoded from JSON, so lists are []interface{} of map[string]interface{}.
func executorOutputViaJSON(t *testing.T) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	raw := `{"missing_config":[{"table":"list_task","field":"list_id","direction":"source"},
		{"table":"list_comment","field":"list_id","direction":"source"}]}`
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestConnectionFixWaitNamesTheMissingSettingAndItsConnection(t *testing.T) {
	state := &WorkflowState{
		SourceConnectionID:      "src-1",
		DestinationConnectionID: "dst-1",
		SourceConnectorSnapshot: &ConnectorSnapshot{Type: "t-clickup"},
	}
	got := connectionFixWaitDetails(executorOutputViaJSON(t), state)

	missing, ok := got["missing_connections"].([]interface{})
	if !ok || len(missing) != 1 {
		t.Fatalf("want one missing connection (the source), got %#v", got["missing_connections"])
	}
	m := missing[0].(map[string]interface{})
	if m["direction"] != "source" || m["connection_id"] != "src-1" || m["connector_type"] != "t-clickup" {
		t.Fatalf("missing connection must name the source it failed on, got %#v", m)
	}
	fields, _ := m["missing_fields"].([]interface{})
	if len(fields) != 1 || fields[0] != "list_id" {
		t.Fatalf("want missing_fields [list_id] (once), got %#v", m["missing_fields"])
	}
	if got["source_type"] != "t-clickup" {
		t.Fatalf("source_type must come from the run's connector, got %#v", got["source_type"])
	}
	desc := waitingEventDescription("connection_config", got)
	if !strings.Contains(desc, "list_id") || strings.Contains(strings.ToLower(desc), "unknown") {
		t.Fatalf("the wait must name the setting, got %q", desc)
	}
}

func TestConnectionFixWaitTakesTheTypeFromTheRunBeforeTheIntent(t *testing.T) {
	// A pipeline started from Run has no cached intent; its type is on the
	// resolved connector type, else its pinned snapshot.
	cases := []struct {
		name  string
		state *WorkflowState
		want  string
	}{
		{"resolved type", &WorkflowState{SourceConnectorType: "postgresql",
			SourceConnectorSnapshot: &ConnectorSnapshot{Type: "other"}}, "postgresql"},
		{"snapshot", &WorkflowState{SourceConnectorSnapshot: &ConnectorSnapshot{Type: "t-clickup"}}, "t-clickup"},
		{"intent", &WorkflowState{CachedIntent: map[string]interface{}{"source_type": "mysql"}}, "mysql"},
		{"nested intent", &WorkflowState{CachedIntent: map[string]interface{}{
			"intent": map[string]interface{}{"source_type": "mongodb"}}}, "mongodb"},
	}
	for _, c := range cases {
		got := connectionFixWaitDetails(nil, c.state)
		if got["source_type"] != c.want {
			t.Errorf("%s: source_type = %#v, want %q", c.name, got["source_type"], c.want)
		}
	}
	// A type the executor already reported is kept.
	got := connectionFixWaitDetails(map[string]interface{}{"destination_type": "bigquery"},
		&WorkflowState{DestinationConnectorType: "mongodb"})
	if got["destination_type"] != "bigquery" {
		t.Fatalf("destination_type = %#v, want the reported bigquery", got["destination_type"])
	}
}

func TestConnectionFixWaitWithNoNamedSettingKeepsTheOldShape(t *testing.T) {
	// An auth or config failure with no setting name: no missing_connections,
	// so the dialog still offers both connections as before.
	got := connectionFixWaitDetails(map[string]interface{}{"agent_error": "invalid configuration"},
		&WorkflowState{SourceConnectionID: "s", DestinationConnectionID: "d"})
	if _, ok := got["missing_connections"]; ok {
		t.Fatalf("no setting named, so no missing_connections; got %#v", got["missing_connections"])
	}
	if got["request_type"] != "connection_config" || got["source_connection_id"] != "s" || got["destination_connection_id"] != "d" {
		t.Fatalf("UX hints missing: %#v", got)
	}
}

func TestResumeKeepsAConnectionIDTheSignalLeftBlank(t *testing.T) {
	cases := []struct {
		name             string
		p                ConnectionsConfiguredPayload
		wantSrc, wantDst string
	}{
		{"blank resume keeps both", ConnectionsConfiguredPayload{}, "src-1", "dst-1"},
		{"new source only", ConnectionsConfiguredPayload{SourceConnectionID: "src-2"}, "src-2", "dst-1"},
		{"whitespace is blank", ConnectionsConfiguredPayload{DestinationConnectionID: "  "}, "src-1", "dst-1"},
		{"both new", ConnectionsConfiguredPayload{SourceConnectionID: "a", DestinationConnectionID: "b"}, "a", "b"},
	}
	for _, c := range cases {
		src, dst := resumedConnectionIDs("src-1", "dst-1", c.p)
		if src != c.wantSrc || dst != c.wantDst {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, src, dst, c.wantSrc, c.wantDst)
		}
	}
}
